#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the MinIO / CUBE_S3_* mutex: a previous local-MinIO fill
# must be allowed on upgrade; an operator-supplied external store must not
# be combined with CUBE_SANDBOX_MINIO_ENABLED=1.
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

expect_ok() {
  local label="$1"
  if ! check_minio_not_combined_with_user_s3; then
    fail "${label}: expected check to pass"
  fi
}

expect_die() {
  local label="$1"
  local err="${TMP_DIR}/die.err"
  if ( check_minio_not_combined_with_user_s3 ) >"${err}" 2>&1; then
    fail "${label}: expected check to die"
  fi
  grep -Fq "cannot be combined with CUBE_S3_ENDPOINT" "${err}" \
    || fail "${label}: die message missing (got $(cat "${err}"))"
}

expect_s3_ok() {
  local label="$1"
  if ! check_compute_s3_required; then
    fail "${label}: expected check to pass"
  fi
}

expect_s3_die() {
  local label="$1"
  local err="${TMP_DIR}/s3.err"
  if ( check_compute_s3_required ) >"${err}" 2>&1; then
    fail "${label}: expected check to die"
  fi
  grep -Fq "CUBE_S3_ENDPOINT" "${err}" \
    || fail "${label}: die message missing (got $(cat "${err}"))"
}

reset_minio_s3_vars() {
  CUBE_SANDBOX_MINIO_ENABLED=1
  CUBE_SANDBOX_MINIO_API_BIND=""
  CUBE_SANDBOX_NODE_IP="10.0.0.11"
  CUBE_SANDBOX_MINIO_API_PORT=9000
  CUBE_SANDBOX_MINIO_ROOT_USER="cubeminio"
  CUBE_SANDBOX_MINIO_ROOT_PASSWORD="minio-root-password-1"
  CUBE_S3_ENDPOINT=""
  CUBE_S3_ACCESS_KEY_ID=""
  CUBE_S3_SECRET_ACCESS_KEY=""
}

test_local_minio_s3_endpoint_formula() {
  CUBE_SANDBOX_MINIO_API_BIND="10.0.0.11"
  CUBE_SANDBOX_MINIO_API_PORT=9000
  local got
  got="$(local_minio_s3_endpoint)"
  [[ "${got}" == "http://10.0.0.11:9000" ]] \
    || fail "local_minio_s3_endpoint bind+port (got ${got})"

  CUBE_SANDBOX_MINIO_API_BIND=""
  CUBE_SANDBOX_NODE_IP="192.168.1.8"
  CUBE_SANDBOX_MINIO_API_PORT=9000
  got="$(local_minio_s3_endpoint)"
  [[ "${got}" == "http://192.168.1.8:9000" ]] \
    || fail "local_minio_s3_endpoint falls back to node IP (got ${got})"
}

test_minio_enabled_empty_endpoint_ok() {
  reset_minio_s3_vars
  CUBE_S3_ENDPOINT=""
  expect_ok "minio on + empty endpoint"
}

test_minio_enabled_local_endpoint_ok() {
  reset_minio_s3_vars
  CUBE_S3_ENDPOINT="http://10.0.0.11:9000"
  CUBE_S3_ACCESS_KEY_ID="cubeminio"
  CUBE_S3_SECRET_ACCESS_KEY="minio-root-password-1"
  expect_ok "minio on + local filled endpoint"
}

test_minio_enabled_old_ip_matching_credentials_ok() {
  reset_minio_s3_vars
  CUBE_SANDBOX_NODE_IP="10.0.0.99"
  CUBE_S3_ENDPOINT="http://10.0.0.11:9000"
  CUBE_S3_ACCESS_KEY_ID="cubeminio"
  CUBE_S3_SECRET_ACCESS_KEY="minio-root-password-1"
  expect_ok "minio on + old IP with matching MinIO credentials"
}

test_minio_enabled_external_endpoint_dies() {
  reset_minio_s3_vars
  CUBE_S3_ENDPOINT="https://s3.example.com"
  CUBE_S3_ACCESS_KEY_ID="external-ak"
  CUBE_S3_SECRET_ACCESS_KEY="external-sk"
  expect_die "minio on + external endpoint"
}

