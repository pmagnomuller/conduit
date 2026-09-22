package route

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pedro-mueller/conduit/internal/config"
)

var testCatalog = []Candidate{
	{Provider: "anthropic", Model: "claude-opus-5", Profile: "hard"},
	{Provider: "anthropic", Model: "claude-sonnet-5", Profile: "medium"},
	{Provider: "glm", Model: "glm-5.3-flash", Profile: "cheap"},
}

// jevStub serves canned answers and records requests.
type jevStub struct {
	choice  string
	lease   string
	status  int
	errBody string // overrides the canned error body when set
	delay   time.Duration
	calls   atomic.Int32
	last    atomic.Pointer[jevRequest]
	gotKey  atomic.Pointer[string]
}

func (s *jevStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		auth := r.Header.Get("Authorization")
		s.gotKey.Store(&auth)
		body, _ := io.ReadAll(r.Body)
		var req jevRequest
		_ = json.Unmarshal(body, &req)
		s.last.Store(&req)
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-r.Context().Done():
				return
			}
		}
		if s.status != 0 && s.status != 200 {
			w.WriteHeader(s.status)
			body := s.errBody
			if body == "" {
				body = `{"error":"nope","token":"Bearer sk-ant-secret123"}`
			}
			_, _ = w.Write([]byte(body))
			return
		}
		_ = json.NewEncoder(w).Encode(jevResponse{Answers: map[string]jevAnswer{
			"model": {Choice: s.choice, Confidence: 0.83},
			"lease": {Choice: s.lease, Confidence: 0.6},
		}})
	})
}

func newTestRouter(t *testing.T, stub *jevStub, mutate func(*config.JevConfig)) (*Router, *jevStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	cfg := config.JevConfig{
		BaseURL:         srv.URL,
		TimeoutMS:       500,
		LeaseTTLSeconds: 600,
		DecisionsPath:   "",
		Catalog:         testCatalog,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg, "test-key", slog.New(slog.NewTextHandler(io.Discard, nil))), stub
}

func userTurn(t *testing.T, text string) []byte {
	return mustJSON(t, map[string]any{
		"model":  "claude-opus-5",
		"system": "sys",
		"messages": []any{
			map[string]any{"role": "user", "content": "opening message"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": text},
		},
	})
}

func toolStep(t *testing.T, tools ...string) []byte {
	var uses, results []any
	for i, name := range tools {
		id := "t" + string(rune('a'+i))
		uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"})
	}
	return mustJSON(t, map[string]any{
		"model":  "claude-opus-5",
		"system": "sys",
		"messages": []any{
			map[string]any{"role": "user", "content": "opening message"},
			map[string]any{"role": "assistant", "content": uses},
			map[string]any{"role": "user", "content": results},
		},
	})
}

func TestDisabledRouter(t *testing.T) {
	r := New(config.JevConfig{Catalog: testCatalog}, "", nil)
	if r.Enabled() {
		t.Fatal("enabled without key")
	}
	if _, ok := r.Decide(context.Background(), userTurn(t, "x"), testCatalog); ok {
		t.Fatal("disabled router must return ok=false")
	}
	if len(r.Recent(10)) != 0 {
		t.Fatal("disabled router should not record")
	}
	if len(r.Catalog()) != len(testCatalog) {
		t.Fatal("catalog lost")
	}
}

func TestDecideHappyPath(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}, nil)
	d, ok := r.Decide(context.Background(), userTurn(t, "format this file"), testCatalog)
	if !ok {
		t.Fatalf("ok=false: %+v", d)
	}
	if d.Provider != "glm" || d.Model != "glm-5.3-flash" || d.Source != SourceJev || d.Lease != LeaseOneCall {
		t.Fatalf("decision=%+v", d)
	}
	if d.Confidence != 0.83 || d.Step != StepUserTurn || d.RequestedModel != "claude-opus-5" {
		t.Fatalf("decision=%+v", d)
	}
	req := stub.last.Load()
	if req == nil || req.Model != "jev-latest" {
		t.Fatalf("request=%+v", req)
	}
	if got := *stub.gotKey.Load(); got != "Bearer test-key" {
		t.Fatalf("auth=%q", got)
	}
	mq := req.Questions["model"]
	if mq.Type != "choice" || mq.Instructions != modelInstructions {
		t.Fatalf("model question=%+v", mq)
	}
	if mq.Criteria["anthropic/claude-opus-5"] != "hard" || len(mq.Criteria) != 3 {
		t.Fatalf("criteria=%v", mq.Criteria)
	}
	lq := req.Questions["lease"]
	for _, k := range []string{LeaseOneCall, LeaseToolChain, LeaseUserTurn} {
		if lq.Criteria[k] == "" {
			t.Fatalf("lease criteria missing %s", k)
		}
	}
	if req.State.Task != "format this file" {
		t.Fatalf("state=%+v", req.State)
	}
	rec := r.Recent(5)
	if len(rec) != 1 || rec[0].Source != SourceJev {
		t.Fatalf("recent=%+v", rec)
	}
}

