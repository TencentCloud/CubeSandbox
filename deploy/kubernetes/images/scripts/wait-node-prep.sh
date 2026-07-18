#!/bin/sh
set -eu

# Big Pod REV3.2.1: Kruise high-priority sidecar (not an initContainer).
# Poll hostPath node-prep-ready until fingerprint matches, mark Ready, and
# KEEP polling so mutate/reboot that invalidates the sentinel clears readiness.
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
became_ready=0
while true; do
  if node_prep_fingerprint_matches_file; then
    if [ ! -f "$WAIT_READY_MARKER" ]; then
      log "node-prep-ready fingerprint matched; marking ready and continuing to re-validate"
      became_ready=1
    fi
    : > "$WAIT_READY_MARKER"
  else
    if [ -f "$WAIT_READY_MARKER" ]; then
      log "node-prep-ready lost or fingerprint mismatch; clearing readiness marker"
      rm -f "$WAIT_READY_MARKER"
    fi
    if [ "$became_ready" = "0" ]; then
      now="$(date +%s)"
      elapsed=$((now - start))
      if [ "$elapsed" -ge "$WAIT_TIMEOUT_SECONDS" ]; then
        if [ -f "$ready" ]; then
          printf '[wait-node-prep] ERROR: timeout after %ss; sentinel present but fingerprint mismatch\n' "$elapsed" >&2
          printf '%s\n' '--- want ---' >&2
          node_prep_compute_fingerprint >&2
          printf '%s\n' '--- have ---' >&2
          cat "$ready" >&2
          exit 1
        fi
        fail "timeout after ${elapsed}s; ${ready} not ready"
      fi
    fi
  fi
  sleep "$WAIT_POLL_SECONDS"
done
