package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pedro-mueller/conduit/internal/config"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Jev.Catalog = config.DefaultCatalog()
	cfg.Spend.UsagePath = filepath.Join(t.TempDir(), "usage.jsonl")
	cfg.Spend.BalanceURL = ""
	cfg.Spend.ClaudeQuota = false
	return cfg
}

func TestUSDCostMath(t *testing.T) {
	cfg := config.Default()
	cfg.Jev.Catalog = config.DefaultCatalog()
	cfg.Spend.Prices = []config.Candidate{
		// A synthetic model with round prices to check every term exactly.
		{Provider: "deepseek", Model: "test-model", PriceIn: 1, PriceOut: 2, PriceCacheRead: 0.1, PriceCacheWrite: 1.5},
	}
	p := NewPrices(cfg)

	// 1M of each kind: 1 + 2 + 0.1 + 1.5.
	usd, exact := p.USD("deepseek", "test-model", Usage{1e6, 1e6, 1e6, 1e6})
	if !exact || !near(usd, 4.6) {
		t.Fatalf("all terms: %v exact=%v", usd, exact)
	}
	// Input excludes cache reads (Messages API semantics): a mostly-cached
	// prompt is cheap.
	usd, _ = p.USD("deepseek", "test-model", Usage{InputTokens: 1000, CacheReadTokens: 999000, OutputTokens: 0})
	if !near(usd, 0.1009) {
		t.Fatalf("cached prompt: %v", usd)
	}
	// Built-in rate: deepseek-v4-flash 0.30 in / 1.20 out.
	usd, exact = p.USD("deepseek", "deepseek-v4-flash", Usage{InputTokens: 100000, OutputTokens: 10000})
	if !exact || !near(usd, 0.042) {
		t.Fatalf("v4-flash: %v exact=%v", usd, exact)
	}
	// A dated id prices as its undated entry.
	if _, exact := p.Price("anthropic", "claude-sonnet-5-20260101"); !exact {
		t.Fatal("dated id not matched")
	}
	// Unknown model of a known provider: the provider's most expensive rate,
	// flagged as estimated.
	c, exact := p.Price("deepseek", "deepseek-v9-ultra")
	if exact || c.Model != "deepseek-v4-pro" {
		t.Fatalf("unknown model priced as %q exact=%v", c.Model, exact)
	}
	// Unknown provider costs nothing rather than inventing a rate.
	if usd, _ := p.USD("nobody", "x", Usage{InputTokens: 1e6}); usd != 0 {
		t.Fatalf("unknown provider: %v", usd)
	}
	// Rounding keeps sub-cent costs.
	usd, _ = p.USD("deepseek", "deepseek-v4-flash", Usage{InputTokens: 100})
	if !near(usd, 0) && !near(usd, 0.0001) {
		t.Fatalf("tiny call: %v", usd)
	}
}

func TestSpendPricesOverrideOneRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[[spend.prices]]\nprovider = \"deepseek\"\nmodel = \"deepseek-v4-flash\"\nprice_out = 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := NewPrices(cfg).Price("deepseek", "deepseek-v4-flash")
	if c.PriceOut != 9 || c.PriceIn != 0.30 || c.PriceCacheRead != 0.006 {
		t.Fatalf("override: %+v", c)
	}
}

func TestBillingFor(t *testing.T) {
	cfg := config.Default()
	if BillingFor(cfg, "anthropic") != BillingSubscription || BillingFor(cfg, "glm") != BillingSubscription {
		t.Fatal("plan traffic must default to subscription")
	}
	if BillingFor(cfg, "deepseek") != BillingMetered || BillingFor(cfg, "mystery") != BillingMetered {
		t.Fatal("deepseek and unknown providers are metered")
	}
	cfg.AnthropicAPIKey = "not-a-real-key"
	cfg.Spend.GLMBilling = BillingMetered
	if BillingFor(cfg, "anthropic") != BillingMetered || BillingFor(cfg, "glm") != BillingMetered {
		t.Fatal("API keys make traffic metered")
	}
}

