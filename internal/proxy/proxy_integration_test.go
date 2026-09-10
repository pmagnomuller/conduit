package proxy_test

import (
	"bytes"
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
		"claude-opus-5": "glm-5.3",
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
	gw := proxy.New(cfg, br, capWriter, met, log)

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
		ForcedProvider string `json:"forced_provider"`
		ForcedModel    string `json:"forced_model"`
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
