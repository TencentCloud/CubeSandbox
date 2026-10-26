#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# CUBE_SET_* allowlisted yaml/toml overlays used by install.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"
# shellcheck source=../scripts/common/config_set.sh
source "${ONE_CLICK_DIR}/scripts/common/config_set.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  local path="$1"
  local needle="$2"
  grep -Fq -- "${needle}" "${path}" || fail "expected ${path} to contain ${needle}"
}

assert_not_contains() {
  if grep -Fq -- "$2" "$1"; then
    fail "expected $1 NOT to contain: $2"
  fi
}

reset_cube_set_env() {
  local var
  for var in $(compgen -e | grep -E '^CUBE_SET_[0-9]+_' || true); do
    unset "${var}"
  done
  CUBE_SET_THIS_RUN=0
  CUBE_SET_ENV_SNAPSHOT=()
  CUBE_SET_DOTENV_SNAPSHOT=()
}

stage_tree() {
  local root="$1"
  mkdir -p \
    "${root}/Cubelet/config" \
    "${root}/Cubelet/dynamicconf" \
    "${root}/CubeMaster" \
    "${root}/CubeTemplateCenter" \
    "${root}/cube-shim/conf"
  cp -f "${REPO_ROOT}/Cubelet/config/config.toml" "${root}/Cubelet/config/config.toml"
  cp -f "${REPO_ROOT}/configs/single-node/cubemaster.yaml" "${root}/CubeMaster/conf.yaml"
  cat > "${root}/Cubelet/dynamicconf/conf.yaml" <<'EOF'
log:
  level: info
common:
  default_dns_servers:
    - 119.29.29.29
  default_dns_searches: []
  default_dns_options: []
host:
  quota:
    mem_limit: ""
    mcpu_limit: 0
EOF
  cat > "${root}/CubeTemplateCenter/conf.yaml" <<'EOF'
instance_db_config:
  addr: "127.0.0.1:3306"
redis:
  db_no: 0
EOF
  cat > "${root}/cube-shim/conf/config-cube.toml" <<'EOF'
[hypervisor]
  default_vcpus = 1
EOF
}

test_toml_insert_image_base_path() {
  local root="${TMP_DIR}/toml-insert"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/cubebox_os_image"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
  apply_cube_set_overrides "${root}"
  local count
  count="$(grep -c 'image_base_path = "/data/cubebox_os_image"' "${root}/Cubelet/config/config.toml")"
  [[ "${count}" -eq 1 ]] || fail "image_base_path insert must be idempotent (got ${count})"
}

test_toml_update_existing() {
  local root="${TMP_DIR}/toml-update"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/cubebox_os_image"
  apply_cube_set_overrides "${root}"
  export CUBE_SET_1_VALUE="/data/cubelet/cubebox_os_image"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubelet/cubebox_os_image"'
  assert_not_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
}

test_yaml_update_timeout() {
  local root="${TMP_DIR}/yaml-update"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.create_image_timeout_insec"
  export CUBE_SET_1_VALUE="600"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" "create_image_timeout_insec: 600"
}

test_rejects_unknown_file() {
  local root="${TMP_DIR}/unknown-file"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/secret.toml"
  export CUBE_SET_1_PATH="root"
  export CUBE_SET_1_VALUE="x"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "unknown file must be rejected"
  fi
}

test_rejects_symlink() {
  local root="${TMP_DIR}/symlink"
  rm -rf "${root}"
  stage_tree "${root}"
  mv "${root}/Cubelet/config/config.toml" "${root}/Cubelet/config/real.toml"
  ln -s real.toml "${root}/Cubelet/config/config.toml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/cubebox_os_image"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "symlink config must be rejected"
  fi
}

test_rejects_missing_parent_table() {
  local root="${TMP_DIR}/missing-table"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.does.not.exist".foo'
  export CUBE_SET_1_VALUE="1"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "missing TOML parent table must be rejected"
  fi
}

test_incomplete_group_fails() {
  local root="${TMP_DIR}/incomplete"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "missing CUBE_SET_1_VALUE must fail"
  fi
}

