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
  for var in $(compgen -e | grep -E '^CUBE_SET_' || true); do
    [[ "${var}" == "CUBE_SET_THIS_RUN" ]] && continue
    unset "${var}"
  done
  CUBE_SET_THIS_RUN=0
  CUBE_SET_DOTENV_PRESENT=0
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

test_toml_update_image_base_path() {
  local root="${TMP_DIR}/toml-image-base"
  rm -rf "${root}"
  stage_tree "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = ""'
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/cubebox_os_image"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
  apply_cube_set_overrides "${root}"
  local count
  count="$(grep -c 'image_base_path = ' "${root}/Cubelet/config/config.toml")"
  [[ "${count}" -eq 1 ]] || fail "image_base_path update must be idempotent (got ${count})"
}

test_rejects_missing_key() {
  local root="${TMP_DIR}/missing-key"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${root}/Cubelet/config/config.toml" "${TMP_DIR}/missing-key.toml.orig"
  cp -f "${root}/CubeMaster/conf.yaml" "${TMP_DIR}/missing-key.yaml.orig"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".no_such_key'
  export CUBE_SET_1_VALUE="x"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "missing TOML key must fail dry-run"
  fi
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "missing TOML key must not be inserted"
  fi
  cmp -s "${root}/Cubelet/config/config.toml" "${TMP_DIR}/missing-key.toml.orig" \
    || fail "missing TOML key must leave config.toml untouched"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="cubelet_conf.no_such_key"
  export CUBE_SET_1_VALUE="x"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "missing YAML key must not be inserted"
  fi
  cmp -s "${root}/CubeMaster/conf.yaml" "${TMP_DIR}/missing-key.yaml.orig" \
    || fail "missing YAML key must leave conf.yaml untouched"
}

test_yaml_same_indent_sequence() {
  local root="${TMP_DIR}/yaml-same-indent"
  rm -rf "${root}"
  stage_tree "${root}"
  cat > "${root}/CubeTemplateCenter/conf.yaml" <<'EOF'
common:
  bar:
  - a
  - b
  after: 1
EOF
  cp -f "${root}/CubeTemplateCenter/conf.yaml" "${TMP_DIR}/same-indent.orig"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="common.bar.x"
  export CUBE_SET_1_VALUE="v"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "descending into a same-indent sequence must fail"
  fi
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="common.bar"
  export CUBE_SET_1_VALUE="v"
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "replacing a same-indent sequence with a scalar must fail"
  fi
  cmp -s "${root}/CubeTemplateCenter/conf.yaml" "${TMP_DIR}/same-indent.orig" \
    || fail "same-indent sequence file must stay unchanged"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="common.after"
  export CUBE_SET_1_VALUE="2"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeTemplateCenter/conf.yaml" "  after: 2"
  assert_contains "${root}/CubeTemplateCenter/conf.yaml" "  - b"
}

test_rejects_intermediate_symlink() {
  local root="${TMP_DIR}/mid-symlink"
  local outside="${TMP_DIR}/mid-symlink-outside"
  rm -rf "${root}" "${outside}"
  stage_tree "${root}"
  mkdir -p "${outside}"
  mv "${root}/Cubelet/config/config.toml" "${outside}/config.toml"
  rmdir "${root}/Cubelet/config"
  ln -s "${outside}" "${root}/Cubelet/config"
  cp -f "${outside}/config.toml" "${TMP_DIR}/mid-symlink.orig"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/cubebox_os_image"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "intermediate symlink must fail dry-run"
  fi
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "intermediate symlink must be rejected"
  fi
  cmp -s "${outside}/config.toml" "${TMP_DIR}/mid-symlink.orig" \
    || fail "file behind an intermediate symlink must not be modified"
}

