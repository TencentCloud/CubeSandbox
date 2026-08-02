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
# CubeEgress's Dockerfile sets STOPSIGNAL SIGQUIT. The shared helper performs
# that graceful stop first, then removes the stopped container so down.sh leaves
# a clean host for the next one-click deployment.
docker_rm_if_exists "${CUBE_EGRESS_CONTAINER}" 15
