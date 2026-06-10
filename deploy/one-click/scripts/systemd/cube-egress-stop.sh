#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

CUBE_EGRESS_CONTAINER="${CUBE_SANDBOX_CUBE_EGRESS_CONTAINER:-cube-egress}"
if container_exists "${CUBE_EGRESS_CONTAINER}"; then
  # CubeEgress's Dockerfile sets STOPSIGNAL SIGQUIT (graceful nginx
  # shutdown); docker stop honours that. Generous timeout because
  # nginx will let in-flight upstream connections finish.
  docker stop -t 15 "${CUBE_EGRESS_CONTAINER}" >/dev/null 2>&1 || true
fi