test_reserved_first_class_skipped() {
  local root="${TMP_DIR}/reserved"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.network".cidr'
  export CUBE_SET_1_VALUE="10.1.0.0/16"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'cidr = "192.168.0.0/18"'
  assert_not_contains "${root}/Cubelet/config/config.toml" 'cidr = "10.1.0.0/16"'
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="common.http_bind"
  export CUBE_SET_1_VALUE="127.0.0.1"
  apply_cube_set_overrides "${root}"
  assert_not_contains "${root}/CubeTemplateCenter/conf.yaml" "http_bind"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="common.http_port"
  export CUBE_SET_1_VALUE="9090"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" "http_port: 8089"
  assert_not_contains "${root}/CubeMaster/conf.yaml" "http_port: 9090"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.grpc.grpc_port"
  export CUBE_SET_1_VALUE="8888"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" "grpc_port: 9999"
  assert_not_contains "${root}/CubeMaster/conf.yaml" "grpc_port: 8888"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH="grpc.tcp_address"
  export CUBE_SET_1_VALUE=":8888"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'tcp_address = ":9999"'
  assert_not_contains "${root}/Cubelet/config/config.toml" 'tcp_address = ":8888"'
}

test_persist_and_merge_keeps_cube_set() {
  local new_example="${TMP_DIR}/new.example"
  local old_runtime="${TMP_DIR}/old.env"
  local out="${TMP_DIR}/merged.env"
  local diff="${TMP_DIR}/diff.txt"
  cat > "${new_example}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
CUBE_PVM_ENABLE=0
EOF
  cat > "${old_runtime}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
CUBE_PVM_ENABLE=0
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=plugins."io.cubelet.internal.v1.images".image_base_path
CUBE_SET_1_VALUE=/data/cubebox_os_image
EOF
  merge_env_three_way "${new_example}" "${old_runtime}" "" "" "${out}" "${diff}" 2>/dev/null
  grep -Fq 'CUBE_SET_1_FILE=Cubelet/config/config.toml' "${out}" \
    || fail "upgrade merge must keep CUBE_SET_1_FILE"
  grep -Fq 'CUBE_SET_1_VALUE=/data/cubebox_os_image' "${out}" \
    || fail "upgrade merge must keep CUBE_SET_1_VALUE"
}

test_dotenv_unquoted_path() {
  local root="${TMP_DIR}/dotenv-unquoted"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  local envf="${TMP_DIR}/unquoted.env"
  cat > "${envf}" <<'EOF'
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=plugins."io.cubelet.internal.v1.images".image_base_path
CUBE_SET_1_VALUE=/data/cubebox_os_image
EOF
  load_env_file "${envf}"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
}

test_dotenv_single_quoted_path() {
  local root="${TMP_DIR}/dotenv-quoted"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  local envf="${TMP_DIR}/quoted.env"
  cat > "${envf}" <<'EOF'
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
CUBE_SET_1_VALUE=/data/cubebox_os_image
EOF
  load_env_file "${envf}"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
}

test_yaml_refuses_flow_collection() {
  local root="${TMP_DIR}/yaml-flow"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_1_PATH="common.default_dns_searches"
  export CUBE_SET_1_VALUE="corp.local"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "replacing default_dns_searches flow list must fail"
  fi
  assert_contains "${root}/Cubelet/dynamicconf/conf.yaml" "default_dns_searches: []"
  assert_not_contains "${root}/Cubelet/dynamicconf/conf.yaml" "corp.local"
}

test_yaml_refuses_sequence_descent() {
  local root="${TMP_DIR}/yaml-seq"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_1_PATH="common.default_dns_servers.0"
  export CUBE_SET_1_VALUE="223.5.5.5"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "descending into default_dns_servers sequence must fail"
  fi
  assert_not_contains "${root}/Cubelet/dynamicconf/conf.yaml" "223.5.5.5"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="scheduler.filter.enable_filters.cpu"
  export CUBE_SET_1_VALUE="1"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "descending into enable_filters sequence must fail"
  fi
  assert_contains "${root}/CubeMaster/conf.yaml" '- "cpu"'
}