func TestDecideInvalidChoiceFailsOpen(t *testing.T) {
	r, _ := newTestRouter(t, &jevStub{choice: "openai/gpt-9", lease: LeaseOneCall}, nil)
	d, ok := r.Decide(context.Background(), userTurn(t, "x"), testCatalog)
	if ok {
		t.Fatal("expected ok=false")
	}
	if d.Source != SourceFailOpen || !strings.Contains(d.Reason, "not in catalog") || d.Provider != "" {
		t.Fatalf("decision=%+v", d)
	}
	if rec := r.Recent(1); len(rec) != 1 || rec[0].Source != SourceFailOpen {
		t.Fatalf("recent=%+v", rec)
	}
}

func TestDecideHTTPErrorRedacted(t *testing.T) {
	r, _ := newTestRouter(t, &jevStub{status: 500}, nil)
	d, ok := r.Decide(context.Background(), userTurn(t, "x"), testCatalog)
	if ok || d.Source != SourceFailOpen {
		t.Fatalf("decision=%+v ok=%v", d, ok)
	}
	if !strings.Contains(d.Reason, "status 500") || strings.Contains(d.Reason, "secret123") {
		t.Fatalf("reason=%q", d.Reason)
	}
}

func TestDecideTimeoutFailsOpen(t *testing.T) {
	r, _ := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall, delay: 2 * time.Second},
		func(c *config.JevConfig) { c.TimeoutMS = 100 })
	start := time.Now()
	d, ok := r.Decide(context.Background(), userTurn(t, "x"), testCatalog)
	if ok || d.Source != SourceFailOpen {
		t.Fatalf("decision=%+v ok=%v", d, ok)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Decide blocked %v past timeout", el)
	}
	// Caller cancellation is honoured too.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := r.Decide(ctx, userTurn(t, "x"), testCatalog); ok {
		t.Fatal("cancelled ctx should fail open")
	}
}

func TestDecideNoCandidatesFailsOpen(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash"}, nil)
	d, ok := r.Decide(context.Background(), userTurn(t, "x"), nil)
	if ok || d.Source != SourceFailOpen || d.Reason != "no candidates" {
		t.Fatalf("decision=%+v ok=%v", d, ok)
	}
	if stub.calls.Load() != 0 {
		t.Fatal("should not call Jev without candidates")
	}
}

func TestLeaseUserTurnReuse(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "anthropic/claude-sonnet-5", lease: LeaseUserTurn}, nil)
	d1, ok := r.Decide(context.Background(), userTurn(t, "opening message"), testCatalog)
	if !ok || d1.Source != SourceJev {
		t.Fatalf("d1=%+v", d1)
	}
	// Tool continuations reuse regardless of tool set.
	d2, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog)
	if !ok || d2.Source != SourceLease || d2.Model != "claude-sonnet-5" || d2.Confidence != 0.83 || d2.Step != StepToolStep {
		t.Fatalf("d2=%+v", d2)
	}
	d3, ok := r.Decide(context.Background(), toolStep(t, "Bash", "Edit"), testCatalog)
	if !ok || d3.Source != SourceLease {
		t.Fatalf("d3=%+v", d3)
	}
	if stub.calls.Load() != 1 {
		t.Fatalf("jev calls=%d", stub.calls.Load())
	}
	// New user turn ends the lease → re-ask.
	d4, ok := r.Decide(context.Background(), userTurn(t, "now something else"), testCatalog)
	if !ok || d4.Source != SourceJev {
		t.Fatalf("d4=%+v", d4)
	}
	if stub.calls.Load() != 2 {
		t.Fatalf("jev calls=%d", stub.calls.Load())
	}
	rec := r.Recent(10)
	if len(rec) != 4 || rec[0].Source != SourceJev || rec[1].Source != SourceLease {
		t.Fatalf("recent order wrong: %+v", rec)
	}
}

func TestLeaseToolChainToolSetChange(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseToolChain}, nil)
	// Decided on a tool step with {Read}.
	d1, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog)
	if !ok || d1.Source != SourceJev {
		t.Fatalf("d1=%+v", d1)
	}
	d2, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog)
	if !ok || d2.Source != SourceLease {
		t.Fatalf("same tool set should reuse: %+v", d2)
	}
	d3, ok := r.Decide(context.Background(), toolStep(t, "Read", "Bash"), testCatalog)
	if !ok || d3.Source != SourceJev {
		t.Fatalf("tool set change should re-ask: %+v", d3)
	}
	if stub.calls.Load() != 2 {
		t.Fatalf("jev calls=%d", stub.calls.Load())
	}
	// Order of tool names must not matter.
	d4, ok := r.Decide(context.Background(), toolStep(t, "Bash", "Read"), testCatalog)
	if !ok || d4.Source != SourceLease {
		t.Fatalf("d4=%+v", d4)
	}
}

