package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pedro-mueller/conduit/internal/announce"
	"github.com/pedro-mueller/conduit/internal/breaker"
	"github.com/pedro-mueller/conduit/internal/capture"
	"github.com/pedro-mueller/conduit/internal/classify"
	"github.com/pedro-mueller/conduit/internal/config"
	"github.com/pedro-mueller/conduit/internal/metrics"
	"github.com/pedro-mueller/conduit/internal/notify"
	"github.com/pedro-mueller/conduit/internal/redact"
)

const anthropicUpstream = "anthropic"

type Gateway struct {
	cfg     config.Config
	breaker *breaker.Breaker
	capture *capture.Writer
	metrics *metrics.Counters
	client  *http.Client
	log     *slog.Logger

	lastMu           sync.Mutex
	lastProvider     string
	lastUpstream     string
	lastAt           time.Time
}

func New(cfg config.Config, br *breaker.Breaker, cap *capture.Writer, met *metrics.Counters, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		cfg:     cfg,
		breaker: br,
		capture: cap,
		metrics: met,
		client: &http.Client{
			// No global Timeout — streaming requests can run a long time.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
		log: log,
	}
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_gateway/status", g.handleStatus)
	mux.HandleFunc("/_gateway/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/_gateway/route", g.handleRoute)
	mux.HandleFunc("/_gateway/ui", g.handleUI)
	mux.HandleFunc("/", g.handleProxy)
	return mux
}

