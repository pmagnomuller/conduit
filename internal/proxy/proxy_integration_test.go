package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pedro-mueller/conduit/internal/breaker"
	"github.com/pedro-mueller/conduit/internal/capture"
	"github.com/pedro-mueller/conduit/internal/config"
	"github.com/pedro-mueller/conduit/internal/metrics"
	"github.com/pedro-mueller/conduit/internal/notify"
	"github.com/pedro-mueller/conduit/internal/proxy"
	"github.com/pedro-mueller/conduit/internal/redact"
	"github.com/pedro-mueller/conduit/internal/route"
)

const fakeToken = "sk-ant-secret-token-DO-NOT-LEAK-abc123"

type fakeUpstreams struct {
	anthropicHits atomic.Int32
	glmHits       atomic.Int32
	deepSeekHits  atomic.Int32

	anthropic http.HandlerFunc
	glm       http.HandlerFunc
	deepseek  http.HandlerFunc

	enableDeepSeek bool
	// decider, when set, is injected as the jev router (nil = jev disabled).
	decider proxy.Decider

	mu             sync.Mutex
	glmBodies      [][]byte
	deepSeekBodies [][]byte
	anthropicAuth  []string
	glmAuth        []string
	deepSeekAuth   []string
}

func newGateway(t *testing.T, f *fakeUpstreams) (*httptest.Server, *breaker.Breaker, string) {
	t.Helper()
	t.Setenv("CONDUIT_DESKTOP_NOTIFY", "0")
	t.Setenv("CONDUIT_ROUTE_PATH", filepath.Join(t.TempDir(), "route.json"))
	notify.SetEnabledFromEnv()
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.anthropicHits.Add(1)
		f.mu.Lock()
		f.anthropicAuth = append(f.anthropicAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.anthropic(w, r)
	}))
	glm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.glmHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.glmBodies = append(f.glmBodies, body)
		f.glmAuth = append(f.glmAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.glm(w, r)
	}))
	t.Cleanup(anth.Close)
	t.Cleanup(glm.Close)
	deepseek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.deepSeekHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.deepSeekBodies = append(f.deepSeekBodies, body)
		f.deepSeekAuth = append(f.deepSeekAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if f.deepseek != nil {
			f.deepseek(w, r)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(deepseek.Close)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	capturePath := filepath.Join(dir, "upstream-errors.jsonl")
	logPath := filepath.Join(dir, "gateway.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Anthropic.BaseURL = anth.URL
	cfg.Anthropic.MaxTransientRetries = 2
	cfg.GLM.BaseURL = glm.URL
	cfg.GLM.DefaultModel = "glm-5.3"
	cfg.GLM.ModelMap = map[string]string{
		"claude-opus-5":    "glm-5.3",
		"claude-haiku-4-5": "glm-5.3-flash",
	}
	cfg.ZAIAPIKey = "zai-test-key-secret"
	cfg.LocalToken = "conduit-local"
	if f.enableDeepSeek {
		cfg.DeepSeek.BaseURL = deepseek.URL
		cfg.DeepSeek.DefaultModel = "deepseek-v4-flash"
		cfg.DeepSeekAPIKey = "deepseek-test-key-secret"
	}
	cfg.Paths.StatePath = statePath
	cfg.Log.CapturePath = capturePath
	cfg.Log.CaptureUpstreamErrors = true
	cfg.Breaker.FallbackOpenSeconds = 300
	cfg.Breaker.ProbeOnExpiry = true

	log := slog.New(slog.NewTextHandler(io.MultiWriter(logFile, io.Discard), &slog.HandlerOptions{Level: slog.LevelDebug}))
	br := breaker.New(statePath, cfg.FallbackOpenDuration(), true, func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		_, _ = logFile.WriteString(line + "\n")
		log.Warn(line)
	})
	capWriter := capture.New(capturePath, true)
	met := metrics.New()
	gw := proxy.New(cfg, br, capWriter, met, f.decider, log)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv, br, dir
}

func TestHappyPathNonStreamingByteIdentical(t *testing.T) {
	reqBody := []byte(`{"model":"claude-opus-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	respBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"yo"}],"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":42}}`)

	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, reqBody) {
			t.Errorf("anthropic body mismatch:\n got=%s\nwant=%s", got, reqBody)
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			t.Errorf("auth not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(respBody)
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("glm should not be called")
	}

	srv, _, _ := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, got)
	}
	if !bytes.Equal(got, respBody) {
		t.Fatalf("response body not identical")
	}
	if f.glmHits.Load() != 0 {
		t.Fatal("glm hit")
	}
}

