#!/bin/sh
set -eu

# Write node-prep-ready after bootstrap inits (or alone when prep is noop).
# Then sleep forever so the bootstrap DaemonSet stays Running.

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
. "${SCRIPT_DIR}/node-prep-lib.sh"

log() { printf '[write-node-prep-ready] %s\n' "$*"; }

log "computing fingerprint (mode=$(node_prep_resolve_mode))"
write_node_prep_ready
log "node prep ready; holding bootstrap pod"
exec sleep infinity
