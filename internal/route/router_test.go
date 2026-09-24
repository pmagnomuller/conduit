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
	"regexp"
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
	conf    float64            // model confidence; 0 → 0.83
	probs   map[string]float64 // model probabilities; nil → none
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
		conf := s.conf
		if conf == 0 {
			conf = 0.83
		}
		_ = json.NewEncoder(w).Encode(jevResponse{Answers: map[string]jevAnswer{
			"model": {Choice: s.choice, Confidence: conf, Probabilities: s.probs},
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

// Switch policy: the first call establishes the thread's current model; a
// later low-confidence switch on a large context stays put.
func TestSwitchPolicySticky(t *testing.T) {
	stub := &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}
	r, _ := newTestRouter(t, stub, func(c *config.JevConfig) { c.MaxSwitchContext = 10 })
	d1, ok := r.Decide(context.Background(), userTurn(t, "first"), testCatalog)
	if !ok || d1.Source != SourceJev || d1.Provider != "glm" {
		t.Fatalf("d1=%+v", d1)
	}

	stub.choice, stub.conf = "anthropic/claude-opus-5", 0.5
	d2, ok := r.Decide(context.Background(), userTurn(t, "second"), testCatalog)
	if !ok || d2.Source != SourceSticky || d2.Provider != "glm" || d2.Model != "glm-5.3-flash" {
		t.Fatalf("d2=%+v", d2)
	}
	if d2.Pick != "anthropic/claude-opus-5" || d2.Lease != LeaseOneCall {
		t.Fatalf("override not recorded: %+v", d2)
	}
	req := stub.last.Load()
	if req.State.Current != "glm/glm-5.3-flash" || !req.State.CacheWarm || req.State.ContextTokensEst <= 10 {
		t.Fatalf("dossier lacks switch cost: %+v", req.State)
	}

	stub.conf = 0.9
	d3, _ := r.Decide(context.Background(), userTurn(t, "third"), testCatalog)
	if d3.Source != SourceJev || d3.Provider != "anthropic" {
		t.Fatalf("confident switch should pass: %+v", d3)
	}
}

func TestSwitchPolicySmallContextSwitchesFreely(t *testing.T) {
	stub := &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}
	r, _ := newTestRouter(t, stub, nil) // default threshold ≫ test bodies
	r.Decide(context.Background(), userTurn(t, "first"), testCatalog)
	stub.choice, stub.conf = "anthropic/claude-opus-5", 0.5
	d, _ := r.Decide(context.Background(), userTurn(t, "second"), testCatalog)
	if d.Source != SourceJev || d.Provider != "anthropic" {
		t.Fatalf("d=%+v", d)
	}
}

func TestSwitchPolicyLowMargin(t *testing.T) {
	stub := &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}
	r, _ := newTestRouter(t, stub, nil)
	r.Decide(context.Background(), userTurn(t, "first"), testCatalog)

	stub.choice = "anthropic/claude-sonnet-5"
	stub.probs = map[string]float64{"anthropic/claude-sonnet-5": 0.45, "glm/glm-5.3-flash": 0.40, "anthropic/claude-opus-5": 0.15}
	d, _ := r.Decide(context.Background(), userTurn(t, "second"), testCatalog)
	if d.Source != SourceLowConfidence || d.Provider != "glm" || d.Pick != "anthropic/claude-sonnet-5" {
		t.Fatalf("d=%+v", d)
	}
	if d.Margin < 0.049 || d.Margin > 0.051 {
		t.Fatalf("margin=%v", d.Margin)
	}

	stub.probs = map[string]float64{"anthropic/claude-sonnet-5": 0.8, "glm/glm-5.3-flash": 0.1}
	d, _ = r.Decide(context.Background(), userTurn(t, "third"), testCatalog)
	if d.Source != SourceJev || d.Model != "claude-sonnet-5" {
		t.Fatalf("clear margin should pass: %+v", d)
	}
}

func TestSwitchPolicyDisabled(t *testing.T) {
	stub := &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}
	r, _ := newTestRouter(t, stub, func(c *config.JevConfig) { c.MinMargin, c.MaxSwitchContext = -1, -1 })
	r.Decide(context.Background(), userTurn(t, "first"), testCatalog)
	stub.choice, stub.conf = "anthropic/claude-opus-5", 0.1
	stub.probs = map[string]float64{"anthropic/claude-opus-5": 0.5, "glm/glm-5.3-flash": 0.49}
	d, _ := r.Decide(context.Background(), userTurn(t, "second"), testCatalog)
	if d.Source != SourceJev || d.Provider != "anthropic" {
		t.Fatalf("d=%+v", d)
	}
}

