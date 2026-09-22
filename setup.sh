#!/usr/bin/env bash
# One command: build, save the Z.ai key (and optional TypeSafe key), point Claude Code at the gateway,
# and keep it running (macOS LaunchAgent, or nohup elsewhere).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${ROOT}/scripts/lib.sh"

ensure_go
build_gateway
build_conduitctl
ensure_config
ensure_env
# Optional: TYPESAFE_API_KEY for Jev routing mode (CONDUIT_SKIP_JEV_PROMPT=1 skips).
ensure_jev_env
patch_claude_settings
# Opt-in: CONDUIT_WIRE_OPENCODE=1 adds an OpenCode provider entry too.
if [[ "${CONDUIT_WIRE_OPENCODE:-0}" == "1" ]]; then
	patch_opencode_settings
fi

if is_macos; then
	install_macos_service
else
	start_nohup
fi
wait_healthy
