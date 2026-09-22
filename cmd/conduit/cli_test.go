package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request the CLI sent, kept so tests can assert the exact
// method, path, content type and body of every route form.
type recorded struct {
	Method string
	Path   string
	CT     string
	Body   string
	Origin string
}

type reply struct {
	status int
	body   string
}

// stub is an httptest gateway that records requests and answers by path.
type stub struct {
	*httptest.Server
	mu   sync.Mutex
	recs []recorded
}

func newStub(t *testing.T, replies map[string]reply) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		s.mu.Lock()
		s.recs = append(s.recs, recorded{
			Method: r.Method,
			Path:   r.URL.Path,
			CT:     r.Header.Get("Content-Type"),
			Body:   string(body),
			Origin: r.Header.Get("Origin"),
		})
		s.mu.Unlock()
		// Mirror routePostAllowed: a POST the real gateway would refuse must
		// fail the test too, so "the CLI's POST is accepted" is asserted
		// rather than assumed.
		if r.Method == http.MethodPost && r.URL.Path == "/_gateway/route" {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, `{"error":"cross-origin request rejected"}`, http.StatusForbidden)
				return
			}
			ct := r.Header.Get("Content-Type")
			if i := strings.IndexByte(ct, ';'); i >= 0 {
				ct = strings.TrimSpace(ct[:i])
			}
			if ct != "application/json" && ct != "application/x-www-form-urlencoded" {
				http.Error(w, `{"error":"Content-Type must be application/json"}`, http.StatusUnsupportedMediaType)
				return
			}
		}
		rep, ok := replies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.recs...)
}

func useStub(t *testing.T, s *stub) {
	t.Helper()
	t.Setenv("CONDUIT_GATEWAY_URL", s.URL)
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	rc := run(args, &out, &errOut)
	return rc, out.String(), errOut.String()
}

func jsonEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("body %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want %q is not JSON: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("body = %s, want %s", got, want)
	}
}

const routeGET = `{
  "mode": "jev",
  "forced_provider": "glm",
  "forced_model": "glm-5.3",
  "available": {"glm": ["glm-5.3"]},
  "jev": {"enabled": true},
  "last_request": {"provider": "glm", "upstream_model": "glm-5.3-flash", "at": "2026-09-22T10:14:03Z"}
}`

const statusGET = `{
  "listen": "127.0.0.1:8787",
  "routing": "anthropic",
  "mode": "auto",
  "forced_provider": "glm",
  "forced_model": "glm-5.3",
  "jev_enabled": false,
  "breaker": {
    "entries": {
      "anthropic|*": {"state": "OPEN", "until": "2099-01-01T00:00:00Z", "reason": "plan quota"},
      "glm|*": {"state": "CLOSED"}
    }
  },
  "counts": {
    "anthropic_requests": 12, "glm_requests": 3, "deepseek_requests": 0,
    "failovers": 1, "transient_retries": 0, "jev_decisions": 2, "jev_fail_open": 1
  },
  "last_request": {"provider": "glm", "upstream_model": "glm-5.3-flash", "at": "2026-09-22T10:14:03Z"}
}`

func TestRouteShowReadsBothEndpoints(t *testing.T) {
	s := newStub(t, map[string]reply{
		"/_gateway/route":  {http.StatusOK, routeGET},
		"/_gateway/status": {http.StatusOK, statusGET},
	})
	useStub(t, s)

	rc, out, errOut := runCLI(t, "route")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	for _, want := range []string{
		"mode: jev  forced: glm/glm-5.3  jev: on",
		"routing: anthropic",
		"last request: glm glm-5.3-flash at 2026-09-22T10:14:03Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	for _, r := range reqs {
		if r.Method != http.MethodGet {
			t.Errorf("%s used %s, want GET", r.Path, r.Method)
		}
		if r.Origin != "" {
			t.Errorf("%s carried Origin %q; the gateway would reject a foreign one", r.Path, r.Origin)
		}
	}
}

