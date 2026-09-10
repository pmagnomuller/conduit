package announce

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Notice is prepended to the first assistant text so it appears inside Claude Code.
const DefaultNotice = "[conduit] Switched to GLM (Z.ai) — Anthropic plan quota is open. This reply is served by GLM.\n\n"

// DeepSeekNotice is used when the terminal DeepSeek tier serves the reply.
const DeepSeekNotice = "[conduit] Switched to DeepSeek — Anthropic quota is open and GLM failed. This reply is served by DeepSeek.\n\n"

// InjectJSON prepends notice to the first text content block of a Messages API body.
func InjectJSON(body []byte, notice string) []byte {
	if len(body) == 0 || notice == "" {
		return body
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return body
	}
	raw, ok := msg["content"]
	if !ok {
		return body
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return body
	}
	injected := false
	for i := range blocks {
		if t, _ := blocks[i]["type"].(string); t == "text" {
			if text, ok := blocks[i]["text"].(string); ok {
				blocks[i]["text"] = notice + text
				injected = true
				break
			}
		}
	}
	if !injected {
		return body
	}
	b, err := json.Marshal(blocks)
	if err != nil {
		return body
	}
	msg["content"] = b
	out, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return out
}

// SSEInjector wraps an SSE body and prepends notice to the first text_delta.
type SSEInjector struct {
	r       io.Reader
	notice  string
	buf     bytes.Buffer
	pending []byte
	done    bool
}

func NewSSEInjector(r io.Reader, notice string) *SSEInjector {
	return &SSEInjector{r: r, notice: notice}
}

func (s *SSEInjector) Read(p []byte) (int, error) {
	for {
		if s.buf.Len() > 0 {
			return s.buf.Read(p)
		}
		if s.done {
			return 0, io.EOF
		}

		chunk := make([]byte, 4096)
		n, err := s.r.Read(chunk)
		if n > 0 {
			s.pending = append(s.pending, chunk[:n]...)
		}

		for {
			idx := bytes.Index(s.pending, []byte("\n\n"))
			if idx < 0 {
				break
			}
			event := s.pending[:idx+2]
			s.pending = s.pending[idx+2:]
			s.buf.Write(s.transformEvent(event))
		}

		if err != nil {
			if len(s.pending) > 0 {
				s.buf.Write(s.transformEvent(s.pending))
				s.pending = nil
			}
			s.done = true
			if s.buf.Len() == 0 {
				return 0, err
			}
			continue
		}
		if s.buf.Len() == 0 {
			continue
		}
	}
}

func (s *SSEInjector) transformEvent(event []byte) []byte {
	if s.notice == "" {
		return event
	}
	// Look for text_delta payloads.
	const marker = `"type":"text_delta"`
	if !bytes.Contains(event, []byte(marker)) && !bytes.Contains(event, []byte(`"type": "text_delta"`)) {
		return event
	}
	const textKey = `"text":"`
	pos := bytes.Index(event, []byte(textKey))
	if pos < 0 {
		return event
	}
	insertAt := pos + len(textKey)
	escaped := escapeJSONString(s.notice)
	out := make([]byte, 0, len(event)+len(escaped))
	out = append(out, event[:insertAt]...)
	out = append(out, escaped...)
	out = append(out, event[insertAt:]...)
	s.notice = "" // one-shot
	return out
}

func escapeJSONString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
