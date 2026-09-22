// Package route implements per-call model routing backed by Jev (TypeSafe
// System One). The gateway asks Jev to pick the cheapest sufficient
// provider/model from a catalog, using a bounded dossier of the inbound
// request; any failure falls open so the caller keeps today's auto behaviour.
package route

import "github.com/pedro-mueller/conduit/internal/config"

// Mode is the gateway's routing strategy.
type Mode string

const (
	ModeAuto   Mode = "auto"   // Anthropic; breaker OPEN → GLM → DeepSeek
	ModePinned Mode = "pinned" // forced provider/model
	ModeJev    Mode = "jev"    // per-call Jev decision, fail-open to auto
)

// ParseMode accepts the wire form; "" means auto so legacy state files load.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case "", ModeAuto:
		return ModeAuto, true
	case ModePinned:
		return ModePinned, true
	case ModeJev:
		return ModeJev, true
	}
	return "", false
}

// Candidate lives in config so config can hold the catalog without importing
// this package (route imports config for JevConfig).
type Candidate = config.Candidate