func TestHappyPathStreamingOrder(t *testing.T) {
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = io.WriteString(w, c)
			fl.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Fatal("glm") }

	srv, _, _ := newGateway(t, f)
	reqBody := []byte(`{"model":"claude-opus-5","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	want := strings.Join(chunks, "")
	if string(got) != want {
		t.Fatalf("sse mismatch:\n%s\n----\n%s", got, want)
	}
}

func TestPreStream429FailsoverToGLM(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "9999999999")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`))
	}
	sse := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"glm-5.2\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, sse)
	}

	srv, br, dir := newGateway(t, f)
	reqBody := []byte(`{"model":"claude-opus-5","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, got)
	}
	if res.Header.Get("X-Conduit-Provider") != "glm" {
		t.Fatalf("provider header=%q", res.Header.Get("X-Conduit-Provider"))
	}
	if res.Header.Get("X-Conduit-Notice") != "glm-failover" {
		t.Fatalf("notice header=%q", res.Header.Get("X-Conduit-Notice"))
	}
	if !bytes.Contains(got, []byte("[conduit] Switched to GLM")) {
		t.Fatalf("expected chat notice in sse, got=%s", got)
	}
	if !bytes.Contains(got, []byte("hi")) {
		t.Fatalf("missing upstream text: %s", got)
	}
	_ = dir
	if f.glmHits.Load() != 1 {
		t.Fatalf("glm hits=%d", f.glmHits.Load())
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Open {
		t.Fatalf("breaker=%s", st)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.glmBodies) != 1 {
		t.Fatal("expected one glm body")
	}
	var m map[string]any
	if err := json.Unmarshal(f.glmBodies[0], &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "glm-5.3" {
		t.Fatalf("model rewrite=%v", m["model"])
	}
	if !strings.HasPrefix(f.glmAuth[0], "Bearer zai-test-key") {
		t.Fatalf("glm auth=%q", f.glmAuth[0])
	}
	if strings.Contains(f.glmAuth[0], fakeToken) {
		t.Fatal("anthropic token leaked to glm")
	}

	// status endpoint
	stRes, err := http.Get(srv.URL + "/_gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	defer stRes.Body.Close()
	var status map[string]any
	_ = json.NewDecoder(stRes.Body).Decode(&status)
	if status["listen"] == nil {
		t.Fatalf("status=%v", status)
	}

	assertNoSecretOnDisk(t, dir, fakeToken)
	assertNoSecretOnDisk(t, dir, "zai-test-key-secret")
}

func TestMidStreamErrorNoSplice(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		fl.Flush()
		// Simulate mid-stream failure by closing without clean end.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		t.Error("glm must not be called on mid-stream failure")
	}

	srv, br, _ := newGateway(t, f)
	reqBody := []byte(`{"model":"claude-opus-5","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	_, _ = io.ReadAll(res.Body)
	if f.glmHits.Load() != 0 {
		t.Fatalf("glm hits=%d", f.glmHits.Load())
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("breaker should stay CLOSED, got %s", st)
	}
}

func Test529RetriesAnthropicBreakerClosed(t *testing.T) {
	var n atomic.Int32
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Fatal("glm") }

	srv, br, _ := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte(`"ok":true`)) {
		t.Fatalf("status=%d body=%s hits=%d", res.StatusCode, body, f.anthropicHits.Load())
	}
	if f.anthropicHits.Load() != 3 {
		t.Fatalf("expected 3 anthropic attempts, got %d", f.anthropicHits.Load())
	}
	if f.glmHits.Load() != 0 {
		t.Fatal("glm called")
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("breaker=%s", st)
	}
}

func Test401SurfacedBreakerClosed(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Fatal("glm") }

	srv, br, _ := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if f.glmHits.Load() != 0 {
		t.Fatal("glm")
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("breaker=%s", st)
	}
}

func TestBreakerExpiryProbeClosedOnSuccess(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Fatal("glm on probe success path") }

	srv, br, _ := newGateway(t, f)
	now := time.Now()
	br.Open("anthropic", "claude-opus-5", "rate_limit_error", now.Add(-time.Second), now.Add(-time.Minute))
	// Decide should flip to PROBE
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Probe {
		t.Fatalf("want PROBE got %s", st)
	}
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("want CLOSED got %s", st)
	}
}

func TestBreakerExpiryProbeReopensOn429(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":"glm"}`))
	}

	srv, br, _ := newGateway(t, f)
	now := time.Now()
	br.Open("anthropic", "claude-opus-5", "rate_limit_error", now.Add(-time.Second), now.Add(-time.Minute))
	_ = br.Decide("anthropic", "claude-opus-5", time.Now()) // PROBE

	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte(`glm`)) {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Open {
		t.Fatalf("want OPEN got %s", st)
	}
}

func TestModelRewriteOnlyOnGLM(t *testing.T) {
	f := &fakeUpstreams{}
	var anthBody []byte
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		anthBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}

	srv, br, _ := newGateway(t, f)
	raw := []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	res := post(t, srv.URL+"/v1/messages", raw, fakeToken)
	res.Body.Close()
	if !bytes.Contains(anthBody, []byte(`"claude-opus-5"`)) {
		t.Fatalf("anthropic should keep model id: %s", anthBody)
	}

	br.Open("anthropic", "claude-opus-5", "test", time.Now().Add(time.Hour), time.Now())
	res2 := post(t, srv.URL+"/v1/messages", raw, fakeToken)
	res2.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.glmBodies) == 0 {
		t.Fatal("no glm body")
	}
	if !bytes.Contains(f.glmBodies[0], []byte(`"glm-5.3"`)) {
		t.Fatalf("glm body=%s", f.glmBodies[0])
	}
	if bytes.Contains(f.glmBodies[0], []byte(`claude-opus-5`)) {
		t.Fatal("claude model should be rewritten away")
	}
}

func TestSecurityNoCredentialsInCaptureOrLogs(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}

	srv, _, dir := newGateway(t, f)
	_ = post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken).Body.Close()
	assertNoSecretOnDisk(t, dir, fakeToken)
	assertNoSecretOnDisk(t, dir, "zai-test-key-secret")

	// Redact helper sanity
	leaked := "Authorization: Bearer " + fakeToken
	if redact.ContainsCredential(redact.String(leaked), fakeToken) {
		t.Fatal("redact failed")
	}
}

func TestCapacityThrottle429DoesNotFailover(t *testing.T) {
	f := &fakeUpstreams{}
	var n atomic.Int32
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("X-Should-Retry", "true")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Server is temporarily limiting requests (not your usage limit)"}}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Fatal("glm") }

	srv, br, _ := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if f.glmHits.Load() != 0 {
		t.Fatal("should not failover capacity throttle")
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("breaker=%s", st)
	}
}

