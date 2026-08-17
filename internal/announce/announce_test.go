package announce_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/pedro-mueller/conduit/internal/announce"
)

func TestInjectJSON(t *testing.T) {
	in := []byte(`{"id":"m1","content":[{"type":"text","text":"hello"}],"role":"assistant"}`)
	out := announce.InjectJSON(in, "[conduit] note\n\n")
	if !bytes.Contains(out, []byte(`"[conduit] note\n\nhello"`)) && !bytes.Contains(out, []byte("[conduit] note")) {
		t.Fatalf("inject failed: %s", out)
	}
	if !bytes.Contains(out, []byte("hello")) {
		t.Fatalf("lost original text: %s", out)
	}
}

func TestSSEInjector(t *testing.T) {
	raw := "" +
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	r := announce.NewSSEInjector(strings.NewReader(raw), "[conduit] GLM\n\n")
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`[conduit] GLM\n\nHi`)) {
		t.Fatalf("sse inject failed: %s", got)
	}
	// Only once.
	if bytes.Count(got, []byte("[conduit] GLM")) != 1 {
		t.Fatalf("expected one notice, got: %s", got)
	}
}
