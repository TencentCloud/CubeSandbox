#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
READLINK_BIN="$(command -v readlink)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# docker_bin_is_snap compares readlink -f output with /snap/* and /usr/bin/snap,
# which an unprivileged runner cannot create. Build a fake root and strip its
# prefix instead. The fake /usr/bin/snap is a regular file on purpose: some
# hosts ship it as a symlink (/usr/bin/snap -> /usr/lib/snapd/snapd).
fake_root_readlink() {
  local root="$1"
  shift
  local resolved
  if [[ "${1:-}" != "-f" || $# -ne 2 ]]; then
    "${READLINK_BIN}" "$@"
    return
  fi
  resolved="$("${READLINK_BIN}" -f "$2")" || return 1
  case "${resolved}" in
    "${root}"/*) printf '%s\n' "${resolved#"${root}"}" ;;
    *) printf '%s\n' "${resolved}" ;;
  esac
}

run_in_fake_root() {
  local root="$1"
  shift
  (
    readlink() {
      fake_root_readlink "${root}" "$@"
    }
    "$@"
  )
}

run_with_fake_root_path() {
  local root="$1"
  local path_value="$2"
  shift 2
  (
    readlink() {
      fake_root_readlink "${root}" "$@"
    }
    PATH="${path_value}" "$@"
  )
}

test_docker_bin_is_snap() {
  docker_bin_is_snap "" && fail "empty path should not be snap"
  docker_bin_is_snap "/usr/bin/env" && fail "/usr/bin/env should not be snap"
  docker_bin_is_snap "/snap/bin/docker" || fail "/snap/bin/docker should be snap"

  local root="${TMP_DIR}/snap-root"
  local resolved
  mkdir -p \
    "${root}/opt/bin" \
    "${root}/snap/bin" \
    "${root}/usr/bin" \
    "${root}/usr/local/bin"
  : > "${root}/opt/bin/plain"
  : > "${root}/snap/bin/docker"
  chmod +x "${root}/snap/bin/docker"
  : > "${root}/usr/bin/snap"
  ln -s ../../../usr/bin/snap "${root}/usr/local/bin/docker"
  ln -s ../../usr/local/bin/docker "${root}/opt/bin/docker-chain"
  ln -s ../../snap/bin/docker "${root}/opt/bin/docker-snap"
  ln -s plain "${root}/opt/bin/plain-link"

  run_in_fake_root "${root}" docker_bin_is_snap "${root}/snap/bin/docker" \
    || fail "path under /snap should be snap"

  resolved="$(run_in_fake_root "${root}" readlink -f "${root}/usr/local/bin/docker")" \
    || fail "failed to resolve fake /usr/bin/snap alias"
  [[ "${resolved}" == "/usr/bin/snap" ]] \
    || fail "fake alias should resolve to /usr/bin/snap (got: ${resolved})"
  run_in_fake_root "${root}" docker_bin_is_snap "${root}/usr/local/bin/docker" \
    || fail "symlink resolving to /usr/bin/snap should be snap"

  resolved="$(run_in_fake_root "${root}" readlink -f "${root}/opt/bin/docker-chain")" \
    || fail "failed to resolve fake symlink chain"
  [[ "${resolved}" == "/usr/bin/snap" ]] \
    || fail "fake symlink chain should resolve to /usr/bin/snap (got: ${resolved})"
  run_in_fake_root "${root}" docker_bin_is_snap "${root}/opt/bin/docker-chain" \
    || fail "symlink chain resolving to /usr/bin/snap should be snap"

  resolved="$(run_in_fake_root "${root}" readlink -f "${root}/opt/bin/docker-snap")" \
    || fail "failed to resolve fake /snap alias"
  [[ "${resolved}" == "/snap/bin/docker" ]] \
    || fail "fake alias should resolve under /snap (got: ${resolved})"
  run_in_fake_root "${root}" docker_bin_is_snap "${root}/opt/bin/docker-snap" \
    || fail "symlink resolving under /snap should be snap"

  if run_in_fake_root "${root}" docker_bin_is_snap "${root}/opt/bin/plain"; then
    fail "regular binary outside /snap should not be snap"
  fi
  if run_in_fake_root "${root}" docker_bin_is_snap "${root}/opt/bin/plain-link"; then
    fail "symlink to a regular binary outside /snap should not be snap"
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

  # The production no-arg path: expose only the fixture and cat, so a host
  # docker on PATH cannot satisfy command -v.
  local root="${TMP_DIR}/path-root"
  local tools="${TMP_DIR}/path-tools"
  mkdir -p "${root}/snap/bin" "${root}/usr/local/bin" "${tools}"
  : > "${root}/snap/bin/docker"
  chmod +x "${root}/snap/bin/docker"
  ln -s ../../../snap/bin/docker "${root}/usr/local/bin/docker"
  ln -s "$(command -v cat)" "${tools}/cat"
  if out="$(run_with_fake_root_path "${root}" "${root}/usr/local/bin:${tools}" reject_snap_docker 2>&1)"; then
    fail "snap docker on PATH should be rejected"
  fi
  grep -Fq "snap Docker is not supported" <<<"${out}" \
    || fail "PATH reject message missing (got: ${out})"
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