test_toml_refuses_array_and_child_table() {
  local root="${TMP_DIR}/toml-composite"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.containerd.runtime.v2.task".platforms'
  export CUBE_SET_1_VALUE="linux/arm64"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "replacing TOML array platforms must fail"
  fi
  assert_contains "${root}/Cubelet/config/config.toml" 'platforms = ["linux/amd64", "linux/arm64"]'
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.storage".volume_plugins'
  export CUBE_SET_1_VALUE="s3"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "shadowing volume_plugins array-of-tables must fail"
  fi
  assert_not_contains "${root}/Cubelet/config/config.toml" "volume_plugins ="
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.storage".cow'
  export CUBE_SET_1_VALUE="x"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "shadowing implicit ancestor table cow must fail"
  fi
  assert_not_contains "${root}/Cubelet/config/config.toml" "cow ="
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.containerd.cri.v1.images".registry'
  export CUBE_SET_1_VALUE="x"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "shadowing implicit ancestor table registry must fail"
  fi
  assert_not_contains "${root}/Cubelet/config/config.toml" "registry ="
}

test_yaml_refuses_mapping_and_sequence() {
  local root="${TMP_DIR}/yaml-refuse"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf"
  export CUBE_SET_1_VALUE="nope"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "replacing cubelet_conf mapping must fail"
  fi
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.exposed_port_list"
  export CUBE_SET_1_VALUE="80"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "replacing exposed_port_list sequence must fail"
  fi
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="instance_db_config.addr.port"
  export CUBE_SET_1_VALUE="3307"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "inserting under colon-bearing scalar addr must fail"
  fi
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "dry-run must also refuse inserting under colon-bearing scalar addr"
  fi
  assert_contains "${root}/CubeMaster/conf.yaml" 'addr: "127.0.0.1:__CUBE_SANDBOX_MYSQL_PORT__"'
  assert_not_contains "${root}/CubeMaster/conf.yaml" "port: 3307"
  printf '\nflow_parent: {a: 1}\n' >> "${root}/CubeMaster/conf.yaml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="flow_parent.child"
  export CUBE_SET_1_VALUE="x"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "inserting under a flow mapping must fail"
  fi
  assert_not_contains "${root}/CubeMaster/conf.yaml" "child: x"
}

test_yaml_refuses_invalid_segment() {
  local root="${TMP_DIR}/yaml-segment"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH='common.http_port '
  export CUBE_SET_1_VALUE="9090"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "YAML path with trailing space must fail"
  fi
  assert_contains "${root}/CubeMaster/conf.yaml" "http_port: 8089"
  assert_not_contains "${root}/CubeMaster/conf.yaml" "http_port :"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH='common."foo: bar"'
  export CUBE_SET_1_VALUE="x"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "YAML path with colon in segment must fail"
  fi
  assert_not_contains "${root}/CubeMaster/conf.yaml" "foo: bar:"
}

test_compute_skips_control_plane_file() {
  local root="${TMP_DIR}/compute-skip"
  local envf="${TMP_DIR}/compute-skip.env"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  DEPLOY_ROLE=compute
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.create_image_timeout_insec"
  export CUBE_SET_1_VALUE="600"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_2_VALUE="/data/cubebox_os_image"
  apply_cube_set_overrides "${root}" \
    || fail "compute role must skip control-plane CUBE_SET instead of aborting"
  assert_contains "${root}/CubeMaster/conf.yaml" "create_image_timeout_insec: 300"
  assert_not_contains "${root}/CubeMaster/conf.yaml" "create_image_timeout_insec: 600"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
  : > "${envf}"
  CUBE_SET_THIS_RUN=1
  persist_cube_set_overrides "${envf}"
  if grep -Eq '^CUBE_SET_1_' "${envf}"; then
    fail "compute persist must drop control-plane CUBE_SET_1"
  fi
  grep -Fq 'CUBE_SET_2_FILE=Cubelet/config/config.toml' "${envf}" \
    || fail "compute persist must keep Cubelet CUBE_SET_2"
  DEPLOY_ROLE=control
}