func TestGLMHardDownFailsOverToDeepSeek(t *testing.T) {
	f := &fakeUpstreams{enableDeepSeek: true}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"glm down"}}`))
	}
	f.deepseek = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_ds","type":"message","role":"assistant","content":[{"type":"text","text":"ds"}]}`))
	}

	srv, _, dir := newGateway(t, f)
	reqBody := []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(got, []byte("Switched to DeepSeek")) {
		t.Fatalf("status=%d body=%s", res.StatusCode, got)
	}
	if res.Header.Get("X-Conduit-Provider") != "deepseek" {
		t.Fatalf("provider header=%q", res.Header.Get("X-Conduit-Provider"))
	}
	if res.Header.Get("X-Conduit-Notice") != "deepseek-failover" {
		t.Fatalf("notice header=%q", res.Header.Get("X-Conduit-Notice"))
	}
	if f.deepSeekHits.Load() != 1 {
		t.Fatalf("deepseek hits=%d", f.deepSeekHits.Load())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deepSeekBodies) != 1 {
		t.Fatal("expected one deepseek body")
	}
	var m map[string]any
	if err := json.Unmarshal(f.deepSeekBodies[0], &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "deepseek-v4-flash" {
		t.Fatalf("model rewrite=%v", m["model"])
	}
	if !strings.HasPrefix(f.deepSeekAuth[0], "Bearer deepseek-test-key") {
		t.Fatalf("deepseek auth=%q", f.deepSeekAuth[0])
	}
	if strings.Contains(f.deepSeekAuth[0], fakeToken) || strings.Contains(f.deepSeekAuth[0], "zai-test-key-secret") {
		t.Fatal("upstream keys leaked to deepseek")
	}

	assertNoSecretOnDisk(t, dir, fakeToken)
	assertNoSecretOnDisk(t, dir, "zai-test-key-secret")
	assertNoSecretOnDisk(t, dir, "deepseek-test-key-secret")
}

func TestDeepSeekStripsArtifactTool(t *testing.T) {
	f := &fakeUpstreams{enableDeepSeek: true}
	f.deepseek = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_ds","type":"message","role":"assistant","content":[{"type":"text","text":"ds"}]}`))
	}

	srv, br, _ := newGateway(t, f)
	br.SetForce("deepseek", "")

	reqBody := []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[
		{"name":"Artifact","description":"render","input_schema":{"type":"object","properties":{"content":{"anyOf":[{"type":"string","minLength":1,"maxLength":1024,"pattern":"^[^\u0000]*$"},{"type":"object"}]}}}},
		{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}
	]}`)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deepSeekBodies) != 1 {
		t.Fatal("expected one deepseek body")
	}
	var m struct {
		Model string `json:"model"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(f.deepSeekBodies[0], &m); err != nil {
		t.Fatal(err)
	}
	if m.Model != "deepseek-v4-flash" {
		t.Fatalf("model=%q, want deepseek-v4-flash", m.Model)
	}
	var names []string
	for _, tl := range m.Tools {
		names = append(names, tl.Name)
	}
	if len(names) != 1 || names[0] != "Bash" {
		t.Fatalf("tools=%v, want [Bash] (Artifact stripped)", names)
	}
}

func TestDeepSeekTierSkippedWhenNoKey(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	}

	srv, _, _ := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("expected glm 503 surfaced, got %d", res.StatusCode)
	}
	if f.deepSeekHits.Load() != 0 {
		t.Fatalf("deepseek hits=%d", f.deepSeekHits.Load())
	}
}

func TestGLMClientErrorDoesNotFallToDeepSeek(t *testing.T) {
	f := &fakeUpstreams{enableDeepSeek: true}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	}

	srv, _, _ := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("expected glm 400 surfaced, got %d", res.StatusCode)
	}
	if f.deepSeekHits.Load() != 0 {
		t.Fatalf("deepseek hits=%d", f.deepSeekHits.Load())
	}
}

func TestForcedGLMBypassesAnthropicAndOverridesModel(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		t.Error("anthropic must not be called while glm is forced")
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":"glm"}`))
	}

	srv, br, _ := newGateway(t, f)
	br.SetForce("glm", "glm-5.3-flash")

	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("glm")) {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.glmBodies[len(f.glmBodies)-1], &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "glm-5.3-flash" {
		t.Fatalf("forced model override=%v, want glm-5.3-flash", m["model"])
	}
}

func TestForcedAnthropicSuppressesFailover(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":"glm"}`))
	}

	srv, br, _ := newGateway(t, f)
	br.SetForce("anthropic", "")

	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 429 {
		t.Fatalf("expected 429 surfaced (no failover), got %d", res.StatusCode)
	}
	if f.glmHits.Load() != 0 {
		t.Fatal("glm hit during forced anthropic")
	}

	br.ClearForce()
	res2 := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res2.Body.Close()
	if res2.StatusCode != 200 || f.glmHits.Load() != 1 {
		t.Fatalf("after clear: status=%d glmHits=%d", res2.StatusCode, f.glmHits.Load())
	}
}

