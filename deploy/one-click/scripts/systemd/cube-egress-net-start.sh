#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Wrapper around CubeEgress's host-side network setup script. Invoked
# by cube-sandbox-cube-egress-net.service as a oneshot before the
# cube-egress container service starts.
#
# We separate this from cube-egress-start.sh so the container service
# remains a simple long-running unit; the iptables/route state
# is owned by its own systemd unit with a matching ExecStop=down.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-${TOOLBOX_ROOT}/scripts/cube-egress/cube-proxy-iptables-init.sh}"
ensure_executable "${INIT_SCRIPT}"

# cube-dev is created lazily by network-agent at first-sandbox time,
# so on a fresh boot it isn't there yet. We give it a short window to
# appear (covers the common case where someone restarts services after
# already having sandboxes), then bail out *successfully* if it still
# isn't there: the unit declares RemainAfterExit=yes, so a successful
# exit here keeps the unit "active (exited)" and a future explicit
# restart (e.g. from cubelet's first sandbox-creation hook) will redo
# the install. Failing the unit instead would block the whole
# cube-sandbox-cube-egress.service from ever starting on a fresh box.
INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"
for _ in {1..30}; do
  if ip link show "${INGRESS_IFACE}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! ip link show "${INGRESS_IFACE}" >/dev/null 2>&1; then
  log "WARN: interface ${INGRESS_IFACE} not present after 30s; deferring iptables install"
  log "      (network-agent creates ${INGRESS_IFACE} on the first sandbox; restart this unit then)"
  exit 0
fi

exec "${INIT_SCRIPT}" up
