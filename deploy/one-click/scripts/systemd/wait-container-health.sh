#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

container="${1:?usage: wait-container-health.sh <container> [retries] [delay]}"
retries="${2:-40}"
delay="${3:-2}"

wait_for_container_health "${container}" "${retries}" "${delay}" || die "container did not become healthy: ${container}"
