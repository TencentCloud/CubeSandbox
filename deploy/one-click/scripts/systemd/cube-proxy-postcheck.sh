#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

postcheck_port="${CUBE_PROXY_HTTP_PORT:-80}"
postcheck_retries="${CUBE_PROXY_POSTCHECK_RETRIES:-30}"
postcheck_delay="${CUBE_PROXY_POSTCHECK_DELAY:-2}"
deprecated_host_port="${CUBE_PROXY_HOST_PORT:-}"

if [[ -n "${deprecated_host_port}" ]]; then
  log "CUBE_PROXY_HOST_PORT is deprecated and ignored; set CUBE_PROXY_HTTP_PORT to change the post-start check port"
fi

log "checking cube-proxy HTTP tcp port ${postcheck_port}"
wait_for_tcp_port "${postcheck_port}" "${postcheck_retries}" "${postcheck_delay}" || die "cube-proxy HTTP tcp port not ready: ${postcheck_port}"