func TestRouteEndpointForceAndClear(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }
	f.glm = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }

	srv, _, _ := newGateway(t, f)

	res, err := http.Post(srv.URL+"/_gateway/route", "application/json",
		strings.NewReader(`{"provider":"glm","model":"glm-5.3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("force status=%d", res.StatusCode)
	}
	res.Body.Close()

	st, err := http.Get(srv.URL + "/_gateway/route")
	if err != nil {
		t.Fatal(err)
	}
	var route struct {
		ForcedProvider string              `json:"forced_provider"`
		ForcedModel    string              `json:"forced_model"`
		Available      map[string][]string `json:"available"`
	}
	_ = json.NewDecoder(st.Body).Decode(&route)
	st.Body.Close()
	if route.ForcedProvider != "glm" || route.ForcedModel != "glm-5.3" {
		t.Fatalf("route=%+v", route)
	}
	if len(route.Available["glm"]) == 0 || len(route.Available["deepseek"]) == 0 {
		t.Fatalf("available=%v", route.Available)
	}

	res2, err := http.Post(srv.URL+"/_gateway/route", "application/json",
		strings.NewReader(`{"clear":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()

	bad, err := http.Post(srv.URL+"/_gateway/route", "application/json",
		strings.NewReader(`{"provider":"openai"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("invalid provider accepted: %d", bad.StatusCode)
	}
}

func TestLocalTokenRoutesToGLMWithBreakerClosed(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":"anthropic"}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":"glm"}`))
	}

	srv, _, _ := newGateway(t, f)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer conduit-local")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if f.glmHits.Load() != 1 || f.anthropicHits.Load() != 0 {
		t.Fatalf("glm=%d anthropic=%d", f.glmHits.Load(), f.anthropicHits.Load())
	}

	// OAuth-style traffic must keep the automatic path.
	res2 := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`), fakeToken)
	defer res2.Body.Close()
	if f.anthropicHits.Load() != 1 {
		t.Fatalf("oauth traffic should hit anthropic, got %d", f.anthropicHits.Load())
	}
}

// fakeDecider is a scripted jev router: returns pick (ok=true) or fails open
// when pick.Provider is empty, and records the candidate lists it was offered.
type fakeDecider struct {
	pick    route.Decision
	enabled bool

	mu    sync.Mutex
	seen  [][]route.Candidate
	calls int
}

func newFakeDecider(provider, model string) *fakeDecider {
	return &fakeDecider{
		enabled: true,
		pick:    route.Decision{Provider: provider, Model: model, Source: route.SourceJev, Step: "user_turn", Lease: "one_call"},
	}
}

func (d *fakeDecider) Enabled() bool { return d.enabled }

func (d *fakeDecider) Catalog() []route.Candidate { return config.DefaultCatalog() }

func (d *fakeDecider) Decide(_ context.Context, body []byte, cands []route.Candidate) (route.Decision, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.seen = append(d.seen, append([]route.Candidate(nil), cands...))
	if d.pick.Provider == "" {
		return route.Decision{Source: route.SourceFailOpen, Reason: "scripted"}, false
	}
	out := d.pick
	out.At = time.Now()
	out.RequestedModel = extractModelForTest(body)
	return out, true
}

func (d *fakeDecider) Recent(n int) []route.Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls == 0 {
		return nil
	}
	return []route.Decision{d.pick}
}

func (d *fakeDecider) callsCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *fakeDecider) lastCandidates() []route.Candidate {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.seen) == 0 {
		return nil
	}
	return d.seen[len(d.seen)-1]
}

func extractModelForTest(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

func hasProvider(cands []route.Candidate, provider string) bool {
	for _, c := range cands {
		if c.Provider == provider {
			return true
		}
	}
	return false
}

func postRoute(t *testing.T, base, body string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(base+"/_gateway/route", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func getRoute(t *testing.T, base string) map[string]any {
	t.Helper()
	res, err := http.Get(base + "/_gateway/route")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

const jevReq = `{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`

func okJSON(tag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":"` + tag + `"}`))
	}
}

