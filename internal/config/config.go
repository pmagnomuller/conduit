package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Listen    string          `toml:"listen"`
	Anthropic AnthropicConfig `toml:"anthropic"`
	GLM       GLMConfig       `toml:"glm"`
	DeepSeek  DeepSeekConfig  `toml:"deepseek"`
	Breaker   BreakerConfig   `toml:"breaker"`
	Log       LogConfig       `toml:"log"`
	Paths     PathsConfig     `toml:"paths"`
	Jev       JevConfig       `toml:"jev"`

	// Resolved at load time.
	ZAIAPIKey      string `toml:"-"`
	DeepSeekAPIKey string `toml:"-"`
	// TypeSafeAPIKey enables Jev routing mode. Optional: when empty the
	// router reports Enabled()==false and the gateway stays in auto/pinned.
	TypeSafeAPIKey string `toml:"-"`

	// Local-token routing: inbound requests whose credential matches
	// LocalToken bypass Anthropic and go straight to LocalTokenProvider.
	// Lets token-less clients (e.g. OpenCode) share the gateway while
	// Claude Code OAuth traffic keeps the automatic breaker chain.
	LocalToken         string `toml:"-"`
	LocalTokenProvider string `toml:"-"`
}

type AnthropicConfig struct {
	BaseURL             string `toml:"base_url"`
	MaxTransientRetries int    `toml:"max_transient_retries"`
}

type GLMConfig struct {
	BaseURL      string            `toml:"base_url"`
	APIKeyEnv    string            `toml:"api_key_env"`
	DefaultModel string            `toml:"default_model"`
	ModelMap     map[string]string `toml:"model_map"`
}

// DeepSeekConfig is the terminal failover tier behind GLM. Anthropic-compatible
// endpoint (https://api.deepseek.com/anthropic). The tier is enabled only when
// the resolved API key is non-empty; otherwise it is silently skipped.
type DeepSeekConfig struct {
	BaseURL      string            `toml:"base_url"`
	APIKeyEnv    string            `toml:"api_key_env"`
	DefaultModel string            `toml:"default_model"`
	ModelMap     map[string]string `toml:"model_map"`
}

type BreakerConfig struct {
	FallbackOpenSeconds  int     `toml:"fallback_open_seconds"`
	ProactiveThreshold   int     `toml:"proactive_threshold"`
	ProactiveUtilization float64 `toml:"proactive_utilization"`
	ProbeOnExpiry        bool    `toml:"probe_on_expiry"`
}

type LogConfig struct {
	Level                 string `toml:"level"`
	CaptureUpstreamErrors bool   `toml:"capture_upstream_errors"`
	CapturePath           string `toml:"capture_path"`
}

type PathsConfig struct {
	StatePath string `toml:"state_path"`
}

// JevConfig drives per-call model routing via Jev (TypeSafe System One).
// Catalog entries are the candidates Jev may choose between; when empty the
// built-in DefaultCatalog is used.
type JevConfig struct {
	BaseURL         string      `toml:"base_url"`
	APIKeyEnv       string      `toml:"api_key_env"`
	TimeoutMS       int         `toml:"timeout_ms"`
	LeaseTTLSeconds int         `toml:"lease_ttl_seconds"`
	DecisionsPath   string      `toml:"decisions_path"` // "" disables the JSONL decision log
	Catalog         []Candidate `toml:"catalog"`
}

// Candidate is one provider/model Jev may pick. It lives here (not in
// internal/route) so route can import config without a cycle; route aliases it.
type Candidate struct {
	Provider string `json:"provider" toml:"provider"` // anthropic|glm|deepseek
	Model    string `json:"model"    toml:"model"`
	Profile  string `json:"profile"  toml:"profile"` // capability/cost prior sent to Jev as criteria text
}

// Key is the catalog key sent to Jev, e.g. "anthropic/claude-opus-5".
func (c Candidate) Key() string { return c.Provider + "/" + c.Model }

