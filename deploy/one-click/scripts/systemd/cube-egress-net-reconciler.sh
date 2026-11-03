#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Long-running reconciler for cube-egress host-side network rules.
# Runs as the ExecStart of cube-sandbox-cube-egress-net.service
# (Type=simple). Waits for the cube-dev interface to appear (created
# by Cubelet's embedded network runtime), then applies the idempotent
# iptables/ip-rule setup and periodically rechecks for drift.
#
# Readiness reporting:
#   STATUS=waiting    — cube-dev not present yet
#   STATUS=ready      — rules applied and verified
#   STATUS=reconciled — drift detected, rules reapplied
# The companion cube-egress-net-check.sh can be used as a real
# readiness check command (exit 0 only when rules are live).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-${TOOLBOX_ROOT}/scripts/cube-egress/cube-egress-iptables-init.sh}"
ensure_executable "${INIT_SCRIPT}"

INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"
POLL_INTERVAL="${CUBE_EGRESS_RECONCILE_INTERVAL:-5}"
APPLIED=false

notify() {
  if command -v systemd-notify >/dev/null 2>&1; then
    systemd-notify "STATUS=$1" 2>/dev/null || true
  fi
  log "reconciler: $1"
}

iface_present() {
  ip link show "${INGRESS_IFACE}" >/dev/null 2>&1
}

rules_applied() {
  # The init script is idempotent; "up" against an already-configured
  # host is a fast no-op. We detect drift by checking for the presence
  # of the TPROXY mangle chain that the init script creates.
  iptables -t mangle -n -L CUBE_EGRESS 2>/dev/null | grep -q "TPROXY"
}

apply_rules() {
  "${INIT_SCRIPT}" up
}

# --- main loop ---

# Initial wait: give cube-dev a short window to appear synchronously,
# covering the common case where services restart while sandboxes
# already exist. This avoids a flash of "waiting" on every reboot.
for _ in {1..10}; do
  iface_present && break
  sleep 1
done

while true; do
  if iface_present; then
    if ! rules_applied; then
      apply_rules
      APPLIED=true
      notify "ready (cube-dev present, rules applied)"
    elif [[ "${APPLIED}" == false ]]; then
      APPLIED=true
      notify "ready (cube-dev present, rules already in place)"
    else
      # Periodic drift check: rules could have been flushed externally.
      # The init script is idempotent, so calling it again is safe.
      apply_rules
      notify "reconciled (periodic check)"
    fi
  else
    if [[ "${APPLIED}" == true ]]; then
      APPLIED=false
      notify "waiting (cube-dev disappeared)"
    else
      notify "waiting (cube-dev not yet present)"
    fi
  fi
  sleep "${POLL_INTERVAL}"
done
