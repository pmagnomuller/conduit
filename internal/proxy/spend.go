package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/pedro-mueller/conduit/internal/usage"
)

// handleSpend serves GET /_gateway/spend: the payload the spend tab and
// `conduitctl spend` read. Read-only and loopback-only, like status.
func (g *Gateway) handleSpend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if g.spend == nil {
		// No spend service: an empty payload rather than an error, so the
		// tab renders its empty state instead of "unreachable".
		_ = json.NewEncoder(w).Encode(usage.Spend{
			Providers: map[string]usage.Totals{},
			Hours:     []map[string]int64{},
			Days:      []usage.Day{},
			Recent:    []usage.Record{},
		})
		return
	}
	q := r.URL.Query()
	days := queryInt(q.Get("days"), 14, 1, 366)
	recent := queryInt(q.Get("recent"), 40, 0, 200)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(g.spend.BuildSpend(r.Context(), g.spendServing(), days, recent))
}

// queryInt parses a bounded integer query value, falling back to def when it
// is absent or malformed and clamping it into [lo, hi].
func queryInt(v string, def, lo, hi int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return max(lo, min(n, hi))
}
