#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

WEB_UI_CONTAINER_NAME="${WEB_UI_CONTAINER_NAME:-cube-webui}"
if container_exists "${WEB_UI_CONTAINER_NAME}"; then
  docker stop -t 10 "${WEB_UI_CONTAINER_NAME}" >/dev/null 2>&1 || true
fi
