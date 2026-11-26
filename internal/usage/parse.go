package usage

import (
	"bytes"
	"encoding/json"
)

// Result is what a Tap recovered from one upstream response.
type Result struct {
	// Usage holds the final token counts; empty means the response carried
	// none.
	Usage Usage
	// Model is the model the provider named in its response, if any.
	Model string
	// RequestID is the provider's message id, if it named one.
	RequestID string
}

const (
	// maxJSONBody bounds how much of a non-streaming body a Tap keeps for
	// parsing. Messages replies are far smaller; a larger body is forwarded
	// to the client untouched and simply not accounted.
	maxJSONBody = 8 << 20 // 8 MiB
	// maxSSELine bounds one event-stream line kept while waiting for its
	// newline. Events are small; the bound only stops a stream that never
	// sends a newline from growing the buffer. Such a line is dropped.
	maxSSELine = 1 << 20 // 1 MiB
)

// Tap parses usage out of a response body as the body is copied to the
// client: it is an io.Writer for io.TeeReader, so the stream is never held
// back or buffered whole. For an event stream it keeps at most one line; for
// a JSON body it keeps the body (bounded) and parses it in Result.
//
// Write never fails, so a parse problem can never break the response copy.
type Tap struct {
	sse     bool
	pending []byte
	body    []byte
	dropped bool // JSON body exceeded maxJSONBody, or an SSE line maxSSELine
	skip    bool // discarding the rest of an over-long SSE line
	res     Result
}

// NewTap returns a tap for an event stream (sse) or a JSON body.
func NewTap(sse bool) *Tap { return &Tap{sse: sse} }

// Write feeds raw response bytes.
func (t *Tap) Write(p []byte) (int, error) {
	if !t.sse {
		if !t.dropped {
			if len(t.body)+len(p) > maxJSONBody {
				t.body, t.dropped = nil, true
			} else {
				t.body = append(t.body, p...)
			}
		}
		return len(p), nil
	}
	t.pending = append(t.pending, p...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		line := t.pending[:i]
		t.pending = t.pending[i+1:]
		if t.skip {
			t.skip = false
			continue
		}
		t.line(line)
	}
	if len(t.pending) > maxSSELine {
		t.pending, t.skip, t.dropped = nil, true, true
	}
	// Reuse the buffer's backing array once it has drained.
	if len(t.pending) == 0 {
		t.pending = t.pending[:0]
	}
	return len(p), nil
}

// Result finishes the parse and returns what was found. Safe to call once the
// copy is over, however it ended; a truncated stream yields whatever events
// arrived.
func (t *Tap) Result() Result {
	if t.sse {
		if len(t.pending) > 0 && !t.skip {
			t.line(t.pending)
		}
		t.pending = nil
		return t.res
	}
	if t.dropped {
		return Result{}
	}
	return ParseJSON(t.body)
}

func (t *Tap) line(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 || data[0] != '{' {
		return
	}
	applyEvent(&t.res, data)
}

// payload is the union of the fields usage cares about, in a non-streaming
// reply or in a streaming event: message_start nests id/model/usage under
// "message", message_delta carries "usage" at the top level.
type payload struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Usage   *Usage `json:"usage"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *Usage `json:"usage"`
	} `json:"message"`
}

// ParseJSON extracts usage from a non-streaming Messages reply.
func ParseJSON(body []byte) Result {
	var res Result
	applyEvent(&res, body)
	return res
}

// ParseSSE extracts usage from a complete event-stream body (tests, tools).
func ParseSSE(body []byte) Result {
	t := NewTap(true)
	_, _ = t.Write(body)
	return t.Result()
}

// applyEvent folds one JSON payload into res. Usage arrives in pieces on a
// stream — message_start has the input side (input, cache read, cache
// creation), message_delta the output count — so non-zero counts overlay
// field by field; a provider that repeats running totals in message_delta is
// handled the same way.
func applyEvent(res *Result, data []byte) {
	var m payload
	if json.Unmarshal(data, &m) != nil {
		return
	}
	if m.Message != nil {
		if m.Message.Model != "" {
			res.Model = m.Message.Model
		}
		if m.Message.ID != "" {
			res.RequestID = m.Message.ID
		}
		if m.Message.Usage != nil {
			res.Usage = overlay(res.Usage, *m.Message.Usage)
		}
	}
	if m.Model != "" {
		res.Model = m.Model
	}
	if m.ID != "" {
		res.RequestID = m.ID
	}
	if m.Usage != nil {
		res.Usage = overlay(res.Usage, *m.Usage)
	}
}

func overlay(cur, next Usage) Usage {
	if next.InputTokens != 0 {
		cur.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		cur.OutputTokens = next.OutputTokens
	}
	if next.CacheReadTokens != 0 {
		cur.CacheReadTokens = next.CacheReadTokens
	}
	if next.CacheCreationTokens != 0 {
		cur.CacheCreationTokens = next.CacheCreationTokens
	}
	return cur
}
