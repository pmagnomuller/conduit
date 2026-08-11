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
	"time"

	"github.com/pedro-mueller/claude-glm-gateway/internal/breaker"
	"github.com/pedro-mueller/claude-glm-gateway/internal/capture"
	"github.com/pedro-mueller/claude-glm-gateway/internal/classify"
	"github.com/pedro-mueller/claude-glm-gateway/internal/config"
	"github.com/pedro-mueller/claude-glm-gateway/internal/metrics"
	"github.com/pedro-mueller/claude-glm-gateway/internal/redact"
)

const anthropicUpstream = "anthropic"

type Gateway struct {
	cfg     config.Config
	breaker *breaker.Breaker
	capture *capture.Writer
	metrics *metrics.Counters
	client  *http.Client
	log     *slog.Logger
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
	mux.HandleFunc("/", g.handleProxy)
	return mux
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := metrics.StatusResponse{
		Listen:  g.cfg.Listen,
		Breaker: g.breaker.Snapshot(),
		Counts:  g.metrics.Snapshot(),
		Upstream: map[string]string{
			"anthropic": g.cfg.Anthropic.BaseURL,
			"glm":       g.cfg.GLM.BaseURL,
		},
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

		resp, respBody, peeked, err := g.roundTrip("anthropic", g.cfg.Anthropic.BaseURL, r, body, false, model)
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
				g.breaker.Open(anthropicUpstream, model, "proactive_"+reason, until, time.Now())
			}
			if isProbe {
				g.breaker.Close(anthropicUpstream, model, time.Now())
			}
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, peeked)
			g.logRequest(r, model, "anthropic", resp.StatusCode, start, false)
			return
		}

		class := classify.ClassifyAnthropic(resp.StatusCode, resp.Header, respBody)
		_ = g.capture.Write(capture.FromResponse("anthropic", r.Method, r.URL.Path, model, resp.StatusCode, resp.Header, respBody, class.Reason))

		switch class.Kind {
		case classify.Quota:
			g.breaker.Open(anthropicUpstream, model, class.Reason, class.Until, time.Now())
			_ = resp.Body.Close()
			// Pre-stream (or non-stream) failover: client has not seen bytes yet
			// because we buffered the error body before writing.
			if !peeked {
				g.metrics.IncFailover()
				g.serveGLM(w, r, body, model, start, true)
				return
			}
			// Mid-stream: already copied error? Shouldn't happen — we only peek
			// non-2xx before writing. If peeked somehow, surface.
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, false)
			g.logRequest(r, model, "anthropic", resp.StatusCode, start, false)
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
			g.writeUpstream(w, resp, respBody, false)
			g.logRequest(r, model, "anthropic", resp.StatusCode, start, false)
			return
		}
	}

	if lastResp != nil {
		g.metrics.IncAnthropic()
		g.writeUpstream(w, lastResp, lastBody, false)
		g.logRequest(r, model, "anthropic", lastResp.StatusCode, start, false)
		return
	}
	msg := "upstream anthropic unreachable"
	if lastErr != nil {
		msg = redact.String(lastErr.Error())
	}
	http.Error(w, msg, http.StatusBadGateway)
	g.logRequest(r, model, "anthropic", 502, start, false)
}

func (g *Gateway) serveGLM(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool) {
	glmModel, ok := g.cfg.MapModel(model)
	if !ok {
		http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"gateway_error","message":"no GLM model mapping for %q and default_model unset"}}`, model), http.StatusBadGateway)
		g.logRequest(r, model, "glm", 502, start, failover)
		return
	}
	rewritten, err := rewriteModel(body, glmModel)
	if err != nil {
		// Non-JSON body (e.g. GET): forward as-is.
		rewritten = body
	}

	resp, respBody, peeked, err := g.roundTrip("glm", g.cfg.GLM.BaseURL, r, rewritten, true, glmModel)
	if err != nil {
		http.Error(w, redact.String(err.Error()), http.StatusBadGateway)
		g.logRequest(r, model, "glm", 502, start, failover)
		return
	}
	if resp.StatusCode >= 400 {
		_ = g.capture.Write(capture.FromResponse("glm", r.Method, r.URL.Path, glmModel, resp.StatusCode, resp.Header, respBody, "glm_error"))
	}
	g.metrics.IncGLM()
	g.writeUpstream(w, resp, respBody, peeked)
	g.logRequest(r, model, "glm", resp.StatusCode, start, failover)
}

// roundTrip performs the upstream request. For non-2xx responses it reads the
// full body into memory (for classification / failover) and returns peeked=false
// meaning nothing has been written to the client. For 2xx it returns the live
// body for streaming copy; peeked is unused in that path.
func (g *Gateway) roundTrip(provider, baseURL string, r *http.Request, body []byte, useGLMAuth bool, model string) (*http.Response, []byte, bool, error) {
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

	if useGLMAuth {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		// OAuth / experimental betas are Anthropic-subscription specific and
		// have been observed to make Z.ai reject requests (provider error 1210).
		req.Header.Del("Anthropic-Beta")
		req.Header.Set("Authorization", "Bearer "+g.cfg.ZAIAPIKey)
		req.Header.Set("X-Api-Key", g.cfg.ZAIAPIKey)
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
func (g *Gateway) writeUpstream(w http.ResponseWriter, resp *http.Response, bodyBuf []byte, _ bool) {
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "connection" || lk == "keep-alive" || lk == "transfer-encoding" || lk == "proxy-connection" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	// Disable buffering for SSE where possible.
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
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

func (g *Gateway) logRequest(r *http.Request, model, provider string, status int, start time.Time, failover bool) {
	g.log.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"model", model,
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