test_value_newline_fails_validate() {
  local root="${TMP_DIR}/value-newline"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="log.level"
  export CUBE_SET_1_VALUE=$'line1\nline2'
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "newline in CUBE_SET VALUE must fail dry-run"
  fi
}

test_auto_keeps_non_roundtrip_literals() {
  local root="${TMP_DIR}/auto-literal"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.some_version"
  export CUBE_SET_1_VALUE="1.10"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" 'some_version: "1.10"'
  assert_not_contains "${root}/CubeMaster/conf.yaml" "some_version: 1.1"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.some_mode"
  export CUBE_SET_1_VALUE="0755"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" 'some_mode: "0755"'
  assert_not_contains "${root}/CubeMaster/conf.yaml" "some_mode: 755"
}

test_auto_keeps_quoted_yaml_string() {
  local root="${TMP_DIR}/auto-quoted"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_1_PATH="host.quota.mem_limit"
  export CUBE_SET_1_VALUE="1024"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/dynamicconf/conf.yaml" 'mem_limit: "1024"'
  assert_not_contains "${root}/Cubelet/dynamicconf/conf.yaml" "mem_limit: 1024"
}

test_toml_header_trailing_comment() {
  local root="${TMP_DIR}/toml-comment-header"
  rm -rf "${root}"
  stage_tree "${root}"
  cat >> "${root}/Cubelet/config/config.toml" <<'EOF'

[plugins."io.cubelet.internal.v1.images".comment_overlay]  # trailing
  keep_me = 1

[plugins."io.cubelet.internal.v1.images".after_comment]
  keep_me = 9
EOF
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".comment_overlay.keep_me'
  export CUBE_SET_1_VALUE="2"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" "keep_me = 2"
  assert_contains "${root}/Cubelet/config/config.toml" "keep_me = 9"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".comment_overlay.new_leaf'
  export CUBE_SET_1_VALUE="ok"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'new_leaf = "ok"'
  if awk '/after_comment/{p=1} p && /new_leaf/{exit 1}' "${root}/Cubelet/config/config.toml"; then
    :
  else
    fail "new_leaf must land in the commented table, not after_comment"
  fi
}

test_toml_body_end_skips_nested_arrays() {
  local root="${TMP_DIR}/toml-array-body"
  rm -rf "${root}"
  stage_tree "${root}"
  cat >> "${root}/Cubelet/config/config.toml" <<'EOF'

[plugins."io.cubelet.internal.v1.images".test_overlay]
  actions = [
    ["cubebox"],
    ["images", "volume"],
  ]
  keep_me = 1
EOF
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".test_overlay.keep_me'
  export CUBE_SET_1_VALUE="2"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" "keep_me = 2"
  count="$(grep -c 'keep_me =' "${root}/Cubelet/config/config.toml")"
  [[ "${count}" -eq 1 ]] || fail "keep_me must update in place (got ${count})"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".test_overlay.new_leaf'
  export CUBE_SET_1_VALUE="ok"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'new_leaf = "ok"'
  if grep -A2 'actions = \[' "${root}/Cubelet/config/config.toml" | grep -q 'new_leaf'; then
    fail "new_leaf must not be inserted inside the actions array"
  fi
}

test_yaml_type_string_quotes() {
  local root="${TMP_DIR}/yaml-string"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="log.level"
  export CUBE_SET_1_VALUE="on"
  export CUBE_SET_1_TYPE="string"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" 'level: "on"'
}

test_persist_this_run_drops_stale_groups() {
  reset_cube_set_env
  local envf="${TMP_DIR}/persist.env"
  cat > "${envf}" <<'EOF'
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=old.path
CUBE_SET_1_VALUE=/old
CUBE_SET_2_FILE=CubeMaster/conf.yaml
CUBE_SET_2_PATH=log.level
CUBE_SET_2_VALUE=debug
EOF
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/new"
  CUBE_SET_THIS_RUN=1
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_FILE"]="${CUBE_SET_1_FILE}"
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_PATH"]="${CUBE_SET_1_PATH}"
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_VALUE"]="${CUBE_SET_1_VALUE}"
  persist_cube_set_overrides "${envf}"
  grep -Fq 'CUBE_SET_1_VALUE=/data/new' "${envf}" \
    || fail "persist must write this-run CUBE_SET_1_VALUE"
  if grep -Eq '^CUBE_SET_2_' "${envf}"; then
    fail "this-run persist must drop stale CUBE_SET_2"
  fi
}

