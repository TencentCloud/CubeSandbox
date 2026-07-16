#!/bin/sh
set -eu

# Big Pod REV3.2.1: Kruise high-priority sidecar (not an initContainer).
# Poll hostPath node-prep-ready until fingerprint matches, then mark Ready and
# sleep forever so Container Launch Priority can release lower-priority containers.
# Does NOT watch Installer Pod Ready.
# Day-1: freeze wait env/mounts; bumping only images.waitNodePrep may InPlace.

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/node-prep-lib.sh"

log() { printf '[wait-node-prep] %s\n' "$*"; }
fail() { printf '[wait-node-prep] ERROR: %s\n' "$*" >&2; exit 1; }

WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-600}"
WAIT_POLL_SECONDS="${WAIT_POLL_SECONDS:-2}"
WAIT_READY_MARKER="${WAIT_READY_MARKER:-/run/wait-node-prep.ready}"

ready="$(node_prep_ready_path)"
rm -f "$WAIT_READY_MARKER"
log "waiting for ${ready} (timeout=${WAIT_TIMEOUT_SECONDS}s)"
start="$(date +%s)"
while true; do
  if node_prep_fingerprint_valid_for_wait; then
    log "node-prep-ready fingerprint matched; marking ready and holding"
    : > "$WAIT_READY_MARKER"
    exec sleep infinity
  fi
  now="$(date +%s)"
  elapsed=$((now - start))
  if [ "$elapsed" -ge "$WAIT_TIMEOUT_SECONDS" ]; then
    if [ -f "$ready" ]; then
      printf '[wait-node-prep] ERROR: timeout after %ss; sentinel present but fingerprint mismatch\n' "$elapsed" >&2
      printf '--- want ---\n' >&2
      node_prep_compute_fingerprint >&2
      printf '--- have ---\n' >&2
      cat "$ready" >&2
      exit 1
    fi
    fail "timeout after ${elapsed}s; ${ready} not ready"
  fi
  sleep "$WAIT_POLL_SECONDS"
done
