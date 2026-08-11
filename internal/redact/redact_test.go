package redact_test

import (
	"net/http"
	"testing"

	"github.com/pedro-mueller/claude-glm-gateway/internal/redact"
)

func TestHeadersRedact(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-secret")
	h.Set("X-Api-Key", "super-secret")
	h.Set("Content-Type", "application/json")
	out := redact.Headers(h)
	if out.Get("Authorization") != "[REDACTED]" {
		t.Fatalf("auth=%q", out.Get("Authorization"))
	}
	if out.Get("X-Api-Key") != "[REDACTED]" {
		t.Fatalf("key=%q", out.Get("X-Api-Key"))
	}
	if out.Get("Content-Type") != "application/json" {
		t.Fatal("content-type mutated")
	}
}

func TestStringRedact(t *testing.T) {
	s := redact.String(`Authorization: Bearer sk-ant-abc123xyz and sk-ant-abc123xyz`)
	if redact.ContainsCredential(s, "sk-ant-abc123xyz") {
		t.Fatalf("still present: %s", s)
	}
}
