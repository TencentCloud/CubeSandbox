#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
mkdir -p "${TMP_DIR}/empty-bin" "${TMP_DIR}/web-dist"

error_log="${TMP_DIR}/error.log"
if (PATH="${TMP_DIR}/empty-bin" require_web_build_prerequisites "") >"${error_log}" 2>&1; then
  fail "missing npm should fail when WebUI must be built"
fi
grep -Fq "required command not found: npm" "${error_log}" \
  || fail "missing npm error should identify npm"

PATH="${TMP_DIR}/empty-bin" \
  require_web_build_prerequisites "${TMP_DIR}/web-dist" \
  || fail "a prebuilt WebUI dist should bypass the npm requirement"

for entrypoint in build-release-bundle-builder.sh build-release-bundle.sh; do
  grep -Fq "require_web_build_prerequisites" "${ONE_CLICK_DIR}/${entrypoint}" \
    || fail "${entrypoint} must run the WebUI prerequisite check"
done

echo "PASS: WebUI build prerequisites"