test_preserves_crlf_and_inline_comment() {
  local root="${TMP_DIR}/crlf-comment"
  rm -rf "${root}"
  stage_tree "${root}"
  printf 'log:\r\n  level: info  # keep me\r\nredis:\r\n  db_no: 0\r\n' \
    > "${root}/CubeTemplateCenter/conf.yaml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="log.level"
  export CUBE_SET_1_VALUE="debug"
  apply_cube_set_overrides "${root}"
  local expected
  expected="$(printf 'log:\r\n  level: "debug"  # keep me\r\nredis:\r\n  db_no: 0\r\n')"
  [[ "$(cat "${root}/CubeTemplateCenter/conf.yaml")" == "${expected}" ]] \
    || fail "YAML update must keep CRLF and the trailing comment: $(od -c "${root}/CubeTemplateCenter/conf.yaml" | head -5)"
  cat >> "${root}/Cubelet/config/config.toml" <<'EOF'

[comment_overlay]
  file_num = 5 # rotate count
  label = "a # not a comment" # real comment
EOF
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH="comment_overlay.file_num"
  export CUBE_SET_1_VALUE="9"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH="comment_overlay.label"
  export CUBE_SET_2_VALUE="b"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" "file_num = 9 # rotate count"
  assert_contains "${root}/Cubelet/config/config.toml" 'label = "b" # real comment'
  printf '\ndesc: it'"'"'s here  # note\n' >> "${root}/CubeMaster/conf.yaml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="desc"
  export CUBE_SET_1_VALUE="new"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" 'desc: "new"  # note'
}

test_dotenv_without_cube_set_clears() {
  reset_cube_set_env
  snapshot_cube_set_process_env
  local envf="${TMP_DIR}/no-cube-set.env"
  cat > "${envf}" <<'EOF'
ONE_CLICK_ENABLE_S3LVOL=1
EOF
  # Simulate the upgrade merge restoring a previously persisted group.
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/old"
  load_env_file "${envf}"
  capture_cube_set_dotenv_values "${envf}"
  apply_cube_set_intent > "${TMP_DIR}/no-cube-set.log" 2>&1
  grep -Fq "dropping previously persisted group(s) not listed this run: 1" "${TMP_DIR}/no-cube-set.log" \
    || fail "dropping persisted groups must be logged: $(cat "${TMP_DIR}/no-cube-set.log")"
  [[ -z "${CUBE_SET_1_VALUE+x}" ]] \
    || fail "a .env without CUBE_SET_* must drop the merged CUBE_SET_1 (got ${CUBE_SET_1_VALUE})"
  local runtime="${TMP_DIR}/no-cube-set-runtime.env"
  cat > "${runtime}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=plugins."io.cubelet.internal.v1.images".image_base_path
CUBE_SET_1_VALUE=/data/old
EOF
  persist_cube_set_overrides "${runtime}"
  if grep -Eq 'CUBE_SET_' "${runtime}"; then
    fail "a .env without CUBE_SET_* must clear persisted groups"
  fi
  grep -Fq 'ONE_CLICK_DEPLOY_ROLE=control' "${runtime}" || fail "clearing must keep unrelated keys"
  unset ONE_CLICK_ENABLE_S3LVOL
}

test_dotenv_and_process_env_groups_both_apply() {
  reset_cube_set_env
  export CUBE_SET_2_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_2_PATH="host.scheduler_label"
  export CUBE_SET_2_VALUE="from-cli"
  export CUBE_SET_1_VALUE="cli-loses"
  snapshot_cube_set_process_env
  local envf="${TMP_DIR}/dotenv-plus-cli.env"
  cat > "${envf}" <<'EOF'
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=plugins."io.cubelet.internal.v1.images".image_base_path
CUBE_SET_1_VALUE=/data/from-dotenv
EOF
  load_env_file "${envf}"
  capture_cube_set_dotenv_values "${envf}"
  apply_cube_set_intent
  [[ "${CUBE_SET_1_VALUE}" == "/data/from-dotenv" ]] \
    || fail ".env must win on the same name (got ${CUBE_SET_1_VALUE})"
  [[ "${CUBE_SET_2_VALUE:-}" == "from-cli" ]] \
    || fail "command-line group not in .env must still apply (got ${CUBE_SET_2_VALUE:-unset})"
  local runtime="${TMP_DIR}/dotenv-plus-cli-runtime.env"
  printf 'ONE_CLICK_DEPLOY_ROLE=control\n' > "${runtime}"
  persist_cube_set_overrides "${runtime}"
  grep -Fq 'CUBE_SET_1_VALUE=/data/from-dotenv' "${runtime}" || fail ".env group must persist"
  grep -Fq 'CUBE_SET_2_VALUE=from-cli' "${runtime}" || fail "command-line group must persist"
}