func TestRoutePostForms(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		body    string
		reply   string
		wantOut string
	}{
		{
			"auto clears",
			[]string{"route", "auto"},
			`{"clear":true}`,
			`{"status":"ok","mode":"auto","provider":"","model":""}`,
			"mode: auto  forced: none",
		},
		{
			"jev sets mode",
			[]string{"route", "jev"},
			`{"mode":"jev"}`,
			`{"status":"ok","mode":"jev"}`,
			"mode: jev  forced: none",
		},
		{
			"pin with model",
			[]string{"route", "pin", "glm", "glm-5.3"},
			`{"provider":"glm","model":"glm-5.3"}`,
			`{"status":"ok","mode":"pinned","provider":"glm","model":"glm-5.3"}`,
			"mode: pinned  forced: glm/glm-5.3",
		},
		{
			"pin without a model",
			[]string{"route", "pin", "anthropic"},
			`{"provider":"anthropic"}`,
			`{"status":"ok","mode":"pinned","provider":"anthropic"}`,
			"mode: pinned  forced: anthropic",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, map[string]reply{"/_gateway/route": {http.StatusOK, tc.reply}})
			useStub(t, s)

			rc, out, errOut := runCLI(t, tc.args...)
			if rc != exitOK {
				t.Fatalf("rc = %d, stderr = %s", rc, errOut)
			}
			reqs := s.requests()
			if len(reqs) != 1 {
				t.Fatalf("want 1 request, got %d (%v)", len(reqs), reqs)
			}
			r := reqs[0]
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if r.Path != "/_gateway/route" {
				t.Errorf("path = %s", r.Path)
			}
			if r.CT != "application/json" {
				t.Errorf("content type = %q, want application/json", r.CT)
			}
			if r.Origin != "" {
				t.Errorf("origin = %q; a foreign origin would be refused", r.Origin)
			}
			jsonEqual(t, r.Body, tc.body)
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantOut)
			}
		})
	}
}

func TestRouteJevDisabledHintsAtEnvFile(t *testing.T) {
	s := newStub(t, map[string]reply{
		"/_gateway/route": {http.StatusBadRequest, `{"error":"jev disabled (no TYPESAFE_API_KEY)"}`},
	})
	useStub(t, s)

	rc, _, errOut := runCLI(t, "route", "jev")
	if rc != exitError {
		t.Fatalf("rc = %d, want %d", rc, exitError)
	}
	if !strings.Contains(errOut, "jev disabled (no TYPESAFE_API_KEY)") {
		t.Errorf("stderr missing the gateway message:\n%s", errOut)
	}
	if !strings.Contains(errOut, "TYPESAFE_API_KEY") || !strings.Contains(errOut, "~/.config/conduit/.env") {
		t.Errorf("stderr missing the env-file hint:\n%s", errOut)
	}
}

func TestRoutePinValidatesProviderLocally(t *testing.T) {
	s := newStub(t, map[string]reply{})
	useStub(t, s)

	rc, _, errOut := runCLI(t, "route", "pin", "openai", "gpt-5")
	if rc != exitUsage {
		t.Fatalf("rc = %d, want %d", rc, exitUsage)
	}
	if !strings.Contains(errOut, "anthropic|glm|deepseek") {
		t.Errorf("stderr should name the valid providers:\n%s", errOut)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("a rejected provider still cost %d request(s)", n)
	}
}

func TestUsageErrors(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/route": {http.StatusOK, `{"mode":"auto"}`}})
	useStub(t, s)
	t.Setenv("CONDUIT_DECISIONS_PATH", filepath.Join(t.TempDir(), "decisions.jsonl"))

	for _, args := range [][]string{
		{"frobnicate"},
		{"route", "sideways"},
		{"route", "pin"},
		{"route", "pin", "glm", "glm-5.3", "extra"},
		{"status", "extra"},
		{"decisions", "extra"},
		{"decisions", "-n", "0"},
		{"ui", "extra"},
		{"status", "-nope"},
	} {
		rc, _, errOut := runCLI(t, args...)
		if rc != exitUsage {
			t.Errorf("%v: rc = %d, want %d (stderr %q)", args, rc, exitUsage, errOut)
		}
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("usage errors sent %d request(s)", n)
	}
	// An unknown flag is reported exactly once, by the FlagSet itself.
	_, _, errOut := runCLI(t, "status", "-nope")
	if got := strings.Count(errOut, "flag provided but not defined"); got != 1 {
		t.Errorf("bad flag reported %d times on stderr: %q", got, errOut)
	}
}

func TestConfigFlagIsIgnored(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/status": {http.StatusOK, statusGET}})
	useStub(t, s)

	rc, out, errOut := runCLI(t, "status", "-config", "/nonexistent/config.toml")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	if !strings.Contains(out, "mode: auto") {
		t.Errorf("output = %q", out)
	}
}

