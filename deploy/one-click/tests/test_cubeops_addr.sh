#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cubelet cubeops_addr injection: all-in-one (control) must write the warehouse
# address, not only compute nodes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

: > "${TMP_DIR}/empty-runtime.env"
export ONE_CLICK_RUNTIME_ENV_FILE="${TMP_DIR}/empty-runtime.env"
export ONE_CLICK_RUNTIME_DIR="${TMP_DIR}/run"
export ONE_CLICK_LOG_DIR="${TMP_DIR}/log"

# shellcheck source=../scripts/common/cubelet_config.sh
source "${ONE_CLICK_DIR}/scripts/common/cubelet_config.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  local path="$1"
  local needle="$2"
  grep -Fq -- "${needle}" "${path}" || fail "expected ${path} to contain ${needle}"
}

write_sample_config() {
  local path="$1"
  cat > "${path}" <<'EOF'
[plugins]
  [plugins."io.cubelet.controller.config.v1.cubelet"]
    node_status_update_frequency = "1s"
    cubeops_addr = ""
    cubeops_timeout = "10m"
EOF
}

test_control_default_writes_local_cubeops() {
  local cfg="${TMP_DIR}/control.toml"
  write_sample_config "${cfg}"
  write_cubelet_cubeops_addr "${cfg}" "127.0.0.1:3010"
  assert_contains "${cfg}" 'cubeops_addr = "http://127.0.0.1:3010"'
}

test_scheme_passthrough() {
  local cfg="${TMP_DIR}/https.toml"
  write_sample_config "${cfg}"
  write_cubelet_cubeops_addr "${cfg}" "https://ops.example:3010"
  assert_contains "${cfg}" 'cubeops_addr = "https://ops.example:3010"'
}

test_compute_override_addr() {
  local cfg="${TMP_DIR}/override.toml"
  write_sample_config "${cfg}"
  write_cubelet_cubeops_addr "${cfg}" "http://10.0.0.5:3010"
  assert_contains "${cfg}" 'cubeops_addr = "http://10.0.0.5:3010"'
}

test_inserts_when_key_missing() {
  local cfg="${TMP_DIR}/missing.toml"
  cat > "${cfg}" <<'EOF'
    node_status_update_frequency = "1s"
    cubeops_timeout = "10m"
EOF
  write_cubelet_cubeops_addr "${cfg}" "127.0.0.1:3010"
  assert_contains "${cfg}" 'cubeops_addr = "http://127.0.0.1:3010"'
}

# All-in-one used to skip write_cubeops_addr because is_compute_role returned
# false. The write must happen before that early exit.
test_prepare_writes_before_compute_only_exit() {
  local path="${ONE_CLICK_DIR}/scripts/systemd/prepare-compute-role.sh"
  local write_line exit_line
  write_line="$(grep -n 'write_cubelet_cubeops_addr' "${path}" | head -1 | cut -d: -f1)"
  exit_line="$(grep -n 'if ! is_compute_role; then' "${path}" | head -1 | cut -d: -f1)"
  [[ -n "${write_line}" ]] || fail "prepare-compute-role.sh must call write_cubelet_cubeops_addr"
  [[ -n "${exit_line}" ]] || fail "prepare-compute-role.sh must still gate compute-only work"
  (( write_line < exit_line )) || fail "write_cubelet_cubeops_addr must run before is_compute_role early exit (write=${write_line} exit=${exit_line})"
}

test_up_compute_uses_shared_helper() {
  assert_contains "${ONE_CLICK_DIR}/scripts/one-click/up-compute.sh" "write_cubelet_cubeops_addr"
  if grep -Eq 'sed -i -e "s#\^\[\[:space:\]\]\*cubeops_addr' "${ONE_CLICK_DIR}/scripts/one-click/up-compute.sh"; then
    fail "up-compute.sh must not keep an inline cubeops_addr sed"
  fi
}

