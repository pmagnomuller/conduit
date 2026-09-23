package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pedro-mueller/conduit/internal/config"
)

func TestMapModelDefault(t *testing.T) {
	cfg := config.Default()
	m, ok := cfg.MapModel("claude-opus-5")
	if !ok || m != "glm-5.3" {
		t.Fatalf("got %q ok=%v", m, ok)
	}
	m, ok = cfg.MapModel("totally-unknown-model")
	if !ok || m != "glm-5.3" {
		t.Fatalf("default fallback got %q ok=%v", m, ok)
	}
}

func TestLoadRequiresAPIKey(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8787\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error when ZAI_API_KEY unset")
	}
}

func TestLoadFromDotEnv(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8787\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("ZAI_API_KEY=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ZAIAPIKey != "from-dotenv" {
		t.Fatalf("ZAIAPIKey=%q", cfg.ZAIAPIKey)
	}
}

func TestLoadOK(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "k")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8799\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8799" {
		t.Fatalf("listen=%s", cfg.Listen)
	}
}

func TestMapModelDeepSeekDefault(t *testing.T) {
	cfg := config.Default()
	m, ok := cfg.MapModelDeepSeek("claude-opus-5")
	if !ok || m != "deepseek-v4-flash" {
		t.Fatalf("got %q ok=%v", m, ok)
	}
}

func TestMapModelLongestPrefixDeterministic(t *testing.T) {
	cfg := config.Default()
	cfg.GLM.ModelMap = map[string]string{
		"claude-sonnet-5":          "glm-5.3",
		"claude-sonnet-5-20251001": "glm-5.3-flash",
	}
	for i := 0; i < 100; i++ {
		m, ok := cfg.MapModel("claude-sonnet-5-20251001-extra")
		if !ok || m != "glm-5.3-flash" {
			t.Fatalf("iter %d: got %q ok=%v, want glm-5.3-flash", i, m, ok)
		}
	}
}

func TestLoadDeepSeekKeyOptional(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "k")
	t.Setenv("DEEPSEEK_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8787\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeepSeekAPIKey != "" {
		t.Fatalf("DeepSeekAPIKey=%q, want empty", cfg.DeepSeekAPIKey)
	}

	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("DEEPSEEK_API_KEY=ds-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeepSeekAPIKey != "ds-dotenv" {
		t.Fatalf("DeepSeekAPIKey=%q", cfg.DeepSeekAPIKey)
	}
}

func TestRejectNonLoopback(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "k")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8787\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected non-loopback rejection")
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJevDefaultsAndMissingKey(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "k")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("CONDUIT_JEV_TIMEOUT_MS", "")
	cfg, err := config.Load(writeCfg(t, "listen = \"127.0.0.1:8787\"\n"))
	if err != nil {
		t.Fatalf("missing TYPESAFE_API_KEY must not error: %v", err)
	}
	if cfg.TypeSafeAPIKey != "" {
		t.Fatalf("TypeSafeAPIKey=%q", cfg.TypeSafeAPIKey)
	}
	j := cfg.Jev
	if j.BaseURL != "https://api.typesafe.ai/v1/systemone" || j.APIKeyEnv != "TYPESAFE_API_KEY" {
		t.Fatalf("jev=%+v", j)
	}
	if j.TimeoutMS != 4000 || j.LeaseTTLSeconds != 600 {
		t.Fatalf("jev=%+v", j)
	}
	if !strings.HasSuffix(j.DecisionsPath, "/.local/state/conduit/decisions.jsonl") || strings.HasPrefix(j.DecisionsPath, "~") {
		t.Fatalf("decisions_path=%q", j.DecisionsPath)
	}
	// Capability-first policy: the strongest model leads the list and every
	// provider the gateway can reach is represented. Asserting the shape rather
	// than fixed indices keeps this honest when the catalog is retuned.
	if len(j.Catalog) != 9 {
		t.Fatalf("catalog has %d entries: %+v", len(j.Catalog), j.Catalog)
	}
	if j.Catalog[0].Key() != "anthropic/claude-fable-5-1" {
		t.Fatalf("first catalog entry = %q, want the strongest anthropic model", j.Catalog[0].Key())
	}
	providers := map[string]bool{}
	for _, c := range j.Catalog {
		providers[c.Provider] = true
		if c.Profile == "" {
			t.Fatalf("empty profile for %s", c.Key())
		}
	}
	for _, p := range []string{"anthropic", "glm", "deepseek"} {
		if !providers[p] {
			t.Fatalf("catalog has no %s candidate: %+v", p, j.Catalog)
		}
	}
}

func TestJevTOMLCatalogOverrideAndKey(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "k")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("CONDUIT_JEV_TIMEOUT_MS", "")
	path := writeCfg(t, `listen = "127.0.0.1:8787"

[jev]
timeout_ms = 1500
lease_ttl_seconds = 30
decisions_path = ""

[[jev.catalog]]
provider = "glm"
model = "glm-5.3"
profile = "bounded"

[[jev.catalog]]
provider = "anthropic"
model = "claude-sonnet-5"
profile = "hard"
`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".env"), []byte("TYPESAFE_API_KEY=ts-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TypeSafeAPIKey != "ts-dotenv" {
		t.Fatalf("TypeSafeAPIKey=%q", cfg.TypeSafeAPIKey)
	}
	j := cfg.Jev
	if j.TimeoutMS != 1500 || j.LeaseTTLSeconds != 30 || j.DecisionsPath != "" {
		t.Fatalf("jev=%+v", j)
	}
	if len(j.Catalog) != 2 || j.Catalog[0].Key() != "glm/glm-5.3" || j.Catalog[1].Profile != "hard" {
		t.Fatalf("catalog=%+v", j.Catalog)
	}
	// Explicit env beats .env; env timeout override applies.
	t.Setenv("TYPESAFE_API_KEY", "ts-env")
	t.Setenv("CONDUIT_JEV_TIMEOUT_MS", "250")
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TypeSafeAPIKey != "ts-env" || cfg.Jev.TimeoutMS != 250 {
		t.Fatalf("key=%q timeout=%d", cfg.TypeSafeAPIKey, cfg.Jev.TimeoutMS)
	}
}

func TestCandidateKey(t *testing.T) {
	c := config.Candidate{Provider: "glm", Model: "glm-5.3-flash"}
	if c.Key() != "glm/glm-5.3-flash" {
		t.Fatalf("key=%q", c.Key())
	}
}

func TestEnvFileValuesReachApplyEnv(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "k")
	t.Setenv("CONDUIT_JEV_TIMEOUT_MS", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8787\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("CONDUIT_JEV_TIMEOUT_MS=1500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jev.TimeoutMS != 1500 {
		t.Fatalf("CONDUIT_JEV_TIMEOUT_MS from .env ignored: timeout=%d", cfg.Jev.TimeoutMS)
	}

	// A real environment value still wins over the file.
	t.Setenv("CONDUIT_JEV_TIMEOUT_MS", "2500")
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jev.TimeoutMS != 2500 {
		t.Fatalf("env must beat .env: timeout=%d", cfg.Jev.TimeoutMS)
	}
}
