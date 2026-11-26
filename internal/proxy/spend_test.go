package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pedro-mueller/conduit/internal/usage"
)

// readLedger returns the ledger once it holds want records. The record is
// appended when the handler finishes copying, which can be just after the
// client has read the whole body, so the read polls briefly.
func readLedger(t *testing.T, dir string, want int) []usage.Record {
	t.Helper()
	var out []usage.Record
	deadline := time.Now().Add(2 * time.Second)
	for {
		out = out[:0]
		raw, _ := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var r usage.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("ledger line %q: %v", line, err)
			}
			out = append(out, r)
		}
		if len(out) >= want || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(out) != want {
		t.Fatalf("ledger: want %d records, got %+v", want, out)
	}
	return out
}

const promptCanary = "PROMPT-CANARY-do-not-store-this-text"

// Anthropic subscription traffic, streamed: tokens recorded, USD 0, billing
// subscription; the stream reaches the client byte-identical.
func TestSpendRecordsAnthropicStream(t *testing.T) {
	stream := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s1\",\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":2200,\"cache_creation_input_tokens\":33,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":44}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	f := &fakeUpstreams{withSpend: true}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Odd-sized writes so events straddle reads.
		for i := 0; i < len(stream); i += 13 {
			_, _ = io.WriteString(w, stream[i:min(i+13, len(stream))])
			w.(http.Flusher).Flush()
		}
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { t.Fatal("glm") }
	srv, _, dir := newGateway(t, f)

	body := []byte(`{"model":"claude-opus-5","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"` + promptCanary + `"}]}`)
	res := post(t, srv.URL+"/v1/messages", body, fakeToken)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(got) != stream {
		t.Fatalf("stream altered:\n%s", got)
	}
	r := readLedger(t, dir, 1)[0]
	if r.Provider != "anthropic" || r.Billing != usage.BillingSubscription || r.USD != 0 ||
		r.InputTokens != 11 || r.OutputTokens != 44 || r.CacheReadTokens != 2200 || r.CacheWriteToken != 33 ||
		r.RequestedModel != "claude-opus-5" || r.UpstreamModel != "claude-opus-5" || r.RequestID != "msg_s1" {
		t.Fatalf("record: %+v", r)
	}
}

// Quota failover lands on DeepSeek (GLM fails): the record names the tier that
// actually answered, is priced, and carries the failover flag. The chat notice
// injected into the reply does not disturb the accounting.
func TestSpendRecordsFailoverToDeepSeekPriced(t *testing.T) {
	f := &fakeUpstreams{withSpend: true, enableDeepSeek: true}
	f.anthropic = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"exceeded"}}`))
	}
	f.glm = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
	f.deepseek = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ds_1","type":"message","model":"deepseek-v4-flash","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1000000,"output_tokens":0}}`))
	}
	srv, _, dir := newGateway(t, f)
	res := post(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-opus-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`), fakeToken)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.Header.Get("X-Conduit-Provider") != "deepseek" {
		t.Fatalf("provider %q body %s", res.Header.Get("X-Conduit-Provider"), got)
	}
	r := readLedger(t, dir, 1)[0]
	// 1M input on deepseek-v4-flash at the built-in 0.30/MTok.
	if r.Provider != "deepseek" || r.Billing != usage.BillingMetered || r.USD != 0.30 || !r.Failover ||
		r.RequestedModel != "claude-opus-5" || r.UpstreamModel != "deepseek-v4-flash" {
		t.Fatalf("record: %+v", r)
	}
	// Error responses (the 429 and the 503) are never recorded.
	sp := getSpend(t, srv.URL)
	if sp.TodayPaid != 0.30 || sp.DeepSeekRequests != 1 || sp.DeepSeekFailovers != 1 || sp.Serving == nil || sp.Serving.Provider != "deepseek" {
		t.Fatalf("spend: %+v", sp)
	}
}

func getSpend(t *testing.T, base string) usage.Spend {
	t.Helper()
	res, err := http.Get(base + "/_gateway/spend")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("spend status %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var sp usage.Spend
	if err := json.NewDecoder(res.Body).Decode(&sp); err != nil {
		t.Fatal(err)
	}
	return sp
}

// Neither the prompt nor any credential — inbound OAuth token, provider keys —
// may reach usage.jsonl, even when the upstream echoes them back.
func TestSpendLedgerHoldsNoCredentialOrPrompt(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		reply, _ := json.Marshal(map[string]any{
			"id":      "msg_echo",
			"model":   "glm-5.3",
			"content": []map[string]string{{"type": "text", "text": string(b) + r.Header.Get("Authorization")}},
			"usage":   map[string]int{"input_tokens": 5, "output_tokens": 6},
		})
		_, _ = w.Write(reply)
	}
	f := &fakeUpstreams{withSpend: true, enableDeepSeek: true}
	f.anthropic = echo
	f.glm = echo
	f.deepseek = echo
	srv, br, dir := newGateway(t, f)
	body := []byte(`{"model":"claude-opus-5","max_tokens":10,"system":"` + promptCanary + `","messages":[{"role":"user","content":"` + promptCanary + `"}]}`)
	for _, pin := range []string{"anthropic", "glm", "deepseek"} {
		br.SetForce(pin, "")
		res := post(t, srv.URL+"/v1/messages", body, fakeToken)
		got, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("X-Conduit-Provider") != pin {
			t.Fatalf("pin %s: status %d provider %q: %s", pin, res.StatusCode, res.Header.Get("X-Conduit-Provider"), got)
		}
	}
	recs := readLedger(t, dir, 3)
	raw, err := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{fakeToken, "zai-test-key-secret", "deepseek-test-key-secret", promptCanary, "Bearer"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("usage.jsonl leaks %q:\n%s", needle, raw)
		}
	}
	for _, r := range recs {
		// The conversation id is an opaque hash, never prompt text.
		if r.RequestID != "msg_echo" {
			t.Fatalf("request id: %+v", r)
		}
	}
}

func TestSpendEndpoint(t *testing.T) {
	// Without a spend service the endpoint still answers, empty.
	srv, _, _ := newGateway(t, &fakeUpstreams{anthropic: func(http.ResponseWriter, *http.Request) {}, glm: func(http.ResponseWriter, *http.Request) {}})
	if sp := getSpend(t, srv.URL); sp.TodayPaid != 0 || sp.Days == nil || sp.Recent == nil {
		t.Fatalf("empty spend: %+v", sp)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/_gateway/spend", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", res.StatusCode)
	}
	// The control page carries the spend tab.
	ui, err := http.Get(srv.URL + "/_gateway/ui")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(ui.Body)
	ui.Body.Close()
	for _, want := range []string{`id="view-spend"`, `/_gateway/spend`, `refreshSpend, 10000`} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("ui lacks %q", want)
		}
	}
}