func TestRouteModeTransitionsAndValidation(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = okJSON("anthropic")
	f.glm = okJSON("glm")
	// jev disabled (nil decider)
	srv, br, dir := newGateway(t, f)

	if m := getRoute(t, srv.URL); m["mode"] != "auto" {
		t.Fatalf("initial mode=%v", m["mode"])
	}
	if code, m := postRoute(t, srv.URL, `{"mode":"jev"}`); code != 400 || !strings.Contains(fmt.Sprint(m["error"]), "jev disabled") {
		t.Fatalf("jev disabled: code=%d m=%v", code, m)
	}
	if code, m := postRoute(t, srv.URL, `{"mode":"pinned"}`); code != 400 || !strings.Contains(fmt.Sprint(m["error"]), "needs provider") {
		t.Fatalf("pinned w/o provider: code=%d m=%v", code, m)
	}
	if code, _ := postRoute(t, srv.URL, `{"mode":"bogus"}`); code != 400 {
		t.Fatalf("bogus mode accepted: %d", code)
	}
	if code, _ := postRoute(t, srv.URL, `{"provider":"glm","mode":"jev"}`); code != 400 {
		t.Fatalf("conflicting provider+mode accepted: %d", code)
	}
	// Legacy pin ⇒ pinned.
	code, m := postRoute(t, srv.URL, `{"provider":"glm","model":"glm-5.3"}`)
	if code != 200 || m["status"] != "ok" || m["mode"] != "pinned" || m["provider"] != "glm" || m["model"] != "glm-5.3" {
		t.Fatalf("pin: code=%d m=%v", code, m)
	}
	if br.Mode() != route.ModePinned {
		t.Fatalf("breaker mode=%s", br.Mode())
	}
	// Explicit pinned with existing force is fine.
	if code, m := postRoute(t, srv.URL, `{"mode":"pinned"}`); code != 200 || m["mode"] != "pinned" || m["provider"] != "glm" {
		t.Fatalf("pinned: code=%d m=%v", code, m)
	}
	// mode auto clears force.
	if code, m := postRoute(t, srv.URL, `{"mode":"auto"}`); code != 200 || m["mode"] != "auto" || m["provider"] != "" {
		t.Fatalf("auto: code=%d m=%v", code, m)
	}
	if br.ForcedProvider() != "" || br.Mode() != route.ModeAuto {
		t.Fatalf("force not cleared: %q %s", br.ForcedProvider(), br.Mode())
	}
	// Legacy {"clear":true} still ⇒ auto.
	postRoute(t, srv.URL, `{"provider":"anthropic"}`)
	if code, m := postRoute(t, srv.URL, `{"clear":true}`); code != 200 || m["mode"] != "auto" {
		t.Fatalf("clear: code=%d m=%v", code, m)
	}
	// Status carries mode/jev_enabled.
	st, err := http.Get(srv.URL + "/_gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	_ = json.NewDecoder(st.Body).Decode(&status)
	st.Body.Close()
	if status["mode"] != "auto" || status["jev_enabled"] != false {
		t.Fatalf("status=%v", status)
	}
	_ = dir
}

func TestRouteModeJevEnabledAndPersists(t *testing.T) {
	f := &fakeUpstreams{decider: newFakeDecider("glm", "glm-5.3-flash")}
	f.anthropic = okJSON("anthropic")
	f.glm = okJSON("glm")
	srv, br, dir := newGateway(t, f)

	postRoute(t, srv.URL, `{"provider":"glm","model":"glm-5.3"}`)
	code, m := postRoute(t, srv.URL, `{"mode":"jev"}`)
	if code != 200 || m["mode"] != "jev" || m["provider"] != "" {
		t.Fatalf("jev: code=%d m=%v", code, m)
	}
	if br.Mode() != route.ModeJev || br.ForcedProvider() != "" {
		t.Fatalf("jev must clear force: %s %q", br.Mode(), br.ForcedProvider())
	}
	g := getRoute(t, srv.URL)
	jev, _ := g["jev"].(map[string]any)
	if g["mode"] != "jev" || jev["enabled"] != true {
		t.Fatalf("GET route=%v", g)
	}
	if cat, _ := jev["catalog"].([]any); len(cat) == 0 {
		t.Fatalf("catalog empty: %v", jev)
	}

	// Persistence round-trip: a fresh breaker on the same state file loads jev.
	statePath := filepath.Join(dir, "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"mode": "jev"`)) {
		t.Fatalf("state.json lacks mode: %s", raw)
	}
	br2 := breaker.New(statePath, time.Minute, true, nil)
	if br2.Mode() != route.ModeJev {
		t.Fatalf("reloaded mode=%s", br2.Mode())
	}

	// Status reflects it.
	st, err := http.Get(srv.URL + "/_gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	_ = json.NewDecoder(st.Body).Decode(&status)
	st.Body.Close()
	if status["mode"] != "jev" || status["jev_enabled"] != true {
		t.Fatalf("status=%v", status)
	}
}

func TestJevRoutesAnthropicWithModelOverride(t *testing.T) {
	d := newFakeDecider("anthropic", "claude-haiku-4-5")
	f := &fakeUpstreams{decider: d}
	var anthBody []byte
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		anthBody, _ = io.ReadAll(r.Body)
		okJSON("anthropic")(w, r)
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Error("glm must not be hit") }
	srv, br, dir := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if got := res.Header.Get("X-Conduit-Decision"); got != "jev" {
		t.Fatalf("X-Conduit-Decision=%q", got)
	}
	if res.Header.Get("X-Conduit-Provider") != "anthropic" {
		t.Fatalf("provider header=%q", res.Header.Get("X-Conduit-Provider"))
	}
	var m map[string]any
	if err := json.Unmarshal(anthBody, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "claude-haiku-4-5" {
		t.Fatalf("anthropic model=%v want claude-haiku-4-5", m["model"])
	}
	if f.anthropicHits.Load() != 1 {
		t.Fatalf("anthropic hits=%d", f.anthropicHits.Load())
	}
	logs, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
	if !bytes.Contains(logs, []byte("mode=jev")) || !bytes.Contains(logs, []byte("decision_source=jev")) {
		t.Fatalf("log missing mode/decision_source:\n%s", logs)
	}
	if !bytes.Contains(logs, []byte("upstream_model=claude-haiku-4-5")) {
		t.Fatalf("log missing upstream_model:\n%s", logs)
	}
}

func TestJevSameModelKeepsAnthropicBodyByteIdentical(t *testing.T) {
	d := newFakeDecider("anthropic", "claude-opus-5")
	f := &fakeUpstreams{decider: d}
	var anthBody []byte
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		anthBody, _ = io.ReadAll(r.Body)
		okJSON("anthropic")(w, r)
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Error("glm must not be hit") }
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	res.Body.Close()
	if !bytes.Equal(anthBody, []byte(jevReq)) {
		t.Fatalf("body rewritten although model unchanged:\n%s", anthBody)
	}
}

func TestJevRoutesGLMWithChosenModel(t *testing.T) {
	d := newFakeDecider("glm", "glm-5.3-flash")
	f := &fakeUpstreams{decider: d}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) { t.Error("anthropic must not be hit") }
	f.glm = okJSON("glm")
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Conduit-Decision") != "jev" || res.Header.Get("X-Conduit-Provider") != "glm" {
		t.Fatalf("status=%d decision=%q provider=%q", res.StatusCode, res.Header.Get("X-Conduit-Decision"), res.Header.Get("X-Conduit-Provider"))
	}
	if res.Header.Get("X-Conduit-Notice") != "" {
		t.Fatal("jev pick must not be announced as failover")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(f.glmBodies[0], &m)
	if m["model"] != "glm-5.3-flash" {
		t.Fatalf("glm model=%v", m["model"])
	}
}

func TestJevRoutesDeepSeekWithChosenModel(t *testing.T) {
	d := newFakeDecider("deepseek", "deepseek-v4-flash")
	f := &fakeUpstreams{decider: d, enableDeepSeek: true}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) { t.Error("anthropic must not be hit") }
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Error("glm must not be hit") }
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Conduit-Decision") != "jev" || res.Header.Get("X-Conduit-Provider") != "deepseek" {
		t.Fatalf("status=%d headers=%v", res.StatusCode, res.Header)
	}
	if !hasProvider(d.lastCandidates(), "deepseek") {
		t.Fatalf("deepseek missing from candidates with key set: %v", d.lastCandidates())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(f.deepSeekBodies[0], &m)
	if m["model"] != "deepseek-v4-flash" {
		t.Fatalf("deepseek model=%v", m["model"])
	}
}

func TestJevSkipsRequestsWithoutModel(t *testing.T) {
	d := newFakeDecider("glm", "glm-5.3")
	f := &fakeUpstreams{decider: d}
	f.anthropic = okJSON("anthropic")
	f.glm = okJSON("glm")
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res, err := http.Get(srv.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// No model field means there is nothing to route: Jev must not be asked,
	// and the call must not show up as a decision.
	if n := d.callsCount(); n != 0 {
		t.Fatalf("decider consulted %d times for a modelless request", n)
	}
	if got := res.Header.Get("X-Conduit-Decision"); got != "" {
		t.Fatalf("X-Conduit-Decision on modelless request = %q", got)
	}
	st, err := http.Get(srv.URL + "/_gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Counts struct {
			JevDecisions int64 `json:"jev_decisions"`
		} `json:"counts"`
	}
	_ = json.NewDecoder(st.Body).Decode(&status)
	st.Body.Close()
	if status.Counts.JevDecisions != 0 {
		t.Fatalf("jev decisions recorded for modelless request: %d", status.Counts.JevDecisions)
	}
}

