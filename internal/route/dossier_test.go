package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExtractUserTurn(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model":  "claude-sonnet-5",
		"system": "you are helpful",
		"tools":  []any{map[string]any{"name": "Read"}, map[string]any{"name": "Edit"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "first ask"},
			map[string]any{"role": "assistant", "content": "sure"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "rename foo"},
				map[string]any{"type": "text", "text": "to bar"},
			}},
		},
	})
	d := Extract(body)
	if d.Step != StepUserTurn {
		t.Fatalf("step=%s", d.Step)
	}
	if d.Task != "rename foo\nto bar" {
		t.Fatalf("task=%q", d.Task)
	}
	if d.RequestedModel != "claude-sonnet-5" || d.NMessages != 3 || d.NTools != 2 {
		t.Fatalf("meta: %+v", d)
	}
	if d.ToolBatch != nil || d.IntentTail != "" || d.HasImage {
		t.Fatalf("unexpected tool fields: %+v", d)
	}
	if len(d.fingerprint) != 64 {
		t.Fatalf("fingerprint=%q", d.fingerprint)
	}
	// Same system + first user message ⇒ same fingerprint regardless of later turns.
	other := Extract(mustJSON(t, map[string]any{
		"system":   "you are helpful",
		"messages": []any{map[string]any{"role": "user", "content": "first ask"}},
	}))
	if other.fingerprint != d.fingerprint {
		t.Fatal("fingerprint should be stable across turns")
	}
}

func TestExtractToolStepWithErrors(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "claude-opus-5",
		"system": []any{
			map[string]any{"type": "text", "text": "sys"},
		},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048},
		"messages": []any{
			map[string]any{"role": "user", "content": "fix the build"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "I will read the file then run tests"},
				map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "t2", "name": "Bash", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "file body " + strings.Repeat("x", 300)},
				map[string]any{"type": "tool_result", "tool_use_id": "t2", "is_error": true, "content": []any{
					map[string]any{"type": "text", "text": "exit 1: boom"},
				}},
			}},
		},
	})
	d := Extract(body)
	if d.Step != StepToolStep {
		t.Fatalf("step=%s", d.Step)
	}
	if d.Task != "fix the build" {
		t.Fatalf("task=%q", d.Task)
	}
	if d.ThinkingBudget != 2048 {
		t.Fatalf("thinking=%d", d.ThinkingBudget)
	}
	if d.IntentTail != "I will read the file then run tests" {
		t.Fatalf("intent=%q", d.IntentTail)
	}
	tb := d.ToolBatch
	if tb == nil || tb.Count != 2 || tb.Errors != 1 {
		t.Fatalf("batch=%+v", tb)
	}
	if strings.Join(tb.Names, ",") != "Read,Bash" {
		t.Fatalf("names=%v", tb.Names)
	}
	if d.Tool != "" { // two results ⇒ no single tool
		t.Fatalf("tool=%q", d.Tool)
	}
	if len(tb.Excerpts) != 2 || tb.Excerpts[0] != "exit 1: boom" {
		t.Fatalf("excerpts (errors first)=%v", tb.Excerpts)
	}
	if len(tb.Excerpts[1]) > excerptClip {
		t.Fatalf("excerpt not clipped: %d", len(tb.Excerpts[1]))
	}
	if strings.Join(d.toolSet, ",") != "Bash,Read" {
		t.Fatalf("toolSet=%v", d.toolSet)
	}
}

func TestExtractSingleToolAndImage(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is in this screenshot"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AAAA"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "a", "name": "Glob", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "a.go\nb.go"},
			}},
		},
	})
	d := Extract(body)
	if d.Step != StepToolStep || d.Tool != "Glob" {
		t.Fatalf("step=%s tool=%q", d.Step, d.Tool)
	}
	if !d.HasImage {
		t.Fatal("expected has_image")
	}
	if d.Task != "what is in this screenshot" {
		t.Fatalf("task=%q", d.Task)
	}
}

func TestExtractClipsAndTolerates(t *testing.T) {
	long := strings.Repeat("a", 2000) + "END"
	d := Extract(mustJSON(t, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": long}},
	}))
	if len(d.Task) > taskClip || !strings.HasSuffix(d.Task, "END") || !strings.HasPrefix(d.Task, "aaa") {
		t.Fatalf("task clip len=%d", len(d.Task))
	}
	// Garbage body must not panic and must classify as other.
	g := Extract([]byte("{not json"))
	if g.Step != StepOther || g.NMessages != 0 {
		t.Fatalf("garbage: %+v", g)
	}
	// Last message from assistant ⇒ other.
	a := Extract(mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "prefill"},
		},
	}))
	if a.Step != StepOther || a.Task != "hi" {
		t.Fatalf("assistant-last: %+v", a)
	}
	// Dossier must never carry the system prompt.
	raw, _ := json.Marshal(Extract(mustJSON(t, map[string]any{
		"system":   "SECRET SYSTEM PROMPT",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})))
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("system prompt leaked into dossier")
	}
}

