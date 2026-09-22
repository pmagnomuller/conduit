#!/usr/bin/env bash
# Shared helpers for setup/start/stop/status/uninstall.
# shellcheck disable=SC2034

if [[ -z "${ROOT:-}" ]]; then
	ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

APP_NAME="conduit"
LABEL="com.pedro.conduit"
BIN_DIR="${HOME}/.local/bin"
BIN="${BIN_DIR}/conduit"
# The control client (cmd/conduit). Deliberately NOT installed as `conduit`:
# scripts/conduit-run and the launchd plist exec that path as the gateway.
CTL_BIN="${BIN_DIR}/conduitctl"
CONFIG_DIR="${CONFIG_DIR:-${HOME}/.config/conduit}"
STATE_DIR="${STATE_DIR:-${HOME}/.local/state/conduit}"
CONFIG="${CONFIG_DIR}/config.toml"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/.env}"
CLAUDE_SETTINGS="${HOME}/.claude/settings.json"
GATEWAY_URL="http://127.0.0.1:8787"
LOCAL_TOKEN="${CONDUIT_LOCAL_TOKEN:-conduit-local}"
PLIST_SRC="${ROOT}/contrib/macos/${LABEL}.plist"
PLIST_DST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
LOGFILE="${STATE_DIR}/gateway.log"
KEY_PROMPT="Paste your Z.ai API key (input hidden)"
KEY_EMPTY_HINT="Re-run ./setup.sh after putting ZAI_API_KEY in ${ENV_FILE}"
JEV_PROMPT="TypeSafe API key for Jev routing mode (optional, Enter to skip):"
# Optional: launchd label of another gateway that must not share :8787.
# Export CONDUIT_OTHER_GATEWAY_LABEL to have setup stop it before install.
OTHER_LABEL="${CONDUIT_OTHER_GATEWAY_LABEL:-}"

ensure_go() {
	if command -v go >/dev/null 2>&1; then
		return 0
	fi
	echo "Go 1.26+ is required." >&2
	if command -v brew >/dev/null 2>&1; then
		echo "Install it with:  brew install go" >&2
	else
		echo "Install it from https://go.dev/dl/ then re-run." >&2
	fi
	return 1
}

build_gateway() {
	mkdir -p "$BIN_DIR" "$CONFIG_DIR" "$STATE_DIR"
	echo "Building ${BIN} …"
	(cd "$ROOT" && go build -o "$BIN" ./cmd/gateway)
}

# build_conduitctl installs the control client beside the gateway. Idempotent
# and non-interactive: an existing binary is simply replaced, and a failure to
# build it must not fail setup, since the gateway is the part that matters.
build_conduitctl() {
	mkdir -p "$BIN_DIR"
	(cd "$ROOT" && go build -o "$CTL_BIN" ./cmd/conduit) || {
		echo "warning: could not build ${CTL_BIN}; the gateway is unaffected" >&2
		return 0
	}
	echo "Building ${CTL_BIN} …"
}

# conduitctl_bin echoes the control client to use, if one is installed: either
# on PATH or at the path setup.sh installs it to. Empty when absent, which is
# how callers keep their pre-CLI behaviour.
conduitctl_bin() {
	# $CTL_BIN first, then PATH: `make conduitctl` leaves a ./conduitctl in the
	# repo root, and a repo dir on PATH would otherwise shadow the installed
	# binary with a stale build.
	if [[ -x "$CTL_BIN" ]]; then
		printf '%s' "$CTL_BIN"
		return 0
	fi
	command -v conduitctl 2>/dev/null || true
}

ensure_config() {
	mkdir -p "$CONFIG_DIR" "$STATE_DIR"
	if [[ ! -f "$CONFIG" ]]; then
		cp "${ROOT}/config.example.toml" "$CONFIG"
		echo "Wrote $CONFIG"
	fi
}

