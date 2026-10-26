#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the one-click MySQL fallback wiring: start scripts must
# export the CUBE_SANDBOX_MYSQL_* split fields instead of synthesizing a
# DATABASE_URL (an unescaped password breaks URL parsing, issue #1559), and
# persist_one_click_database_runtime_env must percent-encode DATABASE_URL.
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

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected $1 to contain: $2"
}

assert_not_contains() {
  if grep -Fq -- "$2" "$1"; then
    fail "expected $1 NOT to contain: $2"
  fi
}

# Run persist in a clean subshell so leftover CUBE_EXTERNAL_* / driver vars
# from other tests cannot flip the local vs external branch.
persist_db_clean() {
  local env_file="$1"
  shift
  (
    unset CUBE_DATABASE_DRIVER \
      CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_MYSQL_PORT CUBE_EXTERNAL_MYSQL_USER \
      CUBE_EXTERNAL_MYSQL_PASSWORD CUBE_EXTERNAL_MYSQL_DB \
      CUBE_EXTERNAL_POSTGRES_HOST CUBE_EXTERNAL_POSTGRES_PORT CUBE_EXTERNAL_POSTGRES_USER \
      CUBE_EXTERNAL_POSTGRES_PASSWORD CUBE_EXTERNAL_POSTGRES_DB \
      CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_PORT CUBE_SANDBOX_MYSQL_USER \
      CUBE_SANDBOX_MYSQL_PASSWORD CUBE_SANDBOX_MYSQL_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    if [[ "$#" -gt 0 ]]; then
      export "$@"
    fi
    persist_one_click_database_runtime_env "${env_file}"
  )
}

test_start_scripts_export_split_fields() {
  local cubeops="${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh"
  local cubeapi="${ONE_CLICK_DIR}/scripts/systemd/cube-api-start.sh"
  local up="${ONE_CLICK_DIR}/scripts/one-click/up.sh"
  for script in "${cubeops}" "${cubeapi}" "${up}"; do
    [[ -f "${script}" ]] || fail "missing start script: ${script}"
    # The fallback must not build a URL: an unescaped password breaks it.
    assert_not_contains "${script}" 'mysql://${mysql_user}'
    assert_contains "${script}" 'CUBE_SANDBOX_MYSQL_HOST:-127.0.0.1'
    assert_contains "${script}" 'CUBE_SANDBOX_MYSQL_PORT:-3306'
    assert_contains "${script}" 'CUBE_SANDBOX_MYSQL_USER:-cube'
    assert_contains "${script}" 'CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass'
    assert_contains "${script}" 'CUBE_SANDBOX_MYSQL_DB:-cube_mvp'
  done
}

test_persist_encodes_external_mysql_password() {
  local env_file="${TMP_DIR}/external-mysql.env"
  : > "${env_file}"

  persist_db_clean "${env_file}" \
    CUBE_EXTERNAL_MYSQL_HOST=10.0.0.21 \
    CUBE_EXTERNAL_MYSQL_PASSWORD='p#a?b/c'

  assert_value "${env_file}" CUBE_DATABASE_DRIVER mysql
  assert_value "${env_file}" CUBE_EXTERNAL_MYSQL_HOST 10.0.0.21
  assert_value "${env_file}" DATABASE_URL \
    'mysql://cube:p%23a%3Fb%2Fc@10.0.0.21:3306/cube_mvp'
}

test_persist_encodes_local_mysql_password() {
  local env_file="${TMP_DIR}/local-mysql.env"
  : > "${env_file}"

  persist_db_clean "${env_file}" CUBE_SANDBOX_MYSQL_PASSWORD='p#a?b/c'

  assert_value "${env_file}" DATABASE_URL \
    'mysql://cube:p%23a%3Fb%2Fc@127.0.0.1:3306/cube_mvp'
}

test_start_scripts_export_split_fields
test_persist_encodes_external_mysql_password
test_persist_encodes_local_mysql_password

echo "database runtime env tests OK"
