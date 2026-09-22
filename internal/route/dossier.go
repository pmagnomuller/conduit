package route

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// Step classifies where the conversation is when the request arrives.
const (
	StepUserTurn = "user_turn"
	StepToolStep = "tool_step"
	StepOther    = "other"
)

const (
	taskClip        = 1200
	intentTailClip  = 400
	excerptClip     = 120
	maxExcerpts     = 3
	fingerprintClip = 2000
	imageLookback   = 6
	// Client-supplied strings reach Jev, the in-memory ring and the decisions
	// log, and Recent is polled by the UI: an unclipped field is replayed on
	// every poll, so clip every one of them at the source.
	modelClip    = 128
	toolNameClip = 64
	// maxToolNames bounds the tool names kept per step. toolSet (lease matching)
	// is built from the same list, so steps differing only past the cap look
	// alike; the worst case is one extra reuse, never a wrong provider.
	maxToolNames = 16
)

// Dossier is the bounded state sent to Jev. It deliberately omits the system
// prompt and tool schemas: Jev needs evidence about the remaining work, not
// the whole context window.
type Dossier struct {
	Task           string     `json:"task"`
	Step           string     `json:"step"`
	Tool           string     `json:"tool,omitempty"`
	ToolBatch      *ToolBatch `json:"tool_batch,omitempty"`
	IntentTail     string     `json:"intent_tail,omitempty"`
	RequestedModel string     `json:"requested_model"`
	ThinkingBudget int        `json:"thinking_budget,omitempty"`
	NMessages      int        `json:"n_messages"`
	HasImage       bool       `json:"has_image"`
	NTools         int        `json:"n_tools"`

	// fingerprint is the lease key; not sent to Jev.
	fingerprint string
	// toolSet is the sorted unique tool names of this step; lease matching.
	toolSet []string
}

// ToolBatch summarises the tool_result blocks of a tool_step.
type ToolBatch struct {
	Names    []string `json:"names"`
	Count    int      `json:"count"`
	Errors   int      `json:"errors"`
	Excerpts []string `json:"excerpts,omitempty"` // errors first, ≤3, ≤120 chars each
}

// Wire shapes for the subset of the Anthropic Messages body we inspect.
type inbound struct {
	Model    string            `json:"model"`
	System   json.RawMessage   `json:"system"`
	Messages []inMessage       `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
	Thinking *struct {
		BudgetTokens int `json:"budget_tokens"`
	} `json:"thinking"`
}

type inMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type inBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"` // tool_result: string or []block
}