test_auto_keeps_bare_float_field_numeric() {
  local root="${TMP_DIR}/auto-float"
  rm -rf "${root}"
  stage_tree "${root}"
  printf '\nsome_ratio: 0.0\n' >> "${root}/CubeMaster/conf.yaml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="some_ratio"
  export CUBE_SET_1_VALUE="0.10"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH='plugins."io.containerd.gc.v1.scheduler".pause_threshold'
  export CUBE_SET_2_VALUE="0.050"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" "some_ratio: 0.1"
  assert_not_contains "${root}/CubeMaster/conf.yaml" 'some_ratio: "'
  assert_contains "${root}/Cubelet/config/config.toml" "pause_threshold = 0.05"
}

fake_generate_adds_key() {
  printf 'generated_key: 1\n' >> "${PKG_ROOT}/CubeMaster/conf.yaml"
}

fake_generate_fails() {
  echo "fake generator boom" >&2
  return 3
}

test_validate_replays_generate_hook() {
  local root="${TMP_DIR}/validate-hook"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${root}/CubeMaster/conf.yaml" "${TMP_DIR}/validate-hook.orig"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="generated_key"
  export CUBE_SET_1_VALUE="2"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "without the hook generated_key does not exist yet"
  fi
  validate_cube_set_overrides "${root}" fake_generate_adds_key \
    || fail "dry run must see keys added by the generate hook"
  cmp -s "${root}/CubeMaster/conf.yaml" "${TMP_DIR}/validate-hook.orig" \
    || fail "dry run must not modify the real root"
  local out
  if out="$( ( validate_cube_set_overrides "${root}" fake_generate_fails ) 2>&1 )"; then
    fail "a failing generate hook must abort validation"
  fi
  [[ "${out}" == *"fake generator boom"* ]] || fail "hook failure must surface its log (got: ${out})"
}

test_jfs_runtime_keys_reserved() {
  local root="${TMP_DIR}/jfs-reserved"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${root}/Cubelet/config/config.toml" "${TMP_DIR}/jfs-reserved.orig"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.storage".cow.jfs.mount_path'
  export CUBE_SET_1_VALUE="/data/other"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH='plugins."io.cubelet.internal.v1.storage".cow.jfs.enable'
  export CUBE_SET_2_VALUE="true"
  apply_cube_set_overrides "${root}"
  cmp -s "${root}/Cubelet/config/config.toml" "${TMP_DIR}/jfs-reserved.orig" \
    || fail "runtime-owned jfs keys must be skipped"
}