// A current model that is no longer a candidate (breaker OPEN) is not a place
// to stay, and is not reported to Jev.
func TestSwitchPolicyCurrentFilteredOut(t *testing.T) {
	stub := &jevStub{choice: "anthropic/claude-opus-5", lease: LeaseOneCall}
	r, _ := newTestRouter(t, stub, func(c *config.JevConfig) { c.MaxSwitchContext = 10 })
	r.Decide(context.Background(), userTurn(t, "first"), testCatalog)
	stub.choice, stub.conf = "glm/glm-5.3-flash", 0.1
	d, _ := r.Decide(context.Background(), userTurn(t, "second"), testCatalog[2:])
	if d.Source != SourceJev || d.Provider != "glm" {
		t.Fatalf("d=%+v", d)
	}
	if cur := stub.last.Load().State.Current; cur != "" {
		t.Fatalf("current=%q", cur)
	}
}

func TestMargin(t *testing.T) {
	crit := map[string]string{"a": "", "b": "", "c": ""}
	cases := []struct {
		probs map[string]float64
		want  float64
		ok    bool
	}{
		{nil, 0, false},
		{map[string]float64{"a": 1}, 0, false},
		{map[string]float64{"a": 0.6, "x": 0.9}, 0, false}, // unknown keys ignored
		{map[string]float64{"a": 0.2, "b": 0.7, "c": 0.1}, 0.5, true},
		{map[string]float64{"c": 0.3, "b": 0.3}, 0, true},
	}
	for _, tc := range cases {
		got, ok := margin(tc.probs, crit)
		if ok != tc.ok || (ok && (got < tc.want-1e-9 || got > tc.want+1e-9)) {
			t.Errorf("margin(%v)=%v,%v want %v,%v", tc.probs, got, ok, tc.want, tc.ok)
		}
	}
}

func restrictedRouter(t *testing.T, stub *jevStub) (*Router, *jevStub) {
	return newTestRouter(t, stub, func(c *config.JevConfig) {
		c.RestrictSensitive = true
		c.RestrictedProviders = []string{"glm", "deepseek"}
		c.RestrictedPatterns = config.DefaultRestrictedPatterns()
	})
}

func readStep(t *testing.T, path, result string) []byte {
	return mustJSON(t, map[string]any{
		"model":  "claude-opus-5",
		"system": "sys",
		"messages": []any{
			map[string]any{"role": "user", "content": "opening message"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "ta", "name": "Read", "input": map[string]any{"file_path": path},
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "ta", "content": result,
			}}},
		},
	})
}

func TestRestrictedDropsUntrustedProviders(t *testing.T) {
	r, stub := restrictedRouter(t, &jevStub{choice: "anthropic/claude-sonnet-5", lease: LeaseOneCall})
	d, ok := r.Decide(context.Background(), readStep(t, "/repo/.env", "API_KEY=x"), testCatalog)
	if !ok || d.Policy != PolicyRestricted || d.Provider != "anthropic" {
		t.Fatalf("d=%+v", d)
	}
	req := stub.last.Load()
	if !req.State.Sensitive {
		t.Fatal("dossier not marked sensitive")
	}
	for k := range req.Questions["model"].Criteria {
		if strings.HasPrefix(k, "glm/") {
			t.Fatalf("untrusted candidate offered: %s", k)
		}
	}
}

// Jev's answer is untrusted: naming a filtered provider falls open rather
// than routing secrets to it.
func TestRestrictedJevCannotRouteAround(t *testing.T) {
	r, _ := restrictedRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall})
	d, ok := r.Decide(context.Background(), readStep(t, "/home/u/.ssh/id_ed25519", "-----BEGIN"), testCatalog)
	if ok || d.Source != SourceFailOpen || d.Policy != PolicyRestricted {
		t.Fatalf("d=%+v ok=%v", d, ok)
	}
}

func TestRestrictedNoTrustedCandidate(t *testing.T) {
	r, stub := restrictedRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall})
	_, ok := r.Decide(context.Background(), readStep(t, "infra/prod.tfvars", "x"), testCatalog[2:])
	if ok || stub.calls.Load() != 0 {
		t.Fatalf("ok=%v calls=%d", ok, stub.calls.Load())
	}
}