// DefaultCatalog returns the built-in candidate set. Profiles are terse
// priors: Jev sees them as criteria text and picks the cheapest sufficient one.
func DefaultCatalog() []Candidate {
	return []Candidate{
		{Provider: "anthropic", Model: "claude-opus-5", Profile: "Hardest work: architecture, security/concurrency review, ambiguous broad tasks, multi-file debugging. Most expensive."},
		{Provider: "anthropic", Model: "claude-sonnet-5", Profile: "Complex implementation, inferring intent, cross-file refactors, robust tests. Expensive."},
		{Provider: "anthropic", Model: "claude-haiku-4-5", Profile: "Trivial work: titles, summaries, a single mechanical edit with a known target. Cheap on-plan."},
		{Provider: "glm", Model: "glm-5.3", Profile: "Bounded implementation with clear requirements and known patterns. Cheap, off-plan."},
		{Provider: "glm", Model: "glm-5.3-flash", Profile: "Mechanical follow-through, formatting, simple tool continuations. Cheapest."},
		{Provider: "deepseek", Model: "deepseek-v4-flash", Profile: "Bounded implementation; cheap alternative to glm-5.3. Only available when DEEPSEEK_API_KEY is set."},
	}
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8787",
		Anthropic: AnthropicConfig{
			BaseURL:             "https://api.anthropic.com",
			MaxTransientRetries: 2,
		},
		GLM: GLMConfig{
			BaseURL:      "https://api.z.ai/api/anthropic",
			APIKeyEnv:    "ZAI_API_KEY",
			DefaultModel: "glm-5.3",
			ModelMap: map[string]string{
				"claude-opus-5":             "glm-5.3",
				"claude-sonnet-5":           "glm-5.3",
				"claude-haiku-4-5":          "glm-5.3-flash",
				"claude-opus-4-6":           "glm-5.3",
				"claude-sonnet-4-6":         "glm-5.3",
				"claude-haiku-4-5-20251001": "glm-5.3-flash",
			},
		},
		DeepSeek: DeepSeekConfig{
			BaseURL:      "https://api.deepseek.com/anthropic",
			APIKeyEnv:    "DEEPSEEK_API_KEY",
			DefaultModel: "deepseek-v4-flash",
			ModelMap:     map[string]string{},
		},
		Breaker: BreakerConfig{
			FallbackOpenSeconds:  300,
			ProactiveThreshold:   0,
			ProactiveUtilization: 0,
			ProbeOnExpiry:        true,
		},
		Log: LogConfig{
			Level:                 "info",
			CaptureUpstreamErrors: true,
			CapturePath:           "~/.local/state/conduit/upstream-errors.jsonl",
		},
		Jev: JevConfig{
			BaseURL:         "https://api.typesafe.ai/v1/systemone",
			APIKeyEnv:       "TYPESAFE_API_KEY",
			TimeoutMS:       4000,
			LeaseTTLSeconds: 600,
			DecisionsPath:   "~/.local/state/conduit/decisions.jsonl",
			// Catalog left nil so a TOML [[jev.catalog]] replaces rather than
			// appends; Load fills DefaultCatalog when still empty.
		},
	}
}

func DefaultPath() string {
	if p := os.Getenv("CLAUDE_GLM_GATEWAY_CONFIG"); p != "" {
		return ExpandHome(p)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "conduit", "config.toml")
	}
	return ExpandHome("~/.config/conduit/config.toml")
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	path = ExpandHome(path)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file is optional; defaults + env are enough.
		} else {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
	} else {
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	// .env first: applyEnv reads the process environment, so env-file values
	// (CONDUIT_JEV_TIMEOUT_MS and friends) have to be in it by then. Loading
	// earlier is safe for the key resolution below — loadDotEnvFile never
	// overrides a variable that is already exported, so a real env value still
	// wins over the file.
	loadDotEnvFile(filepath.Join(filepath.Dir(path), ".env"))
	applyEnv(&cfg)

	if cfg.GLM.ModelMap == nil {
		cfg.GLM.ModelMap = map[string]string{}
	}
	if cfg.DeepSeek.ModelMap == nil {
		cfg.DeepSeek.ModelMap = map[string]string{}
	}

	keyEnv := cfg.GLM.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "ZAI_API_KEY"
	}
	cfg.ZAIAPIKey = os.Getenv(keyEnv)
	if cfg.ZAIAPIKey == "" {
		return Config{}, fmt.Errorf("%s is unset — put it in %s/.env or export it before starting the gateway", keyEnv, filepath.Dir(path))
	}

	dsKeyEnv := cfg.DeepSeek.APIKeyEnv
	if dsKeyEnv == "" {
		dsKeyEnv = "DEEPSEEK_API_KEY"
	}
	cfg.DeepSeekAPIKey = os.Getenv(dsKeyEnv)

	// Jev key is optional: absence just disables jev mode.
	tsKeyEnv := cfg.Jev.APIKeyEnv
	if tsKeyEnv == "" {
		tsKeyEnv = "TYPESAFE_API_KEY"
	}
	cfg.TypeSafeAPIKey = os.Getenv(tsKeyEnv)
	if len(cfg.Jev.Catalog) == 0 {
		cfg.Jev.Catalog = DefaultCatalog()
	}
	if cfg.Jev.TimeoutMS <= 0 {
		cfg.Jev.TimeoutMS = 4000
	}
	if cfg.Jev.LeaseTTLSeconds <= 0 {
		cfg.Jev.LeaseTTLSeconds = 600
	}
	if cfg.Jev.BaseURL == "" {
		cfg.Jev.BaseURL = "https://api.typesafe.ai/v1/systemone"
	}
	cfg.Jev.DecisionsPath = ExpandHome(cfg.Jev.DecisionsPath)

	cfg.LocalToken = os.Getenv("CONDUIT_LOCAL_TOKEN")
	if cfg.LocalToken == "" {
		cfg.LocalToken = "conduit-local"
	}
	cfg.LocalTokenProvider = os.Getenv("CONDUIT_LOCAL_PROVIDER")
	switch cfg.LocalTokenProvider {
	case "", "glm":
		cfg.LocalTokenProvider = "glm"
	case "deepseek", "anthropic":
	default:
		return Config{}, fmt.Errorf("CONDUIT_LOCAL_PROVIDER must be glm, deepseek, or anthropic (got %q)", cfg.LocalTokenProvider)
	}

	cfg.Log.CapturePath = ExpandHome(cfg.Log.CapturePath)
	cfg.Paths.StatePath = ExpandHome(cfg.Paths.StatePath)
	if cfg.Paths.StatePath == "" {
		cfg.Paths.StatePath = DefaultStatePath()
	}

	if cfg.Anthropic.MaxTransientRetries < 0 {
		cfg.Anthropic.MaxTransientRetries = 0
	}
	if cfg.Breaker.FallbackOpenSeconds <= 0 {
		cfg.Breaker.FallbackOpenSeconds = 300
	}
	if !strings.HasPrefix(cfg.Listen, "127.0.0.1") && !strings.HasPrefix(cfg.Listen, "localhost") {
		return Config{}, fmt.Errorf("listen must bind to loopback only (got %q)", cfg.Listen)
	}

	return cfg, nil
}