func TestHelpEverywhere(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help"},
		{"route", "--help"}, {"status", "--help"}, {"decisions", "--help"}, {"ui", "--help"},
	} {
		rc, out, errOut := runCLI(t, args...)
		if rc != exitOK {
			t.Errorf("%v: rc = %d, stderr = %q", args, rc, errOut)
		}
		if !strings.Contains(out, "usage:") {
			t.Errorf("%v: usage not printed:\n%s", args, out)
		}
	}
	// No args at all is a usage error on stderr.
	rc, _, errOut := runCLI(t)
	if rc != exitUsage || !strings.Contains(errOut, "usage:") {
		t.Errorf("bare invocation: rc = %d, stderr = %q", rc, errOut)
	}
}

func TestStatusHumanSummary(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/status": {http.StatusOK, statusGET}})
	useStub(t, s)

	rc, out, errOut := runCLI(t, "status")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	for _, want := range []string{
		"mode: auto  routing: anthropic  forced: glm/glm-5.3",
		"breaker: anthropic|* OPEN until ",
		"glm|* CLOSED",
		"counts: anthropic=12 glm=3 deepseek=0 failovers=1 transient=0 jev=2 fail_open=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusCountdownFormatsExpiry(t *testing.T) {
	soon := time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339)
	past := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
	if got := countdown(mustParse(t, soon)); !strings.HasSuffix(got, "s") || strings.HasPrefix(got, "-") {
		t.Errorf("countdown(%s) = %q", soon, got)
	}
	if got := countdown(mustParse(t, past)); got != "expired" {
		t.Errorf("countdown(%s) = %q, want expired", past, got)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestStatusJSONIsRawPayload(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/status": {http.StatusOK, statusGET}})
	useStub(t, s)

	rc, out, errOut := runCLI(t, "status", "-json")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("-json output is not JSON: %v\n%s", err, out)
	}
	if _, ok := raw["breaker"]; !ok {
		t.Errorf("-json dropped fields not modelled by the CLI:\n%s", out)
	}
	if !strings.Contains(out, "\n  \"listen\"") {
		t.Errorf("-json should be indented:\n%s", out)
	}
}

// deadURL returns the URL of a server that has already been shut down, i.e.
// what a stopped gateway looks like on the loopback port.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestGatewayNotReachable(t *testing.T) {
	url := deadURL(t)
	t.Setenv("CONDUIT_GATEWAY_URL", url)
	t.Setenv("CONDUIT_DECISIONS_PATH", filepath.Join(t.TempDir(), "decisions.jsonl"))

	want := fmt.Sprintf("gateway not reachable at %s — is it running? (./status.sh)", url)
	for _, args := range [][]string{
		{"route"}, {"route", "auto"}, {"route", "jev"}, {"route", "pin", "glm"}, {"status"}, {"status", "-json"}, {"ui"},
	} {
		rc, out, errOut := runCLI(t, args...)
		if rc != exitError {
			t.Errorf("%v: rc = %d, want %d", args, rc, exitError)
		}
		if !strings.Contains(errOut, want) {
			t.Errorf("%v: stderr = %q, want it to contain %q", args, errOut, want)
		}
		if strings.Contains(errOut, "dial tcp") {
			t.Errorf("%v: leaked the raw dial error: %q", args, errOut)
		}
		if out != "" {
			t.Errorf("%v: wrote %q to stdout", args, out)
		}
	}
	// decisions reads a local file, so it keeps working without a gateway.
	rc, out, _ := runCLI(t, "decisions")
	if rc != exitOK || !strings.Contains(out, "has not produced a decision yet") {
		t.Errorf("decisions with no gateway and no log: rc = %d, out = %q", rc, out)
	}
}

func TestDecisionsTailsLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions.jsonl")
	lines := []string{
		`{"at":"2026-09-22T10:00:00Z","requested_model":"claude-opus-5","provider":"anthropic","model":"claude-opus-5","step":"user_turn","lease":"one_call","source":"jev","confidence":0.5,"latency_ms":300}`,
		`{"at":"2026-09-22T10:00:01Z","requested_model":"claude-sonnet-5","provider":"glm","model":"glm-5.3-flash","step":"tool_step","lease":"tool_chain","source":"jev","confidence":0.81,"latency_ms":412}`,
		`{"at":"2026-09-22T10:00:02Z","requested_model":"claude-haiku-4-5","provider":"glm","model":"glm-5.3","step":"other","lease":"one_call","source":"fail_open","reason":"timeout","latency_ms":4001}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUIT_DECISIONS_PATH", path)

	rc, out, errOut := runCLI(t, "decisions", "-n", "2")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	got := nonEmptyLines(out)
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(got), out)
	}
	for _, want := range []string{
		"claude-sonnet-5→glm/glm-5.3-flash", "step=tool_step", "lease=tool_chain", "src=jev", "conf=0.81", "412ms",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("first line %q missing %q", got[0], want)
		}
	}
	for _, want := range []string{"claude-haiku-4-5→glm/glm-5.3", "src=fail_open", "reason=timeout", "4001ms"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("second line %q missing %q", got[1], want)
		}
	}
	if strings.Contains(out, "claude-opus-5") {
		t.Errorf("-n 2 should have dropped the oldest line:\n%s", out)
	}
}

func TestDecisionsDefaultAndTornLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions.jsonl")
	var b strings.Builder
	line := `{"at":"2026-09-22T10:00:00Z","requested_model":"m","provider":"glm","model":"glm-5.3","step":"other","lease":"one_call","source":"jev","latency_ms":1}`
	for i := 0; i < 25; i++ {
		b.WriteString(line + "\n")
	}
	b.WriteString(`{"at":"2026-09-22T10:00:0`) // torn last line, as after a kill
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUIT_DECISIONS_PATH", path)

	rc, out, errOut := runCLI(t, "decisions")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	// 25 valid lines plus the torn one is 26 physical lines; the default tail
	// takes the last 20, so 19 survive the skip.
	if got := len(nonEmptyLines(out)); got != 19 {
		t.Errorf("default tail = %d lines, want 19", got)
	}
	if !strings.Contains(errOut, "skipped 1 unparseable line") {
		t.Errorf("stderr should mention the torn line: %q", errOut)
	}
}

func TestDecisionsMissingAndEmptyFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.jsonl")
	t.Setenv("CONDUIT_DECISIONS_PATH", missing)

	rc, out, errOut := runCLI(t, "decisions")
	if rc != exitOK {
		t.Fatalf("a missing log is not an error: rc = %d, stderr = %s", rc, errOut)
	}
	if !strings.Contains(out, "jev has not produced a decision yet") || !strings.Contains(out, missing) {
		t.Errorf("output = %q", out)
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUIT_DECISIONS_PATH", empty)
	rc, out, _ = runCLI(t, "decisions")
	if rc != exitOK || !strings.Contains(out, "no decisions in") {
		t.Errorf("empty log: rc = %d, out = %q", rc, out)
	}
}

func TestDecisionsPathDefault(t *testing.T) {
	t.Setenv("CONDUIT_DECISIONS_PATH", "")
	t.Setenv("HOME", "/home/u")
	// XDG_STATE_HOME must NOT redirect this. The gateway expands its configured
	// default with ExpandHome and never writes under XDG, so an XDG-aware CLI
	// would read a path that does not exist and still exit 0.
	t.Setenv("XDG_STATE_HOME", "/xdg")
	if got, want := decisionsPath(), "/home/u/.local/state/conduit/decisions.jsonl"; got != want {
		t.Errorf("decisionsPath() = %q, want %q", got, want)
	}
	t.Setenv("CONDUIT_DECISIONS_PATH", "/tmp/explicit.jsonl")
	if got, want := decisionsPath(), "/tmp/explicit.jsonl"; got != want {
		t.Errorf("decisionsPath() override = %q, want %q", got, want)
	}
}

func TestOpenCommandPerPlatform(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want string
	}{
		{"darwin", "open"},
		{"linux", "xdg-open"},
	} {
		name, argv, err := openCommand(tc.goos, "http://127.0.0.1:8787/_gateway/ui")
		if err != nil {
			t.Fatalf("%s: %v", tc.goos, err)
		}
		if name != tc.want || len(argv) != 1 || argv[0] != "http://127.0.0.1:8787/_gateway/ui" {
			t.Errorf("%s: got %s %v", tc.goos, name, argv)
		}
	}
	if _, _, err := openCommand("plan9", "http://x/"); err == nil {
		t.Error("an unsupported platform should error, not guess an opener")
	}
}

func TestUIOpensControlPage(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/health": {http.StatusOK, `{"ok":true}`}})
	useStub(t, s)
	opened := ""
	openURL = func(url string) error {
		opened = url
		return nil
	}
	t.Cleanup(func() { openURL = defaultOpenURL })

	rc, out, errOut := runCLI(t, "ui")
	if rc != exitOK {
		t.Fatalf("rc = %d, stderr = %s", rc, errOut)
	}
	if want := s.URL + "/_gateway/ui"; opened != want {
		t.Errorf("opened %q, want %q", opened, want)
	}
	if !strings.Contains(out, "/_gateway/ui") {
		t.Errorf("output = %q", out)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
