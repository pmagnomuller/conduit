# conduit

Local loopback HTTP gateway between **Claude Code** and two upstreams:

1. **Anthropic** (Claude Code subscription OAuth) — default  
2. **GLM via Z.ai** (`https://api.z.ai/api/anthropic`) — when plan quota is exhausted

While the subscription window has capacity, every request goes to Anthropic.
When a real plan-quota signal is observed, the gateway opens a circuit breaker
and transparently replays to GLM. When the open window expires it probes
Anthropic again and switches back automatically.

```
Claude Code
  │  ANTHROPIC_BASE_URL=http://127.0.0.1:8787
  ▼
conduit (127.0.0.1 only)
  ├─ breaker CLOSED / PROBE → api.anthropic.com
  └─ breaker OPEN           → api.z.ai/api/anthropic  (GLM)
```

## Quick start

```bash
# 1. Build
go build -o conduit ./cmd/gateway

# 2. Config + secrets
mkdir -p ~/.config/conduit ~/.local/state/conduit
cp config.example.toml ~/.config/conduit/config.toml
cp .env.example .env   # then put your Z.ai key in .env
# .env is gitignored — never commit it

# 3. One-shot install (macOS LaunchAgent at login)
./setup.sh
# starts now, restarts if it dies, starts again on login
# logs: tail -f ~/.local/state/conduit/gateway.log
# stop: launchctl bootout gui/$(id -u)/com.pedro.conduit

# Or foreground:
# set -a && source .env && set +a
# ./conduit -config ~/.config/conduit/config.toml
```

Point Claude Code at the gateway (new sessions). In `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"
  }
}
```

Or for one shell only:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
claude
```

Keep normal Claude OAuth. Do **not** put the Z.ai key in Claude’s auth — the gateway injects it only on the GLM path.

Already-running Claude sessions keep their old base URL until restarted.

## Verify it’s working

```bash
curl -s http://127.0.0.1:8787/_gateway/health
# → {"ok":true}

curl -s http://127.0.0.1:8787/_gateway/status | jq .
tail -f ~/.local/state/conduit/gateway.log   # if running in background
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
| `ZAI_API_KEY` | **yes** (gateway) | Z.ai API key used when routing to GLM |
| `ANTHROPIC_BASE_URL` | for Claude Code | Set to `http://127.0.0.1:8787` |
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
(see `config.example.toml`). Default: opus/sonnet → `glm-5.2`, haiku →
`glm-4.5-air`.

## Turn it off

1. Stop the gateway (`launchctl bootout gui/$(id -u)/com.pedro.conduit`, or `kill "$(cat ~/.local/state/conduit/gateway.pid)"` if you started it by hand).  
2. Remove `ANTHROPIC_BASE_URL` from `~/.claude/settings.json` (or unset it).  
3. Restart Claude Code.

## Tests

```bash
go test ./...
```

## Non-goals

No cost dashboards, no mid-stream provider splicing, no response caching, no
non-loopback bind, no gateway auth beyond localhost.
