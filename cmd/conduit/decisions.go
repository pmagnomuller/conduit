package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// decision mirrors one line of the Jev decisions log written by the gateway
// (internal/route.Decision).
type decision struct {
	At             time.Time `json:"at"`
	RequestedModel string    `json:"requested_model"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Step           string    `json:"step"`
	Lease          string    `json:"lease"`
	Source         string    `json:"source"`
	Reason         string    `json:"reason,omitempty"`
	Confidence     float64   `json:"confidence,omitempty"`
	Margin         float64   `json:"margin,omitempty"`
	Pick           string    `json:"pick,omitempty"`
	LatencyMS      int64     `json:"latency_ms"`
}

// decisionsPath mirrors config's decisions_path default so the CLI can find the
// log without loading the config file. Deliberately HOME-based, not XDG-aware:
// the gateway expands "~/.local/state/conduit/decisions.jsonl" with ExpandHome
// and never consults XDG_STATE_HOME, so an XDG-aware reader would look in a
// directory the gateway never writes to — and, because a missing log is not an
// error, would report "no decisions yet" over a log that is sitting right there.
// CONDUIT_DECISIONS_PATH overrides (tests, or a relocated state dir).
func decisionsPath() string {
	if p := os.Getenv("CONDUIT_DECISIONS_PATH"); p != "" {
		return p
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "state", "conduit", "decisions.jsonl")
}

func runDecisions(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("decisions", stdout, stderr)
	n := fs.Int("n", 20, "how many decisions to show (most recent last)")
	if rc, ok := parseFlags(fs, args); !ok {
		return rc
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "conduitctl: decisions takes no arguments\n\n%s", usageText)
		return exitUsage
	}
	if *n < 1 {
		fmt.Fprintf(stderr, "conduitctl: -n must be at least 1\n")
		return exitUsage
	}
	path := decisionsPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Not an error: jev has simply never routed a call.
			fmt.Fprintf(stdout, "jev has not produced a decision yet (%s)\n", path)
			return exitOK
		}
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	defer func() { _ = f.Close() }()

	// The gateway rotates the log at 4 MiB, so reading it whole to take the
	// tail is bounded and much simpler than seeking backwards.
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	if len(lines) > *n {
		lines = lines[len(lines)-*n:]
	}
	var skipped int
	for _, line := range lines {
		var d decision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			// A gateway killed mid-append leaves a torn last line; skip it
			// rather than failing the whole tail.
			skipped++
			continue
		}
		fmt.Fprintln(stdout, formatDecision(d))
	}
	if skipped > 0 {
		fmt.Fprintf(stderr, "conduitctl: skipped %d unparseable line(s)\n", skipped)
	}
	if len(lines) == 0 {
		fmt.Fprintf(stdout, "no decisions in %s yet\n", path)
	}
	return exitOK
}

// formatDecision is one line per decision: time, requested→chosen, step, lease,
// source, confidence, latency.
func formatDecision(d decision) string {
	chosen := d.Provider
	if d.Model != "" {
		if chosen != "" {
			chosen += "/"
		}
		chosen += d.Model
	}
	if chosen == "" {
		chosen = "?"
	}
	line := fmt.Sprintf("%s %s→%s step=%s lease=%s src=%s",
		d.At.Local().Format("2006-01-02 15:04:05"), d.RequestedModel, chosen, d.Step, d.Lease, d.Source)
	if d.Confidence > 0 {
		line += fmt.Sprintf(" conf=%.2f", d.Confidence)
	}
	if d.Margin > 0 {
		line += fmt.Sprintf(" margin=%.2f", d.Margin)
	}
	if d.Pick != "" {
		line += " pick=" + d.Pick
	}
	line += fmt.Sprintf(" %dms", d.LatencyMS)
	if d.Reason != "" {
		line += " reason=" + d.Reason
	}
	return line
}