func TestLeaseOneCallNeverReuses(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}, nil)
	for i := 0; i < 3; i++ {
		if d, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog); !ok || d.Source != SourceJev {
			t.Fatalf("i=%d d=%+v", i, d)
		}
	}
	if stub.calls.Load() != 3 {
		t.Fatalf("jev calls=%d", stub.calls.Load())
	}
}

func TestLeaseExpiry(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseUserTurn},
		func(c *config.JevConfig) { c.LeaseTTLSeconds = 60 })
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	if _, ok := r.Decide(context.Background(), userTurn(t, "opening message"), testCatalog); !ok {
		t.Fatal("d1")
	}
	now = now.Add(30 * time.Second)
	if d, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog); !ok || d.Source != SourceLease {
		t.Fatalf("within ttl: %+v", d)
	}
	now = now.Add(31 * time.Second) // 61s after the decision that started the lease
	if d, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog); !ok || d.Source != SourceJev {
		t.Fatalf("after ttl: %+v", d)
	}
	if stub.calls.Load() != 2 {
		t.Fatalf("jev calls=%d", stub.calls.Load())
	}
}

func TestLeaseInvalidWhenCandidateFiltered(t *testing.T) {
	r, stub := newTestRouter(t, &jevStub{choice: "anthropic/claude-sonnet-5", lease: LeaseUserTurn}, nil)
	if _, ok := r.Decide(context.Background(), userTurn(t, "opening message"), testCatalog); !ok {
		t.Fatal("d1")
	}
	// Breaker opened: caller drops anthropic. Lease points at a dropped candidate → re-ask.
	stub.choice = "glm/glm-5.3-flash"
	filtered := []Candidate{testCatalog[2]}
	d, ok := r.Decide(context.Background(), toolStep(t, "Read"), filtered)
	if !ok || d.Source != SourceJev || d.Provider != "glm" {
		t.Fatalf("d=%+v", d)
	}
	if stub.calls.Load() != 2 {
		t.Fatalf("jev calls=%d", stub.calls.Load())
	}
}

func TestDecisionsJSONLAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "decisions.jsonl")
	r, _ := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall},
		func(c *config.JevConfig) { c.DecisionsPath = path })
	_, _ = r.Decide(context.Background(), userTurn(t, "a"), testCatalog)
	_, _ = r.Decide(context.Background(), userTurn(t, "b"), nil) // fail_open also logged
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d: %s", len(lines), data)
	}
	var d0, d1 Decision
	if err := json.Unmarshal([]byte(lines[0]), &d0); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &d1); err != nil {
		t.Fatal(err)
	}
	if d0.Source != SourceJev || d1.Source != SourceFailOpen {
		t.Fatalf("d0=%+v d1=%+v", d0, d1)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm=%o", st.Mode().Perm())
	}
}

func TestRecentRingCap(t *testing.T) {
	r, _ := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}, nil)
	for i := 0; i < recentCap+5; i++ {
		_, _ = r.Decide(context.Background(), userTurn(t, "x"), nil)
	}
	if got := len(r.Recent(0)); got != recentCap {
		t.Fatalf("recent=%d", got)
	}
	if got := len(r.Recent(3)); got != 3 {
		t.Fatalf("recent(3)=%d", got)
	}
}

func TestParseMode(t *testing.T) {
	cases := map[string]struct {
		m  Mode
		ok bool
	}{
		"": {ModeAuto, true}, "auto": {ModeAuto, true}, "pinned": {ModePinned, true},
		"jev": {ModeJev, true}, "JEV": {"", false}, "bogus": {"", false},
	}
	for in, want := range cases {
		m, ok := ParseMode(in)
		if m != want.m || ok != want.ok {
			t.Errorf("ParseMode(%q)=%q,%v want %q,%v", in, m, ok, want.m, want.ok)
		}
	}
}

// readDecisions parses the lines of a decisions log, newest last, and reports
// its size on disk. It fails on a torn line: rotation must never leave one.
func readDecisions(t *testing.T, path string) ([]Decision, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []Decision
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if ln == "" {
			continue
		}
		var d Decision
		if err := json.Unmarshal([]byte(ln), &d); err != nil {
			t.Fatalf("torn line in %s: %q", path, ln)
		}
		out = append(out, d)
	}
	return out, len(raw)
}