write_sample_storage_config() {
  local path="$1"
  cat > "${path}" <<'EOF'
    [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    [plugins."io.cubelet.internal.v1.storage".cow.log]
    level = "info"
  [plugins."io.cubelet.internal.v1.images"]
    runtime_type = "io.containerd.cube.v2"
EOF
}

assert_s3_before_images() {
  local path="$1"
  local s3_line images_line
  s3_line="$(grep -nE "$(cubelet_s3_header_re)" "${path}" | head -1 | cut -d: -f1)"
  images_line="$(grep -nE '^[[:space:]]*\[plugins\."io\.cubelet\.internal\.v1\.images"\]' "${path}" | head -1 | cut -d: -f1)"
  [[ -n "${s3_line}" ]] || fail "expected ${path} to contain a live cow.s3 header"
  [[ -n "${images_line}" ]] || fail "expected ${path} to contain images"
  (( s3_line < images_line )) || fail "cow.s3 must sit before images (s3=${s3_line} images=${images_line})"
}

test_s3lvol_enable_inserts_after_cow_log() {
  local cfg="${TMP_DIR}/s3lvol-insert.toml"
  write_sample_storage_config "${cfg}"
  write_cubelet_s3lvol_enable "${cfg}" 1
  [[ "$(count_s3_headers "${cfg}")" == "1" ]] \
    || fail "insert path must write exactly one cow.s3 table"
  assert_contains "${cfg}" '[plugins."io.cubelet.internal.v1.storage".cow.s3]'
  assert_contains "${cfg}" 'enable = true'
  assert_contains "${cfg}" 'socket_path = "/var/run/s3lvol.sock"'
  assert_s3_before_images "${cfg}"
}

test_s3lvol_enable_flips_existing() {
  local cfg="${TMP_DIR}/s3lvol-flip.toml"
  cat > "${cfg}" <<'EOF'
    [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    [plugins."io.cubelet.internal.v1.storage".cow.log]
    level = "info"
    [plugins."io.cubelet.internal.v1.storage".cow.s3]
    enable = false
    socket_path = "/tmp/handwritten.sock"
  [plugins."io.cubelet.internal.v1.images"]
    runtime_type = "io.containerd.cube.v2"
EOF
  write_cubelet_s3lvol_enable "${cfg}" 1
  assert_contains "${cfg}" 'enable = true'
  assert_contains "${cfg}" 'socket_path = "/tmp/handwritten.sock"'
  write_cubelet_s3lvol_enable "${cfg}" 0
  assert_contains "${cfg}" 'enable = false'
  if grep -Eq '^[[:space:]]*enable[[:space:]]*=[[:space:]]*true' "${cfg}"; then
    fail "write_cubelet_s3lvol_enable 0 must clear enable = true"
  fi
  assert_contains "${cfg}" 'socket_path = "/tmp/handwritten.sock"'
  assert_s3_before_images "${cfg}"
}

count_s3_headers() {
  count_live_cubelet_s3_headers "$1"
}

test_s3lvol_enable_inserts_when_header_is_commented() {
  local cfg="${TMP_DIR}/s3lvol-commented.toml"
  cat > "${cfg}" <<'EOF'
    [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    [plugins."io.cubelet.internal.v1.storage".cow.log]
    level = "info"
    # [plugins."io.cubelet.internal.v1.storage".cow.s3]
    # enable = false
  [plugins."io.cubelet.internal.v1.images"]
    runtime_type = "io.containerd.cube.v2"
EOF
  write_cubelet_s3lvol_enable "${cfg}" 1
  [[ "$(count_s3_headers "${cfg}")" == "1" ]] \
    || fail "commented cow.s3 must not count as a live table"
  assert_contains "${cfg}" '# [plugins."io.cubelet.internal.v1.storage".cow.s3]'
  assert_contains "${cfg}" 'enable = true'
  assert_contains "${cfg}" 'socket_path = "/var/run/s3lvol.sock"'
  assert_s3_before_images "${cfg}"
}

test_s3lvol_enable_flips_section_after_images() {
  local cfg="${TMP_DIR}/s3lvol-after-images.toml"
  cat > "${cfg}" <<'EOF'
    [plugins."io.cubelet.internal.v1.storage"]
    storage_backend = "cubecow"
    [plugins."io.cubelet.internal.v1.storage".cow.log]
    level = "info"
  [plugins."io.cubelet.internal.v1.images"]
    runtime_type = "io.containerd.cube.v2"
    [plugins."io.cubelet.internal.v1.storage".cow.s3]
    enable = false
    socket_path = "/tmp/appended.sock"
EOF
  write_cubelet_s3lvol_enable "${cfg}" 1
  [[ "$(count_s3_headers "${cfg}")" == "1" ]] \
    || fail "write_cubelet_s3lvol_enable must not insert a second cow.s3 table"
  assert_contains "${cfg}" 'enable = true'
  assert_contains "${cfg}" 'socket_path = "/tmp/appended.sock"'
  if grep -Fq 'socket_path = "/var/run/s3lvol.sock"' "${cfg}"; then
    fail "existing handwritten socket_path must be kept when the section already exists"
  fi
}

test_cubelet_start_writes_s3lvol_enable() {
  assert_contains "${ONE_CLICK_DIR}/scripts/systemd/cubelet-start.sh" "write_cubelet_s3lvol_enable"
  assert_contains "${ONE_CLICK_DIR}/scripts/systemd/cubelet-start.sh" "ONE_CLICK_ENABLE_S3LVOL"
  if grep -Fq "write_cubelet_s3lvol_socket" "${ONE_CLICK_DIR}/scripts/systemd/cubelet-start.sh"; then
    fail "cubelet-start.sh must not keep write_cubelet_s3lvol_socket"
  fi
}

test_control_default_writes_local_cubeops
test_scheme_passthrough
test_compute_override_addr
test_inserts_when_key_missing
test_prepare_writes_before_compute_only_exit
test_up_compute_uses_shared_helper
test_s3lvol_enable_inserts_after_cow_log
test_s3lvol_enable_flips_existing
test_s3lvol_enable_flips_section_after_images
test_s3lvol_enable_inserts_when_header_is_commented
test_cubelet_start_writes_s3lvol_enable

echo "cubeops_addr tests OK"
