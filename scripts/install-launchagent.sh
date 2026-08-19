#!/usr/bin/env bash
# Install conduit as a macOS LaunchAgent: starts at login, restarts if it dies.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.pedro.conduit"
UID_NUM="$(id -u)"
DOMAIN="gui/${UID_NUM}"
BIN_DIR="${HOME}/.local/bin"
CONFIG_DIR="${HOME}/.config/conduit"
STATE_DIR="${HOME}/.local/state/conduit"
PLIST_SRC="${ROOT}/contrib/macos/${LABEL}.plist"
PLIST_DST="${HOME}/Library/LaunchAgents/${LABEL}.plist"

mkdir -p "$BIN_DIR" "$CONFIG_DIR" "$STATE_DIR" "${HOME}/Library/LaunchAgents"

echo "Building ${BIN_DIR}/conduit …"
(cd "$ROOT" && go build -o "${BIN_DIR}/conduit" ./cmd/gateway)

install -m 755 "${ROOT}/scripts/conduit-run" "${BIN_DIR}/conduit-run"

if [[ ! -f "${CONFIG_DIR}/config.toml" ]]; then
	cp "${ROOT}/config.example.toml" "${CONFIG_DIR}/config.toml"
	echo "Wrote ${CONFIG_DIR}/config.toml"
fi

if [[ ! -f "${CONFIG_DIR}/.env" ]]; then
	if [[ -f "${ROOT}/.env" ]]; then
		cp "${ROOT}/.env" "${CONFIG_DIR}/.env"
		chmod 600 "${CONFIG_DIR}/.env"
		echo "Wrote ${CONFIG_DIR}/.env"
	else
		echo "Missing ${CONFIG_DIR}/.env (copy ${ROOT}/.env.example and set ZAI_API_KEY)" >&2
		exit 1
	fi
fi

sed "s|@HOME@|${HOME}|g" "$PLIST_SRC" > "$PLIST_DST"

if launchctl print "${DOMAIN}/${LABEL}" >/dev/null 2>&1; then
	launchctl bootout "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || true
fi
launchctl bootstrap "$DOMAIN" "$PLIST_DST"
launchctl enable "${DOMAIN}/${LABEL}"
launchctl kickstart -k "${DOMAIN}/${LABEL}"

echo "Installed ${LABEL} (RunAtLoad + KeepAlive)"
echo "Logs: ${STATE_DIR}/gateway.log"
echo "Stop: launchctl bootout ${DOMAIN}/${LABEL}"