// LoadForTest loads config without requiring ZAI_API_KEY (injects a placeholder).
func LoadForTest(path string) (Config, error) {
	if os.Getenv("ZAI_API_KEY") == "" {
		_ = os.Setenv("ZAI_API_KEY", "test-zai-key-not-real")
	}
	if os.Getenv("DEEPSEEK_API_KEY") == "" {
		_ = os.Setenv("DEEPSEEK_API_KEY", "test-deepseek-key-not-real")
	}
	return Load(path)
}

func DefaultStatePath() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "conduit", "state.json")
	}
	return ExpandHome("~/.local/state/conduit/state.json")
}

func ExpandHome(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

// loadDotEnvFile sets KEY=VALUE pairs from a local .env without overriding
// variables already present in the process environment.
func loadDotEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if os.Getenv(key) != "" {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		_ = os.Setenv(key, val)
	}
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_ANTHROPIC_BASE_URL"); v != "" {
		cfg.Anthropic.BaseURL = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_GLM_BASE_URL"); v != "" {
		cfg.GLM.BaseURL = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_GLM_DEFAULT_MODEL"); v != "" {
		cfg.GLM.DefaultModel = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_FALLBACK_OPEN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Breaker.FallbackOpenSeconds = n
		}
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_PROACTIVE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Breaker.ProactiveThreshold = n
		}
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_CAPTURE_PATH"); v != "" {
		cfg.Log.CapturePath = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_STATE_PATH"); v != "" {
		cfg.Paths.StatePath = v
	}
	if v := os.Getenv("CLAUDE_GLM_GATEWAY_MAX_TRANSIENT_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Anthropic.MaxTransientRetries = n
		}
	}
	if v := os.Getenv("CONDUIT_JEV_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Jev.TimeoutMS = n
		}
	}
}

func (c Config) FallbackOpenDuration() time.Duration {
	return time.Duration(c.Breaker.FallbackOpenSeconds) * time.Second
}

// MapModel returns the GLM model ID for an Anthropic model ID.
func (c Config) MapModel(anthropicModel string) (string, bool) {
	return mapModel(c.GLM.DefaultModel, c.GLM.ModelMap, anthropicModel)
}

// MapModelDeepSeek returns the DeepSeek model ID for an Anthropic model ID.
func (c Config) MapModelDeepSeek(anthropicModel string) (string, bool) {
	return mapModel(c.DeepSeek.DefaultModel, c.DeepSeek.ModelMap, anthropicModel)
}

func mapModel(defaultModel string, mm map[string]string, anthropicModel string) (string, bool) {
	if m, ok := mm[anthropicModel]; ok && m != "" {
		return m, true
	}
	// Prefix / fuzzy: if exact miss, try longest prefix match on known keys.
	// Map iteration order is random, so scan all candidates and keep the
	// longest matching key rather than returning the first hit.
	bestKey, bestVal := "", ""
	for k, v := range mm {
		if v == "" || !strings.HasPrefix(anthropicModel, k) {
			continue
		}
		if len(k) > len(bestKey) {
			bestKey, bestVal = k, v
		}
	}
	if bestKey != "" {
		return bestVal, true
	}
	if defaultModel != "" {
		return defaultModel, true
	}
	return "", false
}
