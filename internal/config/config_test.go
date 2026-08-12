package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pedro-mueller/conduit/internal/config"
)

func TestMapModelDefault(t *testing.T) {
	cfg := config.Default()
	m, ok := cfg.MapModel("claude-opus-5")
	if !ok || m != "glm-5.2" {
		t.Fatalf("got %q ok=%v", m, ok)
	}
	m, ok = cfg.MapModel("totally-unknown-model")
	if !ok || m != "glm-5.2" {
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