test_upgrade_intent_replaces_old_value() {
  reset_cube_set_env
  CUBE_SET_THIS_RUN=0
  snapshot_cube_set_process_env
  local envf="${TMP_DIR}/intent.env"
  cat > "${envf}" <<'EOF'
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
CUBE_SET_1_VALUE=/data/b
EOF
  # Simulate merge clobbering this-run .env with the previous runtime value.
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/a"
  load_env_file "${envf}"
  capture_cube_set_dotenv_values "${envf}"
  export CUBE_SET_1_VALUE="/data/a"
  apply_cube_set_intent
  [[ "${CUBE_SET_1_VALUE}" == "/data/b" ]] || fail "this-run .env CUBE_SET value must win over merged /data/a (got ${CUBE_SET_1_VALUE})"
  # process-env snapshot is empty on this path; the count guard must not trip.
}

test_dotenv_export_and_mixed_style() {
  reset_cube_set_env
  snapshot_cube_set_process_env
  local envf="${TMP_DIR}/export-mixed.env"
  cat > "${envf}" <<'EOF'
export CUBE_SET_1_FILE=Cubelet/config/config.toml
  CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
CUBE_SET_1_VALUE=/data/b
EOF
  export CUBE_SET_1_VALUE="/data/a"
  load_env_file "${envf}"
  capture_cube_set_dotenv_values "${envf}"
  apply_cube_set_intent
  cube_set_dotenv_declared \
    || fail "export/indented CUBE_SET lines must count as this-run .env"
  [[ -n "${CUBE_SET_1_FILE+x}" && -n "${CUBE_SET_1_PATH+x}" && -n "${CUBE_SET_1_VALUE+x}" ]] \
    || fail "mixed export/plain CUBE_SET keys must all be restored"
  [[ "${CUBE_SET_1_VALUE}" == "/data/b" ]] \
    || fail "export/mixed .env value must win (got ${CUBE_SET_1_VALUE})"
}

test_no_cube_set_skips_python3() {
  reset_cube_set_env
  local root="${TMP_DIR}/no-set"
  mkdir -p "${root}"
  (
    require_cmd() {
      fail "require_cmd $* must not run when no CUBE_SET is set"
    }
    validate_cube_set_overrides "${root}"
    apply_cube_set_overrides "${root}"
  ) || fail "no CUBE_SET must be a no-op without python3"
}

test_process_env_only_overlays_merged_set() {
  reset_cube_set_env
  export CUBE_SET_2_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_2_PATH="cubelet_conf.create_image_timeout_insec"
  export CUBE_SET_2_VALUE="600"
  snapshot_cube_set_process_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/keep"
  local envf="${TMP_DIR}/process-only.env"
  cat > "${envf}" <<'EOF'
# this-run .env lists no CUBE_SET_* assignments
INSTALL_PREFIX=/usr/local/services/cubetoolbox
EOF
  capture_cube_set_dotenv_values "${envf}"
  apply_cube_set_intent
  [[ "${CUBE_SET_1_VALUE:-}" == "/data/keep" ]] \
    || fail "process-env-only must keep merged CUBE_SET_1 (got ${CUBE_SET_1_VALUE:-unset})"
  [[ "${CUBE_SET_2_VALUE:-}" == "600" ]] \
    || fail "process-env-only must apply CUBE_SET_2 (got ${CUBE_SET_2_VALUE:-unset})"
  local runtime="${TMP_DIR}/process-only-runtime.env"
  cat > "${runtime}" <<'EOF'
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=old.path
CUBE_SET_1_VALUE=/old
CUBE_SET_3_FILE=CubeMaster/conf.yaml
CUBE_SET_3_PATH=log.level
CUBE_SET_3_VALUE=debug
EOF
  persist_cube_set_overrides "${runtime}"
  grep -Fq 'CUBE_SET_1_VALUE=/data/keep' "${runtime}" \
    || fail "process-env persist must keep/update merged CUBE_SET_1"
  grep -Fq 'CUBE_SET_2_VALUE=600' "${runtime}" \
    || fail "process-env persist must write CUBE_SET_2"
  grep -Fq 'CUBE_SET_3_VALUE=debug' "${runtime}" \
    || fail "process-env persist must leave other persisted groups in the file"
}

