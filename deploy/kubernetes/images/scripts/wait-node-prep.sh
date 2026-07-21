#!/bin/sh
set -eu

# Big Pod initContainer: poll hostPath node-prep-ready until fingerprint matches,
# then exit 0 so run containers can start. Does NOT watch Installer Pod Ready.

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/node-prep-lib.sh"

log() { printf '[wait-node-prep] %s\n' "$*"; }
fail() { printf '[wait-node-prep] ERROR: %s\n' "$*" >&2; exit 1; }

# Initial timeout is an image contract, not a Helm value.
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-600}"
WAIT_POLL_SECONDS="${WAIT_POLL_SECONDS:-2}"
WAIT_READY_MARKER="${WAIT_READY_MARKER:-/run/wait-node-prep.ready}"

ready="$(node_prep_ready_path)"
rm -f "$WAIT_READY_MARKER"
log "waiting for ${ready} (timeout=${WAIT_TIMEOUT_SECONDS}s)"
start="$(date +%s)"
while true; do
  if node_prep_host_sentinel_is_ready; then
    log "node-prep-ready fingerprint matched; exiting"
    : > "$WAIT_READY_MARKER"
    exit 0
  fi
  now="$(date +%s)"
  elapsed=$((now - start))
  if [ "$elapsed" -ge "$WAIT_TIMEOUT_SECONDS" ]; then
    if [ -f "$ready" ]; then
      printf '[wait-node-prep] ERROR: timeout after %ss; sentinel present but fingerprint mismatch\n' "$elapsed" >&2
      printf '%s\n' '--- host sentinel ---' >&2
      cat "$ready" >&2
      exit 1
    fi
    fail "timeout after ${elapsed}s; ${ready} not ready"
  fi
  sleep "$WAIT_POLL_SECONDS"
done
