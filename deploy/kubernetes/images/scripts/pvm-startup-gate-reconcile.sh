#!/bin/sh
set -eu

log() { printf '[pvm-startup-gate] %s\n' "$*"; }
fail() { printf '[pvm-startup-gate] ERROR: %s\n' "$*" >&2; exit 1; }

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/node-prep-lib.sh"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/pvm-startup-gate-lib.sh"

NODE_NAME="${NODE_NAME:-$(hostname)}"
NAMESPACE="${POD_NAMESPACE:-default}"
INTERVAL="${STARTUP_GATE_RECONCILE_INTERVAL_SECONDS:-30}"

if ! startup_gate_active; then
  log "startup gate disabled; holding"
  exec sleep infinity
fi

reconcile_startup_gate_once() {
  if pvm_host_fingerprint_matches_file; then
    clear_startup_gate_taint
  else
    ensure_startup_gate_taint
  fi
}

if [ "${STARTUP_GATE_RECONCILE_ONCE:-false}" = "true" ]; then
  reconcile_startup_gate_once
  exit 0
fi

while true; do
  reconcile_startup_gate_once
  sleep "$INTERVAL"
done
