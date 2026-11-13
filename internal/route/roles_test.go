package route

import "testing"

func TestPickRoleSkipsAnOpenProvider(t *testing.T) {
	roles := map[string][]string{
		"scout": {"anthropic/claude-haiku-4-5", "glm/glm-5.3-flash"},
	}
	provider, model, ok := PickRole(roles, "scout", func(provider, model string) bool {
		return provider == "anthropic"
	})
	if !ok || provider != "glm" || model != "glm-5.3-flash" {
		t.Fatalf("got %s %s %v", provider, model, ok)
	}
	if _, _, ok := PickRole(roles, "missing", nil); ok {
		t.Fatal("unknown role should not resolve")
	}
}
