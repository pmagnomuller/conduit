package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// spendInfo is the subset of GET /_gateway/spend this client reads. It mirrors
// internal/usage.Spend but is declared here: the CLI builds as its own binary
// and must keep working against a gateway that knows newer fields.
type spendInfo struct {
	Now       string              `json:"now"`
	Day       string              `json:"day"`
	Today     billable            `json:"today"`
	TodayPaid float64             `json:"today_paid_usd"`
	Providers map[string]billable `json:"providers"`
	Days      []spendDay          `json:"days"`
	Recent    []spendRecord       `json:"recent"`
	Since     *time.Time          `json:"since"`
	Truncated bool                `json:"truncated"`

	DeepSeekRequests  int64 `json:"deepseek_requests"`
	DeepSeekFailovers int64 `json:"deepseek_failovers"`
	Balance           *struct {
		USD    float64   `json:"usd"`
		At     time.Time `json:"at"`
		Reused bool      `json:"reused"`
	} `json:"deepseek_balance"`
	BalanceError string `json:"deepseek_balance_error"`

	Claude *struct {
		Plan    string        `json:"plan"`
		Status  string        `json:"status"`
		Windows []spendWindow `json:"windows"`
		Extra   *spendExtra   `json:"extra_usage"`
	} `json:"claude"`
	ClaudeError string `json:"claude_error"`

	Serving *struct {
		Provider      string    `json:"provider"`
		UpstreamModel string    `json:"upstream_model"`
		At            time.Time `json:"at"`
	} `json:"serving"`
}

// spendWindow is one Claude plan window.
type spendWindow struct {
	ID               string     `json:"id"`
	Label            string     `json:"label"`
	Kind             string     `json:"kind"`
	PercentRemaining *float64   `json:"percent_remaining"`
	ResetsAt         *time.Time `json:"resets_at"`
}

// spendExtra is Claude pay-as-you-go overflow beyond the plan.
type spendExtra struct {
	SpentUSD float64 `json:"spent_usd"`
	LimitUSD float64 `json:"limit_usd"`
}

