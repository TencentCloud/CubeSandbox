#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Confirm CubeEgress's nginx is actually serving on the expected
# listeners. Bare TCP probe is enough; we're not exercising the MITM
# path here (that requires sandbox traffic), just the bind/listen
# health.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

CUBE_EGRESS_CONTAINER="${CUBE_SANDBOX_CUBE_EGRESS_CONTAINER:-cube-egress}"
wait_for_container_health "${CUBE_EGRESS_CONTAINER}" 40 2 \
  || die "cube-egress container not running"

# Admin API on loopback (always reachable in --network=host mode).
ADMIN_PORT="${CUBE_EGRESS_ADMIN_PORT:-9090}"
wait_for_tcp_port "${ADMIN_PORT}" 30 2 \
  || die "cube-egress admin listener (:${ADMIN_PORT}) not bound"

# /admin/v1/health returns a JSON body; we don't parse it, just
# confirm the endpoint is alive.
ADMIN_HEALTH_URL="${CUBE_EGRESS_ADMIN_HEALTH_URL:-http://127.0.0.1:${ADMIN_PORT}/admin/v1/health}"
wait_for_http "${ADMIN_HEALTH_URL}" 30 2 \
  || die "cube-egress admin health (${ADMIN_HEALTH_URL}) not responsive"

log "cube-egress healthy: admin=${ADMIN_HEALTH_URL}"
