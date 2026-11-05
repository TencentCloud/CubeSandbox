#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install the cube-egress host-side iptables/ip-rule rules, blocking until
# the sandbox ingress interface (cube-dev) exists.
#
# cube-dev is created by Cubelet's embedded network runtime shortly after
# cubelet starts. This unit runs after cube-sandbox-cubelet.service, but on
# boot it can still win the race; a plain oneshot would then install
# nothing while RemainAfterExit=yes masks the failure until the next
# reboot (#2141). So we simply wait for the interface — guarded by
# TimeoutStartSec=infinity in the unit.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-}"
if [[ -z "${INIT_SCRIPT}" ]]; then
  # Auto-detect: deployments shipped before the #2034 rename call this
  # script cube-proxy-iptables-init.sh.
  for _candidate in \
      "${TOOLBOX_ROOT}/scripts/cube-egress/cube-egress-iptables-init.sh" \
      "${TOOLBOX_ROOT}/scripts/cube-egress/cube-proxy-iptables-init.sh"; do
    if [[ -x "${_candidate}" ]]; then
      INIT_SCRIPT="${_candidate}"
      break
    fi
  done
fi
if [[ -z "${INIT_SCRIPT}" ]]; then
  die "no egress iptables init script found (tried cube-egress-* and cube-proxy-*)"
fi
ensure_executable "${INIT_SCRIPT}"

INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"
POLL_INTERVAL="${CUBE_EGRESS_NET_POLL_INTERVAL:-1}"

until ip link show dev "${INGRESS_IFACE}" >/dev/null 2>&1; do
  log "waiting for ${INGRESS_IFACE} to appear (created by cubelet)..."
  sleep "${POLL_INTERVAL}"
done

log "${INGRESS_IFACE} present, installing egress rules"
exec "${INIT_SCRIPT}" up
