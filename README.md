# claude-glm-gateway

Local loopback HTTP gateway between **Claude Code** and two upstreams:

1. **Anthropic** (your Claude Code subscription OAuth / API key) — default  
2. **GLM via Z.ai** (`https://api.z.ai/api/anthropic`) — when subscription quota is exhausted

While the subscription window has capacity, every request goes to Anthropic.
When a **real plan-quota** signal is observed, the gateway opens a circuit breaker
and transparently replays to GLM. When the open window expires it **probes**
Anthropic again and switches back automatically. No Claude Code restart, no
mid-session config edits.

```
Claude Code
  │  ANTHROPIC_BASE_URL=http://127.0.0.1:8787
  ▼
claude-glm-gateway (127.0.0.1 only)
  ├─ breaker CLOSED / PROBE → api.anthropic.com
  └─ breaker OPEN           → api.z.ai/api/anthropic  (GLM)
```

## Critical constraint

**The Anthropic API does not expose a per-request “this will be billed to API
credits” flag.** Billing mode is a property of the credential. This gateway only
reacts to observable HTTP signals (status, `error.type`, rate-limit headers).

Importantly, **not every `429` is quota.** Claude Code distinguishes plan limits
(unified `anthropic-ratelimit-unified-*` headers) from transient “Server is
temporarily limiting requests (not your usage limit)” throttles. Only the former
opens the breaker. Details and sources: [`FINDINGS.md`](./FINDINGS.md).

## Install

```bash
cd ~/Developer/personal/claude-glm-gateway
go build -o claude-glm-gateway ./cmd/gateway
```

Optional: copy the binary onto your `PATH`.

```bash
mkdir -p ~/.config/claude-glm-gateway
cp config.example.toml ~/.config/claude-glm-gateway/config.toml
# edit model_map / listen if you want
```

## Environment

| Variable | Required | Purpose |
|---|---|---|
| `ZAI_API_KEY` | **yes** (gateway) | Z.ai API key used when routing to GLM |
| `ANTHROPIC_BASE_URL` | for Claude Code | Set to `http://127.0.0.1:8787` |
| `CLAUDE_GLM_GATEWAY_CONFIG` | no | Alternate config path |
| `CLAUDE_GLM_GATEWAY_LISTEN` | no | Override `listen` |
| `CLAUDE_GLM_GATEWAY_STATE_PATH` | no | Breaker state file |
| `CLAUDE_GLM_GATEWAY_CAPTURE_PATH` | no | Upstream error JSONL |

Claude Code keeps using its normal subscription OAuth (`Authorization: Bearer …`
+ `anthropic-beta: oauth-2025-04-20`). The gateway forwards those headers
**untouched** to Anthropic. On the GLM path it strips them and substitutes
`Authorization: Bearer $ZAI_API_KEY`.

## Run (foreground)

```bash
export ZAI_API_KEY="your-z-ai-key"
./claude-glm-gateway
# or: ./claude-glm-gateway -config ~/.config/claude-glm-gateway/config.toml
```

## Run (background)

```bash
export ZAI_API_KEY="your-z-ai-key"
nohup ./claude-glm-gateway >~/.local/state/claude-glm-gateway/gateway.log 2>&1 &
echo $! > ~/.local/state/claude-glm-gateway/gateway.pid
```

Stop:

```bash
kill "$(cat ~/.local/state/claude-glm-gateway/gateway.pid)"
```

## Point Claude Code at the gateway

In the shell where you launch Claude Code (or in `~/.claude/settings.json` `env`):

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
# Do NOT set ANTHROPIC_AUTH_TOKEN to the Z.ai key — keep subscription OAuth as usual.
claude
```

Example `~/.claude/settings.json` fragment:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"
  }
}
```

## Verify

```bash
curl -s http://127.0.0.1:8787/_gateway/health
curl -s http://127.0.0.1:8787/_gateway/status | jq .
```

`/_gateway/status` shows breaker state per model, `until`, session request counts
per provider, and the last quota event (no credentials).

Structured logs (stdout) include one line per request (`provider`, `status`,
`failover`) and a loud `BREAKER OPEN|PROBE|CLOSED …` line on every transition.

Upstream non-2xx responses are captured (redacted) to:

`~/.local/state/claude-glm-gateway/upstream-errors.jsonl`

### Prompt caching smoke check

With the gateway on Anthropic (breaker CLOSED), repeat a large stable-prefix
request and confirm `usage.cache_read_input_tokens` is non-zero. The gateway does
not reserialize JSON on the Anthropic path.

## Turn it off

1. Stop the gateway process.  
2. Unset `ANTHROPIC_BASE_URL` (or remove it from `settings.json`).  
3. Restart Claude Code / open a new terminal.

## Model mapping

Claude Code sends Anthropic model IDs. When the breaker is OPEN the gateway
rewrites only the JSON `model` field using `[glm.model_map]` / `default_model`
(see `config.example.toml`). Default here: opus/sonnet → `glm-5.2`, haiku →
`glm-4.5-air`.

## Tests

```bash
go test ./...
```

## Non-goals

No cost dashboards, no mid-stream provider splicing, no response caching, no
non-loopback bind, no gateway auth beyond localhost.