# shell_quote VALUE — single-quote a value for ENV_FILE, which the scripts
# source under `set -euo pipefail` and config.Load re-reads itself. Unquoted, a
# key or value containing $ or ` was expanded when sourced (and under `set -u`
# an unset variable aborted the read half way), which broke the next
# ./setup.sh. config.Load trims the surrounding quotes, so both readers see the
# same value.
shell_quote() {
	local v="$1"
	printf "'%s'" "${v//\'/\'\\\'\'}"
}

# upsert_env KEY VALUE — set KEY in ENV_FILE, preserving every other line.
# The plain `>` writes this replaces truncated the file, which dropped a
# previously saved TYPESAFE_API_KEY (or any other key) on the next ZAI re-save.
upsert_env() {
	local key="$1" val="$2" tmp
	mkdir -p "$CONFIG_DIR"
	touch "$ENV_FILE"
	tmp="$(mktemp "${ENV_FILE}.XXXXXX")"
	{
		grep -v "^${key}=" "$ENV_FILE" || true
		printf '%s=%s\n' "$key" "$(shell_quote "$val")"
	} >"$tmp"
	mv "$tmp" "$ENV_FILE"
	chmod 600 "$ENV_FILE"
}

# Copies a repo .env if present; otherwise creates from the example.
# Prompts on a TTY when the key is still empty.
ensure_env() {
	mkdir -p "$CONFIG_DIR"
	if [[ ! -f "$ENV_FILE" ]]; then
		if [[ -f "${ROOT}/.env" ]]; then
			cp "${ROOT}/.env" "$ENV_FILE"
		else
			cp "${ROOT}/.env.example" "$ENV_FILE"
		fi
		chmod 600 "$ENV_FILE"
		echo "Wrote $ENV_FILE"
	fi
	chmod 600 "$ENV_FILE" 2>/dev/null || true

	# shellcheck disable=SC1090
	set -a && source "$ENV_FILE" && set +a
	if [[ -z "${ZAI_API_KEY:-}" && -f "${ROOT}/.env" ]]; then
		# shellcheck disable=SC1091
		set -a && source "${ROOT}/.env" && set +a
		if [[ -n "${ZAI_API_KEY:-}" ]]; then
			upsert_env ZAI_API_KEY "$ZAI_API_KEY"
			echo "Copied ZAI_API_KEY into $ENV_FILE"
		fi
	fi
	if [[ -n "${ZAI_API_KEY:-}" ]]; then
		if ! grep -q '^ZAI_API_KEY=.' "$ENV_FILE" 2>/dev/null; then
			upsert_env ZAI_API_KEY "$ZAI_API_KEY"
			echo "Saved ZAI_API_KEY to $ENV_FILE"
		fi
		return 0
	fi
	if [[ ! -t 0 ]]; then
		echo "ZAI_API_KEY is empty in $ENV_FILE" >&2
		echo "$KEY_EMPTY_HINT" >&2
		return 1
	fi
	echo "$KEY_PROMPT"
	local key=""
	read -r -s key
	echo
	if [[ -z "$key" ]]; then
		echo "No key entered." >&2
		echo "$KEY_EMPTY_HINT" >&2
		return 1
	fi
	upsert_env ZAI_API_KEY "$key"
	export ZAI_API_KEY="$key"
}

