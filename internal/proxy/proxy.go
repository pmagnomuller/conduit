package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime"
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
	"github.com/pedro-mueller/conduit/internal/route"
)

const anthropicUpstream = "anthropic"

// maxProxyBodyBytes caps the inbound proxy body before anything reads it.
// /v1/messages bodies carry the whole conversation plus tool schemas, so the
// cap is generous; it exists to bound the gateway's own memory — the body is
// read once to find the model field and, in jev mode, parsed a second time for
// the Jev dossier — rather than to police clients. A body past the cap would
// almost certainly be rejected upstream anyway.
const maxProxyBodyBytes = 32 << 20 // 32 MiB

// Decider is the per-call router consulted in jev mode. *route.Router
// satisfies it; nil means jev is unavailable.
type Decider interface {
	Enabled() bool
	Catalog() []route.Candidate
	Decide(ctx context.Context, body []byte, candidates []route.Candidate) (route.Decision, bool)
	Recent(n int) []route.Decision
}

// decisionKey carries the jev decision source through the request context so
// logRequest can report it regardless of which serve* path handled the call.
type decisionKey struct{}

type Gateway struct {
	cfg     config.Config
	breaker *breaker.Breaker
	capture *capture.Writer
	metrics *metrics.Counters
	router  Decider
	client  *http.Client
	log     *slog.Logger

	lastMu       sync.Mutex
	lastProvider string
	lastUpstream string
	lastAt       time.Time
}

