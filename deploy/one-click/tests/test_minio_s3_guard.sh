#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the MinIO / CUBE_S3_* mutex: a previous local-MinIO fill
# must be allowed on upgrade; an operator-supplied external store must not
# be combined with CUBE_SANDBOX_MINIO_ENABLED=1. Also covers the compute-node
# missing-S3 warning (warn_compute_s3_missing warns instead of aborting) and
# warn()'s plain-text output when stderr is not a TTY.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT_DIR="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"
RCOW_COMMON="${ROOT_DIR}/CubeS3lvol/scripts/rcow_common.sh"
RCOW_PURGE="${ROOT_DIR}/CubeS3lvol/scripts/rcow_purge.sh"
S3LVOL_SUPERVISE="${ONE_CLICK_DIR}/scripts/systemd/cube-s3lvol-supervise.sh"

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
  if ! warn_compute_s3_missing; then
    fail "${label}: expected check to pass"
  fi
}

expect_s3_warn() {
  local label="$1"
  local err="${TMP_DIR}/s3-warn.err"
  if ! ( warn_compute_s3_missing ) >"${err}" 2>&1; then
    fail "${label}: expected check to warn and return 0"
  fi
  grep -Fq "no CUBE_S3_* backend configured" "${err}" \
    || fail "${label}: warning message missing (got $(cat "${err}"))"
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

test_compute_empty_s3_endpoint_warns() {
  reset_minio_s3_vars
  ONE_CLICK_DEPLOY_ROLE=compute
  CUBE_S3_ENDPOINT=""
  expect_s3_warn "compute + empty endpoint"
  unset ONE_CLICK_DEPLOY_ROLE
}

test_warn_plain_when_not_tty() {
  local out
  out="$(warn "hello" 2>&1 || true)"
  grep -Fq "[one-click] WARNING: hello" <<<"${out}" \
    || fail "warn must print plain text when stderr is not a TTY (got ${out})"
  if grep -Fq $'\033' <<<"${out}"; then
    fail "warn must not emit ANSI escapes when stderr is not a TTY (got ${out})"
  fi
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
test_compute_empty_s3_endpoint_warns
test_compute_set_s3_endpoint_ok
test_control_empty_s3_endpoint_ok
test_warn_plain_when_not_tty
s3cfg_get() {
  local conf="$1" key="$2"
  sed -n "s/^[[:space:]]*${key}[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" \
    "${conf}" | head -1 | tr -d '\r'
}

s3cfg_buckets() {
  local conf="$1"
  sed -n 's/^[[:space:]]*buckets[[:space:]]*=[[:space:]]*\[\(.*\)\].*/\1/p' \
    "${conf}" | head -1 | tr ',' '\n' |
    sed -n 's/^[[:space:]]*"\([^"]*\)".*/\1/p' | tr -d '\r'
}

test_s3lvol_host_from_endpoint() {
  local got
  got="$(s3lvol_host_from_endpoint "http://10.0.0.11:9000")"
  [[ "${got}" == "10.0.0.11:9000" ]] || fail "strip http:// (got ${got})"
  got="$(s3lvol_host_from_endpoint "https://s3.example.com")"
  [[ "${got}" == "s3.example.com" ]] || fail "strip https:// (got ${got})"
  got="$(s3lvol_host_from_endpoint "https://s3.example.com/path")"
  [[ "${got}" == "s3.example.com" ]] || fail "strip path (got ${got})"
  s3lvol_no_tls_from_endpoint "http://10.0.0.11:9000" \
    || fail "http:// must be no_tls"
  if s3lvol_no_tls_from_endpoint "https://s3.example.com"; then
    fail "https:// must not be no_tls"
  fi
}

test_write_s3lvol_cfg_file_roundtrip() {
  local conf="${TMP_DIR}/s3.cfg"
  write_s3lvol_cfg_file \
    "${conf}" "ak-id" "sk-secret" "cube-s3lvol" \
    "10.0.0.11:9000" "us-east-1" "1" "1"
  grep -Fq "${S3LVOL_CFG_SENTINEL}" "${conf}" \
    || fail "generated s3.cfg must carry the one-click sentinel"
  [[ "$(s3cfg_get "${conf}" access_key_id)" == "ak-id" ]] \
    || fail "access_key_id roundtrip"
  [[ "$(s3cfg_get "${conf}" secret_access_key)" == "sk-secret" ]] \
    || fail "secret_access_key roundtrip"
  [[ "$(s3cfg_get "${conf}" endpoint)" == "10.0.0.11:9000" ]] \
    || fail "endpoint must have no scheme (got $(s3cfg_get "${conf}" endpoint))"
  [[ "$(s3cfg_get "${conf}" region)" == "us-east-1" ]] || fail "region roundtrip"
  [[ "$(s3cfg_buckets "${conf}")" == "cube-s3lvol" ]] || fail "buckets roundtrip"
  [[ "$(s3cfg_get "${conf}" path_style)" == "true" ]] || fail "path_style=true"
  [[ "$(s3cfg_get "${conf}" no_tls)" == "true" ]] || fail "no_tls=true"
}

test_write_s3lvol_cfg_minio_vs_volume_bucket() {
  reset_minio_s3_vars
  ONE_CLICK_ENABLE_S3LVOL=1
  CUBE_S3_ENDPOINT="http://10.0.0.11:9000"
  CUBE_S3_ACCESS_KEY_ID="cubeminio"
  CUBE_S3_SECRET_ACCESS_KEY="minio-root-password-1"
  CUBE_S3_BUCKET="cube-volumes"
  CUBE_S3LVOL_BUCKET="cube-s3lvol"
  CUBE_S3_REGION="us-east-1"
  CUBE_S3_S3FS_EXTRA_OPTS="-ouse_path_request_style"
  RCOW_S3_CFG="${TMP_DIR}/generated-s3.cfg"
  rm -f "${RCOW_S3_CFG}"
  write_s3lvol_cfg
  [[ -f "${RCOW_S3_CFG}" ]] || fail "write_s3lvol_cfg must create ${RCOW_S3_CFG}"
  [[ "$(s3cfg_get "${RCOW_S3_CFG}" endpoint)" == "10.0.0.11:9000" ]] \
    || fail "minio endpoint must drop scheme"
  [[ "$(s3cfg_get "${RCOW_S3_CFG}" no_tls)" == "true" ]] || fail "minio no_tls"
  [[ "$(s3cfg_get "${RCOW_S3_CFG}" path_style)" == "true" ]] || fail "minio path_style"
  local bucket
  bucket="$(s3cfg_buckets "${RCOW_S3_CFG}")"
  [[ "${bucket}" == "cube-s3lvol" ]] || fail "s3lvol bucket (got ${bucket})"
  [[ "${bucket}" != "${CUBE_S3_BUCKET}" ]] \
    || fail "s3lvol bucket must differ from volume plugin bucket"
  unset RCOW_S3_CFG ONE_CLICK_ENABLE_S3LVOL CUBE_S3LVOL_BUCKET
}

test_write_s3lvol_cfg_external_https() {
  reset_minio_s3_vars
  CUBE_SANDBOX_MINIO_ENABLED=0
  ONE_CLICK_ENABLE_S3LVOL=1
  CUBE_S3_ENDPOINT="https://s3.example.com"
  CUBE_S3_ACCESS_KEY_ID="external-ak"
  CUBE_S3_SECRET_ACCESS_KEY="external-sk"
  CUBE_S3_REGION="ap-guangzhou"
  CUBE_S3_S3FS_EXTRA_OPTS=""
  CUBE_S3LVOL_PATH_STYLE=""
  RCOW_S3_CFG="${TMP_DIR}/external-s3.cfg"
  rm -f "${RCOW_S3_CFG}"
  write_s3lvol_cfg
  [[ "$(s3cfg_get "${RCOW_S3_CFG}" endpoint)" == "s3.example.com" ]] \
    || fail "external endpoint host"
  [[ -z "$(s3cfg_get "${RCOW_S3_CFG}" no_tls)" ]] \
    || fail "https must omit no_tls (got $(s3cfg_get "${RCOW_S3_CFG}" no_tls))"
  [[ -z "$(s3cfg_get "${RCOW_S3_CFG}" path_style)" ]] \
    || fail "external https must omit path_style by default"
  unset RCOW_S3_CFG ONE_CLICK_ENABLE_S3LVOL CUBE_S3LVOL_PATH_STYLE
}

test_write_s3lvol_cfg_quote_dies() {
  local err="${TMP_DIR}/quote.err"
  if ( write_s3lvol_cfg_file \
        "${TMP_DIR}/bad.cfg" 'ak"id' "sk" "b" "host" "us-east-1" "0" "0" ) \
      >"${err}" 2>&1; then
    fail "quoted access key must die"
  fi
  grep -Fq "must not contain double quotes" "${err}" \
    || fail "quote die message (got $(cat "${err}"))"
}

test_write_s3lvol_cfg_keeps_handwritten() {
  reset_minio_s3_vars
  ONE_CLICK_ENABLE_S3LVOL=1
  CUBE_S3_ENDPOINT="http://10.0.0.11:9000"
  CUBE_S3_ACCESS_KEY_ID="cubeminio"
  CUBE_S3_SECRET_ACCESS_KEY="minio-root-password-1"
  RCOW_S3_CFG="${TMP_DIR}/handwritten-s3.cfg"
  printf 'access_key_id="keep-me"\nendpoint="manual.host"\nbuckets=["keep"]\n' \
    >"${RCOW_S3_CFG}"
  write_s3lvol_cfg
  grep -Fq 'access_key_id="keep-me"' "${RCOW_S3_CFG}" \
    || fail "hand-written s3.cfg without sentinel must be kept"
  unset RCOW_S3_CFG ONE_CLICK_ENABLE_S3LVOL
}

test_supervise_preserves_rcow_start_exit_code() {
  [[ -f "${S3LVOL_SUPERVISE}" ]] || fail "missing ${S3LVOL_SUPERVISE}"
  if grep -F -q 'if ! "${RCOW_START}"' "${S3LVOL_SUPERVISE}"; then
    fail "supervise must not use if ! RCOW_START (washes rc to 0)"
  fi
  grep -F -q '"${RCOW_START}" || rc=$?' "${S3LVOL_SUPERVISE}" \
    || fail "supervise must capture rcow_start status with || rc=\$?"
  local rc=0
  false || rc=$?
  [[ "${rc}" -eq 1 ]] || fail "rc=0; false || rc=\$? must yield 1 (got ${rc})"
}

test_rcow_s3_addr_flags_roundtrip() {
  [[ -f "${RCOW_COMMON}" ]] || fail "missing ${RCOW_COMMON}"
  RCOW_S3_CFG="${TMP_DIR}/addr-unused.cfg"
  RCOW_ACTIVE_FILE="${TMP_DIR}/rcow/active_lvols"
  RCOW_BSTORE_FILE="${TMP_DIR}/rcow/bstore.json"
  RCOW_WAL_IMG="${TMP_DIR}/rcow/wal_bdev.img"
  RCOW_RUN_DIR="${TMP_DIR}/rcow-run"
  # Pin RCOW_LVS_NAME so sourcing does not depend on hostname -s
  # (empty in some builder containers; rcow_common.sh then exits 1).
  RCOW_LVS_NAME="${RCOW_LVS_NAME:-minio-guard-test}"
  # shellcheck source=/dev/null
  source "${RCOW_COMMON}"

  RCOW_S3_CFG="${TMP_DIR}/addr-minio.cfg"
  printf '%s\n' \
    'path_style="true"' \
    'no_tls="true"' \
    >"${RCOW_S3_CFG}"
  local s3_flags=()
  rcow_s3_addr_flags s3_flags
  [[ "${#s3_flags[@]}" -eq 2 ]] || fail "minio flags count (got ${#s3_flags[@]})"
  [[ "${s3_flags[0]}" == "--path-style" ]] || fail "minio path-style (got ${s3_flags[0]})"
  [[ "${s3_flags[1]}" == "--no-tls" ]] || fail "minio no-tls (got ${s3_flags[1]})"

  RCOW_S3_CFG="${TMP_DIR}/addr-https.cfg"
  printf '%s\n' \
    'endpoint="s3.example.com"' \
    'region="ap-guangzhou"' \
    >"${RCOW_S3_CFG}"
  s3_flags=()
  rcow_s3_addr_flags s3_flags
  [[ "${#s3_flags[@]}" -eq 0 ]] || fail "https flags must be empty (got ${s3_flags[*]})"
}

test_rcow_purge_expands_s3_addr_flags() {
  [[ -f "${RCOW_PURGE}" ]] || fail "missing ${RCOW_PURGE}"
  local n
  n="$(grep -c 'S3_ADDR_FLAGS\[@\]' "${RCOW_PURGE}" || true)"
  [[ "${n}" -eq 3 ]] \
    || fail "rcow_purge.sh must expand S3_ADDR_FLAGS at 3 PREFIX_RM sites (got ${n})"
  grep -q 'rcow_s3_addr_flags S3_ADDR_FLAGS' "${RCOW_PURGE}" \
    || fail "rcow_purge.sh must call rcow_s3_addr_flags S3_ADDR_FLAGS"
}

test_ensure_nvme_cli_skips_when_disabled() {
  local marker="${TMP_DIR}/nvme-off.marker"
  rm -f "${marker}"
  (
    record_nvme_install() { printf '%s\n' "$1" >"${marker}"; }
    ONE_CLICK_ENABLE_S3LVOL=0
    ONE_CLICK_NVME_CLI_INSTALLER=record_nvme_install
    s3lvol_nvme_present() { return 1; }
    s3lvol_nvme_pkg_manager() { printf 'dnf'; }
    ensure_nvme_cli
  ) || fail "ensure_nvme_cli must succeed when s3lvol is off"
  [[ ! -f "${marker}" ]] \
    || fail "ensure_nvme_cli must not invoke the installer when ONE_CLICK_ENABLE_S3LVOL!=1"
}

test_ensure_nvme_cli_skips_when_present() {
  local marker="${TMP_DIR}/nvme-present.marker"
  rm -f "${marker}"
  (
    record_nvme_install() { printf '%s\n' "$1" >"${marker}"; }
    ONE_CLICK_ENABLE_S3LVOL=1
    ONE_CLICK_NVME_CLI_INSTALLER=record_nvme_install
    s3lvol_nvme_present() { return 0; }
    s3lvol_nvme_pkg_manager() { printf 'apt'; }
    ensure_nvme_cli
  ) || fail "ensure_nvme_cli must succeed when nvme is already in PATH"
  [[ ! -f "${marker}" ]] \
    || fail "ensure_nvme_cli must not invoke the installer when nvme is already present"
}

test_validate_s3lvol_rpc_client_accepts_help() {
  local rpc_py="${TMP_DIR}/rpc-ok.py"
  cat >"${rpc_py}" <<'PY'
#!/usr/bin/env python3
print("usage: rpc.py [options]")
PY
  validate_s3lvol_rpc_client "${rpc_py}" \
    || fail "validate_s3lvol_rpc_client must accept a rpc.py that prints --help"
}

test_validate_s3lvol_rpc_client_dies_on_incompat() {
  local rpc_py="${TMP_DIR}/rpc-bad.py"
  local err="${TMP_DIR}/rpc-bad.err"
  cat >"${rpc_py}" <<'PY'
#!/usr/bin/env python3
import sys
sys.stderr.write("AttributeError: module 'argparse' has no attribute 'BooleanOptionalAction'\n")
sys.exit(1)
PY
  if ( validate_s3lvol_rpc_client "${rpc_py}" ) >"${err}" 2>&1; then
    fail "validate_s3lvol_rpc_client must die when rpc.py --help fails"
  fi
  grep -Fq "Python/rpc.py incompatibility" "${err}" \
    || fail "die message must name the client/interpreter mismatch (got $(cat "${err}"))"
}

test_validate_cubelet_s3lvol_startup_deps_runs_rpc_help() {
  grep -q 'validate_s3lvol_rpc_client' "${ONE_CLICK_DIR}/lib/common.sh" \
    || fail "validate_cubelet_s3lvol_startup_deps must call validate_s3lvol_rpc_client"
  grep -q '"${rpc_py}" --help' "${ONE_CLICK_DIR}/lib/common.sh" \
    || fail "validate_s3lvol_rpc_client must invoke rpc.py --help"
}

test_ensure_nvme_cli_installs_when_missing() {
  local marker="${TMP_DIR}/nvme-missing.marker"
  rm -f "${marker}"
  (
    installed=0
    record_nvme_install() {
      printf '%s\n' "$1" >"${marker}"
      installed=1
    }
    ONE_CLICK_ENABLE_S3LVOL=1
    ONE_CLICK_NVME_CLI_INSTALLER=record_nvme_install
    s3lvol_nvme_present() { [[ "${installed}" -eq 1 ]]; }
    s3lvol_nvme_pkg_manager() { printf 'dnf'; }
    ensure_nvme_cli
  ) || fail "ensure_nvme_cli must succeed after the mock installer records one call"
  [[ -f "${marker}" ]] || fail "ensure_nvme_cli must invoke the installer when nvme is missing"
  [[ "$(cat "${marker}")" == "dnf" ]] \
    || fail "ensure_nvme_cli must pass the detected pm to the installer (got $(cat "${marker}"))"
}

test_write_volume_s3_conf_file_source_roundtrip
test_s3lvol_host_from_endpoint
test_write_s3lvol_cfg_file_roundtrip
test_write_s3lvol_cfg_minio_vs_volume_bucket
test_write_s3lvol_cfg_external_https
test_write_s3lvol_cfg_quote_dies
test_write_s3lvol_cfg_keeps_handwritten
test_supervise_preserves_rcow_start_exit_code
test_rcow_s3_addr_flags_roundtrip
test_rcow_purge_expands_s3_addr_flags
test_ensure_nvme_cli_skips_when_disabled
test_ensure_nvme_cli_skips_when_present
test_ensure_nvme_cli_installs_when_missing
test_validate_s3lvol_rpc_client_accepts_help
test_validate_s3lvol_rpc_client_dies_on_incompat
test_validate_cubelet_s3lvol_startup_deps_runs_rpc_help

echo "minio s3 guard tests OK"
