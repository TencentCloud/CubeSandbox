#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
wait_for_http "http://${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}/healthz" 30 1 || die "network-agent healthz not ready"
