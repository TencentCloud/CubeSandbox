#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install the cube-egress host-side iptables/ip-rule rules, waiting for the
# sandbox ingress interface (cube-dev) to appear — but only for a bounded
# time, then failing explicitly.
#
# cube-dev is created by Cubelet's embedded network runtime during plugin
# init, after cubelet's PIDFile already appeared (its unit is Type=forking),
# so After=cube-sandbox-cubelet.service does NOT imply the interface exists.
# Waiting inside this wrapper (#2141) must still be bounded: install.sh
# calls `systemctl enable --now` on this unit, which blocks until the start
# job finishes — an unbounded wait would hang the installer forever.
#
# The default matches the k8s entrypoint's 300s initial-wait figure
# (deploy/kubernetes/images/scripts/cube-egress-net-entrypoint.sh); unlike
# the container, which has no synchronous caller, we turn it into a hard
# deadline and let Restart=on-failure retry.

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
WAIT_TIMEOUT="${CUBE_EGRESS_NET_WAIT_TIMEOUT:-300}"
POLL_INTERVAL="${CUBE_EGRESS_NET_POLL_INTERVAL:-1}"
[[ "${WAIT_TIMEOUT}" =~ ^[0-9]+$ ]] || die "CUBE_EGRESS_NET_WAIT_TIMEOUT must be a non-negative integer, got '${WAIT_TIMEOUT}'"
[[ "${POLL_INTERVAL}" =~ ^[1-9][0-9]*$ ]] || die "CUBE_EGRESS_NET_POLL_INTERVAL must be a positive integer, got '${POLL_INTERVAL}'"

deadline=$(( SECONDS + WAIT_TIMEOUT ))
until ip link show dev "${INGRESS_IFACE}" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    die "timed out after ${WAIT_TIMEOUT}s waiting for ${INGRESS_IFACE} (created by cubelet); failing so the loss is visible instead of hanging install.sh"
  fi
  log "waiting for ${INGRESS_IFACE} to appear (created by cubelet)..."
  sleep "${POLL_INTERVAL}"
done

log "${INGRESS_IFACE} present, installing egress rules"
exec "${INIT_SCRIPT}" up