// New builds the gateway. router may be nil (jev mode disabled).
func New(cfg config.Config, br *breaker.Breaker, cap *capture.Writer, met *metrics.Counters, router Decider, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		cfg:     cfg,
		breaker: br,
		capture: cap,
		metrics: met,
		router:  router,
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

func (g *Gateway) jevEnabled() bool {
	return g.router != nil && g.router.Enabled()
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
			"mode":            string(g.breaker.Mode()),
			"forced_provider": g.breaker.ForcedProvider(),
			"forced_model":    g.breaker.ForcedModel(),
			"available":       g.availableModels(),
			"jev":             g.jevSnapshot(),
			"last_request":    g.lastRequestSnapshot(),
		})
	case http.MethodPost:
		if !g.routePostAllowed(w, r) {
			return
		}
		var req struct {
			Clear    bool   `json:"clear"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Mode     string `json:"mode"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		var mode route.Mode
		if req.Mode != "" {
			m, ok := route.ParseMode(req.Mode)
			if !ok {
				http.Error(w, `{"error":"mode must be auto|pinned|jev"}`, http.StatusBadRequest)
				return
			}
			mode = m
		}
		switch {
		case req.Clear:
			g.breaker.ClearForce()
		case req.Provider != "":
			// Legacy pin shape; an explicit non-pinned mode alongside it is contradictory.
			if mode != "" && mode != route.ModePinned {
				http.Error(w, `{"error":"provider implies pinned mode; drop mode or use pinned"}`, http.StatusBadRequest)
				return
			}
			switch req.Provider {
			case "anthropic", "glm":
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
		case mode == route.ModeJev:
			if !g.jevEnabled() {
				http.Error(w, `{"error":"jev disabled (no TYPESAFE_API_KEY)"}`, http.StatusBadRequest)
				return
			}
			g.breaker.SetMode(route.ModeJev)
		case mode == route.ModePinned:
			if g.breaker.ForcedProvider() == "" {
				http.Error(w, `{"error":"pinned mode needs provider"}`, http.StatusBadRequest)
				return
			}
			g.breaker.SetMode(route.ModePinned)
		default:
			// {"mode":"auto"} or legacy {"provider":""}: back to automatic.
			g.breaker.ClearForce()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":   "ok",
			"mode":     string(g.breaker.Mode()),
			"provider": g.breaker.ForcedProvider(),
			"model":    g.breaker.ForcedModel(),
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// routePostAllowed vets a state-changing POST to /_gateway/route.
//
// The gateway listens on loopback, but that does not make it private: any page
// the user happens to have open can reach it. A POST that is a "simple request"
// — form-encoded or text/plain, no preflight — is enough for such a page to
// flip the routing mode, and jev mode starts sending prompt excerpts to an
// external service, so the flip is a data-exposure decision.
//
// Two checks, in this order:
//   - an Origin naming anything other than this gateway is rejected. Browsers
//     attach Origin to every cross-origin POST, so the form-post vector is
//     covered here; requests with no Origin (curl, the UI's same-origin fetch)
//     are unaffected.
//   - the content type must be one the documented clients actually send:
//     application/json (the UI, the README) and application/x-www-form-urlencoded
//     (what a bare `curl -d '{...}'` sends by default). Anything else — notably
//     text/plain, the classic way to sneak a body past a preflight-free
//     request — is refused. The body is parsed as JSON either way.
func (g *Gateway) routePostAllowed(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(r, origin) {
		http.Error(w, `{"error":"cross-origin request rejected"}`, http.StatusForbidden)
		return false
	}
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	switch ct {
	case "application/json", "application/x-www-form-urlencoded":
		return true
	}
	http.Error(w, `{"error":"Content-Type must be application/json"}`, http.StatusUnsupportedMediaType)
	return false
}

// sameOrigin reports whether an Origin header value names this gateway. The
// Host the client used is the only origin worth trusting: the listener is
// loopback-only and a rebound/foreign origin will not match it.
func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}

// availableModels lists selectable upstream models per provider for the UI.
func (g *Gateway) availableModels() map[string][]string {
	return map[string][]string{
		"anthropic": {"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"},
		"glm":       {"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-4.5-air"},
		"deepseek":  {"deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"},
	}
}

// jevSnapshot describes the Jev router for the UI: enabled flag, catalog and
// recent decisions (newest first, ≤50).
func (g *Gateway) jevSnapshot() map[string]any {
	out := map[string]any{
		"enabled": g.jevEnabled(),
		"catalog": []route.Candidate{},
		"recent":  []route.Decision{},
	}
	if g.router == nil {
		return out
	}
	if c := g.router.Catalog(); c != nil {
		out["catalog"] = c
	}
	if rc := g.router.Recent(50); rc != nil {
		out["recent"] = rc
	}
	return out
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
		Mode:           string(g.breaker.Mode()),
		JevEnabled:     g.jevEnabled(),
		LastRequest:    g.lastRequestSnapshot(),
	}
	if g.cfg.DeepSeekAPIKey != "" {
		resp.Upstream["deepseek"] = g.cfg.DeepSeek.BaseURL
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(&resp)
}

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// 413 with a readable body: the client sees a clean rejection
			// rather than a silently truncated prompt.
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"gateway_error","message":"request body exceeds the %d MiB gateway limit"}}`, maxProxyBodyBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	model := extractModel(body)

	switch g.breaker.ForcedProvider() {
	case "glm":
		g.serveGLM(w, r, body, model, start, false, "")
		return
	case "deepseek":
		if !g.tryDeepSeek(w, r, body, model, start, false, "forced", "") {
			http.Error(w, `{"type":"error","error":{"type":"gateway_error","message":"route forced to deepseek but DEEPSEEK_API_KEY is unset"}}`, http.StatusBadGateway)
			g.logRequest(r, model, model, "deepseek", 502, start, false)
		}
		return
	case "anthropic":
		g.serveAnthropic(w, r, body, model, start, false, "")
		return
	}

	// Token-less clients (OpenCode etc.) carry a marker credential instead of
	// Claude OAuth; they cannot authenticate against Anthropic directly, so
	// route them straight to their configured provider.
	if g.usesLocalToken(r) {
		switch g.cfg.LocalTokenProvider {
		case "deepseek":
			if g.tryDeepSeek(w, r, body, model, start, false, "local_token", "") {
				return
			}
		case "anthropic":
			g.serveAnthropic(w, r, body, model, start, false, "")
			return
		default:
			g.serveGLM(w, r, body, model, start, false, "")
			return
		}
	}

	state := g.breaker.Decide(anthropicUpstream, model, time.Now())

	// jev mode: ask the router to pick provider+model from the catalog. The
	// breaker still wins (Anthropic dropped while OPEN); any router failure
	// falls open to the auto path below for this call.
	//
	// Only real model calls qualify: a body with no model field (a favicon or
	// other GET the client fires at the gateway) has nothing to choose between,
	// and consulting Jev for it would spend a round-trip and log a decision
	// with an empty requested_model.
	//
	// A due PROBE is the other exception: the half-open probe asks whether the
	// requested model recovered, and jev mode may keep picking other providers,
	// so honouring the pick would leave the entry stuck in PROBE (status and UI
	// reporting probe forever). Hand the probe to Anthropic with the requested
	// model and no decision header, exactly as auto mode would.
	if g.breaker.Mode() == route.ModeJev && state != breaker.Probe && g.jevEnabled() && model != "" && r.Method == http.MethodPost {
		// Candidate filtering is read-only: it must not transition, persist or
		// log breaker state for models this call may never reach.
		cands := g.jevCandidates(model, g.breaker.State(anthropicUpstream, model, time.Now()))
		d, ok := g.router.Decide(r.Context(), body, cands)
		g.metrics.IncJevDecision()
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), decisionKey{}, d.Source))
			w.Header().Set("X-Conduit-Decision", d.Source)
			// The model that will actually be sent upstream. Every breaker- or
			// notice-related bookkeeping keys on it rather than on the inbound
			// id: a quota 429 from the picked model must not open the entry for
			// a different, healthy one, and a 2xx must not close someone
			// else's probe.
			served := d.Model
			if served == "" {
				served = model
			}
			switch d.Provider {
			case "anthropic":
				g.serveAnthropic(w, r, body, model, start, g.probeDue(served), served)
				return
			case "glm":
				g.serveGLM(w, r, body, model, start, false, d.Model)
				return
			case "deepseek":
				// Not reachable in practice: jevCandidates drops deepseek unless
				// a key is configured, and the router answers only with offered
				// catalog keys. Falling out of the switch rather than returning
				// keeps the fail-open guarantee if it ever happens anyway, with
				// serveDeepSeek's key check as the backstop.
				if g.tryDeepSeek(w, r, body, model, start, false, "jev", d.Model) {
					return
				}
			default:
				g.log.Warn("jev returned unknown provider; falling back to auto", "provider", d.Provider)
			}
		}
		g.metrics.IncJevFailOpen()
		r = r.WithContext(context.WithValue(r.Context(), decisionKey{}, "fail_open"))
		w.Header().Set("X-Conduit-Decision", "fail_open")
	}

	switch state {
	case breaker.Open:
		g.serveGLM(w, r, body, model, start, false, "")
		return
	case breaker.Probe:
		g.serveAnthropic(w, r, body, model, start, true, "")
		return
	default:
		g.serveAnthropic(w, r, body, model, start, false, "")
	}
}

// jevCandidates filters the catalog for this call: Anthropic entries are
// dropped while the breaker is OPEN (for the requested model, or for the
// candidate's own model), DeepSeek entries when no key is configured.
//
// requested is the caller's breaker reading for the inbound model, taken
// read-only (breaker.State): a Jev request may call none of these models, so
// filtering must not transition, persist or log breaker state for any of them.
func (g *Gateway) jevCandidates(model string, requested breaker.State) []route.Candidate {
	full := g.router.Catalog()
	out := make([]route.Candidate, 0, len(full))
	now := time.Now()
	for _, c := range full {
		switch c.Provider {
		case "anthropic":
			if requested == breaker.Open {
				continue
			}
			if c.Model != model && g.breaker.State(anthropicUpstream, c.Model, now) == breaker.Open {
				continue
			}
		case "deepseek":
			if g.cfg.DeepSeekAPIKey == "" {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// probeDue advances an expired OPEN window for model to PROBE and reports
// whether the next call to that model is the half-open probe. Like Decide, the
// request path may transition state; the probe a successful call resolves must
// be the entry of the model actually sent upstream.
func (g *Gateway) probeDue(model string) bool {
	return g.breaker.Decide(anthropicUpstream, model, time.Now()) == breaker.Probe
}

// serveAnthropic forwards to Anthropic. modelOverride (jev mode) replaces the
// inbound model when it differs; "" keeps the body byte-identical. All breaker
// and notify bookkeeping keys on the model actually sent upstream (model when
// no override applies), never on the inbound id.
func (g *Gateway) serveAnthropic(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, isProbe bool, modelOverride string) {
	var lastResp *http.Response
	var lastBody []byte
	var lastErr error

	upstreamModel := model
	upstreamBody := body
	if modelOverride != "" && modelOverride != model {
		if rewritten, err := rewriteModel(body, modelOverride); err == nil {
			upstreamBody = rewritten
			upstreamModel = modelOverride
		}
	}

	attempts := 1 + g.cfg.Anthropic.MaxTransientRetries
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			g.metrics.IncTransientRetry()
			backoff := jitteredBackoff(attempt)
			g.log.Info("retrying anthropic after transient error",
				"attempt", attempt, "backoff", backoff.String(), "model", upstreamModel)
			time.Sleep(backoff)
		}

		resp, respBody, peeked, err := g.roundTrip("anthropic", g.cfg.Anthropic.BaseURL, r, upstreamBody, "", upstreamModel)
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
				if g.breaker.Open(anthropicUpstream, upstreamModel, "proactive_"+reason, until, time.Now()) {
					notify.FailoverToGLM(upstreamModel, g.glmModelFor(upstreamModel), "proactive_"+reason)
				}
			}
			if isProbe {
				if g.breaker.Close(anthropicUpstream, upstreamModel, time.Now()) {
					notify.BackToAnthropic(upstreamModel)
				}
			}
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, "anthropic", false, "")
			g.logRequest(r, model, upstreamModel, "anthropic", resp.StatusCode, start, false)
			return
		}

		class := classify.ClassifyAnthropic(resp.StatusCode, resp.Header, respBody)
		_ = g.capture.Write(capture.FromResponse("anthropic", r.Method, r.URL.Path, upstreamModel, resp.StatusCode, resp.Header, respBody, class.Reason))

		switch class.Kind {
		case classify.Quota:
			// The GLM tier and the notice are resolved from the model actually
			// sent upstream, so the notice names the pair that will really
			// serve this call. An override equal to "" keeps serveGLM's own
			// mapping, which is what auto mode has always done.
			failoverOverride := ""
			if upstreamModel != model {
				failoverOverride = g.glmModelFor(upstreamModel)
			}
			if g.breaker.Open(anthropicUpstream, upstreamModel, class.Reason, class.Until, time.Now()) {
				notify.FailoverToGLM(upstreamModel, g.glmModelFor(upstreamModel), class.Reason)
			}
			_ = resp.Body.Close()
			// Pre-stream (or non-stream) failover: client has not seen bytes yet
			// because we buffered the error body before writing. Suppressed when
			// routing is manually pinned to anthropic — the user chose to eat
			// quota errors rather than fail over.
			if !peeked && g.breaker.ForcedProvider() != "anthropic" {
				g.metrics.IncFailover()
				g.serveGLM(w, r, body, model, start, true, failoverOverride)
				return
			}
			// Mid-stream: already copied error? Shouldn't happen — we only peek
			// non-2xx before writing. If peeked somehow, surface.
			g.metrics.IncAnthropic()
			g.writeUpstream(w, resp, respBody, "anthropic", false, "")
			g.logRequest(r, model, upstreamModel, "anthropic", resp.StatusCode, start, false)
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
			g.logRequest(r, model, upstreamModel, "anthropic", resp.StatusCode, start, false)
			return
		}
	}

	if lastResp != nil {
		g.metrics.IncAnthropic()
		g.writeUpstream(w, lastResp, lastBody, "anthropic", false, "")
		g.logRequest(r, model, upstreamModel, "anthropic", lastResp.StatusCode, start, false)
		return
	}
	msg := "upstream anthropic unreachable"
	if lastErr != nil {
		msg = redact.String(lastErr.Error())
	}
	http.Error(w, msg, http.StatusBadGateway)
	g.logRequest(r, model, upstreamModel, "anthropic", 502, start, false)
}

// serveGLM forwards to Z.ai. modelOverride (jev mode) bypasses the model map;
// "" uses the configured mapping (and ForcedModel when pinned to glm).
func (g *Gateway) serveGLM(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool, modelOverride string) {
	glmModel := modelOverride
	if glmModel == "" {
		var ok bool
		glmModel, ok = g.cfg.MapModel(model)
		if !ok {
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"gateway_error","message":"no GLM model mapping for %q and default_model unset"}}`, model), http.StatusBadGateway)
			g.logRequest(r, model, model, "glm", 502, start, failover)
			return
		}
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
		if g.tryDeepSeek(w, r, body, model, start, failover, "glm_unreachable", "") {
			return
		}
		http.Error(w, redact.String(err.Error()), http.StatusBadGateway)
		g.logRequest(r, model, glmModel, "glm", 502, start, failover)
		return
	}
	if resp.StatusCode >= 400 {
		_ = g.capture.Write(capture.FromResponse("glm", r.Method, r.URL.Path, glmModel, resp.StatusCode, resp.Header, respBody, "glm_error"))
	}
	if shouldFallToDeepSeek(resp.StatusCode) && g.tryDeepSeek(w, r, body, model, start, failover, fmt.Sprintf("glm_%d", resp.StatusCode), "") {
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

// usesLocalToken reports whether the inbound credential is the local marker
// token rather than a real (OAuth) Anthropic credential.
func (g *Gateway) usesLocalToken(r *http.Request) bool {
	tok := g.cfg.LocalToken
	if tok == "" {
		return false
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		if r.Header.Get("Authorization") == "Bearer "+tok {
			return true
		}
	}
	return r.Header.Get("X-Api-Key") == tok
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
func (g *Gateway) tryDeepSeek(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool, reason string, modelOverride string) bool {
	if g.cfg.DeepSeekAPIKey == "" {
		return false
	}
	g.serveDeepSeek(w, r, body, model, start, failover, reason, modelOverride)
	return true
}

// serveDeepSeek forwards to DeepSeek. modelOverride (jev mode) bypasses the
// model map; "" uses the configured mapping (and ForcedModel when pinned).
func (g *Gateway) serveDeepSeek(w http.ResponseWriter, r *http.Request, body []byte, model string, start time.Time, failover bool, reason string, modelOverride string) {
	dsModel := modelOverride
	if dsModel == "" {
		var ok bool
		dsModel, ok = g.cfg.MapModelDeepSeek(model)
		if !ok {
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"gateway_error","message":"no DeepSeek model mapping for %q and default_model unset"}}`, model), http.StatusBadGateway)
			g.logRequest(r, model, model, "deepseek", 502, start, failover)
			return
		}
	}
	if g.breaker.ForcedProvider() == "deepseek" {
		if m := g.breaker.ForcedModel(); m != "" {
			dsModel = m
		}
	}
	// A deliberate jev pick is not a failover: skip the route file / toast.
	if reason != "jev" {
		notify.FailoverToDeepSeek(model, dsModel, reason)
	}
	rewritten := prepareDeepSeekBody(body, dsModel)

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
	// Only a real model call may move the "now routing" snapshot behind
	// lastRequestSnapshot. Every proxied request passes through here, including
	// model-less ones (a browser's /favicon.ico GET), and a model-less call has
	// no model to attribute: recording it lets the upstream that 404s an asset
	// overwrite the provider actually serving traffic, which is actively
	// misleading during a failover. The structured log line below still covers
	// those requests, which is what diagnostics want.
	if model != "" {
		g.lastMu.Lock()
		g.lastProvider = provider
		g.lastUpstream = upstreamModel
		g.lastAt = time.Now()
		g.lastMu.Unlock()
	}
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"model", model,
		"upstream_model", upstreamModel,
		"provider", provider,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
		"failover", failover,
		"mode", string(g.breaker.Mode()),
	}
	if src, ok := r.Context().Value(decisionKey{}).(string); ok && src != "" {
		attrs = append(attrs, "decision_source", src)
	}
	g.log.Info("request", attrs...)
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
// of the body bytes as far as encoding/json allows. In auto/pinned mode the
// Anthropic path forwards the body byte-identical (no call); in jev mode it is
// applied on the Anthropic path too when Jev picks a different Claude model —
// Anthropic's prompt cache is keyed per model, so re-encoding costs nothing
// that the model switch has not already cost.
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

// prepareDeepSeekBody rewrites the model and drops the "Artifact" tool, whose
// input_schema DeepSeek's Anthropic-compatible validator rejects (400) while
// Anthropic and GLM accept it. Artifact is a client-side render helper the
// failover tier never needs.
//
// Dropping Artifact is a deliberate deviation from pure model rewriting, and it
// applies on every path into this tier (auto failover, pinned deepseek, and a
// jev pick) — not just jev. The alternative is a 400 that fails the whole tier.
// No other tool is touched.
func prepareDeepSeekBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if b, err := json.Marshal(model); err == nil {
		obj["model"] = b
	}
	if rawTools, ok := obj["tools"]; ok {
		// A tools list that only held Artifact is dropped whole: "tools": []
		// is not what the client asked for and some validators reject it.
		if trimmed := dropArtifactTool(rawTools); trimmed == nil {
			delete(obj, "tools")
		} else {
			obj["tools"] = trimmed
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// dropArtifactTool returns raw without the Artifact tool, or raw unchanged when
// it held none. It returns nil when nothing is left, which the caller reads as
// "omit the key".
func dropArtifactTool(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
	kept := make([]map[string]json.RawMessage, 0, len(tools))
	changed := false
	for _, t := range tools {
		var name string
		if n, ok := t["name"]; ok {
			_ = json.Unmarshal(n, &name)
		}
		if name == "Artifact" {
			changed = true
			continue
		}
		kept = append(kept, t)
	}
	if !changed {
		return raw
	}
	if len(kept) == 0 {
		return nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	return out
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