// A model-less request (the browser's /favicon.ico GET is what the gateway
// actually sees) is proxied but is not a model call: it must not move the
// "now routing" snapshot the status endpoint and the UI pill read. Otherwise an
// upstream 404 of an asset reports that upstream as the provider serving
// traffic, which is wrong precisely when it matters — mid-failover.
func TestModellessRequestDoesNotMoveLastRequest(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "9999999999")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`))
	}
	f.glm = okJSON("glm")

	srv, br, _ := newGateway(t, f)

	// Before any model call the snapshot is empty, and a model-less request must
	// not fill it in.
	res, err := http.Get(srv.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 || res.Header.Get("X-Conduit-Provider") != "anthropic" {
		t.Fatalf("model-less request status=%d provider=%q; it is still proxied", res.StatusCode, res.Header.Get("X-Conduit-Provider"))
	}
	if lr := getRoute(t, srv.URL)["last_request"]; lr != nil {
		t.Fatalf("last_request after model-less request = %v, want nil", lr)
	}

	// A real model call goes to Anthropic, quota-fails over to GLM, and moves it.
	res2 := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	res2.Body.Close()
	if res2.StatusCode != 200 || res2.Header.Get("X-Conduit-Provider") != "glm" {
		t.Fatalf("model call status=%d provider=%q", res2.StatusCode, res2.Header.Get("X-Conduit-Provider"))
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Open {
		t.Fatalf("breaker=%s", st)
	}
	lr, _ := getRoute(t, srv.URL)["last_request"].(map[string]any)
	if lr == nil || lr["provider"] != "glm" || lr["upstream_model"] != "glm-5.3" {
		t.Fatalf("last_request after model call = %v", lr)
	}
	at, _ := lr["at"].(string)

	// The favicon 404 must leave it untouched — same provider, same timestamp.
	res3, err := http.Get(srv.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	assertLast := func(where string, got map[string]any) {
		t.Helper()
		if got == nil || got["provider"] != "glm" || got["upstream_model"] != "glm-5.3" || got["at"] != at {
			t.Fatalf("last_request %s = %v, want glm/glm-5.3 at %s", where, got, at)
		}
	}
	assertLast("after model-less request", mustMap(t, getRoute(t, srv.URL)["last_request"]))

	// /_gateway/status carries the same snapshot and must agree.
	st, err := http.Get(srv.URL + "/_gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		LastRequest map[string]any `json:"last_request"`
	}
	if err := json.NewDecoder(st.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	st.Body.Close()
	assertLast("in /_gateway/status", status.LastRequest)
}

func mustMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, _ := v.(map[string]any)
	return m
}

func TestJevFailOpenFallsToAuto(t *testing.T) {
	d := &fakeDecider{enabled: true} // pick.Provider=="" ⇒ ok=false
	f := &fakeUpstreams{decider: d}
	var anthBody []byte
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		anthBody, _ = io.ReadAll(r.Body)
		okJSON("anthropic")(w, r)
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Error("glm must not be hit while breaker closed") }
	srv, br, dir := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Conduit-Decision") != "fail_open" {
		t.Fatalf("status=%d decision=%q", res.StatusCode, res.Header.Get("X-Conduit-Decision"))
	}
	if !bytes.Equal(anthBody, []byte(jevReq)) {
		t.Fatalf("fail-open must forward untouched body: %s", anthBody)
	}
	// Deepseek dropped from candidates: no key configured.
	if hasProvider(d.lastCandidates(), "deepseek") {
		t.Fatalf("deepseek offered without key: %v", d.lastCandidates())
	}
	st, err := http.Get(srv.URL + "/_gateway/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Counts struct {
			JevDecisions int64 `json:"jev_decisions"`
			JevFailOpen  int64 `json:"jev_fail_open"`
		} `json:"counts"`
	}
	_ = json.NewDecoder(st.Body).Decode(&status)
	st.Body.Close()
	if status.Counts.JevDecisions != 1 || status.Counts.JevFailOpen != 1 {
		t.Fatalf("jev counters: decisions=%d fail_open=%d",
			status.Counts.JevDecisions, status.Counts.JevFailOpen)
	}
	logs, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
	if !bytes.Contains(logs, []byte("decision_source=fail_open")) {
		t.Fatalf("log missing fail_open:\n%s", logs)
	}
}

func TestJevDropsAnthropicWhileBreakerOpen(t *testing.T) {
	d := newFakeDecider("glm", "glm-5.3")
	f := &fakeUpstreams{decider: d}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) { t.Error("anthropic must not be hit while OPEN") }
	f.glm = okJSON("glm")
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)
	br.Open("anthropic", "claude-opus-5", "test", time.Now().Add(time.Hour), time.Now())

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	res.Body.Close()
	cands := d.lastCandidates()
	if len(cands) == 0 || hasProvider(cands, "anthropic") {
		t.Fatalf("anthropic must be dropped while OPEN: %v", cands)
	}
	if !hasProvider(cands, "glm") {
		t.Fatalf("glm missing: %v", cands)
	}

	// Closed again ⇒ anthropic offered.
	br.Close("anthropic", "claude-opus-5", time.Now())
	res2 := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	res2.Body.Close()
	if !hasProvider(d.lastCandidates(), "anthropic") {
		t.Fatalf("anthropic missing after close: %v", d.lastCandidates())
	}
}

func TestJevAnthropicQuotaStillFailsOverToGLM(t *testing.T) {
	d := newFakeDecider("anthropic", "claude-sonnet-5")
	f := &fakeUpstreams{decider: d}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = okJSON("glm")
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Conduit-Provider") != "glm" {
		t.Fatalf("status=%d provider=%q", res.StatusCode, res.Header.Get("X-Conduit-Provider"))
	}
	if res.Header.Get("X-Conduit-Decision") != "jev" {
		t.Fatalf("decision header lost on failover: %q", res.Header.Get("X-Conduit-Decision"))
	}
	if st := br.Decide("anthropic", "claude-sonnet-5", time.Now()); st != breaker.Open {
		t.Fatalf("breaker should be OPEN for the served model, got %s", st)
	}
	// Bookkeeping keys on the model actually sent upstream, not on the inbound
	// id: a 429 from sonnet must not quarantine the requested opus entry.
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("requested model must stay CLOSED, got %s", st)
	}
}

func TestAutoModeHasNoDecisionHeader(t *testing.T) {
	d := newFakeDecider("glm", "glm-5.3")
	f := &fakeUpstreams{decider: d}
	f.anthropic = okJSON("anthropic")
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Error("glm must not be hit in auto") }
	srv, _, _ := newGateway(t, f)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.Header.Get("X-Conduit-Decision") != "" || d.calls != 0 {
		t.Fatalf("decider consulted in auto mode: header=%q calls=%d", res.Header.Get("X-Conduit-Decision"), d.calls)
	}
}

func post(t *testing.T, url string, body []byte, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func assertNoSecretOnDisk(t *testing.T, dir, secret string) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if redact.ContainsCredential(string(data), secret) {
			t.Errorf("secret %q found in %s", secret, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// postRaw posts a body with explicit headers; used to exercise the route
// endpoint's Origin / content-type guard.
func postRaw(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRoutePostRejectsCrossOriginAndOddContentTypes(t *testing.T) {
	f := &fakeUpstreams{decider: newFakeDecider("glm", "glm-5.3")}
	f.anthropic = okJSON("anthropic")
	f.glm = okJSON("glm")
	srv, br, _ := newGateway(t, f)

	// A page the user happens to have open: cross-origin POST, no preflight,
	// flipping the mode to jev — which starts sending prompt excerpts to
	// TypeSafe. Must be refused, and must not move the mode.
	res := postRaw(t, srv.URL+"/_gateway/route", `{"mode":"jev"}`, map[string]string{
		"Origin":       "https://evil.example",
		"Content-Type": "application/json",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST status=%d want 403", res.StatusCode)
	}
	// Same request as a preflight-free "simple" form/text-plain post.
	res = postRaw(t, srv.URL+"/_gateway/route", `{"mode":"jev"}`, map[string]string{
		"Origin":       "https://evil.example",
		"Content-Type": "text/plain",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin text/plain status=%d want 403", res.StatusCode)
	}
	if br.Mode() != route.ModeAuto {
		t.Fatalf("cross-origin POST changed the mode: %s", br.Mode())
	}

	// No Origin (curl, UI): the content type decides. text/plain and a missing
	// header are refused, the two documented client forms are not.
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"text/plain", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"none", nil, http.StatusUnsupportedMediaType},
		{"json", map[string]string{"Content-Type": "application/json"}, http.StatusOK},
		{"curl-form", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusOK},
		{"json-charset", map[string]string{"Content-Type": "application/json; charset=utf-8"}, http.StatusOK},
	} {
		res := postRaw(t, srv.URL+"/_gateway/route", `{"mode":"auto"}`, tc.hdr)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s: status=%d want %d", tc.name, res.StatusCode, tc.want)
		}
	}

	// The UI's own fetch: same-origin JSON goes through and takes effect.
	res = postRaw(t, srv.URL+"/_gateway/route", `{"mode":"jev"}`, map[string]string{
		"Origin":       srv.URL,
		"Content-Type": "application/json",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("own-origin POST status=%d want 200", res.StatusCode)
	}
	if br.Mode() != route.ModeJev {
		t.Fatalf("own-origin POST did not apply: mode=%s", br.Mode())
	}
}

func TestJevQuotaKeysBreakerOnServedModel(t *testing.T) {
	// Jev answers with haiku for an opus request. The 429 comes from haiku, so
	// haiku is what opens — the inbound id must stay untouched — and the GLM
	// tier the notice points at is haiku's mapping, not opus's.
	d := newFakeDecider("anthropic", "claude-haiku-4-5")
	f := &fakeUpstreams{decider: d}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}
	f.glm = okJSON("glm")
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Conduit-Provider") != "glm" {
		t.Fatalf("status=%d provider=%q", res.StatusCode, res.Header.Get("X-Conduit-Provider"))
	}

	if st := br.Decide("anthropic", "claude-haiku-4-5", time.Now()); st != breaker.Open {
		t.Fatalf("served model entry=%s want OPEN", st)
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("requested model must stay CLOSED, got %s", st)
	}

	// The GLM tier and the notice name that same served model.
	f.mu.Lock()
	glmBody := f.glmBodies[0]
	f.mu.Unlock()
	var sent map[string]any
	if err := json.Unmarshal(glmBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "glm-5.3-flash" {
		t.Fatalf("glm model=%v want glm-5.3-flash (haiku's mapping)", sent["model"])
	}
	raw, err := os.ReadFile(notify.RouteFile())
	if err != nil {
		t.Fatalf("notice route file: %v", err)
	}
	var notice map[string]any
	if err := json.Unmarshal(raw, &notice); err != nil {
		t.Fatal(err)
	}
	if notice["model"] != "claude-haiku-4-5" || notice["upstream_model"] != "glm-5.3-flash" {
		t.Fatalf("notice names the wrong pair: %s", raw)
	}
}

func TestJevProbeRoutesToAnthropicAndResolves(t *testing.T) {
	// The router would pick glm, but the requested model has a probe due: the
	// probe exists to test Anthropic, so jev must not be consulted at all —
	// otherwise the entry stays in PROBE forever in a jev-only workflow.
	d := newFakeDecider("glm", "glm-5.3")
	f := &fakeUpstreams{decider: d}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Error("glm must not serve a due probe") }
	var anthBody []byte
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		anthBody, _ = io.ReadAll(r.Body)
		okJSON("anthropic")(w, r)
	}
	srv, br, _ := newGateway(t, f)
	br.SetMode(route.ModeJev)

	now := time.Now()
	br.Open("anthropic", "claude-opus-5", "rate_limit_error", now.Add(-time.Second), now.Add(-time.Minute))
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Probe {
		t.Fatalf("setup: state=%s want PROBE", st)
	}

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Conduit-Provider") != "anthropic" {
		t.Fatalf("status=%d provider=%q", res.StatusCode, res.Header.Get("X-Conduit-Provider"))
	}
	if n := d.callsCount(); n != 0 {
		t.Fatalf("router consulted %d times while a probe was due", n)
	}
	// The probe is the auto path: no decision header, requested model unchanged.
	if got := res.Header.Get("X-Conduit-Decision"); got != "" {
		t.Fatalf("X-Conduit-Decision=%q on the probe path", got)
	}
	if !bytes.Equal(anthBody, []byte(jevReq)) {
		t.Fatalf("probe body rewritten: %s", anthBody)
	}
	if st := br.Decide("anthropic", "claude-opus-5", time.Now()); st != breaker.Closed {
		t.Fatalf("probe did not resolve: state=%s", st)
	}
}

func TestJevCandidateFilteringLeavesBreakerStateAlone(t *testing.T) {
	// A Jev request for one model must not move breaker state — or rewrite
	// state.json — for catalog models this call never reaches.
	d := newFakeDecider("glm", "glm-5.3")
	f := &fakeUpstreams{decider: d}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) { t.Error("anthropic must not be hit") }
	f.glm = okJSON("glm")
	srv, br, dir := newGateway(t, f)
	br.SetMode(route.ModeJev)

	// haiku: an OPEN entry whose window already expired, i.e. one Decide would
	// flip to PROBE (persisting and logging) if filtering consulted it.
	now := time.Now()
	br.Open("anthropic", "claude-haiku-4-5", "rate_limit_error", now.Add(-time.Second), now.Add(-time.Minute))
	statePath := filepath.Join(dir, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	res := post(t, srv.URL+"/v1/messages", []byte(jevReq), fakeToken)
	res.Body.Close()

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("candidate filtering rewrote state.json:\n%s\n---\n%s", before, after)
	}
	if e, ok := br.Entry("anthropic", "claude-haiku-4-5"); !ok || e.State != breaker.Open {
		t.Fatalf("candidate filtering transitioned haiku: %+v ok=%v", e, ok)
	}
	logs, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
	if bytes.Contains(logs, []byte("BREAKER PROBE claude-haiku-4-5")) {
		t.Fatalf("candidate filtering logged a probe transition:\n%s", logs)
	}
}

func TestProxyBodyOverCapRejectedCleanly(t *testing.T) {
	f := &fakeUpstreams{}
	f.anthropic = okJSON("anthropic")
	f.glm = okJSON("glm")
	srv, _, _ := newGateway(t, f)

	var buf bytes.Buffer
	buf.WriteString(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"`)
	buf.Write(bytes.Repeat([]byte("x"), 33<<20)) // past the 32 MiB gateway cap
	buf.WriteString(`"}]}`)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fakeToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversized body was not rejected cleanly at the HTTP level: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d want 413 (body=%s)", res.StatusCode, body)
	}
	payload, _ := io.ReadAll(res.Body)
	if !bytes.Contains(payload, []byte("limit")) {
		t.Fatalf("413 body is not a readable error: %s", payload)
	}
	if f.anthropicHits.Load() != 0 || f.glmHits.Load() != 0 || f.deepSeekHits.Load() != 0 {
		t.Fatalf("oversized body reached an upstream: anth=%d glm=%d ds=%d",
			f.anthropicHits.Load(), f.glmHits.Load(), f.deepSeekHits.Load())
	}
}

