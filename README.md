# conduit

Local loopback HTTP gateway between **Claude Code** and two upstreams:

1. **Anthropic** (Claude Code subscription OAuth) — default
2. **GLM via Z.ai** (`https://api.z.ai/api/anthropic`) — when plan quota is exhausted
3. **DeepSeek** (`https://api.deepseek.com/anthropic`) — optional terminal tier, when GLM itself fails

While the subscription window has capacity, every request goes to Anthropic.
When a real plan-quota signal is observed, the gateway opens a circuit breaker
and transparently replays to GLM. When the open window expires it probes
Anthropic again and switches back automatically. If GLM is unreachable or
returns auth/throttle/5xx errors while the breaker is OPEN, the request is
retried once on DeepSeek (`deepseek-chat` by default) — but only when
`DEEPSEEK_API_KEY` is set; without the key the tier is inert.

```
Claude Code
  │  ANTHROPIC_BASE_URL=http://127.0.0.1:8787
  ▼
conduit (127.0.0.1 only)
  ├─ breaker CLOSED / PROBE → api.anthropic.com
  ├─ breaker OPEN           → api.z.ai/api/anthropic  (GLM)
  └─ GLM failed             → api.deepseek.com/anthropic  (DeepSeek, optional)
```

## Quick start

You need [Go 1.26+](https://go.dev/dl/) and Claude Code.

```bash
./setup.sh
```

It builds the binary, asks for `ZAI_API_KEY` if missing, points Claude Code at
the gateway, and keeps conduit running (starts at login on macOS, restarts if
it dies). Then **restart Claude Code**.

```bash
./status.sh        # health + breaker
./stop.sh          # pause
./start.sh         # resume
./uninstall.sh     # stop service and un-point Claude Code
tail -f ~/.local/state/conduit/gateway.log
```

Already-open Claude sessions keep the old base URL until restarted.

Keep normal Claude OAuth. Do **not** put the Z.ai key in Claude’s auth — the
gateway injects it only on the GLM path.

Re-run `./setup.sh` after `git pull` to rebuild and reload.

## Verify it’s working

```bash
./status.sh
# or: curl -s http://127.0.0.1:8787/_gateway/health
```

After a Claude reply you should see `anthropic_requests` rise and log lines with
`"provider":"anthropic"`. On plan-quota failover: a `BREAKER OPEN` line, then
`"provider":"glm"`.

### GLM failover notices

When the breaker opens, conduit tells you in three ways:

1. **Inside Claude Code (chat)** — the failover reply’s first text is prefixed with
   `[conduit] Switched to GLM …`
2. **Inside Claude Code (status line)** — shows `conduit: GLM` (via your
   statusline polling `/_gateway/status`)
3. **Desktop notification** — macOS Notification Center (or `notify-send` on Linux)

Disable with `CONDUIT_NOTIFY=0`, or selectively with `CONDUIT_CHAT_NOTICE=0` /
`CONDUIT_DESKTOP_NOTIFY=0`.

## Critical constraint

The Anthropic API does not expose a per-request “this will be billed to API
credits” flag. Billing mode is a property of the credential. This gateway only
reacts to observable HTTP signals (status, `error.type`, rate-limit headers).

Not every `429` is quota. Claude Code distinguishes plan limits
(`anthropic-ratelimit-unified-*` headers) from transient “Server is temporarily
limiting requests (not your usage limit)” throttles. Only the former opens the
breaker. Details: [`FINDINGS.md`](./FINDINGS.md).

## Environment

| Variable | Required | Purpose |
|---|---|---|
| `ZAI_API_KEY` | **yes** (gateway) | Z.ai API key used when routing to GLM. Stored in `~/.config/conduit/.env` |
| `DEEPSEEK_API_KEY` | no | Enables the terminal DeepSeek tier when set. Stored in `~/.config/conduit/.env` |
| `ANTHROPIC_BASE_URL` | for Claude Code | Set to `http://127.0.0.1:8787` by `./setup.sh` |
| `CLAUDE_GLM_GATEWAY_CONFIG` | no | Alternate config path |
| `CLAUDE_GLM_GATEWAY_LISTEN` | no | Override `listen` |
| `CLAUDE_GLM_GATEWAY_STATE_PATH` | no | Breaker state file |
| `CLAUDE_GLM_GATEWAY_CAPTURE_PATH` | no | Upstream error JSONL |
| `CONDUIT_NOTIFY` | no | Set to `0` to disable all failover notices |
| `CONDUIT_CHAT_NOTICE` | no | Set to `0` to disable in-chat GLM notice |
| `CONDUIT_DESKTOP_NOTIFY` | no | Set to `0` to disable desktop toast |

## Model mapping

Claude Code sends Anthropic model IDs. When the breaker is OPEN the gateway
rewrites only the JSON `model` field using `[glm.model_map]` / `default_model`
(see `config.example.toml`). Default: opus/sonnet → `glm-5.3`, haiku →
`glm-4.5-air`. The DeepSeek tier maps through `[deepseek.model_map]` /
`default_model` the same way (`deepseek-chat` by default).

## Tests

```bash
make test
```

## Non-goals

No cost dashboards, no mid-stream provider splicing, no response caching, no
non-loopback bind, no gateway auth beyond localhost.