// A lease granted before the secret showed up must not carry the thread to
// an untrusted provider.
func TestRestrictedBreaksUntrustedLease(t *testing.T) {
	r, stub := restrictedRouter(t, &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseUserTurn})
	d1, _ := r.Decide(context.Background(), readStep(t, "src/main.go", "package main"), testCatalog)
	if d1.Provider != "glm" || d1.Policy != "" {
		t.Fatalf("d1=%+v", d1)
	}
	stub.choice = "anthropic/claude-opus-5"
	d2, ok := r.Decide(context.Background(), readStep(t, "src/.env.local", "SECRET=1"), testCatalog)
	if !ok || d2.Source != SourceJev || d2.Provider != "anthropic" {
		t.Fatalf("d2=%+v", d2)
	}
}

func TestRestrictedPatterns(t *testing.T) {
	var res []*regexp.Regexp
	for _, p := range config.DefaultRestrictedPatterns() {
		res = append(res, regexp.MustCompile(p))
	}
	hit := func(s string) bool {
		raw, _ := json.Marshal(s)
		for _, re := range res {
			if re.Match(raw) {
				return true
			}
		}
		return false
	}
	for _, s := range []string{".env", "cat .env.production", "a/b/.env", "ls\n.env", "~/.ssh/config",
		"id_rsa", "cert.pem", "prod.tfvars", "~/.kube/config", "~/.aws/credentials", "secrets.yaml"} {
		if !hit(s) {
			t.Errorf("expected match: %q", s)
		}
	}
	for _, s := range []string{"process.env.FOO", "environment", "item.key", "obj.keys()", "src/main.go", "envelope"} {
		if hit(s) {
			t.Errorf("unexpected match: %q", s)
		}
	}
}

// Cost estimate: prices come from the default catalog when the configured
// entry has none; staying on a warm model is priced at the cache-read rate,
// and the baseline (requested model under auto) is warm once the thread is.
func TestDecisionCostEstimate(t *testing.T) {
	stub := &jevStub{choice: "glm/glm-5.3-flash", lease: LeaseOneCall}
	r, _ := newTestRouter(t, stub, nil)
	near := func(got, want float64) bool { return got > want*0.999 && got < want*1.001 }

	d1, _ := r.Decide(context.Background(), userTurn(t, "first"), testCatalog)
	tok := float64(stub.last.Load().State.ContextTokensEst)
	if !near(d1.EstInputUSD, tok*0.15/1e6) || !near(d1.BaselineInputUSD, tok*5/1e6) {
		t.Fatalf("cold: est=%v base=%v tok=%v", d1.EstInputUSD, d1.BaselineInputUSD, tok)
	}

	d2, _ := r.Decide(context.Background(), userTurn(t, "again"), testCatalog)
	tok = float64(stub.last.Load().State.ContextTokensEst)
	if !near(d2.EstInputUSD, tok*0.03/1e6) || !near(d2.BaselineInputUSD, tok*0.5/1e6) {
		t.Fatalf("warm stay: est=%v base=%v", d2.EstInputUSD, d2.BaselineInputUSD)
	}

	stub.choice = "anthropic/claude-opus-5"
	d3, _ := r.Decide(context.Background(), userTurn(t, "switch"), testCatalog)
	tok = float64(stub.last.Load().State.ContextTokensEst)
	if !near(d3.EstInputUSD, tok*5/1e6) || !near(d3.BaselineInputUSD, tok*0.5/1e6) {
		t.Fatalf("switch pays full input: est=%v base=%v", d3.EstInputUSD, d3.BaselineInputUSD)
	}
}

func TestPriceTableOverride(t *testing.T) {
	tbl := priceTable([]Candidate{
		{Provider: "glm", Model: "glm-5.3", PriceIn: 9},
		{Provider: "x", Model: "y", PriceIn: 1, PriceCacheRead: 0.1},
	})
	if p := tbl["glm/glm-5.3"]; p.PriceIn != 9 || p.PriceCacheRead != 0.26 {
		t.Fatalf("overlay: %+v", p)
	}
	if p := tbl["x/y"]; p.PriceIn != 1 {
		t.Fatalf("new entry: %+v", p)
	}
}
