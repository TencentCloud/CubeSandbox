#!/usr/bin/env bash
set -euo pipefail

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-/usr/local/bin/cube-proxy-iptables-init.sh}"
INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"
REAPPLY_INTERVAL_SECONDS="${CUBE_EGRESS_NET_REAPPLY_INTERVAL_SECONDS:-30}"
INITIAL_WAIT_SECONDS="${CUBE_EGRESS_NET_INITIAL_WAIT_SECONDS:-300}"

log() { printf '[cube-egress-net] %s
' "$*" >&2; }
fail() { printf '[cube-egress-net] ERROR: %s
' "$*" >&2; exit 1; }

[[ -x "${INIT_SCRIPT}" ]] || fail "missing executable: ${INIT_SCRIPT}"

cleanup() {
  log "removing CubeEgress transparent proxy rules"
  "${INIT_SCRIPT}" down || true
}
trap cleanup TERM INT HUP EXIT

wait_for_iface() {
  local waited=0
  while ! ip link show "${INGRESS_IFACE}" >/dev/null 2>&1; do
    if (( waited >= INITIAL_WAIT_SECONDS )); then
      log "interface ${INGRESS_IFACE} not present after ${INITIAL_WAIT_SECONDS}s; keep waiting"
      waited=0
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

apply_rules() {
  wait_for_iface
  log "applying CubeEgress transparent proxy rules on ${INGRESS_IFACE}"
  "${INIT_SCRIPT}" up
}

apply_rules
while true; do
  sleep "${REAPPLY_INTERVAL_SECONDS}"
  if ! ip link show "${INGRESS_IFACE}" >/dev/null 2>&1; then
    log "interface ${INGRESS_IFACE} disappeared; waiting before reapply"
    wait_for_iface
  fi
  # Idempotent and keeps rules present after node-level firewall reloads.
  "${INIT_SCRIPT}" up >/dev/null 2>&1 || log "WARN: rule reapply failed; will retry"
done
