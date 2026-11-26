// Package usage turns each upstream response's per-request token usage into a
// cost and appends it to the local usage ledger (usage.jsonl). It is the data
// behind the spend tab, GET /_gateway/spend, and `conduitctl spend`.
//
// What is recorded is deliberately narrow: time, provider, requested and
// upstream model, token counts, USD, billing class, the failover flag, status
// and an opaque request id. Never the prompt, the response text, or any
// credential — see Record and Writer.Append.
package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Billing distinguishes traffic the operator pays for per token from traffic
// covered by a subscription.
const (
	// BillingMetered is a pay-as-you-go API-key call: USD is what it cost.
	BillingMetered = "metered"
	// BillingSubscription is plan traffic (Claude Code OAuth, a coding-plan
	// key). Tokens are recorded so volume stays visible; USD is 0 because the
	// call is not billed per token.
	BillingSubscription = "subscription"
)

// Usage is the token usage of one upstream response, in the field names every
// Anthropic-compatible provider uses. As returned by Parse it is a raw view of
// the response: no defaults are applied, so a response that carried no usage
// at all stays identifiably empty.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// Empty reports whether the response carried no token counts at all.
func (u Usage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheCreationTokens == 0
}

// Record is one line of usage.jsonl.
type Record struct {
	At              time.Time `json:"at"`
	Provider        string    `json:"provider"`
	RequestedModel  string    `json:"requested_model"`
	UpstreamModel   string    `json:"upstream_model"`
	InputTokens     int64     `json:"in"`
	OutputTokens    int64     `json:"out"`
	CacheReadTokens int64     `json:"cache_read"`
	CacheWriteToken int64     `json:"cache_write"`
	USD             float64   `json:"usd"`
	Billing         string    `json:"billing"`
	Failover        bool      `json:"failover"`
	RequestID       string    `json:"request_id,omitempty"`
	// ModelSource says where UpstreamModel came from: "response" (the provider
	// named the model it served) or "sent" (the response was silent, so this is
	// the id conduit sent — which may be a retired alias).
	ModelSource string `json:"model_source,omitempty"`
	// Estimated is true when the price table had no entry for the model and
	// the call was costed at the provider's most expensive known rate.
	Estimated bool `json:"estimated,omitempty"`
}

// ConversationID derives an opaque request id from a request body: a hash of
// the first system and first user text, so every turn of one conversation
// shares an id without any prompt text being stored. Empty when the body
// carries no such text.
func ConversationID(body []byte) string {
	key := conversationKey(body)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func conversationKey(body []byte) string {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	var firstUser string
	for _, m := range req.Messages {
		if m.Role == "user" {
			firstUser = firstText(m.Content)
			break
		}
	}
	sys := firstText(req.System)
	if sys == "" && firstUser == "" {
		return ""
	}
	return clip(sys, 2000) + "\x00" + clip(firstUser, 2000)
}

// firstText returns the text of a content value that is either a string or a
// list of blocks, taking the first text block.
func firstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
