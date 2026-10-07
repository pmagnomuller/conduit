package route

import "strings"

// Complexity tiers. A tier maps to a band (a rank-ordered subset of the
// catalog), never to a pinned model: standard is also the thin-evidence
// fallback for calls the classifier cannot say anything about.
const (
	TierLight    = "light"
	TierStandard = "standard"
	TierHeavy    = "heavy"
)

// Score thresholds: <= -3 classifies light, >= +3 heavy, else standard.
// Light is deliberately hard to reach — a single signal can score at most
// -2, so light always needs a net negative score backed by at least two
// fired signals, because a light misclassification is silent quality loss.
const (
	lightThreshold = -3
	heavyThreshold = 3
)

// Keyword fragments scored against the task text, lowercased substring
// matches; each group fires at most once. They are deliberately coarse:
// the classifier only has to place the call in the right band, not pick a
// model.
var (
	heavyKeywords = []string{
		"design", "architect", "migrat", "race", "deadlock", "refactor",
		"secur", "concurren", "root cause", "why does",
	}
	lightKeywords = []string{
		"typo", "rename", "format", "lint", "summarize", "bump", "version",
	}
	// readNavTools are the read-only navigation tools; a tool step that is
	// nothing but one of them is mechanical follow-through.
	readNavTools = map[string]bool{
		"Read": true, "Grep": true, "Glob": true, "LS": true,
	}
)

// classify scores a dossier into a complexity tier. It is a pure function
// of the dossier: no clock, no I/O, no iteration whose order can vary, so
// the same dossier always yields the same tier, evidence and reasons.
// Evidence is the number of fired signals; reasons is a bounded list from
// a fixed vocabulary (never task text) for logging.
func classify(d Dossier) (string, int, []string) {
	score := 0
	var reasons []string
	fire := func(delta int, why string) {
		score += delta
		reasons = append(reasons, why)
	}

	switch {
	case d.ThinkingBudget >= 16000:
		fire(+4, "thinking budget >= 16000")
	case d.ThinkingBudget > 0:
		fire(+2, "thinking budget on")
	}
	if d.Step == StepToolStep && d.ToolBatch != nil {
		switch {
		case d.ToolBatch.Errors >= 2:
			fire(+3, "tool errors escalating")
		case d.ToolBatch.Errors == 1:
			fire(+1, "one tool error")
		}
	}
	if anyKeyword(d.Task, heavyKeywords) {
		fire(+2, "task reads heavy")
	}
	if d.NTools >= 12 {
		fire(+1, "wide tool set")
	}
	if anyKeyword(d.Task, lightKeywords) {
		fire(-2, "task reads light")
	}
	if len(d.Task) <= 80 && !strings.Contains(d.Task, "```") && d.NMessages <= 2 {
		fire(-2, "short single-turn task")
	}
	if d.Step == StepToolStep && readNavTools[d.Tool] {
		fire(-1, "single navigation tool")
	}

	evidence := len(reasons)
	// No signal fired, or a step the classifier does not understand: the
	// thin-evidence fallback is standard, regardless of score.
	if evidence == 0 || d.Step == StepOther {
		return TierStandard, evidence, reasons
	}
	switch {
	case score >= heavyThreshold:
		return TierHeavy, evidence, reasons
	case score <= lightThreshold:
		// A thinking budget means the client asked for deliberation; the
		// call may be cheap to serve but never light.
		if d.ThinkingBudget > 0 {
			return TierStandard, evidence, reasons
		}
		return TierLight, evidence, reasons
	}
	return TierStandard, evidence, reasons
}

// anyKeyword reports whether any fragment occurs in the lowercased text.
func anyKeyword(text string, frags []string) bool {
	low := strings.ToLower(text)
	for _, f := range frags {
		if strings.Contains(low, f) {
			return true
		}
	}
	return false
}

// bandOf returns the candidates whose tier matches exactly, in input order.
func bandOf(cands []Candidate, tier string) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Tier == tier {
			out = append(out, c)
		}
	}
	return out
}

// wideningOrder lists the tiers to try for a classified call, most
// preferred first. Heavy degrades standard then light; light upgrades only
// to standard (never to heavy); standard prefers to upgrade to heavy before
// downgrading to light. An unknown tier string falls back to standard's
// order, which never matches it.
func wideningOrder(tier string) []string {
	switch tier {
	case TierHeavy:
		return []string{TierHeavy, TierStandard, TierLight}
	case TierLight:
		return []string{TierLight, TierStandard}
	default:
		return []string{TierStandard, TierHeavy, TierLight}
	}
}

// band returns the candidates of the classified tier, widening to adjacent
// tiers only when that band is empty. effective names the tier the result
// belongs to and widened reports whether that is not the classified tier.
// The result is always a subset of the input slice, in its order: widening
// can never invent or reorder a candidate.
func band(cands []Candidate, tier string) (out []Candidate, effective string, widened bool) {
	for _, t := range wideningOrder(tier) {
		if b := bandOf(cands, t); len(b) > 0 {
			return b, t, t != tier
		}
	}
	return nil, tier, false
}
