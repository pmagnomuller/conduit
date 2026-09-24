# Jev Engineering for Coding Agents — notes for conduit

## Sources

| File | What it is |
|---|---|
| [`thoughts-on-a-typesafe-coding-agent.pdf`](thoughts-on-a-typesafe-coding-agent.pdf) | **Primary.** "[public] thoughts on a typesafe coding agent" — Diogo Almeida's (founder, TypeSafe) own bullet-point design notes, a Google Doc export, 11 pp. |
| [`Jev-Engineering-for-Coding-Agents.pdf`](Jev-Engineering-for-Coding-Agents.pdf) | **Derived.** "Jev Engineering for Coding Agents" (September 2026, 12 pp.) — an independent write-up of the notes above in paper form. Not a TypeSafe publication or endorsed by TypeSafe. |

Same argument, same structure (six KV-cache symptoms, basic / advanced / weird
feature tiers, tools appendix, background-processing appendix), same routing
arithmetic. Where the two disagree, trust the primary. What the synthesis
changed:

- **Security routing was softened.** The notes are explicit: *"Chinese models
  are way cheaper and likely yoinking all the data passing through them (e.g.
  DeepSeek V4 is insanely cheap)."* The synthesis turns this into "open-weight
  models served through low-cost providers". For conduit this is the more
  important version: GLM (z.ai) and DeepSeek are both on its failover chain
  and in its default catalog. See suggestion #4.
- **Added by the synthesis, not in the primary's text:** the file-sensitivity
  policy table (open / standard / restricted / custom), the `policy "exec"`
  example, the Jev-questions table, and the "mirror live traffic to a candidate
  model for ~a day" pattern. The primary only lists "creating evals in the
  background" as a link. The token-share table comes from the notes' "SLOP: a
  breakdown of coding agent subtasks", which is an embedded image with no
  extractable numbers.
- **Terminology:** the notes say "TypeSafe" throughout; the synthesis names
  the decision model Jev.
- **Routing arithmetic** is the same (4.15 vs 6.19 → pure Opus ≈ ⅔ the cost).
  The notes' own condition reads "if there is 67% more generated tokens than
  generated output tokens", which looks like a typo for "Z ≥ 1.67 Y" — extra
  reads outweighing output.
- **Left out of the synthesis:** the "open-source hype vehicle" deliverable,
  and an Appendix 3 pointer to a recent X post.

Cost figures in both are illustrative and use the list prices the notes cite.

## Summary

- **Agents are simple loops.** The leverage is not the loop but what the harness
  feeds the model on each turn.
- **Jev is the decision layer, not the coder.** The harness sends explicit state
  (goal, context, rules, available actions, previous actions) plus a predefined
  question; Jev returns a typed answer — *choice*, *score*, or *noul* — with
  probabilities. Typed output can be validated, thresholded and branched on.
- **Organising question:** *how would you design a coding agent if LLMs had no KV
  cache?* The cache is why agents are append-only transcripts, and it silently
  produces six symptoms:

  | # | Symptom | Root cause |
  |---|---|---|
  | 1 | Routing fails | handing back to the big model reprocesses context |
  | 2 | Tools crowd context | schemas must sit in the system message |
  | 3 | Compaction is lossy | compresses before the question is known |
  | 4 | Sub-agents are rare | choosing what to pass in / merge back is hard |
  | 5 | Restarts discard good state | transcript is the only state |
  | 6 | Batteries debate | every built-in costs context permanently |

- **Routing arithmetic.** Opus $5/$25, Sonnet $3/$15 per MTok; X context, Y output,
  Z extra reads. Pure Opus = `25Y + 5Z`. Opus→Sonnet→Opus = `3X + 20Y + 8Z`. For
  X=0.65, Y=0.12, Z=0.23: **4.15 vs 6.19** — the "cheap" route costs ~50% more.
  Routing priced per token is wrong; price it **per context rebuild**. Routing
  only pays when the cheap model gets a small purpose-built context and the
  return trip doesn't force a full reread.