# Optional TypeSafe key enabling Jev routing mode. Prompts only on a TTY when
# TYPESAFE_API_KEY is absent from both the environment and ENV_FILE (ensure_env
# has already sourced it). Appends a single line, never rewrites other lines,
# never echoes the key. CONDUIT_SKIP_JEV_PROMPT=1 skips. Idempotent: a declined
# prompt is persisted as CONDUIT_SKIP_JEV_PROMPT=1 in ENV_FILE, so setup never
# asks twice.
ensure_jev_env() {
	if [[ "${CONDUIT_SKIP_JEV_PROMPT:-0}" == "1" ]]; then
		return 0
	fi
	if [[ -f "$ENV_FILE" ]] && grep -qE "^CONDUIT_SKIP_JEV_PROMPT=['\"]?1" "$ENV_FILE" 2>/dev/null; then
		return 0
	fi
	if [[ -n "${TYPESAFE_API_KEY:-}" ]]; then
		return 0
	fi
	if [[ -f "$ENV_FILE" ]] && grep -q '^TYPESAFE_API_KEY=.' "$ENV_FILE" 2>/dev/null; then
		return 0
	fi
	if [[ ! -t 0 ]]; then
		return 0
	fi
	echo "$JEV_PROMPT"
	local key=""
	read -r -s key
	echo
	if [[ -z "$key" ]]; then
		upsert_env CONDUIT_SKIP_JEV_PROMPT 1
		echo "Skipped. Set TYPESAFE_API_KEY in $ENV_FILE later to enable Jev mode (drop the CONDUIT_SKIP_JEV_PROMPT line to be asked again)."
		return 0
	fi
	upsert_env TYPESAFE_API_KEY "$key"
	export TYPESAFE_API_KEY="$key"
	echo "Saved TYPESAFE_API_KEY to $ENV_FILE (Jev routing mode available)"
}

patch_claude_settings() {
	python3 - "$CLAUDE_SETTINGS" "$GATEWAY_URL" <<'PY'
import json, pathlib, shutil, sys

path = pathlib.Path(sys.argv[1])
url = sys.argv[2]
path.parent.mkdir(parents=True, exist_ok=True)

data = {}
if path.exists() and path.stat().st_size > 0:
    try:
        data = json.loads(path.read_text())
        if not isinstance(data, dict):
            raise ValueError("settings.json root must be an object")
    except Exception as exc:
        print(f"Could not update {path}: {exc}", file=sys.stderr)
        print(f'Set ANTHROPIC_BASE_URL to "{url}" in that file, then restart Claude Code.', file=sys.stderr)
        sys.exit(0)

env = data.setdefault("env", {})
if not isinstance(env, dict):
    env = {}
    data["env"] = env

if env.get("ANTHROPIC_BASE_URL") == url:
    print(f"Claude Code already points at {url}")
    sys.exit(0)

if path.exists():
    shutil.copy2(path, path.with_suffix(".json.bak"))

env["ANTHROPIC_BASE_URL"] = url
path.write_text(json.dumps(data, indent=2) + "\n")
print(f"Set ANTHROPIC_BASE_URL in {path} (backup: {path.with_suffix('.json.bak')})")
PY
}

OPENCODE_CONFIG="${XDG_CONFIG_HOME:-${HOME}/.config}/opencode/opencode.json"

# patch_opencode_settings adds an anthropic provider entry pointing OpenCode at
# the gateway using the local marker token (routed to GLM by default). Noop
# when OpenCode is not installed or already points at the gateway.
patch_opencode_settings() {
	python3 - "$OPENCODE_CONFIG" "$GATEWAY_URL" "$LOCAL_TOKEN" <<'PY'
import json, pathlib, sys

path, url, token = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
if not path.exists():
    print(f"No {path} — skipping OpenCode wiring")
    sys.exit(0)

try:
    data = json.loads(path.read_text())
    if not isinstance(data, dict):
        raise ValueError("root must be an object")
except Exception as exc:
    print(f"Could not update {path}: {exc}", file=sys.stderr)
    sys.exit(0)

provider = data.get("provider")
if not isinstance(provider, dict):
    provider = {}
    data["provider"] = provider

entry = provider.get("anthropic")
if isinstance(entry, dict) and entry.get("options", {}).get("baseURL") == url:
    print(f"OpenCode already points at {url}")
    sys.exit(0)

if entry is not None:
    backup = path.with_suffix(".json.bak")
    backup.write_text(path.read_text())
    print(f"Backed up existing anthropic provider entry to {backup}")

provider["anthropic"] = {
    "options": {"baseURL": url, "apiKey": token},
    "models": {
        "claude-opus-5": {},
        "claude-sonnet-5": {},
        "claude-haiku-4-5": {},
    },
}
path.write_text(json.dumps(data, indent=2) + "\n")
print(f"Wired OpenCode anthropic provider to {url} (local token)")
PY
}