test_install_sh_wires_config_set() {
  assert_contains "${ONE_CLICK_DIR}/install.sh" "scripts/common/config_set.sh"
  assert_contains "${ONE_CLICK_DIR}/install.sh" 'validate_cube_set_overrides "${PKG_ROOT}"'
  assert_contains "${ONE_CLICK_DIR}/install.sh" 'apply_cube_set_overrides "${PKG_ROOT}"'
  assert_contains "${ONE_CLICK_DIR}/install.sh" 'persist_cube_set_overrides "${RUNTIME_ENV_FILE}"'
  assert_contains "${ONE_CLICK_DIR}/install.sh" "snapshot_cube_set_process_env"
  assert_contains "${ONE_CLICK_DIR}/install.sh" "apply_cube_set_intent"
  local validate apply persist assert_line
  validate="$(grep -n 'validate_cube_set_overrides "${PKG_ROOT}"' "${ONE_CLICK_DIR}/install.sh" | head -1 | cut -d: -f1)"
  apply="$(grep -n 'apply_cube_set_overrides "${PKG_ROOT}"' "${ONE_CLICK_DIR}/install.sh" | head -1 | cut -d: -f1)"
  persist="$(grep -n 'persist_cube_set_overrides "${RUNTIME_ENV_FILE}"' "${ONE_CLICK_DIR}/install.sh" | head -1 | cut -d: -f1)"
  assert_line="$(grep -n 'assert_safe_install_prefix "${INSTALL_PREFIX}"' "${ONE_CLICK_DIR}/install.sh" | head -1 | cut -d: -f1)"
  [[ -n "${validate}" && -n "${apply}" && -n "${persist}" && -n "${assert_line}" ]] \
    || fail "install.sh must validate/apply CUBE_SET on PKG_ROOT"
  [[ "${validate}" -lt "${assert_line}" ]] \
    || fail "CUBE_SET validate must run before assert_safe_install_prefix (validate=${validate} assert=${assert_line})"
  [[ "${assert_line}" -lt "${apply}" && "${apply}" -lt "${persist}" ]] \
    || fail "CUBE_SET apply must run after the prefix check (assert=${assert_line} apply=${apply} persist=${persist})"
}

test_toml_insert_image_base_path
test_toml_update_existing
test_yaml_update_timeout
test_rejects_unknown_file
test_rejects_symlink
test_rejects_missing_parent_table
test_incomplete_group_fails
test_reserved_first_class_skipped
test_persist_and_merge_keeps_cube_set
test_dotenv_unquoted_path
test_dotenv_single_quoted_path
test_yaml_refuses_mapping_and_sequence
test_yaml_refuses_sequence_descent
test_yaml_refuses_flow_collection
test_toml_refuses_array_and_child_table
test_yaml_type_string_quotes
test_auto_keeps_quoted_yaml_string
test_auto_keeps_non_roundtrip_literals
test_toml_header_trailing_comment
test_toml_body_end_skips_nested_arrays
test_yaml_refuses_invalid_segment
test_compute_skips_control_plane_file
test_value_newline_fails_validate
test_persist_this_run_drops_stale_groups
test_upgrade_intent_replaces_old_value
test_dotenv_export_and_mixed_style
test_no_cube_set_skips_python3
test_process_env_only_overlays_merged_set
test_install_sh_wires_config_set

echo "config_set tests OK"
