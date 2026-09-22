package breaker_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pedro-mueller/conduit/internal/breaker"
	"github.com/pedro-mueller/conduit/internal/route"
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

func TestRoutingModeInvariantsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var logs []string
	br := breaker.New(path, time.Minute, true, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if br.Mode() != route.ModeAuto {
		t.Fatalf("default mode=%s", br.Mode())
	}

	br.SetForce("glm", "glm-5.3")
	if br.Mode() != route.ModePinned {
		t.Fatalf("SetForce mode=%s", br.Mode())
	}
	br.ClearForce()
	if br.Mode() != route.ModeAuto || br.ForcedProvider() != "" {
		t.Fatalf("ClearForce mode=%s forced=%q", br.Mode(), br.ForcedProvider())
	}

	br.SetForce("anthropic", "")
	br.SetMode(route.ModeJev)
	if br.Mode() != route.ModeJev || br.ForcedProvider() != "" {
		t.Fatalf("SetMode(jev) must clear force: %s %q", br.Mode(), br.ForcedProvider())
	}
	found := false
	for _, l := range logs {
		if l == "ROUTE MODE jev" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing ROUTE MODE log: %v", logs)
	}
	if snap := br.Snapshot(); snap.Mode != "jev" {
		t.Fatalf("snapshot mode=%q", snap.Mode)
	}

	// Round-trip.
	br2 := breaker.New(path, time.Minute, true, nil)
	if br2.Mode() != route.ModeJev {
		t.Fatalf("reloaded mode=%s", br2.Mode())
	}

	// pinned keeps force.
	br2.SetForce("glm", "")
	br2.SetMode(route.ModePinned)
	if br2.Mode() != route.ModePinned || br2.ForcedProvider() != "glm" {
		t.Fatalf("pinned: %s %q", br2.Mode(), br2.ForcedProvider())
	}

	// SetMode(auto) clears force too.
	br2.SetMode(route.ModeAuto)
	if br2.ForcedProvider() != "" || br2.Mode() != route.ModeAuto {
		t.Fatalf("auto: %s %q", br2.Mode(), br2.ForcedProvider())
	}
}

func TestModeMissingOnLoadIsAuto(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	br := breaker.New(path, time.Minute, true, nil)
	if br.Mode() != route.ModeAuto {
		t.Fatalf("mode=%s", br.Mode())
	}
	// Legacy file with forced provider but no mode ⇒ pinned.
	if err := os.WriteFile(path, []byte(`{"entries":{},"forced_provider":"glm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	br = breaker.New(path, time.Minute, true, nil)
	if br.Mode() != route.ModePinned || br.ForcedProvider() != "glm" {
		t.Fatalf("legacy pinned: %s %q", br.Mode(), br.ForcedProvider())
	}
	// Garbage mode ⇒ auto.
	if err := os.WriteFile(path, []byte(`{"entries":{},"mode":"wat"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	br = breaker.New(path, time.Minute, true, nil)
	if br.Mode() != route.ModeAuto {
		t.Fatalf("garbage mode=%s", br.Mode())
	}
}

func TestStateIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var logs []string
	br := breaker.New(path, time.Minute, true, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	now := time.Now()
	if st := br.State("anthropic", "absent", now); st != breaker.Closed {
		t.Fatalf("missing entry state=%s", st)
	}

	// OPEN whose window has already expired: Decide would flip it to PROBE,
	// rewrite state.json and log. State must only report it.
	br.Open("anthropic", "m1", "rate_limit_error", now.Add(-time.Second), now.Add(-time.Minute))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := br.State("anthropic", "m1", now); st != breaker.Probe {
		t.Fatalf("expired OPEN state=%s want PROBE", st)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("State rewrote state.json:\n%s\n---\n%s", before, after)
	}
	if e, ok := br.Entry("anthropic", "m1"); !ok || e.State != breaker.Open {
		t.Fatalf("State mutated the entry: %+v ok=%v", e, ok)
	}
	for _, l := range logs {
		if strings.Contains(l, "BREAKER PROBE") || strings.Contains(l, "BREAKER CLOSED") {
			t.Fatalf("State logged a transition: %s", l)
		}
	}

	// Live window stays OPEN.
	br.Open("anthropic", "m2", "rate_limit_error", now.Add(time.Hour), now)
	if st := br.State("anthropic", "m2", now); st != breaker.Open {
		t.Fatalf("live OPEN state=%s", st)
	}
	// Already-probed entry reports PROBE.
	if st := br.Decide("anthropic", "m1", now); st != breaker.Probe {
		t.Fatalf("Decide after State=%s want PROBE", st)
	}
	if st := br.State("anthropic", "m1", now); st != breaker.Probe {
		t.Fatalf("probe state=%s", st)
	}
}

func TestStateWithoutProbesIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var logs []string
	br := breaker.New(path, time.Minute, false, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	now := time.Now()
	br.Open("anthropic", "m1", "rate_limit_error", now.Add(-time.Second), now.Add(-time.Minute))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// probe_on_expiry=false: an expired window reads as CLOSED — but State must
	// not delete the entry the way Decide would.
	if st := br.State("anthropic", "m1", now); st != breaker.Closed {
		t.Fatalf("state=%s want CLOSED", st)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("State rewrote state.json:\n%s\n---\n%s", before, after)
	}
	if _, ok := br.Entry("anthropic", "m1"); !ok {
		t.Fatal("State deleted the entry")
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "BREAKER OPEN") {
		t.Fatalf("State logged something beyond the Open itself: %v", logs)
	}
}