func TestDeepSeekOmitsToolsKeyWhenOnlyArtifact(t *testing.T) {
	f := &fakeUpstreams{enableDeepSeek: true}
	f.deepseek = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_ds","type":"message","role":"assistant","content":[{"type":"text","text":"ds"}]}`))
	}
	srv, br, _ := newGateway(t, f)
	br.SetForce("deepseek", "")

	// Only tool is Artifact ⇒ the key is dropped entirely, not sent as [].
	reqBody := []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[
		{"name":"Artifact","description":"render","input_schema":{"type":"object","properties":{"content":{"type":"string"}}}}
	]}`)
	res := post(t, srv.URL+"/v1/messages", reqBody, fakeToken)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d", res.StatusCode)
	}
	f.mu.Lock()
	body := f.deepSeekBodies[0]
	f.mu.Unlock()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if raw, ok := m["tools"]; ok {
		t.Fatalf("tools key kept after dropping Artifact: %s", raw)
	}

	// A client-sent empty list, and a list with no Artifact, pass through.
	f.mu.Lock()
	f.deepSeekBodies = nil
	f.mu.Unlock()
	for _, body := range []string{
		`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[]}`,
		`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`,
	} {
		res := post(t, srv.URL+"/v1/messages", []byte(body), fakeToken)
		res.Body.Close()
	}
	f.mu.Lock()
	if len(f.deepSeekBodies) != 2 {
		f.mu.Unlock()
		t.Fatalf("deepseek bodies=%d", len(f.deepSeekBodies))
	}
	var empty struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(f.deepSeekBodies[0], &empty); err != nil {
		f.mu.Unlock()
		t.Fatal(err)
	}
	if len(f.deepSeekBodies[0]) == 0 || !bytes.Contains(f.deepSeekBodies[0], []byte(`"tools":[]`)) {
		f.mu.Unlock()
		t.Fatalf("client-sent empty tools list not preserved: %s", f.deepSeekBodies[0])
	}
	var bash struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(f.deepSeekBodies[1], &bash); err != nil {
		f.mu.Unlock()
		t.Fatal(err)
	}
	f.mu.Unlock()
	if len(bash.Tools) != 1 || bash.Tools[0].Name != "Bash" {
		t.Fatalf("tools=%v want [Bash]", bash.Tools)
	}
}
