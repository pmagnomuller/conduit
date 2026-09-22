package metrics

import (
	"sync"
	"time"

	"github.com/pedro-mueller/conduit/internal/breaker"
)

type Counters struct {
	mu sync.Mutex

	AnthropicRequests int64     `json:"anthropic_requests"`
	GLMRequests       int64     `json:"glm_requests"`
	DeepSeekRequests  int64     `json:"deepseek_requests"`
	Failovers         int64     `json:"failovers"`
	TransientRetries  int64     `json:"transient_retries"`
	JevDecisions      int64     `json:"jev_decisions"`
	JevFailOpen       int64     `json:"jev_fail_open"`
	StartedAt         time.Time `json:"started_at"`
}

func New() *Counters {
	return &Counters{StartedAt: time.Now().UTC()}
}

func (c *Counters) IncAnthropic() {
	c.mu.Lock()
	c.AnthropicRequests++
	c.mu.Unlock()
}

func (c *Counters) IncGLM() {
	c.mu.Lock()
	c.GLMRequests++
	c.mu.Unlock()
}

func (c *Counters) IncDeepSeek() {
	c.mu.Lock()
	c.DeepSeekRequests++
	c.mu.Unlock()
}

func (c *Counters) IncFailover() {
	c.mu.Lock()
	c.Failovers++
	c.mu.Unlock()
}

func (c *Counters) IncTransientRetry() {
	c.mu.Lock()
	c.TransientRetries++
	c.mu.Unlock()
}

func (c *Counters) IncJevDecision() {
	c.mu.Lock()
	c.JevDecisions++
	c.mu.Unlock()
}

func (c *Counters) IncJevFailOpen() {
	c.mu.Lock()
	c.JevFailOpen++
	c.mu.Unlock()
}

func (c *Counters) Snapshot() Counters {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Counters{
		AnthropicRequests: c.AnthropicRequests,
		GLMRequests:       c.GLMRequests,
		DeepSeekRequests:  c.DeepSeekRequests,
		Failovers:         c.Failovers,
		TransientRetries:  c.TransientRetries,
		JevDecisions:      c.JevDecisions,
		JevFailOpen:       c.JevFailOpen,
		StartedAt:         c.StartedAt,
	}
}

type StatusResponse struct {
	Listen   string            `json:"listen"`
	Routing  string            `json:"routing"` // anthropic | glm | probe
	Breaker  breaker.Snapshot  `json:"breaker"`
	Counts   Counters          `json:"counts"`
	Upstream map[string]string `json:"upstream"`
	// LastRequest describes the most recent proxied request (provider +
	// rewritten upstream model). Nil until the first request lands.
	LastRequest map[string]string `json:"last_request,omitempty"`
	// Forced routing state set via /_gateway/route ("" = automatic).
	ForcedProvider string `json:"forced_provider,omitempty"`
	ForcedModel    string `json:"forced_model,omitempty"`
	// Routing mode (auto|pinned|jev) and whether the Jev router is usable.
	Mode       string `json:"mode"`
	JevEnabled bool   `json:"jev_enabled"`
}
