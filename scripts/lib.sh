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
CONFIG_DIR="${HOME}/.config/conduit"
STATE_DIR="${HOME}/.local/state/conduit"
CONFIG="${CONFIG_DIR}/config.toml"
ENV_FILE="${CONFIG_DIR}/.env"
CLAUDE_SETTINGS="${HOME}/.claude/settings.json"
GATEWAY_URL="http://127.0.0.1:8787"
PLIST_SRC="${ROOT}/contrib/macos/${LABEL}.plist"
PLIST_DST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
LOGFILE="${STATE_DIR}/gateway.log"
KEY_PROMPT="Paste your Z.ai API key (input hidden)"
KEY_EMPTY_HINT="Re-run ./setup.sh after putting ZAI_API_KEY in ${ENV_FILE}"

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

ensure_config() {
	mkdir -p "$CONFIG_DIR" "$STATE_DIR"
	if [[ ! -f "$CONFIG" ]]; then
		cp "${ROOT}/config.example.toml" "$CONFIG"
		echo "Wrote $CONFIG"
	fi
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
	if [[ -n "${ZAI_API_KEY:-}" ]]; then
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
	printf 'ZAI_API_KEY=%s\n' "$key" >"$ENV_FILE"
	chmod 600 "$ENV_FILE"
	export ZAI_API_KEY="$key"
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
	install -m 755 "${ROOT}/scripts/conduit-run" "${BIN_DIR}/conduit-run"
	local domain
	domain="$(launch_domain)"
	if launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
		launchctl bootout "${domain}/${LABEL}" >/dev/null 2>&1 || true
	fi
	launchctl bootstrap "$domain" "$PLIST_DST"
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
			return
		fi
		if ! launchctl print "${domain}/${LABEL}" >/dev/null 2>&1; then
			launchctl bootstrap "$domain" "$PLIST_DST"
			launchctl enable "${domain}/${LABEL}"
		fi
		launchctl kickstart -k "${domain}/${LABEL}"
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
		curl -s "${GATEWAY_URL}/_gateway/status" 2>/dev/null || true
		echo
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
	echo "Left ${CONFIG_DIR} in place (contains your key). Delete it by hand if you want a clean slate."
	echo "Restart Claude Code so it stops using ${GATEWAY_URL}."
}