// Client-supplied strings reach Jev, the ring and the decisions log, and Recent
// is polled by the UI: every one of them must arrive clipped.
func TestExtractClipsClientSuppliedStrings(t *testing.T) {
	const nTools = 40
	var uses, results []any
	for i := 0; i < nTools; i++ {
		id := "t" + strconv.Itoa(i)
		uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": fmt.Sprintf("tool-%02d", i), "input": map[string]any{}})
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"})
	}
	d := Extract(mustJSON(t, map[string]any{
		"model": strings.Repeat("m", 1<<20), // 1 MiB client-supplied model
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": uses},
			map[string]any{"role": "user", "content": results},
		},
	}))
	if d.RequestedModel == "" || len(d.RequestedModel) > modelClip {
		t.Fatalf("requested_model not clipped: %d bytes", len(d.RequestedModel))
	}
	tb := d.ToolBatch
	if tb == nil || tb.Count != nTools || tb.Errors != 0 {
		t.Fatalf("totals must stay exact: %+v", tb)
	}
	if len(tb.Names) > maxToolNames {
		t.Fatalf("tool names not capped: %d", len(tb.Names))
	}
	for _, n := range tb.Names {
		if len(n) > toolNameClip {
			t.Fatalf("tool name not clipped: %d bytes", len(n))
		}
	}
	// One oversized name still surfaces as Dossier.Tool, also clipped.
	one := Extract(mustJSON(t, map[string]any{
		"model": "claude-sonnet-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "a", "name": strings.Repeat("n", 4096), "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "ok"},
			}},
		},
	}))
	if one.Tool == "" || len(one.Tool) > toolNameClip {
		t.Fatalf("single tool name not clipped: %d bytes", len(one.Tool))
	}
	if raw, err := json.Marshal(d); err != nil || len(raw) > 8<<10 {
		t.Fatalf("dossier marshal: %d bytes, err=%v", len(raw), err)
	}
}

// Two-byte runes put every budget below on an odd byte count, i.e. mid-rune.
func TestClipHelpersAreRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 4000)
	cases := []struct {
		name string
		got  string
		max  int
	}{
		{"clipHead", clipHead(s, excerptClip), excerptClip},
		{"clipHeadTail", clipHeadTail(s, intentTailClip), intentTailClip},
		{"clipTail", clipTail(s, taskClip), taskClip},
	}
	for _, c := range cases {
		if len(c.got) > c.max {
			t.Errorf("%s: %d bytes exceeds cap %d", c.name, len(c.got), c.max)
		}
		if !utf8.ValidString(c.got) {
			t.Errorf("%s: cut split a rune", c.name)
		}
	}
}

// Every clipped field ends up in a JSON body; the input here is valid UTF-8, so
// a replacement rune or escape in the output means a clip cut a character.
func TestExtractClipsWithoutReplacementRunes(t *testing.T) {
	// json.Marshal's stand-ins for an invalid byte: U+FFFD as UTF-8 and as the
	// escape it writes. Spelled as bytes, so this file needs no escapes itself.
	replacementRune := []byte{0xEF, 0xBF, 0xBD}
	replacementEsc := []byte{0x5C, 'u', 'f', 'f', 'f', 'd'}

	hairy := strings.Repeat("é日", 4000) // 2- and 3-byte runes
	d := Extract(mustJSON(t, map[string]any{
		"model": strings.Repeat("é", 500),
		"messages": []any{
			map[string]any{"role": "user", "content": hairy},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": hairy},
				map[string]any{"type": "tool_use", "id": "t1", "name": hairy, "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": hairy},
			}},
		},
	}))
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(raw) {
		t.Fatal("dossier JSON is not valid UTF-8")
	}
	if bytes.Contains(raw, replacementRune) || bytes.Contains(raw, replacementEsc) {
		t.Fatalf("a clip split a rune: %q", raw)
	}
}

// The batch loop must not build an excerpt for a block it can never report: it
// used to keep one per block — the bulk of a block-dense step's memory — and
// throw all but maxExcerpts of them away. Totals and errors still count every
// block, and the excerpts kept are the earliest ones. The discarded excerpts
// are dead by the time toolBatch returns, so per-call allocation is the
// observable proxy for how many were built: 100k blocks must not cost tens of
// megabytes.
func TestToolBatchCapsExcerptWorkButNotCounts(t *testing.T) {
	const n = 100000
	results := make([]inBlock, n)
	for i := range results {
		results[i] = inBlock{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`"ok-` + strconv.Itoa(i) + `"`)}
	}
	results[1] = inBlock{Type: "tool_result", ToolUseID: "t1", IsError: true, Content: json.RawMessage(`"boom"`)}
	names := map[string]string{"t1": "Read"}

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	tb := toolBatch(results, names)
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(tb)

	if tb.Count != n || tb.Errors != 1 {
		t.Fatalf("counts must stay exact: count=%d errors=%d", tb.Count, tb.Errors)
	}
	if len(tb.Names) != 1 || tb.Names[0] != "Read" {
		t.Fatalf("names=%v", tb.Names)
	}
	if len(tb.Excerpts) != maxExcerpts || tb.Excerpts[0] != "boom" || tb.Excerpts[1] != "ok-0" || tb.Excerpts[2] != "ok-2" {
		t.Fatalf("excerpts (errors first, then the earliest)=%v", tb.Excerpts)
	}
	if got := m1.TotalAlloc - m0.TotalAlloc; got > 64<<10 {
		t.Fatalf("allocated %d bytes for %d blocks: excerpts are built per block, not per slot", got, n)
	}
}
