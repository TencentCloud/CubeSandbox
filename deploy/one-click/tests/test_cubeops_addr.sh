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

test_control_default_writes_local_cubeops
test_scheme_passthrough
test_compute_override_addr
test_inserts_when_key_missing
test_prepare_writes_before_compute_only_exit
test_up_compute_uses_shared_helper

echo "cubeops_addr tests OK"
