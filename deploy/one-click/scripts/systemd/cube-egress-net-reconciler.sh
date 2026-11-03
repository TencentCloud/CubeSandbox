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

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-}"
if [[ -z "${INIT_SCRIPT}" ]]; then
  # Auto-detect: deployment versions name this script differently (egress vs proxy)
  for _candidate in     "${TOOLBOX_ROOT}/scripts/cube-egress/cube-egress-iptables-init.sh"     "${TOOLBOX_ROOT}/scripts/cube-egress/cube-proxy-iptables-init.sh"; do
    if [[ -x "${_candidate}" ]]; then
      INIT_SCRIPT="${_candidate}"
      break
    fi
  done
fi
if [[ -z "${INIT_SCRIPT}" ]]; then
  log "ERROR: no egress iptables init script found (tried cube-egress-* and cube-proxy-*)"
  exit 1
fi

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
  # Detect drift by checking for the TPROXY mangle chain.
  # The init script creates a chain named TRANSPROXY (auto-detected below);
  # we also probe CUBE_EGRESS as a fallback for other deployments.
  for chain in TRANSPROXY CUBE_EGRESS; do
    if iptables -t mangle -n -L "${chain}" 2>/dev/null | grep -q "TPROXY"; then
      return 0
    fi
  done
  return 1
}

apply_rules() {
  # The init script is NOT fully idempotent: its `ip rule add` fails with
  # "File exists" when the fwmark rule is already installed, and it exits
  # before reaching the iptables setup. So we always do a down-up cycle:
  # down is safe (no-op on clean state), up then installs everything fresh.
  "${INIT_SCRIPT}" down 2>&1 | while IFS= read -r line; do
    log "init-down: ${line}"
  done || true
  "${INIT_SCRIPT}" up 2>&1 | while IFS= read -r line; do
    log "init-up: ${line}"
  done
  return 0
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
      apply_rules || true
      APPLIED=true
      if rules_applied; then
        notify "ready (cube-dev present, rules applied)"
      else
        notify "degraded (applied but TPROXY still missing, will retry next cycle)"
      fi
    elif [[ "${APPLIED}" == false ]]; then
      APPLIED=true
      notify "ready (cube-dev present, rules already in place)"
    else
      # Periodic drift check: rules could have been flushed externally.
      # The init script is mostly idempotent; tolerate partial failures.
      apply_rules || true
      if rules_applied; then
        notify "reconciled (periodic check, rules verified)"
      else
        notify "degraded (periodic check, TPROXY missing, will retry)"
      fi
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
