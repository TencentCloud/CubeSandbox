#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install the cube-egress host-side iptables/ip-rule rules.
#
# cube-dev is created during cubelet's network plugin init, which the cubelet
# unit's readiness signal (Type=forking + PIDFile) does not cover, so
# After=cubelet does not imply the interface exists. Wait for it, bounded,
# then fail: install.sh's `enable --now` on the target waits for this unit.
# WAIT_TIMEOUT defaults to 300s, matching the k8s entrypoint.
#
# Retries live here, not in the unit: Restart= on Type=oneshot is refused at
# load time on systemd < 244 (RHEL/CentOS/OpenCloudOS 8).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

# Validate tuning knobs before anything else so a bad value is always
# reported as such, regardless of whether an init script is present.
INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"
WAIT_TIMEOUT="${CUBE_EGRESS_NET_WAIT_TIMEOUT:-300}"
POLL_INTERVAL="${CUBE_EGRESS_NET_POLL_INTERVAL:-1}"
INIT_RETRIES="${CUBE_EGRESS_NET_INIT_RETRIES:-3}"
# Decimal, no leading zeros, digit-capped: 0600 is octal in $(( )), and huge
# values overflow the deadline. 0 means "probe once, fail if absent".
[[ "${WAIT_TIMEOUT}" =~ ^(0|[1-9][0-9]{0,5})$ ]] || die "CUBE_EGRESS_NET_WAIT_TIMEOUT must be a decimal integer between 0 and 999999 without leading zeros, got '${WAIT_TIMEOUT}'"
[[ "${POLL_INTERVAL}" =~ ^[1-9][0-9]{0,3}$ ]] || die "CUBE_EGRESS_NET_POLL_INTERVAL must be an integer between 1 and 9999, got '${POLL_INTERVAL}'"
[[ "${INIT_RETRIES}" =~ ^[1-9][0-9]{0,3}$ ]] || die "CUBE_EGRESS_NET_INIT_RETRIES must be an integer between 1 and 9999, got '${INIT_RETRIES}'"

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-${TOOLBOX_ROOT}/scripts/cube-egress/cube-egress-iptables-init.sh}"
ensure_executable "${INIT_SCRIPT}"

# Checked after each failed probe, so failure can be reported up to one poll
# interval late.
deadline=$(( SECONDS + WAIT_TIMEOUT ))
until ip link show dev "${INGRESS_IFACE}" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    die "timed out after ${WAIT_TIMEOUT}s waiting for ${INGRESS_IFACE} (created by cubelet); failing so the loss is visible instead of hanging install.sh"
  fi
  log "waiting for ${INGRESS_IFACE} to appear (created by cubelet)..."
  sleep "${POLL_INTERVAL}"
done

log "${INGRESS_IFACE} present, installing egress rules"
# After INIT_RETRIES failures, exit non-zero so the unit is visibly failed.
attempt=1
while true; do
  if "${INIT_SCRIPT}" up; then
    log "egress rules installed (attempt ${attempt}/${INIT_RETRIES})"
    exit 0
  else
    # Keep inside `else`: after `fi` this is always 0.
    rc=$?
  fi
  if (( attempt >= INIT_RETRIES )); then
    die "init script failed ${INIT_RETRIES} time(s), last exit ${rc}; leaving the unit failed for an operator"
  fi
  log "init attempt ${attempt}/${INIT_RETRIES} failed (exit ${rc}); retrying in 5s"
  attempt=$(( attempt + 1 ))
  sleep 5
done
