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
	Listen    string         `toml:"listen"`
	Anthropic AnthropicConfig `toml:"anthropic"`
	GLM       GLMConfig      `toml:"glm"`
	DeepSeek  DeepSeekConfig `toml:"deepseek"`
	Breaker   BreakerConfig  `toml:"breaker"`
	Log       LogConfig      `toml:"log"`
	Paths     PathsConfig    `toml:"paths"`

	// Resolved at load time.
	ZAIAPIKey      string `toml:"-"`
	DeepSeekAPIKey string `toml:"-"`

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
				"claude-opus-5":              "glm-5.3",
				"claude-sonnet-5":            "glm-5.3",
				"claude-haiku-4-5":           "glm-5.3-flash",
				"claude-opus-4-6":            "glm-5.3",
				"claude-sonnet-4-6":          "glm-5.3",
				"claude-haiku-4-5-20251001":  "glm-5.3-flash",
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

	applyEnv(&cfg)
	loadDotEnvFile(filepath.Join(filepath.Dir(path), ".env"))

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
	for k, v := range mm {
		if strings.HasPrefix(anthropicModel, k) && v != "" {
			return v, true
		}
	}
	if defaultModel != "" {
		return defaultModel, true
	}
	return "", false
}
