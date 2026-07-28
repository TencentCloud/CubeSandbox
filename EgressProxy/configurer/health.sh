#!/bin/sh
set -eu

STATE_DIR="${CUBE_EGRESS_STATE_DIR:-/run/cube-egress}"
RECONCILE_INTERVAL_SECONDS="${CUBE_EGRESS_RECONCILE_INTERVAL_SECONDS:-10}"
ready_file="${STATE_DIR}/ready"

test -f "${ready_file}"
last_success="$(cat "${ready_file}")"
case "${last_success}" in
  '' | *[!0-9]*) exit 1 ;;
esac
now="$(date +%s)"
max_age=$((RECONCILE_INTERVAL_SECONDS * 3 + 5))
[ $((now - last_success)) -le "${max_age}" ]
