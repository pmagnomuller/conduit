#!/usr/bin/env bash
# Deprecated: ./setup.sh now installs the LaunchAgent.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "$ROOT/setup.sh"
