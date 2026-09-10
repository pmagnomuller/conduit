package breaker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type State string

const (
	Closed State = "CLOSED"
	Open   State = "OPEN"
	Probe  State = "PROBE"
)

type Entry struct {
	State      State     `json:"state"`
	Until      time.Time `json:"until,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	OpenedAt   time.Time `json:"opened_at,omitempty"`
	LastChange time.Time `json:"last_change,omitempty"`
}

type LastQuotaEvent struct {
	At     time.Time `json:"at"`
	Model  string    `json:"model"`
	Reason string    `json:"reason"`
}

type Snapshot struct {
	Entries map[string]Entry `json:"entries"`
	LastQuota *LastQuotaEvent `json:"last_quota,omitempty"`
	// Forced routing (manual override via /_gateway/route). Empty provider
	// means automatic breaker-driven routing.
	ForcedProvider string `json:"forced_provider,omitempty"`
	ForcedModel    string `json:"forced_model,omitempty"`
}

// Key identifies breaker state for an (upstream, model) pair.
func Key(upstream, model string) string {
	if model == "" {
		model = "*"
	}
	return upstream + "|" + model
}

type Logger func(format string, args ...any)

type Breaker struct {
	mu       sync.Mutex
	entries  map[string]Entry
	lastQuota *LastQuotaEvent
	forcedProvider string
	forcedModel    string
	path     string
	fallback time.Duration
	probeOn  bool
	log      Logger
}

func New(path string, fallbackOpen time.Duration, probeOnExpiry bool, log Logger) *Breaker {
	if log == nil {
		log = func(string, ...any) {}
	}
	b := &Breaker{
		entries:  make(map[string]Entry),
		path:     path,
		fallback: fallbackOpen,
		probeOn:  probeOnExpiry,
		log:      log,
	}
	_ = b.load()
	return b
}

func (b *Breaker) load() error {
	if b.path == "" {
		return nil
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if snap.Entries == nil {
		snap.Entries = map[string]Entry{}
	}
	b.entries = snap.Entries
	b.lastQuota = snap.LastQuota
	b.forcedProvider = snap.ForcedProvider
	b.forcedModel = snap.ForcedModel
	return nil
}

func (b *Breaker) persistLocked() error {
	if b.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	snap := Snapshot{Entries: b.entries, LastQuota: b.lastQuota, ForcedProvider: b.forcedProvider, ForcedModel: b.forcedModel}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// Decide returns the routing state for a model right now, advancing OPEN→PROBE
// when the open window has expired.
func (b *Breaker) Decide(upstream, model string, now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := Key(upstream, model)
	e, ok := b.entries[key]
	if !ok {
		return Closed
	}
	switch e.State {
	case Open:
		if !e.Until.IsZero() && !now.Before(e.Until) {
			if b.probeOn {
				e.State = Probe
				e.LastChange = now
				b.entries[key] = e
				_ = b.persistLocked()
				b.log("BREAKER PROBE %s (open window expired at %s)", model, e.Until.Format(time.RFC3339))
				return Probe
			}
			// No probe: close immediately.
			delete(b.entries, key)
			_ = b.persistLocked()
			b.log("BREAKER CLOSED %s (open window expired, probe disabled)", model)
			return Closed
		}
		return Open
	case Probe:
		return Probe
	default:
		return Closed
	}
}

// Open sets the breaker to OPEN. Returns true if this call newly transitioned
// into OPEN (false if it was already OPEN and we only refreshed until/reason).
func (b *Breaker) Open(upstream, model, reason string, until time.Time, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until.IsZero() || !until.After(now) {
		until = now.Add(b.fallback)
	}
	key := Key(upstream, model)
	prev := b.entries[key]
	newly := prev.State != Open
	e := Entry{
		State:      Open,
		Until:      until,
		Reason:     reason,
		OpenedAt:   now,
		LastChange: now,
	}
	if prev.State == Open && !prev.OpenedAt.IsZero() {
		e.OpenedAt = prev.OpenedAt
	}
	b.entries[key] = e
	b.lastQuota = &LastQuotaEvent{At: now, Model: model, Reason: reason}
	_ = b.persistLocked()

	retryAfter := until.Sub(now).Round(time.Second)
	b.log("BREAKER OPEN %s -> glm (%s, retry-after=%s)", model, reason, retryAfter)
	return newly
}

// Close clears breaker state for the model. Returns true if an entry was removed.
func (b *Breaker) Close(upstream, model string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := Key(upstream, model)
	if _, ok := b.entries[key]; !ok {
		return false
	}
	delete(b.entries, key)
	_ = b.persistLocked()
	b.log("BREAKER CLOSED %s (probe succeeded)", model)
	return true
}

func (b *Breaker) ReOpenFromProbe(upstream, model, reason string, until time.Time, now time.Time) bool {
	return b.Open(upstream, model, reason, until, now)
}

// RoutingProvider returns a coarse hint for UIs: "glm" if any entry is OPEN,
// "probe" if any is PROBE and none OPEN, otherwise "anthropic".
func (b *Breaker) RoutingProvider() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	probe := false
	for _, e := range b.entries {
		switch e.State {
		case Open:
			return "glm"
		case Probe:
			probe = true
		}
	}
	if probe {
		return "probe"
	}
	return "anthropic"
}

func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make(map[string]Entry, len(b.entries))
	for k, v := range b.entries {
		cp[k] = v
	}
	var lq *LastQuotaEvent
	if b.lastQuota != nil {
		tmp := *b.lastQuota
		lq = &tmp
	}
	return Snapshot{Entries: cp, LastQuota: lq}
}

func (b *Breaker) Entry(upstream, model string) (Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[Key(upstream, model)]
	return e, ok
}

func (b *Breaker) Path() string { return b.path }

// SetForce pins routing to a provider ("" = automatic) with an optional model
// override applied when that provider serves requests.
func (b *Breaker) SetForce(provider, model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forcedProvider = provider
	b.forcedModel = model
	_ = b.persistLocked()
	b.log("ROUTE FORCED %s %s", provider, model)
}

func (b *Breaker) ClearForce() { b.SetForce("", "") }

func (b *Breaker) ForcedProvider() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forcedProvider
}

func (b *Breaker) ForcedModel() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.forcedModel
}

// ForceOpenForTest opens without logging side effects beyond normal Open.
func (b *Breaker) String() string {
	return fmt.Sprintf("breaker(%d entries)", len(b.entries))
}