test_minio_disabled_external_endpoint_ok() {
  reset_minio_s3_vars
  CUBE_SANDBOX_MINIO_ENABLED=0
  CUBE_S3_ENDPOINT="https://s3.example.com"
  CUBE_S3_ACCESS_KEY_ID="external-ak"
  CUBE_S3_SECRET_ACCESS_KEY="external-sk"
  expect_ok "minio off + external endpoint"
}

test_compute_empty_s3_endpoint_dies() {
  reset_minio_s3_vars
  ONE_CLICK_DEPLOY_ROLE=compute
  CUBE_S3_ENDPOINT=""
  expect_s3_die "compute + empty endpoint"
  unset ONE_CLICK_DEPLOY_ROLE
}

test_compute_set_s3_endpoint_ok() {
  reset_minio_s3_vars
  ONE_CLICK_DEPLOY_ROLE=compute
  CUBE_S3_ENDPOINT="http://10.0.0.1:9000"
  expect_s3_ok "compute + endpoint from control plane"
  unset ONE_CLICK_DEPLOY_ROLE
}

test_control_empty_s3_endpoint_ok() {
  reset_minio_s3_vars
  ONE_CLICK_DEPLOY_ROLE=control
  CUBE_S3_ENDPOINT=""
  expect_s3_ok "control + empty endpoint (filled from local MinIO)"
  unset ONE_CLICK_DEPLOY_ROLE
}

test_write_volume_s3_conf_file_source_roundtrip() {
  local conf="${TMP_DIR}/volume-s3.conf"
  # Hash / spaces / multiple s3fs tokens must survive bash `source`.
  write_volume_s3_conf_file \
    "${conf}" \
    "ak-id" \
    "sec with #hash and spaces" \
    "my-bucket" \
    "https://s3.example.com" \
    "us-west-2" \
    "-ouse_path_request_style -oallow_other"
  grep -Fq "SECRET_ACCESS_KEY='sec with #hash and spaces'" "${conf}" \
    || fail "secret must be single-quoted (got $(cat "${conf}"))"
  grep -Fq "S3FS_EXTRA_OPTS='-ouse_path_request_style -oallow_other'" "${conf}" \
    || fail "extra opts must be single-quoted (got $(cat "${conf}"))"
  # shellcheck source=/dev/null
  source "${conf}"
  [[ "${ACCESS_KEY_ID}" == "ak-id" ]] || fail "ACCESS_KEY_ID roundtrip"
  [[ "${SECRET_ACCESS_KEY}" == "sec with #hash and spaces" ]] \
    || fail "SECRET_ACCESS_KEY roundtrip (got ${SECRET_ACCESS_KEY})"
  [[ "${BUCKET}" == "my-bucket" ]] || fail "BUCKET roundtrip"
  [[ "${ENDPOINT}" == "https://s3.example.com" ]] || fail "ENDPOINT roundtrip"
  [[ "${REGION}" == "us-west-2" ]] || fail "REGION roundtrip"
  [[ "${S3FS_EXTRA_OPTS}" == "-ouse_path_request_style -oallow_other" ]] \
    || fail "S3FS_EXTRA_OPTS roundtrip (got ${S3FS_EXTRA_OPTS})"

  unset S3FS_EXTRA_OPTS
  write_volume_s3_conf_file \
    "${conf}" "ak" "it's" "b" "http://x" "us-east-1" ""
  grep -Fq "S3FS_EXTRA_OPTS=" "${conf}" \
    && fail "empty extra opts must omit S3FS_EXTRA_OPTS"
  # shellcheck source=/dev/null
  source "${conf}"
  [[ "${SECRET_ACCESS_KEY}" == "it's" ]] \
    || fail "apostrophe in secret must roundtrip (got ${SECRET_ACCESS_KEY})"
  [[ -z "${S3FS_EXTRA_OPTS:-}" ]] \
    || fail "empty extra opts must leave S3FS_EXTRA_OPTS unset (got ${S3FS_EXTRA_OPTS:-})"
}

test_local_minio_s3_endpoint_formula
test_minio_enabled_empty_endpoint_ok
test_minio_enabled_local_endpoint_ok
test_minio_enabled_old_ip_matching_credentials_ok
test_minio_enabled_external_endpoint_dies
test_minio_disabled_external_endpoint_ok
test_compute_empty_s3_endpoint_dies
test_compute_set_s3_endpoint_ok
test_control_empty_s3_endpoint_ok
test_write_volume_s3_conf_file_source_roundtrip

echo "minio s3 guard tests OK"
