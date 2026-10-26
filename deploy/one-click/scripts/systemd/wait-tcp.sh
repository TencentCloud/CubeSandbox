#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

port="${1:?usage: wait-tcp.sh <port> [retries] [delay]}"
retries="${2:-30}"
delay="${3:-2}"

wait_for_tcp_port "${port}" "${retries}" "${delay}" || die "tcp port not ready: ${port}"
