#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

url="${1:?usage: wait-http.sh <url> [retries] [delay] [curl-args]}"
retries="${2:-30}"
delay="${3:-2}"
curl_args="${4:-}"

wait_for_http "${url}" "${retries}" "${delay}" "${curl_args}" || die "http endpoint not ready: ${url}"
