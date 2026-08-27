#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for persist_one_click_redis_runtime_env (install.sh Redis
# EnvironmentFile whitelist) and the cubeops-start.sh password fallback.
# --mode=install used to leave CUBE_SANDBOX_REDIS_PASSWORD out of
# .one-click.env; cubeops then AUTH-skipped against requirepass Redis.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
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

assert_value() {
  local file="$1" key="$2" expected="$3"
  local actual
  actual="$(read_env_key "${file}" "${key}")"
  [[ "${actual}" == "${expected}" ]] || fail "expected ${key}='${expected}', got '${actual}'"
}

assert_not_contains() {
  if grep -Fq -- "$2" "$1"; then
    fail "expected $1 NOT to contain: $2"
  fi
}

# Run persist in a clean subshell so leftover CUBE_EXTERNAL_* from other
# tests cannot flip the local vs external branch.
persist_clean() {
  local env_file="$1"
  shift
  (
    unset CUBE_EXTERNAL_REDIS_HOST CUBE_EXTERNAL_REDIS_PORT \
      CUBE_EXTERNAL_REDIS_PASSWORD CUBE_EXTERNAL_REDIS_MASTER_NAME \
      CUBE_EXTERNAL_REDIS_SENTINEL_NODES CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD \
      CUBE_SANDBOX_REDIS_PASSWORD
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    if [[ "$#" -gt 0 ]]; then
      export "$@"
    fi
    persist_one_click_redis_runtime_env "${env_file}"
  )
}

test_local_redis_persists_default_password() {
  local env_file="${TMP_DIR}/local-default.env"
  : > "${env_file}"

  persist_clean "${env_file}"

  assert_value "${env_file}" CUBE_SANDBOX_REDIS_PASSWORD ceuhvu123
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_REDIS_PASSWORD="
}

test_local_redis_persists_custom_password() {
  local env_file="${TMP_DIR}/local-custom.env"
  : > "${env_file}"

  persist_clean "${env_file}" CUBE_SANDBOX_REDIS_PASSWORD=custom

  assert_value "${env_file}" CUBE_SANDBOX_REDIS_PASSWORD custom
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_REDIS_PASSWORD="
}

test_external_redis_persists_external_password() {
  local env_file="${TMP_DIR}/external.env"
  : > "${env_file}"

  persist_clean "${env_file}" \
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.8 \
    CUBE_EXTERNAL_REDIS_PASSWORD=extsecret

  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_HOST 10.0.0.8
  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_PASSWORD extsecret
  assert_not_contains "${env_file}" "CUBE_SANDBOX_REDIS_PASSWORD="
}

test_cubeops_start_redis_password_fallback() {
  local start_sh="${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh"
  grep -Fq 'CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123' "${start_sh}" \
    || fail "cubeops-start.sh must fall back REDIS_PASSWORD to ceuhvu123"
}

test_install_sh_calls_persist_helper() {
  grep -Fq 'persist_one_click_redis_runtime_env "${RUNTIME_ENV_FILE}"' \
    "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must persist Redis runtime env via persist_one_click_redis_runtime_env"
}

test_local_redis_persists_default_password
test_local_redis_persists_custom_password
test_external_redis_persists_external_password
test_cubeops_start_redis_password_fallback
test_install_sh_calls_persist_helper

echo "redis runtime env tests OK"
