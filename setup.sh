#!/usr/bin/env bash
# One command: build, save the Z.ai key, point Claude Code at the gateway,
# and keep it running (macOS LaunchAgent, or nohup elsewhere).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${ROOT}/scripts/lib.sh"

ensure_go
build_gateway
ensure_config
ensure_env
patch_claude_settings

if is_macos; then
	install_macos_service
else
	start_nohup
fi
wait_healthy
