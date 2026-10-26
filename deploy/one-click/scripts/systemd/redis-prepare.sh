#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

exec env \
  ONE_CLICK_RUNTIME_ENV_FILE="${ENV_FILE}" \
  ONE_CLICK_PREPARE_ONLY=1 \
  ONE_CLICK_SUPPORT_SERVICES=redis \
  "${TOOLBOX_ROOT}/scripts/one-click/up-support.sh"
