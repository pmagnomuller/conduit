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

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stdout, stderr)
	asJSON := fs.Bool("json", false, "print the raw /_gateway/status payload")
	if rc, ok := parseFlags(fs, args); !ok {
		return rc
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "conduitctl: status takes no arguments\n\n%s", usageText)
		return exitUsage
	}
	c := newClient()
	if *asJSON {
		// The raw payload, pretty-printed, so this stands in for
		// `curl -s … | jq .` without requiring jq.
		raw, err := c.getRaw("/_gateway/status")
		if err != nil {
			fmt.Fprintf(stderr, "conduitctl: %v\n", err)
			return exitError
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			// Not JSON, or a torn payload: better than nothing is the raw bytes.
			fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
			return exitOK
		}
		fmt.Fprintln(stdout, buf.String())
		return exitOK
	}
	var st statusInfo
	if err := c.get("/_gateway/status", &st); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	forced := "none"
	if st.ForcedProvider != "" {
		forced = st.ForcedProvider
		if st.ForcedModel != "" {
			forced += "/" + st.ForcedModel
		}
	}
	fmt.Fprintf(stdout, "mode: %s  routing: %s  forced: %s\n", st.Mode, st.Routing, forced)
	fmt.Fprintf(stdout, "breaker: %s\n", breakerLine(st))
	ct := st.Counts
	fmt.Fprintf(stdout, "counts: anthropic=%d glm=%d deepseek=%d failovers=%d transient=%d jev=%d fail_open=%d\n",
		ct.AnthropicRequests, ct.GLMRequests, ct.DeepSeekRequests, ct.Failovers,
		ct.TransientRetries, ct.JevDecisions, ct.JevFailOpen)
	return exitOK
}

// breakerLine lists every breaker entry, sorted so the output is stable across
// calls, with a countdown to the end of the open window when one is set.
func breakerLine(st statusInfo) string {
	if len(st.Breaker.Entries) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(st.Breaker.Entries))
	for k := range st.Breaker.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		e := st.Breaker.Entries[k]
		part := fmt.Sprintf("%s %s", k, e.State)
		if !e.Until.IsZero() {
			part += " until " + countdown(e.Until)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// countdown renders the remaining open window; a window already past reads
// "expired" rather than a negative duration.
func countdown(until time.Time) string {
	d := time.Until(until).Round(time.Second)
	if d <= 0 {
		return "expired"
	}
	return d.String()
}
