package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable reports a source that is not configured or not installed. The
// spend view omits that section instead of showing an error.
var ErrUnavailable = errors.New("not configured")

// Balance is a provider's remaining prepaid credit.
type Balance struct {
	USD float64 `json:"usd"`
	// At is when the value was read from the provider.
	At time.Time `json:"at"`
}

// BalanceClient reads DeepSeek's GET /user/balance and caches it for TTL.
type BalanceClient struct {
	URL    string
	APIKey string
	TTL    time.Duration
	HTTP   *http.Client

	mu   sync.Mutex
	last *Balance
	err  error
	// tried is when the last fetch ran, successful or not, so a failing
	// endpoint is not hammered on every poll either.
	tried time.Time
}

// Get returns the balance, cached for TTL. A failed refresh keeps serving the
// last good value alongside the error.
func (b *BalanceClient) Get(ctx context.Context) (*Balance, error) {
	if b == nil || b.URL == "" || b.APIKey == "" {
		return nil, ErrUnavailable
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.tried.IsZero() && time.Since(b.tried) < ttl {
		return b.last, b.err
	}
	b.tried = time.Now()
	val, err := b.fetch(ctx)
	if err != nil {
		b.err = err
		return b.last, err
	}
	b.last, b.err = val, nil
	return val, nil
}

func (b *BalanceClient) fetch(ctx context.Context) (*Balance, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.APIKey)
	req.Header.Set("Accept", "application/json")
	client := b.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// A transport error names the URL, never the header; it is still
		// kept generic so nothing about the request leaks into the payload.
		return nil, errors.New("deepseek balance: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deepseek balance: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, errors.New("deepseek balance: read failed")
	}
	return parseBalance(body)
}

// parseBalance reads DeepSeek's balance shape, keeping the USD total only: a
// non-USD account gets no number rather than one in the wrong unit.
func parseBalance(body []byte) (*Balance, error) {
	var parsed struct {
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("deepseek balance: unexpected payload")
	}
	for _, info := range parsed.BalanceInfos {
		if !strings.EqualFold(info.Currency, "USD") {
			continue
		}
		usd, err := strconv.ParseFloat(strings.TrimSpace(info.TotalBalance), 64)
		if err != nil {
			return nil, errors.New("deepseek balance: unexpected total")
		}
		return &Balance{USD: usd, At: time.Now().UTC()}, nil
	}
	return nil, errors.New("deepseek balance: no USD balance")
}

// ClaudeQuota is the Claude plan's rate windows and extra usage.
type ClaudeQuota struct {
	Plan    string        `json:"plan,omitempty"`
	Status  string        `json:"status,omitempty"`
	Windows []QuotaWindow `json:"windows"`
	Extra   *ExtraUsage   `json:"extra_usage,omitempty"`
}

// QuotaWindow is one rate window (session, week, per-model).
type QuotaWindow struct {
	ID               string     `json:"id"`
	Label            string     `json:"label,omitempty"`
	Kind             string     `json:"kind,omitempty"`
	PercentRemaining *float64   `json:"percent_remaining,omitempty"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
}

// ExtraUsage is pay-as-you-go usage beyond the plan, against its cap.
type ExtraUsage struct {
	SpentUSD float64 `json:"spent_usd"`
	LimitUSD float64 `json:"limit_usd"`
}

// QuotaClient reads Claude plan quota by running `quota-axi --provider claude
// --json --max-age 2m`, cached for TTL. Absent binary → ErrUnavailable.
type QuotaClient struct {
	Enabled bool
	TTL     time.Duration
	// Run executes quota-axi and returns its stdout; nil runs the real
	// binary from PATH. Tests substitute a fixture.
	Run func(ctx context.Context) ([]byte, error)

	mu    sync.Mutex
	last  *ClaudeQuota
	err   error
	tried time.Time
}

// Claude returns the cached quota view, refreshing it when older than TTL.
func (q *QuotaClient) Claude(ctx context.Context) (*ClaudeQuota, error) {
	if q == nil || !q.Enabled {
		return nil, ErrUnavailable
	}
	ttl := q.TTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.tried.IsZero() && time.Since(q.tried) < ttl {
		return q.last, q.err
	}
	q.tried = time.Now()
	run := q.Run
	if run == nil {
		run = runQuotaAXI
	}
	out, err := run(ctx)
	if err != nil {
		q.err = err
		return q.last, err
	}
	cq, err := parseClaudeQuota(out)
	if err != nil {
		q.err = err
		return q.last, err
	}
	q.last, q.err = cq, nil
	return cq, nil
}

func runQuotaAXI(ctx context.Context) ([]byte, error) {
	bin, err := exec.LookPath("quota-axi")
	if err != nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// --max-age lets quota-axi answer from its own cache;
	// --no-credential-refresh keeps a background poll from starting a vendor
	// login flow.
	out, err := exec.CommandContext(ctx, bin, "--provider", "claude", "--json", "--max-age", "2m", "--no-credential-refresh").Output()
	if err != nil {
		return nil, errors.New("quota-axi failed")
	}
	return out, nil
}

// parseClaudeQuota reads the `quota-axi --json` schema, limited to the Claude
// provider's plan, status and windows. A window with spent/limit USD is the
// extra-usage credit window; the rest are rate windows.
func parseClaudeQuota(out []byte) (*ClaudeQuota, error) {
	var p struct {
		Providers []struct {
			Provider string `json:"provider"`
			Plan     string `json:"plan"`
			Windows  []struct {
				ID               string   `json:"id"`
				Label            string   `json:"label"`
				Kind             string   `json:"kind"`
				PercentRemaining *float64 `json:"percentRemaining"`
				ResetsAt         string   `json:"resetsAt"`
				SpentUSD         *float64 `json:"spentUsd"`
				LimitUSD         *float64 `json:"limitUsd"`
			} `json:"windows"`
			State struct {
				Status string `json:"status"`
			} `json:"state"`
			NotSetUp bool `json:"notSetUp"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, errors.New("quota-axi: unexpected output")
	}
	for _, pr := range p.Providers {
		if pr.Provider != "claude" {
			continue
		}
		if pr.NotSetUp {
			return nil, ErrUnavailable
		}
		cq := &ClaudeQuota{Plan: pr.Plan, Status: pr.State.Status, Windows: []QuotaWindow{}}
		for _, w := range pr.Windows {
			if w.SpentUSD != nil || w.LimitUSD != nil {
				ex := &ExtraUsage{}
				if w.SpentUSD != nil {
					ex.SpentUSD = *w.SpentUSD
				}
				if w.LimitUSD != nil {
					ex.LimitUSD = *w.LimitUSD
				}
				cq.Extra = ex
				continue
			}
			qw := QuotaWindow{ID: w.ID, Label: w.Label, Kind: w.Kind, PercentRemaining: w.PercentRemaining}
			if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
				qw.ResetsAt = &t
			}
			cq.Windows = append(cq.Windows, qw)
		}
		return cq, nil
	}
	return nil, ErrUnavailable
}
