#!/bin/sh
set -eu

# Bootstrap init: decide per-node PVM mode and gate on fingerprint-matched
# pvm-host-ready (never "file exists"). Writes effective-pvm for downstream
# node-init / fingerprint / cubelet guest selection.

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/node-prep-lib.sh"

log() { printf '[wait-pvm-host] %s\n' "$*"; }
fail() { printf '[wait-pvm-host] ERROR: %s\n' "$*" >&2; exit 1; }

PVM_FEATURE_ENABLED="${PVM_FEATURE_ENABLED:-0}"
# When chart-managed host bootstrap is off, still honor guest PVM expectation
# for externally pre-provisioned PVM hosts (CUBE_PVM_ENABLE / pvmGuestKernel).
CUBE_PVM_ENABLE="${CUBE_PVM_ENABLE:-0}"
ALLOW_PVM_LABEL="${ALLOW_PVM_LABEL:-cube.tencent.com/allow-pvm-bootstrap}"
NODE_NAME="${NODE_NAME:-}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-900}"
WAIT_POLL_SECONDS="${WAIT_POLL_SECONDS:-2}"
export HOST_ROOT STATE_DIR DESIRED_KERNEL_PATTERN KERNEL_BOOT_ARGS

finish_external_or_disabled() {
  # Chart-managed cube-node-pvm is off.
  if [ "$(node_prep_bool01 "$CUBE_PVM_ENABLE")" != "1" ]; then
    log "PVM host bootstrap disabled and guest PVM off; effective-pvm=0"
    write_effective_pvm 0
    exit 0
  fi
  # External / pre-provisioned PVM: require live kernel, write fingerprint gate.
  PVM_ENABLED=1
  export PVM_ENABLED
  log "PVM host bootstrap disabled but CUBE_PVM_ENABLE=1; requiring live PVM kernel (external prep)"
  if ! node_prep_kernel_ready; then
    fail "CUBE_PVM_ENABLE=1 with bootstrap.pvmHostKernel.enabled=false requires a running PVM host kernel matching DESIRED_KERNEL_PATTERN=${DESIRED_KERNEL_PATTERN} and KERNEL_BOOT_ARGS"
  fi
  write_pvm_host_ready || fail "failed to write pvm-host-ready for external PVM host"
  write_effective_pvm 1
  exit 0
}

# Fail-closed node label lookup. Use go-template index() so dotted label keys
# work; jsonpath {.metadata.labels['a.b/c']} is unreliable for this key shape.
node_allows_pvm() {
  [ -n "$NODE_NAME" ] || fail "NODE_NAME is required"
  command -v kubectl >/dev/null 2>&1 || fail "kubectl is required in cube-node-init image for wait-pvm-host"
  # shellcheck disable=SC2016
  label_val="$(kubectl get node "$NODE_NAME" -o go-template='{{index .metadata.labels "'"${ALLOW_PVM_LABEL}"'"}}{{"\n"}}')" \
    || fail "kubectl get node ${NODE_NAME} failed (API/RBAC); refusing to treat as non-PVM"
  [ "$(node_prep_bool01 "$label_val")" = "1" ]
}

wait_for_pvm_host_ready() {
  ready="$(pvm_host_ready_path)"
  log "PVM node ${NODE_NAME}; waiting for fingerprint-matched ${ready} (timeout=${WAIT_TIMEOUT_SECONDS}s)"
  start="$(date +%s)"
  while true; do
    if pvm_host_fingerprint_matches_file; then
      log "pvm-host-ready fingerprint matched live kernel; effective-pvm=1"
      write_effective_pvm 1
      exit 0
    fi
    now="$(date +%s)"
    elapsed=$((now - start))
    if [ "$elapsed" -ge "$WAIT_TIMEOUT_SECONDS" ]; then
      if [ -f "$ready" ]; then
        printf '[wait-pvm-host] ERROR: timeout after %ss; sentinel present but fingerprint/live mismatch\n' "$elapsed" >&2
        printf '%s\n' '--- want ---' >&2
        pvm_host_compute_fingerprint >&2
        printf '%s\n' '--- have ---' >&2
        cat "$ready" >&2 || true
        printf '%s\n' '--- live kernel ---' >&2
        uname -r >&2 || true
        if pvm_is_mutating; then
          printf '%s\n' '--- pvm-mutating present ---' >&2
        fi
        exit 1
      fi
      fail "timeout after ${elapsed}s; ${ready} not ready (cube-node-pvm may still be installing/rebooting)"
    fi
    sleep "$WAIT_POLL_SECONDS"
  done
}

if [ "$(node_prep_bool01 "$PVM_FEATURE_ENABLED")" != "1" ]; then
  finish_external_or_disabled
fi

PVM_ENABLED=1
export PVM_ENABLED

if ! node_allows_pvm; then
  log "node ${NODE_NAME} lacks ${ALLOW_PVM_LABEL}=true; skip PVM gate; effective-pvm=0"
  write_effective_pvm 0
  exit 0
fi

wait_for_pvm_host_ready
