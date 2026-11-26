package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/pedro-mueller/conduit/internal/config"
)

// Options tunes a Service; the zero value is the production setup.
type Options struct {
	// Now overrides the clock (tests). Nil means time.Now.
	Now func() time.Time
	// Location is the timezone days are bucketed in. Nil means time.Local:
	// a "day" is the day the operator lived through, not a UTC one.
	Location *time.Location
	// Notify posts a desktop notification; nil uses DesktopNotify.
	Notify func(title, message string)
	// HTTP is the client for the DeepSeek balance call; nil uses a default.
	HTTP   *http.Client
	Logger *slog.Logger
}

// Service owns the usage ledger, the cached external reads and the paid-spend
// alert. One per gateway; every method is safe for concurrent use.
type Service struct {
	cfg    config.Config
	opts   Options
	prices *Prices
	w      *Writer
	log    *slog.Logger

	srcMu sync.Mutex
	bal   *BalanceClient
	quota *QuotaClient

	// alertMu guards the in-memory paid total and the alert state file.
	alertMu sync.Mutex
	paidDay string
	paidUSD float64
}

// NewService builds the service from config. It never fails: a ledger that
// cannot be written surfaces as a logged warning on append.
func NewService(cfg config.Config, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	return &Service{
		cfg:    cfg,
		opts:   opts,
		prices: NewPrices(cfg),
		w:      NewWriter(cfg.Spend.UsagePath),
		log:    opts.Logger,
		bal: &BalanceClient{
			URL:    cfg.Spend.BalanceURL,
			APIKey: cfg.DeepSeekAPIKey,
			TTL:    time.Duration(cfg.Spend.BalanceCacheSeconds) * time.Second,
			HTTP:   opts.HTTP,
		},
		quota: &QuotaClient{Enabled: cfg.Spend.ClaudeQuota, TTL: 2 * time.Minute},
	}
}

// SetBalanceClient replaces the DeepSeek credit-balance client (tests).
func (s *Service) SetBalanceClient(b *BalanceClient) {
	s.srcMu.Lock()
	s.bal = b
	s.srcMu.Unlock()
}

// SetQuotaClient replaces the Claude plan-quota client (tests).
func (s *Service) SetQuotaClient(q *QuotaClient) {
	s.srcMu.Lock()
	s.quota = q
	s.srcMu.Unlock()
}

func (s *Service) sources() (*BalanceClient, *QuotaClient) {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()
	return s.bal, s.quota
}

// Close releases the ledger file handle.
func (s *Service) Close() error { return s.w.Close() }

// Record prices one upstream response and appends it to the ledger.
//
// provider is the tier that answered, requestedModel the model the client
// asked for, sentModel the model conduit sent upstream, failover whether the
// call reached this tier by failing over. Nothing else from the request
// travels here: there is no field a prompt or a credential could ride in.
//
// A response without usage (an error body, a non-Messages call) is not
// recorded. Returns the record as written, and false when nothing was.
func (s *Service) Record(provider, requestedModel, sentModel string, failover bool, res Result) (Record, bool) {
	u := res.Usage
	if u.Empty() {
		return Record{}, false
	}
	// The provider's own name for the model is what served the call; it is
	// what the ledger shows, and what is priced when the table knows it.
	upstream, source := sentModel, "sent"
	if res.Model != "" {
		upstream, source = res.Model, "response"
	}
	billing := BillingFor(s.cfg, provider)
	rec := Record{
		At:              s.opts.Now().UTC(),
		Provider:        provider,
		RequestedModel:  requestedModel,
		UpstreamModel:   upstream,
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
		CacheWriteToken: u.CacheCreationTokens,
		Billing:         billing,
		Failover:        failover,
		RequestID:       res.RequestID,
		ModelSource:     source,
	}
	if billing == BillingMetered {
		priced := upstream
		if _, exact := s.prices.Price(provider, upstream); !exact && sentModel != "" {
			priced = sentModel
		}
		usd, exact := s.prices.USD(provider, priced, u)
		rec.USD, rec.Estimated = usd, !exact
	}
	if err := s.w.Append(rec); err != nil {
		s.log.Warn("usage ledger append failed", "err", err)
		return rec, false
	}
	if rec.USD > 0 {
		day, paid := s.addPaid(rec.At, rec.USD)
		s.alertIfDue(day, paid)
	}
	return rec, true
}

// addPaid adds usd to the in-memory paid total of at's local day and returns
// the day and its new total. The first paid record of a day (and the first
// after a restart) seeds the total from the ledger — which already holds that
// record — so a restart mid-day resumes from what was spent, and the request
// path re-reads the ledger at most once a day.
func (s *Service) addPaid(at time.Time, usd float64) (string, float64) {
	day := at.In(s.opts.Location).Format("2006-01-02")
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	if s.paidDay == day {
		s.paidUSD += usd
		return day, s.paidUSD
	}
	s.paidDay, s.paidUSD = day, usd
	if sum, err := s.Summary(); err == nil {
		s.paidUSD = sum.DayFor(day).Totals.PaidUSD
	}
	return day, s.paidUSD
}