test_compute_skip_normalizes_backslash() {
  DEPLOY_ROLE=compute
  if cube_set_file_allowed_for_role 'CubeMaster\conf.yaml'; then
    fail "backslash spelling of a control-plane file must also be skipped on compute"
  fi
  DEPLOY_ROLE=control
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
  cp -f "${root}/CubeTemplateCenter/conf.yaml" "${TMP_DIR}/reserved-tc.orig"
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="common.http_port"
  export CUBE_SET_1_VALUE="9091"
  apply_cube_set_overrides "${root}"
  cmp -s "${root}/CubeTemplateCenter/conf.yaml" "${TMP_DIR}/reserved-tc.orig" \
    || fail "TemplateCenter common.http_port is pinned by its postcheck and must be skipped"
  reset_cube_set_env
  cp -f "${root}/Cubelet/dynamicconf/conf.yaml" "${TMP_DIR}/reserved-dyn.orig"
  cp -f "${root}/Cubelet/config/config.toml" "${TMP_DIR}/reserved-toml.orig"
  export CUBE_SET_1_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_1_PATH="host.quota.mcpu_limit"
  export CUBE_SET_1_VALUE="8000"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH="dynamic_config_path"
  export CUBE_SET_2_VALUE="/tmp/elsewhere.yaml"
  apply_cube_set_overrides "${root}"
  cmp -s "${root}/Cubelet/dynamicconf/conf.yaml" "${TMP_DIR}/reserved-dyn.orig" \
    || fail "host.quota.* is owned by OpsAgent and must be skipped"
  cmp -s "${root}/Cubelet/config/config.toml" "${TMP_DIR}/reserved-toml.orig" \
    || fail "dynamic_config_path is overridden by cubelet-start.sh and must be skipped"
  reset_cube_set_env
  printf '\nredis:\n  cluster_nodes: "a:6379"\n' >> "${root}/CubeTemplateCenter/conf.yaml"
  cp -f "${root}/CubeTemplateCenter/conf.yaml" "${TMP_DIR}/reserved-tc.orig"
  export CUBE_SET_1_FILE="CubeTemplateCenter/conf.yaml"
  export CUBE_SET_1_PATH="redis.cluster_nodes"
  export CUBE_SET_1_VALUE="b:6379"
  apply_cube_set_overrides "${root}"
  cmp -s "${root}/CubeTemplateCenter/conf.yaml" "${TMP_DIR}/reserved-tc.orig" \
    || fail "redis.cluster_nodes is generated from CUBE_EXTERNAL_REDIS_CLUSTER_NODES and must be skipped"
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

test_compute_skips_incomplete_control_group() {
  local root="${TMP_DIR}/compute-incomplete"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  DEPLOY_ROLE=compute
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/cubebox_os_image"
  export CUBE_SET_2_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_2_PATH="log.level"
  apply_cube_set_overrides "${root}" \
    || fail "compute role must skip an incomplete control-plane group"
  assert_contains "${root}/Cubelet/config/config.toml" 'image_base_path = "/data/cubebox_os_image"'
  DEPLOY_ROLE=control
}

test_malformed_cube_set_name_fails() {
  local root="${TMP_DIR}/malformed-name"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_0_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_0_PATH="log.level"
  export CUBE_SET_0_VALUE="debug"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "CUBE_SET_0_* must fail dry-run"
  fi
  reset_cube_set_env
  export CUBE_SET_01_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_01_PATH="log.level"
  export CUBE_SET_01_VALUE="debug"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "CUBE_SET_01_* must fail dry-run"
  fi
}

test_persist_strips_malformed_cube_set() {
  reset_cube_set_env
  local envf="${TMP_DIR}/persist-malformed.env"
  cat > "${envf}" <<'EOF'
CUBE_SET_0_FILE=CubeMaster/conf.yaml
CUBE_SET_0_PATH=log.level
CUBE_SET_0_VALUE=debug
CUBE_SET_1_FILE=Cubelet/config/config.toml
CUBE_SET_1_PATH=old.path
CUBE_SET_1_VALUE=/old
EOF
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/new"
  CUBE_SET_THIS_RUN=1
  CUBE_SET_DOTENV_PRESENT=1
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_FILE"]="${CUBE_SET_1_FILE}"
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_PATH"]="${CUBE_SET_1_PATH}"
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_VALUE"]="${CUBE_SET_1_VALUE}"
  persist_cube_set_overrides "${envf}"
  if grep -Eq 'CUBE_SET_0_' "${envf}"; then
    fail "persist must drop malformed CUBE_SET_0_*"
  fi
  grep -Fq 'CUBE_SET_1_VALUE=/data/new' "${envf}" \
    || fail "persist must keep this-run CUBE_SET_1_VALUE"
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
  printf '\nsome_version: v1\nsome_mode: 0\n' >> "${root}/CubeMaster/conf.yaml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="some_version"
  export CUBE_SET_1_VALUE="1.10"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" 'some_version: "1.10"'
  assert_not_contains "${root}/CubeMaster/conf.yaml" "some_version: 1.1"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="some_mode"
  export CUBE_SET_1_VALUE="0755"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "0755 into an integer field must fail dry-run"
  fi
  export CUBE_SET_1_TYPE="string"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" 'some_mode: "0755"'
}

test_auto_rejects_type_mismatch() {
  local root="${TMP_DIR}/auto-mismatch"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${root}/Cubelet/config/config.toml" "${TMP_DIR}/auto-mismatch.orig"
  local case_path case_value
  for case_path in \
    'plugins."io.cubelet.internal.v1.resource-metrics".enabled=1' \
    'plugins."io.cubelet.internal.v1.cgroup".pool_size=3000.0' \
    'plugins."io.containerd.gc.v1.scheduler".pause_threshold=abc'; do
    case_value="${case_path##*=}"
    reset_cube_set_env
    export CUBE_SET_1_FILE="Cubelet/config/config.toml"
    export CUBE_SET_1_PATH="${case_path%=*}"
    export CUBE_SET_1_VALUE="${case_value}"
    if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
      fail "type mismatch must fail dry-run: ${case_path}"
    fi
  done
  cmp -s "${root}/Cubelet/config/config.toml" "${TMP_DIR}/auto-mismatch.orig" \
    || fail "rejected type mismatch must not modify the file"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.resource-metrics".enabled'
  export CUBE_SET_1_VALUE="FALSE"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH='plugins."io.cubelet.internal.v1.cgroup".pool_size'
  export CUBE_SET_2_VALUE="4000"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" "enabled = false"
  assert_contains "${root}/Cubelet/config/config.toml" "pool_size = 4000"
}

test_duration_requires_unit() {
  local root="${TMP_DIR}/auto-duration"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${REPO_ROOT}/Cubelet/dynamicconf/conf.yaml" "${root}/Cubelet/dynamicconf/conf.yaml"
  cp -f "${root}/CubeMaster/conf.yaml" "${TMP_DIR}/auto-duration.orig"
  # yaml.v3 rejects both a bare 600 and "600" for time.Duration.
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="scheduler.metric_update_timeout"
  export CUBE_SET_1_VALUE="600"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "unit-less duration must fail dry-run"
  fi
  cmp -s "${root}/CubeMaster/conf.yaml" "${TMP_DIR}/auto-duration.orig" \
    || fail "rejected duration must not modify the file"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_1_PATH="host.gc.code_expiration_time"
  export CUBE_SET_1_VALUE="48"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "unit-less quoted duration must fail dry-run"
  fi
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="scheduler.metric_update_timeout"
  export CUBE_SET_1_VALUE="600s"
  export CUBE_SET_2_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_2_PATH="host.gc.code_expiration_time"
  export CUBE_SET_2_VALUE="1h30m"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/CubeMaster/conf.yaml" "  metric_update_timeout: 600s"
  assert_contains "${root}/CubeMaster/conf.yaml" "local_metric_update_timeout: 300s"
  assert_contains "${root}/Cubelet/dynamicconf/conf.yaml" 'code_expiration_time: "1h30m"'
}

test_image_paths_must_be_absolute() {
  local root="${TMP_DIR}/abs-path"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${root}/Cubelet/config/config.toml" "${TMP_DIR}/abs-path.orig"
  local path
  for path in \
    'plugins."io.cubelet.internal.v1.images".image_base_path' \
    'plugins.io.cubelet.internal.v1.images.shared_kernel_path'; do
    reset_cube_set_env
    export CUBE_SET_1_FILE="Cubelet/config/config.toml"
    export CUBE_SET_1_PATH="${path}"
    export CUBE_SET_1_VALUE="relative/path"
    if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
      fail "relative ${path} must fail dry-run"
    fi
  done
  cmp -s "${root}/Cubelet/config/config.toml" "${TMP_DIR}/abs-path.orig" \
    || fail "rejected relative path must not modify the file"
}

test_toml_root_key_update() {
  local root="${TMP_DIR}/toml-root-key"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH="root"
  export CUBE_SET_1_VALUE="/data2/cubelet/root"
  export CUBE_SET_2_FILE="Cubelet/config/config.toml"
  export CUBE_SET_2_PATH="oom_score"
  export CUBE_SET_2_VALUE="-999"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/config/config.toml" 'root = "/data2/cubelet/root"'
  assert_contains "${root}/Cubelet/config/config.toml" "oom_score = -999"
  [[ "$(grep -c '/data2/cubelet/root' "${root}/Cubelet/config/config.toml")" == "1" ]] \
    || fail "root-table update must not touch same-named keys in sub-tables"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH="not_a_root_key"
  export CUBE_SET_1_VALUE="x"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "missing root TOML key must fail dry-run"
  fi
}

test_auto_keeps_quoted_yaml_string() {
  local root="${TMP_DIR}/auto-quoted"
  rm -rf "${root}"
  stage_tree "${root}"
  cp -f "${REPO_ROOT}/Cubelet/dynamicconf/conf.yaml" "${root}/Cubelet/dynamicconf/conf.yaml"
  reset_cube_set_env
  export CUBE_SET_1_FILE="Cubelet/dynamicconf/conf.yaml"
  export CUBE_SET_1_PATH="host.scheduler_label"
  export CUBE_SET_1_VALUE="1024"
  apply_cube_set_overrides "${root}"
  assert_contains "${root}/Cubelet/dynamicconf/conf.yaml" 'scheduler_label: "1024"'
  assert_not_contains "${root}/Cubelet/dynamicconf/conf.yaml" "scheduler_label: 1024"
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
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "new_leaf must not be inserted into the commented table"
  fi
  assert_not_contains "${root}/Cubelet/config/config.toml" "new_leaf"
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
  if ( apply_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "new_leaf must not be inserted after the actions array"
  fi
  assert_not_contains "${root}/Cubelet/config/config.toml" "new_leaf"
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
  CUBE_SET_DOTENV_PRESENT=1
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

test_persist_drops_export_prefixed_stale_groups() {
  reset_cube_set_env
  local envf="${TMP_DIR}/persist-export.env"
  cat > "${envf}" <<'EOF'
export CUBE_SET_1_FILE=Cubelet/config/config.toml
export CUBE_SET_1_PATH=old.path
export CUBE_SET_1_VALUE=/old
  CUBE_SET_2_FILE=CubeMaster/conf.yaml
  CUBE_SET_2_PATH=log.level
  CUBE_SET_2_VALUE=debug
EOF
  export CUBE_SET_1_FILE="Cubelet/config/config.toml"
  export CUBE_SET_1_PATH='plugins."io.cubelet.internal.v1.images".image_base_path'
  export CUBE_SET_1_VALUE="/data/new"
  CUBE_SET_THIS_RUN=1
  CUBE_SET_DOTENV_PRESENT=1
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_FILE"]="${CUBE_SET_1_FILE}"
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_PATH"]="${CUBE_SET_1_PATH}"
  CUBE_SET_DOTENV_SNAPSHOT["CUBE_SET_1_VALUE"]="${CUBE_SET_1_VALUE}"
  persist_cube_set_overrides "${envf}"
  grep -Fq 'CUBE_SET_1_VALUE=/data/new' "${envf}" \
    || fail "persist must rewrite this-run CUBE_SET_1_VALUE"
  if grep -Eq 'CUBE_SET_2_' "${envf}"; then
    fail "persist must drop export/indented stale CUBE_SET_2"
  fi
  if grep -Eq '^[[:space:]]*export[[:space:]]+CUBE_SET_1_' "${envf}"; then
    fail "persist must not leave a duplicate export CUBE_SET_1_ line"
  fi
}

test_float_nonfinite_fails_validate() {
  local root="${TMP_DIR}/float-inf"
  rm -rf "${root}"
  stage_tree "${root}"
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="log.level"
  export CUBE_SET_1_VALUE="inf"
  export CUBE_SET_1_TYPE="float"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "TYPE=float inf must fail dry-run"
  fi
  reset_cube_set_env
  export CUBE_SET_1_FILE="CubeMaster/conf.yaml"
  export CUBE_SET_1_PATH="log.level"
  export CUBE_SET_1_VALUE="nan"
  export CUBE_SET_1_TYPE="float"
  if ( validate_cube_set_overrides "${root}" ) >/dev/null 2>&1; then
    fail "TYPE=float nan must fail dry-run"
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
  # No bundle .env this run: the persisted set stays, process env overlays.
  capture_cube_set_dotenv_values "${TMP_DIR}/process-only-missing.env"
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
  local f="${ONE_CLICK_DIR}/install.sh"
  local validate generate apply persist assert_line stop wipe
  validate="$(grep -n 'validate_cube_set_overrides "${PKG_ROOT}"' "${f}" | head -1 | cut -d: -f1)"
  generate="$(grep -n '^  generate_control_plane_configs$' "${f}" | head -1 | cut -d: -f1)"
  apply="$(grep -n 'apply_cube_set_overrides "${PKG_ROOT}"' "${f}" | head -1 | cut -d: -f1)"
  persist="$(grep -n 'persist_cube_set_overrides "${RUNTIME_ENV_FILE}"' "${f}" | head -1 | cut -d: -f1)"
  assert_line="$(grep -n '^assert_safe_install_prefix "${INSTALL_PREFIX}"' "${f}" | head -1 | cut -d: -f1)"
  stop="$(grep -n '^stop_existing_systemd_deployment$' "${f}" | head -1 | cut -d: -f1)"
  wipe="$(grep -n '^rm -rf \\$' "${f}" | head -1 | cut -d: -f1)"
  [[ -n "${validate}" && -n "${generate}" && -n "${apply}" && -n "${persist}" \
    && -n "${assert_line}" && -n "${stop}" && -n "${wipe}" ]] \
    || fail "install.sh CUBE_SET wiring not found (validate=${validate} generate=${generate} apply=${apply} persist=${persist} assert=${assert_line} stop=${stop} wipe=${wipe})"
  [[ "$(grep -c 'apply_cube_set_overrides "${PKG_ROOT}"' "${f}")" == "1" ]] \
    || fail "install.sh must apply CUBE_SET exactly once"
  [[ "$(grep -c '^  generate_control_plane_configs$' "${f}")" == "1" ]] \
    || fail "install.sh must generate control-plane configs exactly once"
  # validate < prefix check < generate < apply < stop < wipe < persist: a
  # CUBE_SET failure must leave services running and the old tree in place.
  [[ "${validate}" -lt "${assert_line}" && "${assert_line}" -lt "${generate}" \
    && "${generate}" -lt "${apply}" && "${apply}" -lt "${stop}" \
    && "${stop}" -lt "${wipe}" && "${wipe}" -lt "${persist}" ]] \
    || fail "CUBE_SET order broken (validate=${validate} assert=${assert_line} generate=${generate} apply=${apply} stop=${stop} wipe=${wipe} persist=${persist})"
}

test_toml_update_image_base_path
test_rejects_missing_key
test_yaml_same_indent_sequence
test_rejects_intermediate_symlink
test_preserves_crlf_and_inline_comment
test_dotenv_without_cube_set_clears
test_dotenv_and_process_env_groups_both_apply
test_auto_keeps_bare_float_field_numeric
test_auto_rejects_type_mismatch
test_duration_requires_unit
test_image_paths_must_be_absolute
test_toml_root_key_update
test_compute_skip_normalizes_backslash
test_validate_replays_generate_hook
test_jfs_runtime_keys_reserved
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
test_compute_skips_incomplete_control_group
test_malformed_cube_set_name_fails
test_persist_strips_malformed_cube_set
test_value_newline_fails_validate
test_persist_this_run_drops_stale_groups
test_persist_drops_export_prefixed_stale_groups
test_float_nonfinite_fails_validate
test_upgrade_intent_replaces_old_value
test_dotenv_export_and_mixed_style
test_no_cube_set_skips_python3
test_process_env_only_overlays_merged_set
test_install_sh_wires_config_set

echo "config_set tests OK"
