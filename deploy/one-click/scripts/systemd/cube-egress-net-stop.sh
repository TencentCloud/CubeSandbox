#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Tear down CubeEgress's iptables/route rules. Idempotent: calling
# `down` against a host that has nothing installed is a no-op.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-}"
if [[ -z "${INIT_SCRIPT}" ]]; then
  # Auto-detect, mirroring cube-egress-net-start.sh: deployments shipped
  # before the #2034 rename call this script cube-proxy-iptables-init.sh.
  # Detection must stay symmetric — a start that accepts the legacy name
  # and a stop that rejects it would leave the rules behind on teardown.
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

exec "${INIT_SCRIPT}" down