// alertIfDue posts the paid-spend notification when Decide says so. The
// announced level is persisted beside the ledger so a restart does not repeat
// it; the notification runs in the background so it never holds up the
// response that triggered it.
func (s *Service) alertIfDue(day string, paidUSD float64) {
	if !s.cfg.Spend.Alert || s.cfg.Spend.UsagePath == "" {
		return
	}
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	path := s.cfg.Spend.UsagePath + ".alert"
	next, due := Decide(LoadAlertState(path), day, paidUSD, s.cfg.Spend.AlertStepUSD)
	if !due {
		return
	}
	if err := SaveAlertState(path, next); err != nil {
		s.log.Warn("spend alert state save failed", "err", err)
		return
	}
	notify := s.opts.Notify
	if notify == nil {
		notify = DesktopNotify
	}
	go notify("conduit: paid AI spend", fmt.Sprintf("Paid AI spend today: $%.2f", paidUSD))
}

// Summary reads the ledger.
func (s *Service) Summary() (Summary, error) {
	return Read(s.cfg.Spend.UsagePath, s.opts.Location)
}

// Spend is the GET /_gateway/spend payload.
type Spend struct {
	Now string `json:"now"`
	// Day is the local calendar day the today fields cover.
	Day       string  `json:"day"`
	Today     Totals  `json:"today"`
	TodayPaid float64 `json:"today_paid_usd"`
	// Providers and Hours are today's per-provider totals and per-hour
	// request counts.
	Providers map[string]Totals  `json:"providers"`
	Hours     []map[string]int64 `json:"hours"`
	// Days is the per-day table, oldest first, bounded by the days query.
	Days   []Day    `json:"days"`
	Recent []Record `json:"recent"`
	// Since is the oldest record read; Truncated says the ledger was larger
	// than one read covers and only its newest part is reflected.
	Since     *time.Time `json:"since,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
	// DeepSeek requests today, and how many of them were failovers.
	DeepSeekRequests  int64 `json:"deepseek_requests"`
	DeepSeekFailovers int64 `json:"deepseek_failovers"`
	// Balance is the DeepSeek credit balance; absent without a key.
	Balance      *Balance `json:"deepseek_balance,omitempty"`
	BalanceError string   `json:"deepseek_balance_error,omitempty"`
	// Claude is the plan quota; absent without quota-axi.
	Claude      *ClaudeQuota `json:"claude,omitempty"`
	ClaudeError string       `json:"claude_error,omitempty"`
	// Serving is the provider of the last model call, when there was one.
	Serving *Serving `json:"serving,omitempty"`
}

// Serving is the gateway's last-request snapshot.
type Serving struct {
	Provider      string    `json:"provider"`
	UpstreamModel string    `json:"upstream_model,omitempty"`
	At            time.Time `json:"at"`
}

// BuildSpend assembles the payload: days of history (including today) and
// the newest recent requests. The external reads run in parallel and are
// optional — a missing key or tool leaves its section absent.
func (s *Service) BuildSpend(ctx context.Context, serving *Serving, days, recent int) Spend {
	now := s.opts.Now()
	local := now.In(s.opts.Location)
	day := local.Format("2006-01-02")
	sum, err := s.Summary()
	if err != nil {
		s.log.Warn("usage ledger read failed", "err", err)
	}
	today := sum.DayFor(day)
	out := Spend{
		Now:       now.UTC().Format(time.RFC3339),
		Day:       day,
		Today:     today.Totals,
		TodayPaid: today.Totals.PaidUSD,
		Providers: today.Providers,
		Hours:     today.Hours,
		Days:      []Day{},
		Recent:    sum.Recent,
		Since:     sum.Since,
		Truncated: sum.Truncated,
		Serving:   serving,
	}
	if ds, ok := today.Providers["deepseek"]; ok {
		out.DeepSeekRequests, out.DeepSeekFailovers = ds.Requests, ds.Failovers
	}
	if recent >= 0 && len(out.Recent) > recent {
		out.Recent = out.Recent[:recent]
	}
	cut := local.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	for _, d := range sum.Days {
		if d.Day >= cut {
			out.Days = append(out.Days, d)
		}
	}

	bal, quota := s.sources()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b, err := bal.Get(ctx)
		out.Balance = b
		if err != nil && !errors.Is(err, ErrUnavailable) {
			out.BalanceError = err.Error()
		}
	}()
	go func() {
		defer wg.Done()
		q, err := quota.Claude(ctx)
		out.Claude = q
		if err != nil && !errors.Is(err, ErrUnavailable) {
			out.ClaudeError = err.Error()
		}
	}()
	wg.Wait()
	return out
}

// DesktopNotify posts a system notification: osascript on macOS, notify-send
// on Linux. Best effort; a missing tool or a denied permission is ignored.
func DesktopNotify(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, appleScriptEscape(message), appleScriptEscape(title))
		_ = exec.Command("osascript", "-e", script).Run()
	case "linux":
		_ = exec.Command("notify-send", title, message).Run()
	}
}

func appleScriptEscape(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' || v[i] == '"' {
			out = append(out, '\\')
		}
		out = append(out, v[i])
	}
	return string(out)
}