func (g *Gateway) handleRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"forced_provider": g.breaker.ForcedProvider(),
			"forced_model":    g.breaker.ForcedModel(),
			"available":       g.availableModels(),
			"last_request":    g.lastRequestSnapshot(),
		})
	case http.MethodPost:
		var req struct {
			Clear    bool   `json:"clear"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		if req.Clear {
			g.breaker.ClearForce()
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "automatic"})
			return
		}
		switch req.Provider {
		case "", "anthropic", "glm":
		case "deepseek":
			if g.cfg.DeepSeekAPIKey == "" {
				http.Error(w, `{"error":"deepseek tier disabled (no DEEPSEEK_API_KEY)"}`, http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, `{"error":"provider must be anthropic|glm|deepseek or use clear"}`, http.StatusBadRequest)
			return
		}
		g.breaker.SetForce(req.Provider, req.Model)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "forced", "provider": req.Provider, "model": req.Model})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// availableModels lists selectable upstream models per provider for the UI.
func (g *Gateway) availableModels() map[string][]string {
	glm := []string{"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-4.5-air"}
	for k := range g.cfg.GLM.ModelMap {
		glm = append(glm, k)
	}
	ds := []string{"deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"}
	return map[string][]string{
		"anthropic": {"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"},
		"glm":       glm,
		"deepseek":  ds,
	}
}

func (g *Gateway) lastRequestSnapshot() map[string]string {
	g.lastMu.Lock()
	defer g.lastMu.Unlock()
	if g.lastAt.IsZero() {
		return nil
	}
	return map[string]string{
		"provider":       g.lastProvider,
		"upstream_model": g.lastUpstream,
		"at":             g.lastAt.UTC().Format(time.RFC3339),
	}
}

func (g *Gateway) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, uiHTML)
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := metrics.StatusResponse{
		Listen:  g.cfg.Listen,
		Routing: g.breaker.RoutingProvider(),
		Breaker: g.breaker.Snapshot(),
		Counts:  g.metrics.Snapshot(),
		Upstream: map[string]string{
			"anthropic": g.cfg.Anthropic.BaseURL,
			"glm":       g.cfg.GLM.BaseURL,
		},
		ForcedProvider: g.breaker.ForcedProvider(),
		ForcedModel:    g.breaker.ForcedModel(),
		LastRequest:    g.lastRequestSnapshot(),
	}
	if g.cfg.DeepSeekAPIKey != "" {
		resp.Upstream["deepseek"] = g.cfg.DeepSeek.BaseURL
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	model := extractModel(body)

	switch g.breaker.ForcedProvider() {
	case "glm":
		g.serveGLM(w, r, body, model, start, false)
		return
	case "deepseek":
		if !g.tryDeepSeek(w, r, body, model, start, false, "forced") {
			http.Error(w, `{"type":"error","error":{"type":"gateway_error","message":"route forced to deepseek but DEEPSEEK_API_KEY is unset"}}`, http.StatusBadGateway)
			g.logRequest(r, model, model, "deepseek", 502, start, false)
		}
		return
	case "anthropic":
		g.serveAnthropic(w, r, body, model, start, false)
		return
	}

	state := g.breaker.Decide(anthropicUpstream, model, time.Now())

	switch state {
	case breaker.Open:
		g.serveGLM(w, r, body, model, start, false)
		return
	case breaker.Probe:
		g.serveAnthropic(w, r, body, model, start, true)
		return
	default:
		g.serveAnthropic(w, r, body, model, start, false)
	}
}

func (g *Gateway) serveAnthropic(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, isProbe bool) {
	var lastResp *http.Response
	var lastBody []byte
	var lastErr error

	attempts := 1 + g.cfg.Anthropic.MaxTransientRetries
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			g.metrics.IncTransientRetry()
			backoff := jitteredBackoff(attempt)
			g.log.Info("retrying anthropic after transient error",
				"attempt", attempt, "backoff", backoff.String(), "model", model)
			time.Sleep(backoff)
		}

		resp, respBody, peeked, err := g.roundTrip("anthropic", g.cfg.Anthropic.BaseURL, r, body, "", model)
		if err != nil {
			lastErr = err
			continue
		}
		lastResp = resp
		lastBody = respBody

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// Proactive quota check on success headers.
			if ok, reason, until := classify.ProactiveQuota(
				resp.Header,
				g.cfg.Breaker.ProactiveThreshold,
				g.cfg.Breaker.ProactiveUtilization,
			); ok {
				if g.breaker.Open(anthropicUpstream, model, "proactive_"+reason, until, time.Now()) {
					notify.FailoverToGLM(model, g.glmModelFor(model), "proactive_"+reason)
				}
			}
			if isProbe {
				if g.breaker.Close(anthropicUpstream, model, time.Now()) {
					notify.BackToAnthropic(model)
				}
			}
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, "anthropic", false, "")
			g.logRequest(r, model, model, "anthropic", resp.StatusCode, start, false)
			return
		}

		class := classify.ClassifyAnthropic(resp.StatusCode, resp.Header, respBody)
		_ = g.capture.Write(capture.FromResponse("anthropic", r.Method, r.URL.Path, model, resp.StatusCode, resp.Header, respBody, class.Reason))

		switch class.Kind {
		case classify.Quota:
			if g.breaker.Open(anthropicUpstream, model, class.Reason, class.Until, time.Now()) {
				notify.FailoverToGLM(model, g.glmModelFor(model), class.Reason)
			}
			_ = resp.Body.Close()
			// Pre-stream (or non-stream) failover: client has not seen bytes yet
			// because we buffered the error body before writing. Suppressed when
			// routing is manually pinned to anthropic — the user chose to eat
			// quota errors rather than fail over.
			if !peeked && g.breaker.ForcedProvider() != "anthropic" {
				g.metrics.IncFailover()
				g.serveGLM(w, r, body, model, start, true)
				return
			}
			// Mid-stream: already copied error? Shouldn't happen — we only peek
			// non-2xx before writing. If peeked somehow, surface.
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, "anthropic", false, "")
			g.logRequest(r, model, model, "anthropic", resp.StatusCode, start, false)
			return

		case classify.Transient:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("transient %s", class.Reason)
			continue

		case classify.Auth, classify.ClientError, classify.Other:
			if isProbe {
				// Auth/client errors during probe do not re-open; leave probe
				// state so a later request can try again, or close on next success.
			}
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, "anthropic", false, "")
			g.logRequest(r, model, model, "anthropic", resp.StatusCode, start, false)
			return
		}
	}

	if lastResp != nil {
		g.metrics.IncAnthropic()
		g.writeUpstream(w, lastResp, lastBody, "anthropic", false, "")
		g.logRequest(r, model, model, "anthropic", lastResp.StatusCode, start, false)
		return
	}
	msg := "upstream anthropic unreachable"
	if lastErr != nil {
		msg = redact.String(lastErr.Error())
	}
	http.Error(w, msg, http.StatusBadGateway)
	g.logRequest(r, model, model, "anthropic", 502, start, false)
}

func (g *Gateway) serveGLM(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool) {
	glmModel, ok := g.cfg.MapModel(model)
	if !ok {
		http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"gateway_error","message":"no GLM model mapping for %q and default_model unset"}}`, model), http.StatusBadGateway)
		g.logRequest(r, model, model, "glm", 502, start, failover)
		return
	}
	if g.breaker.ForcedProvider() == "glm" {
		if m := g.breaker.ForcedModel(); m != "" {
			glmModel = m
		}
	}
	rewritten, err := rewriteModel(body, glmModel)
	if err != nil {
		// Non-JSON body (e.g. GET): forward as-is.
		rewritten = body
	}

	resp, respBody, _, err := g.roundTrip("glm", g.cfg.GLM.BaseURL, r, rewritten, g.cfg.ZAIAPIKey, glmModel)
	if err != nil {
		if g.tryDeepSeek(w, r, body, model, start, failover, "glm_unreachable") {
			return
		}
		http.Error(w, redact.String(err.Error()), http.StatusBadGateway)
		g.logRequest(r, model, glmModel, "glm", 502, start, failover)
		return
	}
	if resp.StatusCode >= 400 {
		_ = g.capture.Write(capture.FromResponse("glm", r.Method, r.URL.Path, glmModel, resp.StatusCode, resp.Header, respBody, "glm_error"))
	}
	if shouldFallToDeepSeek(resp.StatusCode) && g.tryDeepSeek(w, r, body, model, start, failover, fmt.Sprintf("glm_%d", resp.StatusCode)) {
		_ = resp.Body.Close()
		return
	}
	g.metrics.IncGLM()
	// Chat notice on the failover turn (and only when enabled) so Claude Code
	// surfaces the switch inside the conversation.
	announceChat := failover && notify.ChatNoticeEnabled() && resp.StatusCode >= 200 && resp.StatusCode < 300
	g.writeUpstream(w, resp, respBody, "glm", announceChat, announce.DefaultNotice)
	g.logRequest(r, model, glmModel, "glm", resp.StatusCode, start, failover)
}

