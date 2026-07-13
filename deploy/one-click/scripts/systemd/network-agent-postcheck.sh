#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

network_agent_tap_init_num() {
  local cfg="${CUBELET_CONFIG:-${TOOLBOX_ROOT}/Cubelet/config/config.toml}"
  local value=""

  if [[ -f "${cfg}" ]]; then
    value="$(awk '
      /^[[:space:]]*\[/ {
        in_network = ($0 ~ /^[[:space:]]*\[plugins\."io\.cubelet\.internal\.v1\.network"\][[:space:]]*$/)
      }
      in_network && /^[[:space:]]*tap_init_num[[:space:]]*=/ {
        line = $0
        sub(/#.*/, "", line)
        sub(/^[^=]*=/, "", line)
        gsub(/[[:space:]]/, "", line)
        print line
        exit
      }
    ' "${cfg}")"
  fi

  if [[ "${value}" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "${value}"
  else
    printf '%s\n' "${NETWORK_AGENT_POSTCHECK_DEFAULT_TAP_INIT_NUM:-500}"
  fi
}

network_agent_postcheck_timeout() {
  local tap_init_num="$1"
  local base_taps="${NETWORK_AGENT_POSTCHECK_BASE_TAPS:-500}"
  local base_timeout="${NETWORK_AGENT_POSTCHECK_BASE_TIMEOUT:-300}"
  local min_timeout="${NETWORK_AGENT_POSTCHECK_MIN_TIMEOUT:-30}"
  local timeout

  if [[ -n "${NETWORK_AGENT_POSTCHECK_TIMEOUT:-}" ]]; then
    [[ "${NETWORK_AGENT_POSTCHECK_TIMEOUT}" =~ ^[0-9]+$ ]] || die "invalid NETWORK_AGENT_POSTCHECK_TIMEOUT: ${NETWORK_AGENT_POSTCHECK_TIMEOUT}"
    printf '%s\n' "${NETWORK_AGENT_POSTCHECK_TIMEOUT}"
    return 0
  fi

  [[ "${tap_init_num}" =~ ^[0-9]+$ ]] || tap_init_num="${NETWORK_AGENT_POSTCHECK_DEFAULT_TAP_INIT_NUM:-500}"
  [[ "${base_taps}" =~ ^[1-9][0-9]*$ ]] || die "invalid NETWORK_AGENT_POSTCHECK_BASE_TAPS: ${base_taps}"
  [[ "${base_timeout}" =~ ^[1-9][0-9]*$ ]] || die "invalid NETWORK_AGENT_POSTCHECK_BASE_TIMEOUT: ${base_timeout}"
  [[ "${min_timeout}" =~ ^[0-9]+$ ]] || die "invalid NETWORK_AGENT_POSTCHECK_MIN_TIMEOUT: ${min_timeout}"

  timeout=$(( (tap_init_num * base_timeout + base_taps - 1) / base_taps ))
  if (( timeout < min_timeout )); then
    timeout="${min_timeout}"
  fi
  printf '%s\n' "${timeout}"
}

tap_init_num="$(network_agent_tap_init_num)"
timeout="$(network_agent_postcheck_timeout "${tap_init_num}")"
health_url="http://${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}/healthz"

log "waiting for network-agent healthz: url=${health_url} tap_init_num=${tap_init_num} timeout=${timeout}s"
wait_for_http "${health_url}" "${timeout}" 1 "--max-time 1" || die "network-agent healthz not ready in ${timeout}s"
