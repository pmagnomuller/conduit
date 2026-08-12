package breaker_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pedro-mueller/conduit/internal/breaker"
)

func TestTransitions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var logs []string
	br := breaker.New(path, 5*time.Minute, true, func(format string, args ...any) {
		logs = append(logs, format)
	})

	now := time.Now()
	if st := br.Decide("anthropic", "claude-opus-5", now); st != breaker.Closed {
		t.Fatalf("initial=%s", st)
	}

	until := now.Add(2 * time.Minute)
	br.Open("anthropic", "claude-opus-5", "rate_limit_error", until, now)
	if st := br.Decide("anthropic", "claude-opus-5", now); st != breaker.Open {
		t.Fatalf("after open=%s", st)
	}

	// Expiry → PROBE
	if st := br.Decide("anthropic", "claude-opus-5", until.Add(time.Second)); st != breaker.Probe {
		t.Fatalf("after expiry=%s", st)
	}

	// Probe success → CLOSED
	br.Close("anthropic", "claude-opus-5", until.Add(2*time.Second))
	if st := br.Decide("anthropic", "claude-opus-5", until.Add(3*time.Second)); st != breaker.Closed {
		t.Fatalf("after close=%s", st)
	}

	// OPEN again, then probe fails → OPEN with new until
	br.Open("anthropic", "claude-opus-5", "rate_limit_error", until, now)
	_ = br.Decide("anthropic", "claude-opus-5", until.Add(time.Second)) // → PROBE
	newUntil := until.Add(10 * time.Minute)
	br.ReOpenFromProbe("anthropic", "claude-opus-5", "rate_limit_error", newUntil, until.Add(2*time.Second))
	if st := br.Decide("anthropic", "claude-opus-5", until.Add(3*time.Second)); st != breaker.Open {
		t.Fatalf("reopen=%s", st)
	}
	e, ok := br.Entry("anthropic", "claude-opus-5")
	if !ok || !e.Until.Equal(newUntil) {
		t.Fatalf("until=%v want=%v ok=%v", e.Until, newUntil, ok)
	}

	if len(logs) == 0 {
		t.Fatal("expected transition log lines")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	br := breaker.New(path, time.Minute, true, nil)
	now := time.Now().UTC().Truncate(time.Second)
	until := now.Add(3 * time.Minute)
	br.Open("anthropic", "m1", "unified_rejected", until, now)

	br2 := breaker.New(path, time.Minute, true, nil)
	st := br2.Decide("anthropic", "m1", now.Add(time.Second))
	if st != breaker.Open {
		t.Fatalf("reloaded state=%s", st)
	}
	e, ok := br2.Entry("anthropic", "m1")
	if !ok {
		t.Fatal("missing entry")
	}
	if !e.Until.Equal(until) {
		t.Fatalf("until=%v want=%v", e.Until, until)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty state file")
	}
}

func TestFallbackUntilWhenZero(t *testing.T) {
	br := breaker.New("", 90*time.Second, true, nil)
	now := time.Now()
	br.Open("anthropic", "m", "billing_error", time.Time{}, now)
	e, _ := br.Entry("anthropic", "m")
	delta := e.Until.Sub(now)
	if delta < 80*time.Second || delta > 100*time.Second {
		t.Fatalf("fallback until delta=%s", delta)
	}
}