unpatch_opencode_settings() {
	python3 - "$OPENCODE_CONFIG" "$GATEWAY_URL" <<'PY'
import json, pathlib, sys

path, url = pathlib.Path(sys.argv[1]), sys.argv[2]
if not path.exists():
    sys.exit(0)
try:
    data = json.loads(path.read_text())
except Exception:
    sys.exit(0)
provider = data.get("provider")
if not isinstance(provider, dict):
    sys.exit(0)
entry = provider.get("anthropic")
if not isinstance(entry, dict) or entry.get("options", {}).get("baseURL") != url:
    sys.exit(0)
del provider["anthropic"]
if not provider:
    data.pop("provider", None)
path.write_text(json.dumps(data, indent=2) + "\n")
print("Removed conduit anthropic provider from OpenCode")
PY
}

unpatch_claude_settings() {
	python3 - "$CLAUDE_SETTINGS" "$GATEWAY_URL" <<'PY'
import json, pathlib, sys

path = pathlib.Path(sys.argv[1])
url = sys.argv[2]
if not path.exists():
    print(f"No {path} to edit")
    sys.exit(0)
try:
    data = json.loads(path.read_text() or "{}")
except Exception as exc:
    print(f"Could not read {path}: {exc}", file=sys.stderr)
    sys.exit(0)
env = data.get("env")
if not isinstance(env, dict) or env.get("ANTHROPIC_BASE_URL") != url:
    print(f"ANTHROPIC_BASE_URL in {path} was not {url}; left unchanged")
    sys.exit(0)
del env["ANTHROPIC_BASE_URL"]
if not env:
    data.pop("env", None)
path.write_text(json.dumps(data, indent=2) + "\n")
print(f"Removed ANTHROPIC_BASE_URL from {path}")
PY
}

is_macos() {
	[[ "$(uname -s)" == Darwin ]]
}

launch_domain() {
	echo "gui/$(id -u)"
}

install_macos_service() {
	mkdir -p "${HOME}/Library/LaunchAgents" "$STATE_DIR"
	if [[ ! -f "$PLIST_SRC" ]]; then
		echo "Missing $PLIST_SRC" >&2
		return 1
	fi
	sed "s|@HOME@|${HOME}|g" "$PLIST_SRC" >"$PLIST_DST"
	local domain
	domain="$(launch_domain)"
	if [[ -n "${OTHER_LABEL:-}" ]] && launchctl print "${domain}/${OTHER_LABEL}" >/dev/null 2>&1; then
		launchctl bootout "${domain}/${OTHER_LABEL}" >/dev/null 2>&1 || true
		echo "Stopped ${OTHER_LABEL} (only one gateway can bind :8787)"
	fi
	if launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
		launchctl bootout "${domain}/${LABEL}" >/dev/null 2>&1 || true
		local i
		for i in 1 2 3 4 5 6 7 8 9 10; do
			if ! launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
				break
			fi
			sleep 0.2
		done
	fi
	if ! launchctl bootstrap "$domain" "$PLIST_DST"; then
		sleep 0.5
		launchctl bootstrap "$domain" "$PLIST_DST"
	fi
	launchctl enable "${domain}/${LABEL}"
	launchctl kickstart -k "${domain}/${LABEL}"
	echo "Installed ${LABEL} (starts at login, restarts if it dies)"
}

start_nohup() {
	local pidfile="${STATE_DIR}/gateway.pid"
	mkdir -p "$STATE_DIR"
	if [[ -f "$pidfile" ]]; then
		local old
		old="$(cat "$pidfile")"
		if kill -0 "$old" 2>/dev/null; then
			echo "Already running (pid $old) at $GATEWAY_URL"
			return 0
		fi
		rm -f "$pidfile"
	fi
	nohup "$BIN" -config "$CONFIG" >"$LOGFILE" 2>&1 &
	echo $! >"$pidfile"
}