- **Where tokens go.** Reading files 30–40%, search 10–18%, command output
  10–20%, fixed overhead 5–12%, reasoning 5–15%, *writing code 4–10%*. Retrieval
  is ~⅔ of processed tokens (fastcontext: 56.2% of tool turns, 46.5% of tokens).
- **Proposals.**
  - *Programmable permissions* — policy queries (`deny if touches ~/.ssh`,
    inspect script contents before exec), `allow/ask/deny`.
  - *Harness as tool router* — model states intent, Jev picks top-k tool, harness
    builds args; tiered disclosure (snippet → schema on demand → docs).
  - *Meta-attention* — per query, Jev decides (a) reuse KV cache vs rebuild, cost-aware;
    (b) visibility of every chunk: hide / short / long / full ("visibility ladder",
    query-aware compression).
  - *Sub-agents via cheap context assembly*; subgoal dedup; read/write typing so
    read-only tasks never contend.
  - *Conditional AGENTS.md* — instructions bound to conditions, reloaded while the
    condition holds, immune to compaction.
  - *Security-aware routing* — a third axis beside difficulty and cost: estimate
    which files a subtask touches, attach policy per file class
    (open / standard / restricted = first-party frontier only / custom vendor exclusions).
  - *Background processing* — read-only tasks (cross-model review, eval generation,
    progress pages, **shadow traffic to a candidate model for ~a day before
    switching**) share one retrieval pass.

## Most important thought

> Routing does not fail because cheap models are weak. It fails because context
> is reprocessed. Price routing per context rebuild, not per token.

This is aimed squarely at what conduit does. conduit switches provider/model
mid-conversation while forwarding the *full* transcript every time. Every switch
is a cold prefix on the new model (no shared prompt cache across Anthropic, GLM,
DeepSeek, or even across Claude models), and switching back is a second cold
rebuild. conduit cannot shrink the context it forwards, so by the note's
arithmetic, a switch to a cheaper model on a long session is often a net loss —
and Jev today is never told that.

## Where conduit stands

| Note's idea | conduit today |
|---|---|
| Typed questions, validate answer | ✅ `model` + `lease` choice questions; answer validated against catalog/lease keys; fail-open |
| Bounded, evidence-only state | ✅ `Dossier` omits system prompt + schemas, clips every field |
| Capability-first, don't chase cheap | ✅ `modelInstructions` ladder — consistent with "pure frontier is cheaper" |
| Cache reuse vs rebuild decision | ⚠️ leases approximate stickiness, but no switch cost, context size, or current-model in state |
| Probabilities | ⚠️ requested and decoded, never used (`Probabilities` dropped; `Confidence` only logged) |
| Cost estimate on routing | ❌ none |
| Security-aware routing | ❌ Jev can send any turn to any provider, incl. turns reading `.env` |
| Measured quality (shadow evals) | ❌ README's honesty caveat: priors only |
| Permissions / tool routing / chunk visibility | n/a — client-side (Claude Code) concerns, out of scope for a gateway |

## Suggested improvements to jev mode

Ordered by value / effort.

### 1. Make switch cost explicit (cache affinity) — ✅ implemented

The single biggest gap. Add to `Dossier`:

- `current` — provider/model that served this thread's previous call (from the
  lease map, kept even for `one_call`; key = fingerprint).
- `context_tokens_est` — cheap estimate (`len(body)/4`, or sum of text lengths).
- `cache_warm` — whether `current` served within the provider's cache TTL
  (~5 min Anthropic default).

Extend `modelInstructions`: *"Switching away from `current` forces the new model
to reprocess `context_tokens_est` tokens, and switching back costs it again.
Only switch when the capability difference outweighs that rebuild."*