// glmModelFor resolves the upstream GLM model for route/notify purposes;
// returns the inbound ID unchanged when no mapping exists.
func (g *Gateway) glmModelFor(model string) string {
	if m, ok := g.cfg.MapModel(model); ok {
		return m
	}
	return model
}

// shouldFallToDeepSeek reports whether a GLM failure is worth retrying on the
// DeepSeek tier: auth problems (stale GLM key), throttling, and server errors.
// 4xx client errors (400 bad request etc.) would fail identically downstream.
func shouldFallToDeepSeek(status int) bool {
	switch status {
	case 401, 403, 408, 429:
		return true
	}
	return status >= 500
}

// tryDeepSeek serves the request from the DeepSeek tier when configured.
// Returns false (untouched) when the tier is disabled; the caller must then
// surface its own error.
func (g *Gateway) tryDeepSeek(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool, reason string) bool {
	if g.cfg.DeepSeekAPIKey == "" {
		return false
	}
	g.serveDeepSeek(w, r, body, model, start, failover, reason)
	return true
}

func (g *Gateway) serveDeepSeek(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool, reason string) {
	dsModel, ok := g.cfg.MapModelDeepSeek(model)
	if !ok {
		http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"gateway_error","message":"no DeepSeek model mapping for %q and default_model unset"}}`, model), http.StatusBadGateway)
		g.logRequest(r, model, model, "deepseek", 502, start, failover)
		return
	}
	if g.breaker.ForcedProvider() == "deepseek" {
		if m := g.breaker.ForcedModel(); m != "" {
			dsModel = m
		}
	}
	notify.FailoverToDeepSeek(model, dsModel, reason)
	rewritten, err := rewriteModel(body, dsModel)
	if err != nil {
		rewritten = body
	}

	resp, respBody, _, err := g.roundTrip("deepseek", g.cfg.DeepSeek.BaseURL, r, rewritten, g.cfg.DeepSeekAPIKey, dsModel)
	if err != nil {
		http.Error(w, redact.String(err.Error()), http.StatusBadGateway)
		g.logRequest(r, model, dsModel, "deepseek", 502, start, failover)
		return
	}
	if resp.StatusCode >= 400 {
		_ = g.capture.Write(capture.FromResponse("deepseek", r.Method, r.URL.Path, dsModel, resp.StatusCode, resp.Header, respBody, "deepseek_error"))
	}
	g.metrics.IncDeepSeek()
	announceChat := failover && notify.ChatNoticeEnabled() && resp.StatusCode >= 200 && resp.StatusCode < 300
	g.writeUpstream(w, resp, respBody, "deepseek", announceChat, announce.DeepSeekNotice)
	g.logRequest(r, model, dsModel, "deepseek", resp.StatusCode, start, failover)
}

// roundTrip performs the upstream request. For non-2xx responses it reads the
// full body into memory (for classification / failover) and returns peeked=false
// meaning nothing has been written to the client. For 2xx it returns the live
// body for streaming copy; peeked is unused in that path.
// apiKey replaces inbound credentials when non-empty; empty forwards as-is
// (Anthropic passthrough).
func (g *Gateway) roundTrip(provider, baseURL string, r *http.Request, body []byte, apiKey string, model string) (*http.Response, []byte, bool, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, nil, false, err
	}
	target := *r.URL
	target.Scheme = u.Scheme
	target.Host = u.Host
	// Preserve path; if baseURL includes a path prefix (e.g. /api/anthropic), join it.
	basePath := strings.TrimRight(u.Path, "/")
	if basePath != "" {
		target.Path = basePath + r.URL.Path
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, false, err
	}
	copyHeaders(req.Header, r.Header)
	// Strip hop-by-hop
	req.Header.Del("Host")
	req.Host = u.Host

	if apiKey != "" {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		// OAuth / experimental betas are Anthropic-subscription specific and
		// have been observed to make Z.ai reject requests (provider error 1210).
		req.Header.Del("Anthropic-Beta")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-Api-Key", apiKey)
		if req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, nil, false, err
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Success: leave body open for streaming / passthrough.
		return resp, nil, false, nil
	}

	// Error: buffer body so we can classify before touching the client.
	// For SSE errors that somehow return non-2xx with a stream, still buffer —
	// pre-stream rule: we have not written to the client yet.
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp, nil, false, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	_ = isSSE
	_ = provider
	_ = model
	return resp, buf, false, nil
}

// writeUpstream copies resp to the client. If bodyBuf is non-nil it is used
// instead of resp.Body (already-buffered error). For live streams, bodyBuf is nil.
// When chatNotice is true, notice is injected into the first assistant text so
// it appears inside Claude Code.
func (g *Gateway) writeUpstream(w http.ResponseWriter, resp *http.Response, bodyBuf []byte, provider string, chatNotice bool, notice string) {
	defer resp.Body.Close()

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	// Non-SSE success bodies are streamed live; buffer them when we need to inject.
	if chatNotice && !isSSE && bodyBuf == nil {
		buf, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err == nil {
			bodyBuf = announce.InjectJSON(buf, notice)
		} else {
			bodyBuf = buf
		}
	} else if chatNotice && bodyBuf != nil && !isSSE {
		bodyBuf = announce.InjectJSON(bodyBuf, notice)
	}

	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "connection" || lk == "keep-alive" || lk == "transfer-encoding" || lk == "proxy-connection" {
			continue
		}
		// Body length may change after notice injection.
		if chatNotice && lk == "content-length" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Conduit-Provider", provider)
	if chatNotice {
		w.Header().Set("X-Conduit-Notice", provider+"-failover")
	}
	// Disable buffering for SSE where possible.
	if isSSE {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
	}

	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	var src io.Reader
	if bodyBuf != nil {
		src = bytes.NewReader(bodyBuf)
	} else {
		src = resp.Body
	}
	if chatNotice && isSSE {
		src = announce.NewSSEInjector(src, notice)
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (g *Gateway) logRequest(r *http.Request, model, upstreamModel, provider string, status int, start time.Time, failover bool) {
	if upstreamModel == "" {
		upstreamModel = model
	}
	g.lastMu.Lock()
	g.lastProvider = provider
	g.lastUpstream = upstreamModel
	g.lastAt = time.Now()
	g.lastMu.Unlock()
	g.log.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"model", model,
		"upstream_model", upstreamModel,
		"provider", provider,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
		"failover", failover,
	)
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		if lk == "connection" || lk == "keep-alive" || lk == "proxy-authenticate" ||
			lk == "proxy-authorization" || lk == "te" || lk == "trailers" ||
			lk == "transfer-encoding" || lk == "upgrade" || lk == "content-length" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Model
}

// rewriteModel changes only the "model" JSON string value, preserving the rest
// of the body bytes as far as encoding/json allows. For prompt-cache safety on
// the Anthropic path we never call this; on the GLM path byte-identity of the
// Anthropic body is not required.
func rewriteModel(body []byte, newModel string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	b, err := json.Marshal(newModel)
	if err != nil {
		return nil, err
	}
	obj["model"] = b
	return json.Marshal(obj)
}

func jitteredBackoff(attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt)) * 200 * time.Millisecond
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base/2 + jitter
}

// MidStreamError is recorded when an upstream stream fails after bytes were sent.
// The current design classifies errors only from the response status line before
// any body bytes are written; true mid-stream SSE errors arrive as stream events
// and are proxied as-is (no splice). This helper exists for integration tests
// that simulate a mid-stream failure via a custom RoundTripper.
func DrainContext(ctx context.Context) error {
	return ctx.Err()
}