stop_nohup() {
	local pidfile="${STATE_DIR}/gateway.pid"
	if [[ ! -f "$pidfile" ]]; then
		echo "Gateway is not running."
		return 0
	fi
	local pid
	pid="$(cat "$pidfile")"
	if kill -0 "$pid" 2>/dev/null; then
		kill "$pid"
		echo "Stopped gateway (pid $pid)."
	else
		echo "Gateway was not running (stale pid file)."
	fi
	rm -f "$pidfile"
}

start_service() {
	if [[ ! -x "$BIN" ]]; then
		echo "Gateway is not installed. Run ./setup.sh first." >&2
		return 1
	fi
	if is_macos; then
		local domain
		domain="$(launch_domain)"
		if [[ ! -f "$PLIST_DST" ]]; then
			install_macos_service
		else
			if ! launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
				launchctl bootstrap "$domain" "$PLIST_DST"
				launchctl enable "${domain}/${LABEL}"
			fi
			launchctl kickstart -k "${domain}/${LABEL}"
		fi
	else
		start_nohup
	fi
	wait_healthy
}

stop_service() {
	if is_macos; then
		local domain
		domain="$(launch_domain)"
		if launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
			launchctl bootout "${domain}/${LABEL}" >/dev/null 2>&1 || true
			echo "Stopped ${LABEL}."
			return 0
		fi
		echo "Gateway is not running."
		return 0
	fi
	stop_nohup
}

wait_healthy() {
	local i
	for i in $(seq 1 25); do
		if curl -sf "${GATEWAY_URL}/_gateway/health" >/dev/null 2>&1; then
			echo "Gateway running at ${GATEWAY_URL}"
			echo "Restart Claude Code if this is the first time (open sessions keep the old URL)."
			echo "Logs: tail -f ${LOGFILE}"
			return 0
		fi
		sleep 0.2
	done
	echo "Started but health check failed. See ${LOGFILE}" >&2
	return 1
}

status_service() {
	if curl -sf "${GATEWAY_URL}/_gateway/health" >/dev/null 2>&1; then
		echo "health: ok  (${GATEWAY_URL})"
		local ctl
		ctl="$(conduitctl_bin)"
		if [[ -n "$ctl" ]]; then
			# The CLI reads the same endpoint and needs no jq; fall back to
			# the raw payload plus the jq block when it is not installed.
			"$ctl" status || true
		else
			local st
			st="$(curl -s "${GATEWAY_URL}/_gateway/status" 2>/dev/null || true)"
			printf '%s\n' "$st"
			# mode / jev_enabled are newer fields; print only when present and jq exists.
			if [[ -n "$st" ]] && command -v jq >/dev/null 2>&1; then
				local mode jev
				mode="$(printf '%s' "$st" | jq -r '.mode // empty' 2>/dev/null || true)"
				jev="$(printf '%s' "$st" | jq -r 'if has("jev_enabled") then (.jev_enabled|tostring) else empty end' 2>/dev/null || true)"
				if [[ -n "$mode" ]]; then
					echo "mode: ${mode}"
				fi
				if [[ -n "$jev" ]]; then
					echo "jev_enabled: ${jev}"
				fi
			fi
		fi
	else
		echo "health: down  (${GATEWAY_URL})"
	fi
	if is_macos; then
		local domain
		domain="$(launch_domain)"
		if launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
			echo "service: loaded (${LABEL})"
		else
			echo "service: not loaded (${LABEL})"
		fi
	fi
}

uninstall_service() {
	stop_service || true
	if is_macos; then
		rm -f "$PLIST_DST"
	fi
	unpatch_claude_settings
	# conduitctl is new and ours: clear it, leaving the gateway binary and
	# CONFIG_DIR handling exactly as they were before.
	rm -f "$CTL_BIN"
	echo "Left ${CONFIG_DIR} in place (contains your key). Delete it by hand if you want a clean slate."
	echo "Restart Claude Code so it stops using ${GATEWAY_URL}."
}