// billable is the spend rollup shape (usage.Totals).
type billable struct {
	Requests     int64   `json:"requests"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	Tokens       int64   `json:"tokens"`
	USD          float64 `json:"usd"`
	PaidUSD      float64 `json:"paid_usd"`
	Failovers    int64   `json:"failovers"`
	Subscription int64   `json:"subscription_requests"`
}

// spendDay is one day in the spend table.
type spendDay struct {
	Day       string              `json:"day"`
	Totals    billable            `json:"totals"`
	Providers map[string]billable `json:"providers"`
}

// spendRecord is one request in the ledger.
type spendRecord struct {
	At          time.Time `json:"at"`
	Provider    string    `json:"provider"`
	Requested   string    `json:"requested_model"`
	Upstream    string    `json:"upstream_model"`
	In          int64     `json:"in"`
	Out         int64     `json:"out"`
	CacheRead   int64     `json:"cache_read"`
	CacheWrite  int64     `json:"cache_write"`
	USD         float64   `json:"usd"`
	Billing     string    `json:"billing"`
	Failover    bool      `json:"failover"`
	RequestID   string    `json:"request_id"`
	ModelSource string    `json:"model_source"`
}

func runSpend(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "line" {
		return runSpendLine(args[1:], stdout, stderr)
	}
	fs := newFlagSet("spend", stdout, stderr)
	days := fs.Int("days", 7, "days of history to show")
	asJSON := fs.Bool("json", false, "print the raw /_gateway/spend payload")
	if rc, ok := parseFlags(fs, args); !ok {
		return rc
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "conduitctl: spend takes no positional arguments\n\n%s", usageText)
		return exitUsage
	}
	if *days <= 0 {
		fmt.Fprintf(stderr, "conduitctl: -days must be positive\n")
		return exitUsage
	}
	c := newClient()
	if *asJSON {
		raw, err := c.getRaw(spendPath(*days))
		if err != nil {
			fmt.Fprintf(stderr, "conduitctl: %v\n", err)
			return exitError
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
			return exitOK
		}
		fmt.Fprintln(stdout, buf.String())
		return exitOK
	}
	var sp spendInfo
	if err := c.get(spendPath(*days), &sp); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	printSpend(stdout, sp)
	return exitOK
}

// spendPath is the endpoint with its day window; the gateway clamps it.
func spendPath(days int) string { return fmt.Sprintf("/_gateway/spend?days=%d", days) }

func printSpend(w io.Writer, sp spendInfo) {
	today := sp.Day
	fmt.Fprintf(w, "today (%s): paid $%.2f  requests %d  tokens %s\n", today, sp.TodayPaid, sp.Today.Requests, humanTokens(sp.Today.Tokens))
	if len(sp.Providers) > 0 {
		names := providerOrder(sp.Providers)
		parts := make([]string, 0, len(names))
		for _, p := range names {
			t := sp.Providers[p]
			s := fmt.Sprintf("%s %d req", p, t.Requests)
			if t.Failovers > 0 {
				s += fmt.Sprintf(" (%d failover)", t.Failovers)
			}
			s += fmt.Sprintf(" $%.2f", t.USD)
			if t.Subscription > 0 {
				s += " subscription"
			}
			parts = append(parts, s)
		}
		fmt.Fprintf(w, "  by provider: %s\n", strings.Join(parts, " | "))
	}
	if sp.Balance != nil {
		fmt.Fprintf(w, "deepseek credit: $%.2f\n", sp.Balance.USD)
	} else if sp.BalanceError != "" {
		fmt.Fprintf(w, "deepseek credit: unavailable (%s)\n", sp.BalanceError)
	}
	if sp.Claude != nil {
		var wins []string
		for _, win := range sp.Claude.Windows {
			if win.PercentRemaining == nil {
				continue
			}
			s := fmt.Sprintf("%s %.0f%% left", winLabel(win.Label, win.ID), *win.PercentRemaining)
			if win.ResetsAt != nil {
				s += " (resets " + humanIn(*win.ResetsAt) + ")"
			}
			wins = append(wins, s)
		}
		line := strings.Join(wins, " | ")
		if sp.Claude.Extra != nil {
			if line != "" {
				line += " | "
			}
			line += fmt.Sprintf("extra usage $%.2f/$%.2f", sp.Claude.Extra.SpentUSD, sp.Claude.Extra.LimitUSD)
		}
		if line == "" {
			line = "no windows readable"
		}
		fmt.Fprintf(w, "claude plan: %s\n", line)
	} else if sp.ClaudeError != "" {
		fmt.Fprintf(w, "claude plan: unavailable (%s)\n", sp.ClaudeError)
	}
	if sp.Serving != nil && sp.Serving.Provider != "" {
		fmt.Fprintf(w, "serving now: %s %s\n", sp.Serving.Provider, sp.Serving.UpstreamModel)
	}

	if kept := sp.Days; len(kept) > 0 {
		fmt.Fprintf(w, "\n%-10s  %6s  %10s  %8s  %7s\n", "day", "req", "tokens", "paid $", "failov")
		// Oldest first reads as history; newest-last is where "today" sits.
		for _, d := range kept {
			fmt.Fprintf(w, "%-10s  %6d  %10s  %8.2f  %7d\n", d.Day, d.Totals.Requests, humanTokens(d.Totals.Tokens), d.Totals.PaidUSD, d.Totals.Failovers)
		}
	}
	if sp.Truncated {
		fmt.Fprintf(w, "\n(ledger read is partial: showing the newest records only)\n")
	}
}

// runSpendLine prints the one-line summary used in run reports.
func runSpendLine(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("spend line", stdout, stderr)
	if rc, ok := parseFlags(fs, args); !ok {
		return rc
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "conduitctl: spend line takes no arguments\n\n%s", usageText)
		return exitUsage
	}
	var sp spendInfo
	if err := newClient().get(spendPath(7), &sp); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	parts := []string{
		fmt.Sprintf("AI spend: today $%.2f paid (%d req", sp.TodayPaid, sp.Today.Requests),
	}
	if ds, ok := sp.Providers["deepseek"]; ok && ds.Requests > 0 {
		parts[0] += fmt.Sprintf(", %d DeepSeek", ds.Requests)
	}
	parts[0] += ")"
	week := 0.0
	for _, d := range sp.Days {
		week += d.Totals.PaidUSD
	}
	parts = append(parts, fmt.Sprintf("7d $%.2f", week))
	if sp.Balance != nil {
		parts = append(parts, fmt.Sprintf("DeepSeek credit $%.2f", sp.Balance.USD))
	}
	if sp.Claude != nil {
		if w := findWindow(sp.Claude.Windows, "seven_day"); w != nil && w.PercentRemaining != nil {
			parts = append(parts, fmt.Sprintf("Claude plan %.0f%% week left", *w.PercentRemaining))
		}
		if w := findWindow(sp.Claude.Windows, "five_hour"); w != nil && w.PercentRemaining != nil {
			parts = append(parts, fmt.Sprintf("%.0f%% session left", *w.PercentRemaining))
		}
		if sp.Claude.Extra != nil {
			parts = append(parts, fmt.Sprintf("extra usage $%.2f/$%.2f", sp.Claude.Extra.SpentUSD, sp.Claude.Extra.LimitUSD))
		}
	}
	if sp.Serving != nil && sp.Serving.Provider != "" {
		parts = append(parts, "serving "+sp.Serving.Provider)
	}
	fmt.Fprintln(stdout, strings.Join(parts, " | "))
	return exitOK
}

func findWindow(windows []spendWindow, id string) *spendWindow {
	for i := range windows {
		if windows[i].ID == id {
			return &windows[i]
		}
	}
	return nil
}

func providerOrder(m map[string]billable) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func winLabel(label, id string) string {
	if label != "" {
		return label
	}
	return id
}

func humanIn(t time.Time) string {
	d := time.Until(t).Round(time.Minute)
	if d <= 0 {
		return "now"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