Deterministic guard in Go (Jev answers are untrusted, policy is not): if
`context_tokens_est > jev.max_switch_context` and choice ≠ `current`, require
`confidence ≥ jev.switch_confidence` or keep `current`. Record
`source: "sticky"` so the log shows it.

*Shipped as:* `current`, `cache_warm`, `context_tokens_est` in the dossier;
`max_switch_context` (60000) / `switch_confidence` (0.8) guard; see
ARCHITECTURE §4.4.

### 2. Add a noul "stay or switch" question — medium

This is the note's cache question translated to a proxy: `{type: "noul",
question: "keep current model?"}` asked alongside `model`. Only when `stay=false`
with high probability does the `model` answer apply. It separates *should I
move* from *where to*, and gives a clean probability to threshold on. Can
replace the `lease` question over time: a stay-probability per call is a
finer-grained lease.

### 3. Use the probabilities — ✅ implemented (margin guard)

`jevAnswer.Probabilities` is decoded and discarded. Use it:

- margin = p(top) − p(second); low margin → fall back to `requested_model` (if a
  candidate) rather than Jev's coin flip. New `source: "low_confidence"`.
- Log top-2 + margin in `decisions.jsonl` for later calibration.
- Config: `jev.min_confidence`, `jev.min_margin`.

*Shipped as:* `min_margin` (0.15) → stay on `current`, `source:
"low_confidence"`; `margin` + `pick` logged. Falls to `current`, not
`requested_model` — the requested id is often not what served the thread.

### 4. Security-aware candidate filtering — ✅ implemented (path-pattern filter)

Directly from §IX, and more pointed in the primary: its example is Chinese
providers, which for conduit means GLM and DeepSeek — both in the default
catalog. Deterministic, before Jev sees candidates (same place the
breaker filter runs in `proxy.go`):

- Scan the recent window's `tool_use` inputs / `tool_result` text for path
  classes: `.env*`, `~/.ssh`, `*.pem`, `id_*`, `secrets/`, `infra/`, `terraform`,
  `kubeconfig`, plus user globs.
- Catalog gets `trust = "first_party" | "vetted" | "open"`.
- `[[jev.policy]] match = [".env*", "~/.ssh/**"]  min_trust = "first_party"`.
- Restricted turn → candidates filtered to `min_trust`; decision records
  `policy: "restricted"`; lease cannot carry a restricted thread back to an
  open-trust model.
- Add `sensitivity` (the matched class, not the paths) to the dossier so Jev
  can reason about it, but enforce in Go.

This makes "don't send secrets-adjacent turns to cheap third-party endpoints"
configuration, not discipline — the note's exact point.

*Shipped as:* `restrict_sensitive` / `restricted_providers` /
`restricted_patterns` in `[jev]`, enforced in `internal/route/restrict.go`;
see ARCHITECTURE §4.5. Simpler than the plan above: one trusted/untrusted
split by provider instead of per-entry trust tiers and policy classes. Gap:
auto-mode breaker failover is not covered.

### 5. Cost estimate per decision — ✅ implemented (input side)

Catalog gets `price_in`, `price_out`, `price_cache_read` (per MTok). Per decision
log `est_cost` (input at cache-read price if `cache_warm` and unchanged model,
else full input price) and `baseline_cost` (same call on `requested_model`).
`conduitctl decisions --cost` sums delta per session. This tests the note's
routing arithmetic against real conduit traffic — and would show whether jev
mode saves or burns money.

*Shipped as:* `price_in` / `price_out` / `price_cache_read` on catalog
entries (defaults filled for all nine built-ins, list prices 2026-09-24);
decisions log `est_input_usd` and `baseline_input_usd`; `conduitctl
decisions` prints both per line plus a total with % delta. Input side only —
output size is unknown at decision time. Capturing `usage` from the upstream
response would complete it (and replace the bytes/4 token estimate).

### 6. Shadow mode (measured quality) — high value, larger

Retires the README honesty caveat. (The "mirror traffic for ~a day" framing
comes from the synthesis; the primary only points at background eval
generation.) `jev.shadow_rate = 0.05`: for a sample of
calls, also send the request (non-streaming, `max_tokens` capped) to Jev's pick
while the client is served by the requested model; store both outputs +
latency + tokens to `~/.local/state/conduit/shadow/`. Offline: an eval
(cross-model judge, or tool-call validity / test pass for tool steps) produces
per-(step, model) win rates, which feed back into catalog `profile` text — or
into the dossier as measured priors. Mirrors the note's "mirror live traffic to
a candidate model for ~a day before switching". Mind cost and privacy
(respect policy from #4; never shadow restricted turns).

### 7. Richer, query-aware dossier — small

Retrieval dominates tokens, so describe the *shape* of the work, not just the
last step:

- `recent_tool_mix` — counts over last N tool calls by class (read / search /
  edit / exec). Read/search-heavy chains are the note's cheap-context case; edit
  after long read chains is not.
- `error_streak` — consecutive `is_error` tool results (escalate signal).
- `turn_index_in_chain` — tool calls since last user message.

All derivable in `Extract` from data already parsed.

### 8. Housekeeping

- Dossier goes out on every non-leased call: add `jev.dossier_mode = "full" |
  "minimal"` (minimal = no `task`/`intent_tail`/excerpts) for privacy-sensitive
  use.
- Parse `X-Conduit-Decision` into a `sticky|low_confidence|restricted` enum so
  the UI and `conduitctl` can colour them.
- Tests: table-driven cases for the switch guard and policy filter with the
  existing `jevStub`.

## Backlog (status 2026-09-24)

Open items, highest value first. Shipped: #1, #3, #4, #5 (see above,
ARCHITECTURE §4.4–4.6).

- [ ] **Capture upstream `usage`** — parse input/output/cache tokens from the
  provider response (SSE `message_start` / `message_delta`) into the decision
  record. Completes #5 (output side, real token counts instead of bytes/4) and
  gives shadow mode (#6) its measurements. Medium: streaming path.
- [ ] **#6 Shadow mode** — sample traffic to Jev's pick, eval offline, feed
  results back into profiles. Largest item; retires the README honesty caveat.
- [ ] **#2 Noul "stay or switch" question** — separate *whether* from *where*;
  may replace the lease question. Needs testing against the real Jev API.
- [ ] **#7 Richer dossier** — `recent_tool_mix`, `error_streak`,
  `turn_index_in_chain`. Small, all in `Extract`.
- [ ] **Fix DeepSeek catalog id** — `deepseek-v4-flash` is retired upstream
  and served by V4.1 Flash (`deepseek-flash`), breaking the README's
  "only ids that serve themselves" rule. Update catalog entry, profile, price
  and the DeepSeek model map together. Small.
- [ ] **Sensitive turns in auto mode** — #4 covers jev mode only; with
  Anthropic OPEN, auto (and jev fail-open) still fail over to GLM/DeepSeek.
  Needs an availability decision (refuse / queue / allow) before code.
- [ ] **Per-entry trust tiers** — #4 shipped as a provider-level
  trusted/untrusted split; the plan's `trust` field + policy classes remain.
- [ ] **Review new defaults** — switch guard, margin guard and restriction are
  on by default; check real `decisions.jsonl` after a few sessions and tune
  `max_switch_context`, `switch_confidence`, `min_margin`.
- [ ] **#8 Housekeeping** — `dossier_mode = minimal` for privacy; UI cost
  column; typed decision-source enum.

## Non-goals (for a gateway)

Programmable permissions, tool routing / tiered disclosure, chunk visibility
ladder, conditional AGENTS.md, sub-agent context assembly. All require
controlling how context is assembled per turn — the note's "native only"
thesis. conduit sees the body but must forward it faithfully; rewriting the
client's context is out of scope. Those belong in the harness (or a future
conduit-adjacent agent), not the proxy.
