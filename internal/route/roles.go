package route

import "strings"

// PickRole walks a role's ordered "provider/model" chain and returns the
// first entry whose provider is not skipped. skip reports a provider and
// model the caller will not use, for example because its breaker is open.
func PickRole(roles map[string][]string, role string, skip func(provider, model string) bool) (provider, model string, ok bool) {
	chain := roles[role]
	if len(chain) == 0 {
		return "", "", false
	}
	for _, entry := range chain {
		prov, mod, split := strings.Cut(entry, "/")
		if !split || prov == "" || mod == "" {
			continue
		}
		if skip != nil && skip(prov, mod) {
			continue
		}
		return prov, mod, true
	}
	return "", "", false
}
