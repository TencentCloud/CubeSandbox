#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

ONE_CLICK_SCRIPT_DIR="${TOOLBOX_ROOT}/scripts/one-click"
SEED_SCRIPT="${ONE_CLICK_SCRIPT_DIR}/seed-cubemaster-metrics.sh"
ensure_file "${SEED_SCRIPT}"
exec "${SEED_SCRIPT}"
