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

test_docker_bin_is_snap() {
  docker_bin_is_snap "" && fail "empty path should not be snap"
  docker_bin_is_snap "/usr/bin/env" && fail "/usr/bin/env should not be snap"
  docker_bin_is_snap "/snap/bin/docker" || fail "/snap/bin/docker should be snap"

  if [[ -e /usr/bin/snap ]]; then
    local alias
    alias="$(mktemp)"
    ln -sf /usr/bin/snap "${alias}"
    docker_bin_is_snap "${alias}" || fail "symlink to /usr/bin/snap should be snap"
    rm -f "${alias}"
  fi
}

test_reject_snap_docker() {
  PATH=/usr/bin:/bin reject_snap_docker /usr/bin/env || fail "non-snap docker should pass"
  (
    PATH=/nonexistent
    reject_snap_docker
  ) || fail "missing docker should pass"

  local out
  if out="$(reject_snap_docker /snap/bin/docker 2>&1)"; then
    fail "snap docker should be rejected"
  fi
  grep -Fq "snap Docker is not supported" <<<"${out}" \
    || fail "reject message missing (got: ${out})"
  grep -Fq "sudo snap remove docker" <<<"${out}" \
    || fail "remediation missing (got: ${out})"

  # No-arg PATH resolution: the production call path.
  if [[ -e /usr/bin/snap ]]; then
    local tmp
    tmp="$(mktemp -d)"
    ln -sf /usr/bin/snap "${tmp}/docker"
    if PATH="${tmp}:${PATH}" out="$(reject_snap_docker 2>&1)"; then
      rm -rf "${tmp}"
      fail "snap docker on PATH should be rejected"
    fi
    rm -rf "${tmp}"
  fi
}

test_wiring() {
  grep -Fq "reject_snap_docker" "${ONE_CLICK_DIR}/lib/common.sh" \
    || fail "install_docker should call reject_snap_docker"
  grep -Fq "reject_snap_docker" "${ONE_CLICK_DIR}/install.sh" \
    || fail "check_install_preflight should call reject_snap_docker"
  grep -Fq "reject_snap_docker" "${ONE_CLICK_DIR}/online-install.sh" \
    || fail "online-install should reject snap Docker"
}

test_docker_bin_is_snap
test_reject_snap_docker
test_wiring

echo "snap docker tests OK"
