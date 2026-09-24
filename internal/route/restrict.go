package route

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
)

// restriction keeps turns that reference secret material (.env, SSH keys,
// cloud credentials, ...) away from providers we do not trust with them. It
// is enforced here, before Jev sees the candidates: a Jev answer is
// untrusted and cannot route around it.
type restriction struct {
	patterns  []*regexp.Regexp
	providers []string
}

// newRestriction compiles patterns; config.Load has already validated them,
// so a bad one here (hand-built config) is logged and skipped.
func newRestriction(patterns, providers []string, log *slog.Logger) *restriction {
	r := &restriction{providers: slices.Clone(providers)}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			log.Warn("jev restricted pattern invalid; skipped", "pattern", p, "err", err)
			continue
		}
		r.patterns = append(r.patterns, re)
	}
	return r
}

// matches scans raw message content (tool_use inputs, tool results, text).
// Raw JSON is scanned as-is: paths survive encoding and nothing is decoded.
func (r *restriction) matches(recent []json.RawMessage) bool {
	for _, raw := range recent {
		for _, re := range r.patterns {
			if re.Match(raw) {
				return true
			}
		}
	}
	return false
}

func (r *restriction) filter(cands []Candidate) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if !slices.Contains(r.providers, c.Provider) {
			out = append(out, c)
		}
	}
	return out
}

func policyOf(d Dossier) string {
	if d.Sensitive {
		return PolicyRestricted
	}
	return ""
}
