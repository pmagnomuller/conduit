package main

import (
	"strings"
	"testing"
)

// spendGET is a synthetic /_gateway/spend payload.
const spendGET = `{
  "now": "2030-01-02T10:00:00Z",
  "day": "2030-01-02",
  "today": {"requests": 12, "tokens": 345678, "usd": 0.62, "paid_usd": 0.62, "failovers": 3, "subscription_requests": 9},
  "today_paid_usd": 0.62,
  "providers": {
    "anthropic": {"requests": 9, "tokens": 300000, "usd": 0, "paid_usd": 0, "subscription_requests": 9},
    "deepseek": {"requests": 3, "tokens": 45678, "usd": 0.62, "paid_usd": 0.62, "failovers": 3}
  },
  "hours": [],
  "days": [
    {"day": "2030-01-01", "totals": {"requests": 40, "tokens": 1200000, "paid_usd": 1.5, "failovers": 2}},
    {"day": "2030-01-02", "totals": {"requests": 12, "tokens": 345678, "paid_usd": 0.62, "failovers": 3}}
  ],
  "recent": [],
  "deepseek_requests": 3,
  "deepseek_failovers": 3,
  "deepseek_balance": {"usd": 42.5, "at": "2030-01-02T09:59:00Z"},
  "claude": {"plan": "pro", "windows": [
    {"id": "five_hour", "label": "session", "percent_remaining": 91},
    {"id": "seven_day", "label": "week", "percent_remaining": 64}
  ], "extra_usage": {"spent_usd": 0, "limit_usd": 50}},
  "serving": {"provider": "anthropic", "upstream_model": "claude-opus-5-5", "at": "2030-01-02T09:58:00Z"}
}`

func TestSpendSummary(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/spend": {200, spendGET}})
	useStub(t, s)
	rc, out, errOut := runCLI(t, "spend", "-days", "30")
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s", rc, errOut)
	}
	for _, want := range []string{
		"today (2030-01-02): paid $0.62  requests 12",
		"deepseek 3 req (3 failover) $0.62",
		"anthropic 9 req $0.00 subscription",
		"deepseek credit: $42.50",
		"session 91% left",
		"week 64% left",
		"extra usage $0.00/$50.00",
		"serving now: anthropic claude-opus-5-5",
		"2030-01-01",
		"1.50",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if reqs := s.requests(); len(reqs) != 1 || reqs[0].Path != "/_gateway/spend" || reqs[0].Query != "days=30" {
		t.Fatalf("requests: %+v", reqs)
	}
}

func TestSpendLine(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/spend": {200, spendGET}})
	useStub(t, s)
	rc, out, errOut := runCLI(t, "spend", "line")
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s", rc, errOut)
	}
	want := "AI spend: today $0.62 paid (12 req, 3 DeepSeek) | 7d $2.12 | DeepSeek credit $42.50 | Claude plan 64% week left | 91% session left | extra usage $0.00/$50.00 | serving anthropic\n"
	if out != want {
		t.Fatalf("line:\n got %q\nwant %q", out, want)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatal("spend line must be exactly one line")
	}
	if q := s.requests()[0].Query; q != "days=7" {
		t.Fatalf("query %q", q)
	}
}

// With no external sources (no DeepSeek key, no quota-axi) the summary still
// prints, without those sections.
func TestSpendWithoutSources(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/spend": {200, `{"day":"2030-01-02","today":{},"today_paid_usd":0,"providers":{},"days":[],"recent":[]}`}})
	useStub(t, s)
	rc, out, _ := runCLI(t, "spend", "line")
	if rc != exitOK || out != "AI spend: today $0.00 paid (0 req) | 7d $0.00\n" {
		t.Fatalf("rc=%d out=%q", rc, out)
	}
	rc, out, _ = runCLI(t, "spend")
	if rc != exitOK || strings.Contains(out, "credit") || strings.Contains(out, "claude plan") {
		t.Fatalf("rc=%d out=%q", rc, out)
	}
}

func TestSpendJSONAndUsage(t *testing.T) {
	s := newStub(t, map[string]reply{"/_gateway/spend": {200, spendGET}})
	useStub(t, s)
	rc, out, _ := runCLI(t, "spend", "-json")
	if rc != exitOK {
		t.Fatalf("rc=%d", rc)
	}
	jsonEqual(t, out, spendGET)

	for _, args := range [][]string{{"spend", "extra"}, {"spend", "-days", "0"}, {"spend", "line", "x"}, {"spend", "-bogus"}} {
		if rc, _, _ := runCLI(t, args...); rc != exitUsage {
			t.Errorf("%v: rc=%d, want usage error", args, rc)
		}
	}
}

func TestSpendGatewayDown(t *testing.T) {
	t.Setenv("CONDUIT_GATEWAY_URL", "http://127.0.0.1:1")
	rc, _, errOut := runCLI(t, "spend", "line")
	if rc != exitError || !strings.Contains(errOut, "gateway not reachable") {
		t.Fatalf("rc=%d stderr=%q", rc, errOut)
	}
}