// blocks decodes content that is either a bare string or an array of blocks.
func blocks(raw json.RawMessage) []inBlock {
	raw = trimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []inBlock{{Type: "text", Text: s}}
	}
	var out []inBlock
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func trimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func joinText(bs []inBlock) string {
	var parts []string
	for _, b := range bs {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// excerptOf is the one-line evidence a tool_result contributes to the batch.
func excerptOf(b inBlock) string {
	return clipHead(strings.TrimSpace(joinText(blocks(b.Content))), excerptClip)
}

// toolBatch summarises the tool_result blocks of a tool_step, keeping totals
// and error counts exact while holding only what it can report: a block-dense
// body would otherwise keep one excerpt per block — the bulk of the step's
// memory — and throw all but maxExcerpts of them away. Excerpt building stops
// with the slots, so the content of later blocks is never decoded. Names are
// clipped before they are deduped, so long names sharing a prefix count once.
func toolBatch(results []inBlock, names map[string]string) *ToolBatch {
	tb := &ToolBatch{}
	// okExcerpts is only a holding pen until the error excerpts have had first
	// pick, so it is capped at maxExcerpts too.
	var okExcerpts []string
	seen := map[string]bool{}
	for _, b := range results {
		if b.Type != "tool_result" {
			continue
		}
		tb.Count++
		name := names[b.ToolUseID]
		if name == "" {
			name = "unknown"
		}
		name = clipHead(name, toolNameClip)
		if !seen[name] && len(tb.Names) < maxToolNames {
			seen[name] = true
			tb.Names = append(tb.Names, name)
		}
		if b.IsError {
			tb.Errors++
			if len(tb.Excerpts) < maxExcerpts {
				if ex := excerptOf(b); ex != "" {
					tb.Excerpts = append(tb.Excerpts, ex)
				}
			}
			continue
		}
		if len(okExcerpts) < maxExcerpts {
			if ex := excerptOf(b); ex != "" {
				okExcerpts = append(okExcerpts, ex)
			}
		}
	}
	for _, ex := range okExcerpts {
		if len(tb.Excerpts) >= maxExcerpts {
			break
		}
		tb.Excerpts = append(tb.Excerpts, ex)
	}
	return tb
}

// headCut is the largest rune boundary ≤ i. Cuts happen at byte indexes so the
// caps stay in bytes, but a byte cut can split a multi-byte rune, and
// json.Marshal silently rewrites the orphaned fragment as U+FFFD — so back off
// to the previous boundary. Backing off never exceeds the budget.
func headCut(s string, i int) int {
	if i > len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// tailCut is the smallest rune boundary ≥ i; stepping forward keeps a tail
// slice within the budget its cap promises.
func tailCut(s string, i int) int {
	if i < 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// clipHeadTail keeps the start and end of long text; the middle is usually
// pasted noise and the ends carry the ask and the constraint.
func clipHeadTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := (max - 5) / 2
	return s[:headCut(s, half)] + " ... " + s[tailCut(s, len(s)-half):]
}

func clipTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "..." + s[tailCut(s, len(s)-(max-3)):]
}

func clipHead(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:headCut(s, max-3)] + "..."
}

// Extract builds the dossier from an inbound /v1/messages body. It never
// fails: malformed bodies yield an "other" step with whatever was readable.
func Extract(body []byte) Dossier {
	var in inbound
	_ = json.Unmarshal(body, &in)

	d := Dossier{
		Step:           StepOther,
		RequestedModel: clipHead(in.Model, modelClip),
		NMessages:      len(in.Messages),
		NTools:         len(in.Tools),
	}
	if in.Thinking != nil {
		d.ThinkingBudget = in.Thinking.BudgetTokens
	}

	msgs := in.Messages
	n := len(msgs)

	// Task: last user message that carries text.
	for i := n - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if t := joinText(blocks(msgs[i].Content)); t != "" {
			d.Task = clipHeadTail(t, taskClip)
			break
		}
	}

	// Step from the final message only.
	if n > 0 && msgs[n-1].Role == "user" {
		last := blocks(msgs[n-1].Content)
		if len(last) > 0 {
			switch last[len(last)-1].Type {
			case "tool_result":
				d.Step = StepToolStep
			case "text":
				d.Step = StepUserTurn
			}
		}
	}

	if d.Step == StepToolStep {
		results := blocks(msgs[n-1].Content)
		// tool_use_id → name from the preceding assistant message.
		names := map[string]string{}
		var assistantText string
		for i := n - 2; i >= 0; i-- {
			if msgs[i].Role != "assistant" {
				continue
			}
			bs := blocks(msgs[i].Content)
			for _, b := range bs {
				if b.Type == "tool_use" && b.ID != "" {
					names[b.ID] = b.Name
				}
			}
			assistantText = joinText(bs)
			break
		}
		d.IntentTail = clipTail(assistantText, intentTailClip)

		tb := toolBatch(results, names)
		if tb.Count == 1 && len(tb.Names) == 1 {
			d.Tool = tb.Names[0]
		}
		d.ToolBatch = tb
		d.toolSet = append([]string(nil), tb.Names...)
		sort.Strings(d.toolSet)
	}

	// Images anywhere in the recent window (including inside tool_result).
	start := n - imageLookback
	if start < 0 {
		start = 0
	}
scan:
	for _, m := range msgs[start:] {
		for _, b := range blocks(m.Content) {
			if b.Type == "image" {
				d.HasImage = true
				break scan
			}
			if b.Type == "tool_result" {
				for _, inner := range blocks(b.Content) {
					if inner.Type == "image" {
						d.HasImage = true
						break scan
					}
				}
			}
		}
	}

	d.fingerprint = fingerprint(in)
	return d
}

// fingerprint identifies a conversation across turns: system prompt text plus
// the opening user message. Stable while the client keeps the same thread.
func fingerprint(in inbound) string {
	var sys string
	if sb := blocks(in.System); len(sb) > 0 {
		for _, b := range sb {
			if b.Type == "text" {
				sys = b.Text
				break
			}
		}
	}
	var first string
	for _, m := range in.Messages {
		if m.Role == "user" {
			first = joinText(blocks(m.Content))
			break
		}
	}
	if len(first) > fingerprintClip {
		first = first[:fingerprintClip]
	}
	h := sha256.Sum256([]byte(sys + "\x00" + first))
	return hex.EncodeToString(h[:])
}
