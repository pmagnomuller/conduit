package usage

import (
	"strings"
	"testing"
)

// Synthetic fixtures in each provider's response shape. The token counts are
// arbitrary; what matters is which field each one lands in.

const anthropicJSON = `{"id":"msg_01abc","type":"message","role":"assistant","model":"claude-sonnet-5",
"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",
"usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":56}}`

const glmJSON = `{"id":"glm-msg-1","type":"message","role":"assistant","model":"glm-5.3",
"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":900,"output_tokens":120,"cache_read_input_tokens":64}}`

const deepseekJSON = `{"id":"ds-msg-1","type":"message","role":"assistant","model":"deepseek-v4-flash",
"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1500,"output_tokens":42,"cache_read_input_tokens":1024,"cache_creation_input_tokens":0}}`

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		name := e[strings.Index(e, `"type":"`)+8:]
		name = name[:strings.IndexByte(name, '"')]
		b.WriteString("event: " + name + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

var anthropicSSE = sse(
	`{"type":"message_start","message":{"id":"msg_01sse","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":8,"cache_creation_input_tokens":120,"cache_read_input_tokens":9000,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"secret-looking output text"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":321}}`,
	`{"type":"message_stop"}`,
)

// GLM repeats running totals in message_delta, input included.
var glmSSE = sse(
	`{"type":"message_start","message":{"id":"glm-sse-1","model":"glm-5.3-flash","usage":{"input_tokens":0,"output_tokens":0}}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":700,"output_tokens":90,"cache_read_input_tokens":32}}`,
	`{"type":"message_stop"}`,
)

var deepseekSSE = sse(
	`{"type":"message_start","message":{"id":"ds-sse-1","model":"deepseek-v4-pro","usage":{"input_tokens":2048,"output_tokens":0,"cache_read_input_tokens":1536}}}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":77}}`,
	`{"type":"message_stop"}`,
)

func TestParseJSONPerProvider(t *testing.T) {
	cases := []struct {
		name, body, model, id string
		want                  Usage
	}{
		{"anthropic", anthropicJSON, "claude-sonnet-5", "msg_01abc", Usage{12, 56, 4000, 300}},
		{"glm", glmJSON, "glm-5.3", "glm-msg-1", Usage{900, 120, 64, 0}},
		{"deepseek", deepseekJSON, "deepseek-v4-flash", "ds-msg-1", Usage{1500, 42, 1024, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseJSON([]byte(c.body))
			if got.Usage != c.want || got.Model != c.model || got.RequestID != c.id {
				t.Fatalf("got %+v, want usage %+v model %q id %q", got, c.want, c.model, c.id)
			}
		})
	}
}

func TestParseSSEPerProvider(t *testing.T) {
	cases := []struct {
		name, body, model, id string
		want                  Usage
	}{
		{"anthropic", anthropicSSE, "claude-opus-5-5", "msg_01sse", Usage{8, 321, 9000, 120}},
		{"glm running totals", glmSSE, "glm-5.3-flash", "glm-sse-1", Usage{700, 90, 32, 0}},
		{"deepseek", deepseekSSE, "deepseek-v4-pro", "ds-sse-1", Usage{2048, 77, 1536, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseSSE([]byte(c.body))
			if got.Usage != c.want || got.Model != c.model || got.RequestID != c.id {
				t.Fatalf("got %+v, want usage %+v model %q id %q", got, c.want, c.model, c.id)
			}
		})
	}
}

// A stream arrives in arbitrary chunks; the tap must reassemble lines across
// every possible split, including inside "data:" and inside the JSON, and
// cope with CRLF line ends.
func TestTapChunkBoundaries(t *testing.T) {
	body := strings.ReplaceAll(anthropicSSE, "\n", "\r\n")
	want := Usage{8, 321, 9000, 120}
	for size := 1; size <= 64; size++ {
		tap := NewTap(true)
		for i := 0; i < len(body); i += size {
			end := min(i+size, len(body))
			if n, err := tap.Write([]byte(body[i:end])); n != end-i || err != nil {
				t.Fatalf("Write returned %d, %v", n, err)
			}
		}
		if got := tap.Result(); got.Usage != want {
			t.Fatalf("chunk %d: usage %+v, want %+v", size, got.Usage, want)
		}
	}
	// JSON split the same way.
	tap := NewTap(false)
	for i := 0; i < len(anthropicJSON); i += 7 {
		_, _ = tap.Write([]byte(anthropicJSON[i:min(i+7, len(anthropicJSON))]))
	}
	if got := tap.Result(); got.Usage != (Usage{12, 56, 4000, 300}) {
		t.Fatalf("json chunks: %+v", got.Usage)
	}
}

// A stream cut before message_delta still yields the input side, and the last
// event without a trailing newline is still read.
func TestTapTruncatedStream(t *testing.T) {
	cut := anthropicSSE[:strings.Index(anthropicSSE, "event: message_delta")]
	got := ParseSSE([]byte(cut))
	if got.Usage != (Usage{8, 1, 9000, 120}) {
		t.Fatalf("truncated: %+v", got.Usage)
	}
	noNL := `data: {"type":"message_delta","usage":{"output_tokens":5}}`
	if got := ParseSSE([]byte(noNL)); got.Usage.OutputTokens != 5 {
		t.Fatalf("final unterminated line: %+v", got.Usage)
	}
}

func TestTapIgnoresNoiseAndBounds(t *testing.T) {
	noise := ": keepalive\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\ndata: [DONE]\ndata: not json\n\n"
	if got := ParseSSE([]byte(noise + deepseekSSE)); got.Usage != (Usage{2048, 77, 1536, 0}) {
		t.Fatalf("noise: %+v", got.Usage)
	}
	// An over-long line is dropped without growing the buffer, and parsing
	// resumes after its newline.
	tap := NewTap(true)
	_, _ = tap.Write([]byte("data: " + strings.Repeat("x", maxSSELine+10)))
	_, _ = tap.Write([]byte("still the long line\n" + deepseekSSE))
	if len(tap.pending) > maxSSELine {
		t.Fatalf("pending grew to %d", len(tap.pending))
	}
	if got := tap.Result(); got.Usage != (Usage{2048, 77, 1536, 0}) {
		t.Fatalf("after long line: %+v", got.Usage)
	}
	// A JSON body past the bound is not accounted (and not kept).
	big := NewTap(false)
	_, _ = big.Write([]byte(anthropicJSON))
	_, _ = big.Write(make([]byte, maxJSONBody))
	if got := big.Result(); !got.Usage.Empty() || big.body != nil {
		t.Fatalf("oversized json: %+v, kept %d bytes", got.Usage, len(big.body))
	}
}

func TestParseNoUsage(t *testing.T) {
	for _, body := range []string{``, `{}`, `{"type":"error","error":{"type":"overloaded_error"}}`, `<html>`} {
		if got := ParseJSON([]byte(body)); !got.Usage.Empty() {
			t.Fatalf("%q: %+v", body, got.Usage)
		}
	}
}
