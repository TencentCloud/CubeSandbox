#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# /health, not /notify/health: that route belongs to CubeMaster's notify API,
# which TC does not serve.
tc_health_base="${CUBE_TEMPLATE_CENTER_DOWNLOAD_ADDR:-${CUBETEMPLATECENTER_ADDR:-127.0.0.1:8090}}"
case "${tc_health_base}" in
  http://*|https://*) ;;
  *) tc_health_base="http://${tc_health_base}" ;;
esac
wait_for_http "${tc_health_base%/}/health" 30 1 \
  || die "cubetemplatecenter health not ready"