// The log is append-only diagnostics on the request path: it must rotate at
// its cap (keeping one predecessor) instead of growing without bound.
func TestDecisionsLogRotatesAtCap(t *testing.T) {
	old := decisionsMaxBytes
	decisionsMaxBytes = 512
	t.Cleanup(func() { decisionsMaxBytes = old })

	dir := t.TempDir()
	path := filepath.Join(dir, "decisions.jsonl")
	r, _ := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall},
		func(c *config.JevConfig) { c.DecisionsPath = path })

	const n = 20
	var last time.Time
	for i := 0; i < n; i++ {
		d, ok := r.Decide(context.Background(), toolStep(t, "Read"), testCatalog)
		if !ok {
			t.Fatalf("decision %d not ok: %+v", i, d)
		}
		last = d.At
	}
	live, liveSize := readDecisions(t, path)
	prev, prevSize := readDecisions(t, path+".1")
	if len(live) == 0 || len(prev) == 0 {
		t.Fatalf("want both files populated: live=%d prev=%d", len(live), len(prev))
	}
	if int64(liveSize) > decisionsMaxBytes || int64(prevSize) > decisionsMaxBytes {
		t.Fatalf("cap %d exceeded: live=%d prev=%d", decisionsMaxBytes, liveSize, prevSize)
	}
	// The newest decision is always in the live file, the predecessor is older.
	if !live[len(live)-1].At.Equal(last) {
		t.Fatalf("live log does not end with the newest decision: %v != %v", live[len(live)-1].At, last)
	}
	if prev[len(prev)-1].At.After(live[0].At) {
		t.Fatalf("rotated file holds newer decisions: %v > %v", prev[len(prev)-1].At, live[0].At)
	}
}

// Everything client-supplied is clipped before Jev sees it, before the ring
// holds it and before it reaches the log the UI polls.
func TestDecisionsStayBoundedForHugeClientStrings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions.jsonl")
	r, stub := newTestRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall},
		func(c *config.JevConfig) { c.DecisionsPath = path })

	body := mustJSON(t, map[string]any{
		"model": strings.Repeat("m", 1<<20),
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("t", 1<<20)},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": strings.Repeat("n", 1<<19), "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": strings.Repeat("r", 1<<20)},
			}},
		},
	})
	if _, ok := r.Decide(context.Background(), body, testCatalog); !ok {
		t.Fatal("expected a jev decision")
	}

	req := stub.last.Load()
	if req == nil {
		t.Fatal("no request reached the decision service")
	}
	if len(req.State.RequestedModel) > modelClip {
		t.Fatalf("posted %d bytes of requested_model", len(req.State.RequestedModel))
	}
	if tb := req.State.ToolBatch; tb != nil {
		for _, n := range tb.Names {
			if len(n) > toolNameClip {
				t.Fatalf("posted a %d byte tool name", len(n))
			}
		}
	}
	if rec := r.Recent(1); len(rec) != 1 || len(rec[0].RequestedModel) > modelClip {
		t.Fatalf("ring holds %+v", rec)
	}
	_, size := readDecisions(t, path)
	if size > 4<<10 {
		t.Fatalf("decisions log line grew to %d bytes", size)
	}
}

// Reasons land in the ring and in the log, so the far end's error text and any
// echoed choice must be clipped, rune-safe.
func TestFailOpenReasonStaysBounded(t *testing.T) {
	// 9-byte periods: a byte cut at maxErrBody would land mid-rune.
	noise := strings.Repeat("é日😀", 1<<15)

	r, _ := newTestRouter(t, &jevStub{status: 500, errBody: noise}, nil)
	d, ok := r.Decide(context.Background(), userTurn(t, "x"), testCatalog)
	if ok || d.Source != SourceFailOpen {
		t.Fatalf("decision=%+v ok=%v", d, ok)
	}
	if !strings.Contains(d.Reason, "status 500") || len(d.Reason) > 2*maxErrBody {
		t.Fatalf("reason=%d bytes", len(d.Reason))
	}
	if !utf8.ValidString(d.Reason) {
		t.Fatal("error body was cut mid-rune")
	}

	// A service answer that echoes a huge choice must not become a huge reason.
	big := &jevStub{choice: strings.Repeat("é日😀", 1<<15), lease: LeaseOneCall}
	r2, _ := newTestRouter(t, big, nil)
	d2, ok := r2.Decide(context.Background(), userTurn(t, "x"), testCatalog)
	if ok || d2.Source != SourceFailOpen {
		t.Fatalf("decision=%+v ok=%v", d2, ok)
	}
	if !strings.Contains(d2.Reason, "not in catalog") || len(d2.Reason) > 2*maxErrBody {
		t.Fatalf("reason=%d bytes", len(d2.Reason))
	}
	if !utf8.ValidString(d2.Reason) {
		t.Fatal("echoed choice was cut mid-rune")
	}
}
