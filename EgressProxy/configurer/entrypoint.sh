#!/bin/sh
set -eu

RECONCILE_INTERVAL_SECONDS="${CUBE_EGRESS_RECONCILE_INTERVAL_SECONDS:-10}"
STATE_DIR="${CUBE_EGRESS_STATE_DIR:-/run/cube-egress}"
CLEANUP_REQUEST_FILE="${STATE_DIR}/cleanup-requested"

log() {
  printf '[egress-configurer] %s\n' "$*"
}

rm -f "${CLEANUP_REQUEST_FILE}"

while true; do
  if [ -f "${CLEANUP_REQUEST_FILE}" ]; then
    rm -f "${STATE_DIR}/ready"
    sleep 1
    continue
  fi
  if /opt/cube-egress/reconcile.sh; then
    ready_tmp="${STATE_DIR}/ready.tmp.$$"
    printf '%s\n' "$(date +%s)" > "${ready_tmp}"
    mv -f "${ready_tmp}" "${STATE_DIR}/ready"
  else
    rm -f "${STATE_DIR}/ready"
    log "reconcile failed; retrying in ${RECONCILE_INTERVAL_SECONDS}s"
  fi
  sleep "${RECONCILE_INTERVAL_SECONDS}"
done
