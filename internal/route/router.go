package route

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/pedro-mueller/conduit/internal/config"
	"github.com/pedro-mueller/conduit/internal/redact"
)

// Decision is one routing outcome. Every Decide call produces exactly one,
// including fail-open, so the log is a complete record of jev-mode traffic.
type Decision struct {
	At             time.Time `json:"at"`
	RequestedModel string    `json:"requested_model"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Step           string    `json:"step"`             // user_turn|tool_step|other
	Lease          string    `json:"lease"`            // one_call|tool_chain|user_turn
	Source         string    `json:"source"`           // jev|lease|sticky|low_confidence|fail_open
	Reason         string    `json:"reason,omitempty"` // error text on fail_open, redacted
	Confidence     float64   `json:"confidence,omitempty"`
	Margin         float64   `json:"margin,omitempty"` // Jev's p(top)−p(second), when given
	// Pick is Jev's choice when the router overrode it (sticky, low_confidence).
	Pick string `json:"pick,omitempty"`
	// Policy is "restricted" when sensitive content limited the candidates.
	Policy string `json:"policy,omitempty"`
	// EstInputUSD prices this call's input on the chosen model (cache-read
	// rate when it stays on a warm model); BaselineInputUSD the same call on
	// the requested model, as auto would have sent it. Input only: output
	// size is unknown at decision time. List-price estimates, 0 when unpriced.
	EstInputUSD      float64 `json:"est_input_usd,omitempty"`
	BaselineInputUSD float64 `json:"baseline_input_usd,omitempty"`
	LatencyMS        int64   `json:"latency_ms"`
}

const (
	SourceJev      = "jev"
	SourceLease    = "lease"
	SourceFailOpen = "fail_open"
	// SourceSticky: Jev wanted a switch on a large context without enough
	// confidence to pay the rebuild, so the thread stayed on its model.
	SourceSticky = "sticky"
	// SourceLowConfidence: Jev's top two picks were too close to call, so the
	// thread stayed on its model.
	SourceLowConfidence = "low_confidence"

	// cacheWarmWindow approximates the provider prompt-cache TTL (Anthropic's
	// default is five minutes); a thread served within it is cache-warm.
	cacheWarmWindow = 5 * time.Minute

	PolicyRestricted = "restricted"

	recentCap = 200
	// leaseCap bounds the lease map; conversations that never end their
	// lease naturally would otherwise accumulate forever.
	leaseCap = 1000
)

// decisionsMaxBytes caps the decisions log before it rotates. A var, not a
// const, so tests can exercise rotation without writing megabytes. Worst case
// on disk is two files of about this size.
var decisionsMaxBytes int64 = 4 << 20

// served is the last catalog key a thread was routed to.
type served struct {
	key string
	at  time.Time
}

type lease struct {
	decision Decision
	toolSet  []string // sorted names at decision time (tool_chain matching)
	expires  time.Time
}

// Router asks Jev which catalog entry should serve a call and remembers the
// answer per conversation according to the lease Jev granted.
type Router struct {
	cfg     config.JevConfig
	enabled bool
	client  *client
	timeout time.Duration
	ttl     time.Duration
	// switch-cost policy, defaults applied; ≤0 disables the check.
	maxSwitch  int
	switchConf float64
	minMargin  float64
	// restricted is non-nil when sensitive-turn filtering is on.
	restricted *restriction
	prices     map[string]config.Candidate // key → prices, default catalog overlaid by cfg
	log        *slog.Logger
	now        func() time.Time // injectable for lease tests

	mu     sync.Mutex
	leases map[string]lease
	// history is the current model per conversation, kept across one_call
	// decisions (unlike leases) so Jev can be told what a switch costs.
	history map[string]served
	recent  []Decision // newest last; Recent reverses
	fileMu  sync.Mutex
	// decisions log bookkeeping, guarded by fileMu; logBytes is the current
	// append offset, so rotation needs no stat per decision.
	logPath  string
	logBytes int64
}

// New builds a router. apiKey=="" → Enabled()==false and Decide always
// returns ok=false without recording (the gateway never enters jev mode).
func New(cfg config.JevConfig, apiKey string, log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	ttl := time.Duration(cfg.LeaseTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	r := &Router{
		cfg:        cfg,
		enabled:    apiKey != "",
		timeout:    timeout,
		ttl:        ttl,
		maxSwitch:  orDefault(cfg.MaxSwitchContext, config.DefaultMaxSwitchContext),
		switchConf: orDefault(cfg.SwitchConfidence, config.DefaultSwitchConfidence),
		minMargin:  orDefault(cfg.MinMargin, config.DefaultMinMargin),
		log:        log,
		now:        time.Now,
		leases:     map[string]lease{},
		history:    map[string]served{},
	}
	r.prices = priceTable(cfg.Catalog)
	if cfg.RestrictSensitive && len(cfg.RestrictedProviders) > 0 {
		r.restricted = newRestriction(cfg.RestrictedPatterns, cfg.RestrictedProviders, log)
	}
	if r.enabled {
		r.client = &client{
			baseURL: cfg.BaseURL,
			apiKey:  apiKey,
			// Timeout here is a backstop; Decide also bounds ctx.
			http: &http.Client{Timeout: timeout + time.Second},
		}
	}
	return r
}

// orDefault maps the zero value to def; negative stays negative (disabled).
func orDefault[T int | float64](v, def T) T {
	if v == 0 {
		return def
	}
	return v
}

func (r *Router) Enabled() bool { return r.enabled }

// Catalog returns a copy of the configured candidates.
func (r *Router) Catalog() []Candidate {
	return slices.Clone(r.cfg.Catalog)
}

// Decide picks among candidates (already filtered by the caller for breaker
// state and available keys). It never panics and never blocks past the
// configured timeout. ok=false means the caller must fall back to auto.
func (r *Router) Decide(ctx context.Context, body []byte, candidates []Candidate) (d Decision, ok bool) {
	if !r.enabled {
		return Decision{}, false
	}
	start := r.now()
	var dossier Dossier
	defer func() {
		if rec := recover(); rec != nil {
			d = r.failOpen(start, dossier, fmt.Sprintf("panic: %v", rec))
			ok = false
		}
	}()

	dossier = Extract(body)
	if r.restricted != nil && r.restricted.matches(dossier.recent) {
		dossier.Sensitive = true
		candidates = r.restricted.filter(candidates)
	}
	if len(candidates) == 0 {
		reason := "no candidates"
		if dossier.Sensitive {
			reason = "sensitive content and no trusted candidate"
		}
		return r.failOpen(start, dossier, reason), false
	}

	r.fillCurrent(&dossier, candidates, start)

	if ld, reused := r.reuseLease(dossier, candidates, start); reused {
		ld.At = start
		ld.Source = SourceLease
		ld.Step = dossier.Step
		ld.RequestedModel = dossier.RequestedModel
		ld.Reason = ""
		ld.Pick, ld.Margin = "", 0
		ld.Policy = policyOf(dossier)
		r.priceDecision(&ld, dossier)
		ld.LatencyMS = r.now().Sub(start).Milliseconds()
		r.remember(dossier, ld, start)
		r.record(ld)
		return ld, true
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	res, err := r.client.ask(ctx, dossier, candidates)
	if err != nil {
		return r.failOpen(start, dossier, err.Error()), false
	}
	choice, source := r.applySwitchPolicy(dossier, res)
	provider, model := splitKey(choice)
	d = Decision{
		At:             start,
		RequestedModel: dossier.RequestedModel,
		Provider:       provider,
		Model:          model,
		Step:           dossier.Step,
		Lease:          res.Lease,
		Source:         source,
		Confidence:     res.Confidence,
		Margin:         res.Margin,
		Policy:         policyOf(dossier),
		LatencyMS:      r.now().Sub(start).Milliseconds(),
	}
	if source != SourceJev {
		// An override is a one-off: leasing it would pin the thread against
		// Jev's actual answer.
		d.Pick = res.Choice
		d.Lease = LeaseOneCall
	}
	r.priceDecision(&d, dossier)
	r.storeLease(dossier, d, start)
	r.remember(dossier, d, start)
	r.record(d)
	return d, true
}

// applySwitchPolicy decides whether Jev's pick is worth a context rebuild.
// Only a switch away from a known current model can be overridden, and the
// override is always to stay: the router never invents a third choice.
func (r *Router) applySwitchPolicy(d Dossier, res jevResult) (string, string) {
	if d.Current == "" || res.Choice == d.Current {
		return res.Choice, SourceJev
	}
	if r.minMargin > 0 && res.HasMargin && res.Margin < r.minMargin {
		return d.Current, SourceLowConfidence
	}
	if r.maxSwitch > 0 && d.ContextTokensEst > r.maxSwitch && res.Confidence < r.switchConf {
		return d.Current, SourceSticky
	}
	return res.Choice, SourceJev
}

// fillCurrent tells the dossier which candidate served the thread last and
// whether its cache is plausibly warm. A current model no longer offered
// (breaker OPEN, key removed) is not a place to stay, so it is left out.
func (r *Router) fillCurrent(d *Dossier, cands []Candidate, now time.Time) {
	r.mu.Lock()
	h, ok := r.history[d.fingerprint]
	r.mu.Unlock()
	if !ok || now.Sub(h.at) >= r.ttl {
		return
	}
	d.recentlyServed = now.Sub(h.at) < cacheWarmWindow
	if !slices.ContainsFunc(cands, func(c Candidate) bool { return c.Key() == h.key }) {
		return
	}
	d.Current = h.key
	d.CacheWarm = now.Sub(h.at) < cacheWarmWindow
}

// remember records the routed model for the thread. It reflects the routing
// decision, not a later upstream failover.
func (r *Router) remember(d Dossier, dec Decision, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.history) >= leaseCap {
		for k, h := range r.history {
			if now.Sub(h.at) >= r.ttl {
				delete(r.history, k)
			}
		}
		if len(r.history) >= leaseCap {
			for k := range r.history {
				delete(r.history, k)
				if len(r.history) < leaseCap/2 {
					break
				}
			}
		}
	}
	r.history[d.fingerprint] = served{key: dec.Provider + "/" + dec.Model, at: now}
}

// priceTable merges prices: default catalog first, then any configured
// entry's non-zero prices, so a catalog override without prices still gets
// an estimate for the ids conduit knows.
func priceTable(catalog []Candidate) map[string]config.Candidate {
	t := map[string]config.Candidate{}
	for _, c := range config.DefaultCatalog() {
		t[c.Key()] = c
	}
	for _, c := range catalog {
		p := t[c.Key()]
		if c.PriceIn > 0 {
			p.PriceIn = c.PriceIn
		}
		if c.PriceCacheRead > 0 {
			p.PriceCacheRead = c.PriceCacheRead
		}
		if c.PriceOut > 0 {
			p.PriceOut = c.PriceOut
		}
		t[c.Key()] = p
	}
	return t
}

// inputUSD prices tokens on key, at the cache-read rate when warm.
func (r *Router) inputUSD(key string, tokens int, warm bool) float64 {
	p, ok := r.prices[key]
	if !ok {
		return 0
	}
	rate := p.PriceIn
	if warm && p.PriceCacheRead > 0 {
		rate = p.PriceCacheRead
	}
	return float64(tokens) * rate / 1e6
}

// priceDecision fills the cost estimate. The chosen model is warm only when
// the thread stays on it; the baseline (requested model, as auto sends it)
// is warm whenever the thread was active, since auto would never have moved.
// Cache-write premiums are ignored.
func (r *Router) priceDecision(dec *Decision, d Dossier) {
	key := dec.Provider + "/" + dec.Model
	dec.EstInputUSD = r.inputUSD(key, d.ContextTokensEst, d.CacheWarm && d.Current == key)
	dec.BaselineInputUSD = r.inputUSD("anthropic/"+d.RequestedModel, d.ContextTokensEst, d.recentlyServed)
}

func splitKey(key string) (provider, model string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

func (r *Router) failOpen(start time.Time, d Dossier, reason string) Decision {
	dec := Decision{
		At:             start,
		RequestedModel: d.RequestedModel,
		Step:           d.Step,
		Source:         SourceFailOpen,
		Reason:         redact.String(reason),
		Policy:         policyOf(d),
		LatencyMS:      r.now().Sub(start).Milliseconds(),
	}
	r.record(dec)
	return dec
}

// reuseLease returns the leased decision when the lease still applies:
//   - one_call: never
//   - tool_chain: step==tool_step and identical tool set
//   - user_turn: step==tool_step (any tools); a new user turn ends it
//
// Expired leases and leases whose candidate was filtered out are dropped.
func (r *Router) reuseLease(d Dossier, cands []Candidate, now time.Time) (Decision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.leases[d.fingerprint]
	if !ok {
		return Decision{}, false
	}
	if !now.Before(l.expires) {
		delete(r.leases, d.fingerprint)
		return Decision{}, false
	}
	key := l.decision.Provider + "/" + l.decision.Model
	if !slices.ContainsFunc(cands, func(c Candidate) bool { return c.Key() == key }) {
		delete(r.leases, d.fingerprint)
		return Decision{}, false
	}
	switch l.decision.Lease {
	case LeaseToolChain:
		if d.Step == StepToolStep && slices.Equal(l.toolSet, d.toolSet) {
			return l.decision, true
		}
	case LeaseUserTurn:
		if d.Step == StepToolStep {
			return l.decision, true
		}
	}
	if d.Step == StepUserTurn {
		// A fresh user message ends every lease kind.
		delete(r.leases, d.fingerprint)
	}
	return Decision{}, false
}

func (r *Router) storeLease(d Dossier, dec Decision, now time.Time) {
	if dec.Lease == LeaseOneCall {
		// A one_call answer must also clear an older lease for this thread:
		// reuseLease keys off the stored lease kind, not this decision.
		r.mu.Lock()
		delete(r.leases, d.fingerprint)
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.leases) >= leaseCap {
		for k, l := range r.leases {
			if !now.Before(l.expires) {
				delete(r.leases, k)
			}
		}
		if len(r.leases) >= leaseCap {
			// Still full of live leases: drop arbitrary entries rather than grow.
			for k := range r.leases {
				delete(r.leases, k)
				if len(r.leases) < leaseCap/2 {
					break
				}
			}
		}
	}
	r.leases[d.fingerprint] = lease{
		decision: dec,
		toolSet:  slices.Clone(d.toolSet),
		expires:  now.Add(r.ttl),
	}
}

// record appends to the in-memory ring and the JSONL file.
func (r *Router) record(d Decision) {
	r.mu.Lock()
	r.recent = append(r.recent, d)
	if len(r.recent) > recentCap {
		r.recent = r.recent[len(r.recent)-recentCap:]
	}
	r.mu.Unlock()
	r.appendJSONL(d)
}

// appendJSONL writes one line to the decisions log, rotating to a single ".1"
// predecessor at decisionsMaxBytes so the log cannot grow without bound.
//
// It stays on the caller's goroutine: Decide must not hand work to a writer
// goroutine it would then have to reap, and every field is capped, so the
// append is short. Diagnostics only — a failure here must never fail a
// decision and is therefore swallowed after one log line.
func (r *Router) appendJSONL(d Decision) {
	path := r.cfg.DecisionsPath
	if path == "" {
		return
	}
	line, err := json.Marshal(d)
	if err != nil {
		return
	}
	line = append(line, '\n')
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.log.Warn("decisions log mkdir failed", "path", path, "err", err)
		return
	}
	if r.logPath != path {
		r.logPath, r.logBytes = path, 0
		if st, err := os.Stat(path); err == nil {
			r.logBytes = st.Size()
		}
	}
	// logBytes > 0 also stops a line larger than the cap from rotating the
	// empty file it just wrote forever.
	if r.logBytes > 0 && r.logBytes+int64(len(line)) > decisionsMaxBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			r.log.Warn("decisions log rotate failed", "path", path, "err", err)
		} else {
			r.logBytes = 0
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		r.log.Warn("decisions log open failed", "path", path, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		r.log.Warn("decisions log write failed", "path", path, "err", err)
		return
	}
	r.logBytes += int64(len(line))
}

// Recent returns up to n decisions, newest first.
func (r *Router) Recent(n int) []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.recent) {
		n = len(r.recent)
	}
	out := make([]Decision, 0, n)
	for i := len(r.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.recent[i])
	}
	return out
}