func TestRecordSubscriptionIsFreeAndMeteredIsPriced(t *testing.T) {
	cfg := testConfig(t)
	s := NewService(cfg, Options{Notify: func(string, string) {}})
	defer s.Close()

	rec, ok := s.Record("anthropic", "claude-opus-5-5", "claude-opus-5-5", false, ParseSSE([]byte(anthropicSSE)))
	if !ok || rec.USD != 0 || rec.Billing != BillingSubscription || rec.InputTokens != 8 || rec.OutputTokens != 321 {
		t.Fatalf("subscription record: %+v ok=%v", rec, ok)
	}
	rec, ok = s.Record("deepseek", "claude-sonnet-5", "deepseek-v4-flash", true, ParseJSON([]byte(deepseekJSON)))
	// 1500 in * 0.30 + 42 out * 1.20 + 1024 read * 0.006, per million.
	want := (1500*0.30 + 42*1.20 + 1024*0.006) / 1e6
	if !ok || rec.Billing != BillingMetered || !near(rec.USD, math.Round(want*1e4)/1e4) || !rec.Failover {
		t.Fatalf("metered record: %+v (want usd %.6f)", rec, want)
	}
	if rec.RequestedModel != "claude-sonnet-5" || rec.UpstreamModel != "deepseek-v4-flash" || rec.ModelSource != "response" {
		t.Fatalf("models: %+v", rec)
	}
	// No usage → nothing written.
	if _, ok := s.Record("deepseek", "m", "m", false, Result{}); ok {
		t.Fatal("empty usage recorded")
	}
	sum, err := s.Summary()
	if err != nil || len(sum.Recent) != 2 {
		t.Fatalf("ledger: %+v %v", sum.Recent, err)
	}
}

func TestDayRollup(t *testing.T) {
	// UTC+2, so 22:30Z is 00:30 the next local day.
	loc := time.FixedZone("test+2", 2*3600)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	w := NewWriter(path)
	defer w.Close()
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	recs := []Record{
		{At: at("2030-01-01T09:00:00Z"), Provider: "anthropic", InputTokens: 10, OutputTokens: 5, Billing: BillingSubscription},
		{At: at("2030-01-01T09:30:00Z"), Provider: "deepseek", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 7, USD: 0.25, Billing: BillingMetered, Failover: true},
		{At: at("2030-01-01T21:59:00Z"), Provider: "deepseek", InputTokens: 1, USD: 0.10, Billing: BillingMetered},
		{At: at("2030-01-01T22:30:00Z"), Provider: "glm", InputTokens: 3, OutputTokens: 4, Billing: BillingSubscription},
	}
	for _, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	// A torn line and garbage must not hide the records around them.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("{\"at\":\"2030-01-0\nnot json\n\n")
	_ = f.Close()
	if err := w.Append(Record{At: at("2030-01-02T08:00:00Z"), Provider: "deepseek", OutputTokens: 1, USD: 1, Billing: BillingMetered}); err != nil {
		t.Fatal(err)
	}

	sum, err := Read(path, loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Days) != 2 || sum.Days[0].Day != "2030-01-01" || sum.Days[1].Day != "2030-01-02" {
		t.Fatalf("days: %+v", sum.Days)
	}
	d1 := sum.Days[0]
	if d1.Totals.Requests != 3 || !near(d1.Totals.PaidUSD, 0.35) || d1.Totals.Failovers != 1 || d1.Totals.Subscription != 1 {
		t.Fatalf("day 1 totals: %+v", d1.Totals)
	}
	if ds := d1.Providers["deepseek"]; ds.Requests != 2 || ds.Tokens != 158 || !near(ds.USD, 0.35) {
		t.Fatalf("day 1 deepseek: %+v", ds)
	}
	// 09:00Z/09:30Z are 11:xx local; 21:59Z is 23:59 local.
	if d1.Hours[11]["anthropic"] != 1 || d1.Hours[11]["deepseek"] != 1 || d1.Hours[23]["deepseek"] != 1 {
		t.Fatalf("day 1 hours: %v", d1.Hours)
	}
	d2 := sum.Days[1]
	if d2.Totals.Requests != 2 || d2.Providers["glm"].Requests != 1 || !near(d2.Totals.PaidUSD, 1) || d2.Hours[0]["glm"] != 1 {
		t.Fatalf("day 2: %+v", d2)
	}
	if len(sum.Recent) != 5 || sum.Recent[0].USD != 1 || sum.Recent[4].Provider != "anthropic" {
		t.Fatalf("recent must be newest first: %+v", sum.Recent)
	}
	if sum.Since == nil || !sum.Since.Equal(recs[0].At) {
		t.Fatalf("since: %v", sum.Since)
	}
	if empty := sum.DayFor("2031-01-01"); empty.Totals.Requests != 0 || len(empty.Hours) != 24 {
		t.Fatalf("empty day: %+v", empty)
	}
	if missing, err := Read(filepath.Join(t.TempDir(), "none.jsonl"), loc); err != nil || len(missing.Days) != 0 {
		t.Fatalf("missing ledger: %+v %v", missing, err)
	}
}

