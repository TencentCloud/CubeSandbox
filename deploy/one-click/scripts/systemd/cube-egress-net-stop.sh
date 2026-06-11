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

INIT_SCRIPT="${CUBE_EGRESS_NET_INIT:-${TOOLBOX_ROOT}/scripts/cube-egress/cube-proxy-iptables-init.sh}"
ensure_executable "${INIT_SCRIPT}"

exec "${INIT_SCRIPT}" down
