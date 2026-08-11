# FINDINGS.md

Empirical notes on subscription-quota detection and GLM compatibility.
Updated as captures accumulate under `~/.local/state/claude-glm-gateway/upstream-errors.jsonl`.

## 1. Subscription quota exhaustion — wire signal

**Status:** not yet observed first-hand on this machine (no live capture of a Max/Pro
quota 429 through this gateway). Implementation is based on Anthropic's public
Claude Code docs, GitHub issue captures, and reverse-engineering writeups of the
`anthropic-ratelimit-unified-*` headers. When you hit a real limit, inspect the
JSONL capture and update this section if anything disagrees.

### What public evidence says

Claude Code distinguishes **two different 429s**:

| Kind | How to recognize | Gateway action |
|---|---|---|
| **Plan / usage quota** | `429` + `error.type == "rate_limit_error"` **and** `anthropic-ratelimit-unified-*` headers present (often `…-status: rejected\|exceeded\|rate_limited`, plus `…-5h-reset` / utilization). Often also `Retry-After`. | **OPEN breaker → GLM** |
| **Transient capacity throttle** | `429` + message containing `not your usage limit` / `temporarily limiting`, **or** a `rate_limit_error` **without** unified quota headers (Claude Code's own rule). Sometimes `x-should-retry: true`. | **Retry Anthropic. Do not fail over.** |

Example quota-ish body seen in the wild (API / OAuth):

```json
{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "This request would exceed your account's rate limit. Please try again later."
  },
  "request_id": "req_…"
}
```

Example unified headers on successful and limited responses:

```
anthropic-ratelimit-unified-status: allowed|rejected|exceeded|rate_limited
anthropic-ratelimit-unified-representative-claim: five_hour
anthropic-ratelimit-unified-5h-status: allowed
anthropic-ratelimit-unified-5h-reset: <unix epoch>
anthropic-ratelimit-unified-5h-utilization: 0.07
anthropic-ratelimit-unified-7d-status: allowed
anthropic-ratelimit-unified-7d-utilization: 0.53
```

Other rows from the original design table are unchanged:

| Observation | Action |
|---|---|
| `403` + `billing_error` | Quota → GLM |
| `403` + `permission_error` | Surface (auth) |
| `401` | Surface (auth) |
| `529` / `500` / `502` / `503` | Transient retry Anthropic |
| `400` | Surface |

**Deviation from the naive “any 429 = quota” rule:** we **do not** treat every
`rate_limit_error` as quota. Doing so would fail over to GLM during Anthropic
capacity blips. Prefer unified headers / explicit “not your usage limit” text.

`proactive_threshold` / `proactive_utilization` default to disabled until you
confirm header semantics on your account; enable once captures look trustworthy.

## 2. GLM / Z.ai Anthropic-compatible endpoint

**Endpoint:** `https://api.z.ai/api/anthropic`  
**Auth:** API key via `Authorization: Bearer $ZAI_API_KEY` (also send `x-api-key`
with the same value — Z.ai accepts both).  
**Default model ID used here:** `glm-5.2` (haiku-tier mapped to `glm-4.5-air`).

### Compatibility gaps (to verify with a live key)

Not exercised in CI (fake upstream only). When you provide `ZAI_API_KEY`, probe:

- [ ] Tool use / `tool_use` + `tool_result` round-trips
- [ ] `cache_control` / prompt caching fields (may be ignored; confirm no hard error)
- [ ] Thinking / extended thinking blocks Claude Code may send
- [ ] `output_config` and other beta body fields
- [ ] Streaming SSE event ordering vs Anthropic
- [ ] Whether Claude model IDs are accepted server-side without rewrite (Z.ai docs
      sometimes map Claude names internally; we still rewrite for predictability)

Known community notes: some setups set `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`
when talking to Z.ai directly because certain beta headers return provider errors.
This gateway **strips `Anthropic-Beta` on the GLM hop** and substitutes Z.ai auth,
while leaving Anthropic betas intact on the Anthropic path.

## 3. Capture workflow

With `capture_upstream_errors = true` (default), every non-2xx Anthropic (and GLM)
response is appended to:

`~/.local/state/claude-glm-gateway/upstream-errors.jsonl`

Credentials are redacted before write. After your first real quota event, paste the
relevant line (or summarize status/headers/body types) back into this file.