func TestDecideAlert(t *testing.T) {
	var st AlertState
	steps := []struct {
		paid float64
		due  bool
	}{
		{0, false},    // nothing paid
		{0.01, true},  // first paid cent
		{0.01, false}, // unchanged: dedup
		{0.40, false}, // grew by 0.39 < 0.50
		{0.51, true},  // grew by 0.50 since the last alert
		{0.80, false},
		{1.10, true},
	}
	for i, s := range steps {
		var due bool
		st, due = Decide(st, "2030-01-01", s.paid, 0.50)
		if due != s.due {
			t.Fatalf("step %d paid %.2f: due=%v want %v (state %+v)", i, s.paid, due, s.due, st)
		}
	}
	// A new day starts over.
	if st, due := Decide(st, "2030-01-02", 0.01, 0.50); !due || st.Cents != 1 {
		t.Fatalf("new day: %v %+v", due, st)
	}
}

func TestAlertFiresOncePerThresholdAcrossRestart(t *testing.T) {
	cfg := testConfig(t)
	cfg.Spend.AlertStepUSD = 0.50
	var mu sync.Mutex
	var notes []string
	notify := func(_, m string) { mu.Lock(); notes = append(notes, m); mu.Unlock() }
	// waitFor waits for want notifications (they are posted asynchronously),
	// then a little longer to catch an unexpected extra one.
	waitFor := func(want int) int {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(notes)
			mu.Unlock()
			if n >= want {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return len(notes)
	}
	// 1M output on deepseek-v4-flash = $1.20 per call.
	big := Result{Usage: Usage{OutputTokens: 1_000_000}, Model: "deepseek-v4-flash"}
	small := Result{Usage: Usage{OutputTokens: 100_000}, Model: "deepseek-v4-flash"} // $0.12

	s := NewService(cfg, Options{Notify: notify})
	s.Record("deepseek", "m", "deepseek-v4-flash", false, small) // 0.12 → alert
	if n := waitFor(1); n != 1 {
		t.Fatalf("first paid spend: %d notifications", n)
	}
	s.Record("deepseek", "m", "deepseek-v4-flash", false, small) // 0.24 → no
	s.Close()

	// Restart: the in-memory total is reseeded from the ledger, and the
	// persisted state keeps 0.12 from being announced again.
	s = NewService(cfg, Options{Notify: notify})
	defer s.Close()
	s.Record("deepseek", "m", "deepseek-v4-flash", false, small) // 0.36 → no
	s.Record("deepseek", "m", "deepseek-v4-flash", false, big)   // 1.56 → alert
	if n := waitFor(2); n != 2 {
		t.Fatalf("after restart: %d notifications (%v)", n, notes)
	}
	mu.Lock()
	last := notes[len(notes)-1]
	mu.Unlock()
	if !strings.Contains(last, "$1.56") {
		t.Fatalf("message: %q", last)
	}

	// Subscription traffic never alerts, and alert=false silences paid.
	cfg2 := testConfig(t)
	cfg2.Spend.Alert = false
	quiet := NewService(cfg2, Options{Notify: func(_, m string) { t.Errorf("unexpected alert %q", m) }})
	defer quiet.Close()
	quiet.Record("deepseek", "m", "deepseek-v4-flash", false, big)
	quiet2 := NewService(testConfig(t), Options{Notify: func(_, m string) { t.Errorf("unexpected alert %q", m) }})
	defer quiet2.Close()
	quiet2.Record("anthropic", "m", "claude-opus-5-5", false, big)
	time.Sleep(50 * time.Millisecond)
}

// The ledger must never carry the prompt, the response text or a credential.
// Record has no field for them; this pins that the full path — parse a
// response that echoes them, record, write — keeps them out.
func TestLedgerHoldsNoPromptOrCredential(t *testing.T) {
	const secret = "sk-ant-api03-SYNTHETIC-DO-NOT-LEAK-0123456789"
	const prompt = "PROMPT-CANARY-the-launch-codes"
	cfg := testConfig(t)
	s := NewService(cfg, Options{Notify: func(string, string) {}})
	defer s.Close()
	body := fmt.Sprintf(`{"id":"msg_x","model":"deepseek-v4-flash","content":[{"type":"text","text":%q}],"usage":{"input_tokens":10,"output_tokens":2}}`, prompt+" "+secret)
	s.Record("deepseek", "claude-sonnet-5", "deepseek-v4-flash", false, ParseJSON([]byte(body)))
	// A provider echoing a credential into the model name is redacted too.
	s.Record("deepseek", "claude-sonnet-5", "deepseek-v4-flash", false, Result{Usage: Usage{OutputTokens: 1}, Model: "Bearer " + secret})

	raw, err := os.ReadFile(cfg.Spend.UsagePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, prompt, "launch-codes"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("ledger leaks %q:\n%s", needle, raw)
		}
	}
	info, _ := os.Stat(cfg.Spend.UsagePath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode %v", info.Mode().Perm())
	}
}

func TestLedgerRotatesAndFollowsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	w := NewWriter(path)
	defer w.Close()
	if err := w.Append(Record{At: time.Now(), Provider: "a"}); err != nil {
		t.Fatal(err)
	}
	// Moved away by hand: the next append recreates the path.
	if err := os.Rename(path, filepath.Join(dir, "moved.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Record{At: time.Now(), Provider: "b"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"provider":"b"`) || strings.Contains(string(raw), `"provider":"a"`) {
		t.Fatalf("after move: %s", raw)
	}
	// Past the rotation size the file moves to .1.
	if err := os.Truncate(path, maxLedgerBytes); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Record{At: time.Now(), Provider: "c"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() < maxLedgerBytes {
		t.Fatalf("rotated file: %v %v", st, err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Count(string(raw), "\n") != 1 || !strings.Contains(string(raw), `"provider":"c"`) {
		t.Fatalf("fresh ledger: %q", raw)
	}
}

func TestBalanceClient(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"99.00"},{"currency":"USD","total_balance":"12.34","granted_balance":"0.00","topped_up_balance":"12.34"}]}`))
	}))
	defer srv.Close()

	b := &BalanceClient{URL: srv.URL, APIKey: "test-key", TTL: time.Minute}
	for i := 0; i < 3; i++ {
		got, err := b.Get(context.Background())
		if err != nil || got.USD != 12.34 {
			t.Fatalf("balance: %+v %v", got, err)
		}
	}
	if hits != 1 {
		t.Fatalf("cache: %d fetches", hits)
	}
	if _, err := (&BalanceClient{URL: srv.URL}).Get(context.Background()); err != ErrUnavailable {
		t.Fatalf("no key: %v", err)
	}
	bad := &BalanceClient{URL: srv.URL, APIKey: "wrong"}
	if _, err := bad.Get(context.Background()); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("auth failure must error without echoing the key: %v", err)
	}
}

func TestClaudeQuotaParse(t *testing.T) {
	fixture := `{"schemaVersion":5,"providers":[{"provider":"claude","plan":"pro",
"windows":[
 {"id":"five_hour","label":"session","kind":"session","percentRemaining":80,"resetsAt":"2030-01-01T12:00:00.5+00:00"},
 {"id":"seven_day","label":"week","kind":"weekly","percentRemaining":40.5,"resetsAt":"2030-01-05T00:00:00Z"},
 {"id":"extra_usage","label":"extra usage","kind":"credits","percentRemaining":100,"spentUsd":1.5,"limitUsd":20}],
"state":{"status":"fresh"}}]}`
	calls := 0
	q := &QuotaClient{Enabled: true, TTL: time.Minute, Run: func(context.Context) ([]byte, error) { calls++; return []byte(fixture), nil }}
	got, err := q.Claude(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = q.Claude(context.Background())
	if calls != 1 {
		t.Fatalf("cache: %d runs", calls)
	}
	if got.Plan != "pro" || len(got.Windows) != 2 || got.Extra == nil || got.Extra.SpentUSD != 1.5 || got.Extra.LimitUSD != 20 {
		t.Fatalf("parsed: %+v", got)
	}
	if w := got.Windows[1]; w.ID != "seven_day" || *w.PercentRemaining != 40.5 || w.ResetsAt == nil {
		t.Fatalf("window: %+v", w)
	}
	if _, err := (&QuotaClient{}).Claude(context.Background()); err != ErrUnavailable {
		t.Fatalf("disabled: %v", err)
	}
	notSetUp := &QuotaClient{Enabled: true, Run: func(context.Context) ([]byte, error) {
		return []byte(`{"providers":[{"provider":"claude","notSetUp":true}]}`), nil
	}}
	if _, err := notSetUp.Claude(context.Background()); err != ErrUnavailable {
		t.Fatalf("not set up: %v", err)
	}
}

func TestBuildSpend(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	s := NewService(cfg, Options{Now: func() time.Time { return now }, Location: time.UTC, Notify: func(string, string) {}})
	defer s.Close()
	s.SetQuotaClient(&QuotaClient{Enabled: true, Run: func(context.Context) ([]byte, error) {
		return []byte(`{"providers":[{"provider":"claude","windows":[{"id":"seven_day","percentRemaining":50}]}]}`), nil
	}})
	s.Record("deepseek", "claude-sonnet-5", "deepseek-v4-flash", true, ParseJSON([]byte(deepseekJSON)))
	s.Record("anthropic", "claude-opus-5-5", "claude-opus-5-5", false, ParseSSE([]byte(anthropicSSE)))

	sp := s.BuildSpend(context.Background(), &Serving{Provider: "anthropic"}, 14, 1)
	if sp.Day != now.UTC().Format("2006-01-02") || sp.Today.Requests != 2 || sp.TodayPaid <= 0 {
		t.Fatalf("today: %+v", sp)
	}
	if sp.DeepSeekRequests != 1 || sp.DeepSeekFailovers != 1 || len(sp.Recent) != 1 || sp.Recent[0].Provider != "anthropic" {
		t.Fatalf("deepseek/recent: %+v", sp)
	}
	if sp.Claude == nil || len(sp.Claude.Windows) != 1 || sp.Balance != nil || sp.BalanceError != "" {
		t.Fatalf("sources: claude=%+v balance=%+v err=%q", sp.Claude, sp.Balance, sp.BalanceError)
	}
	raw, _ := json.Marshal(sp)
	for _, key := range []string{`"today_paid_usd"`, `"hours"`, `"days"`, `"serving"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("payload lacks %s", key)
		}
	}
}
