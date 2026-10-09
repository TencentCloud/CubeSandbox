#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for persist_one_click_redis_runtime_env (install.sh Redis
# EnvironmentFile whitelist) and the cubeops-start.sh password / DB fallback.
# --mode=install used to leave CUBE_SANDBOX_REDIS_PASSWORD out of
# .one-click.env; cubeops then AUTH-skipped against requirepass Redis.
# CUBE_EXTERNAL_REDIS_DB is the single logical-DB knob for Master/Ops/Proxy/LCM.
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

assert_key_absent() {
  local file="$1" key="$2"
  if grep -Eq "^${key}=" "${file}"; then
    fail "expected ${file} NOT to have key ${key}"
  fi
}

assert_fails() {
  if "$@"; then
    fail "expected command to fail: $*"
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
      CUBE_EXTERNAL_REDIS_DB REDIS_DB CUBE_PROXY_REGISTRY_REDIS_DB CUBE_LCM_REDIS_DB \
      CUBE_SANDBOX_REDIS_PASSWORD REDIS_URL CUBE_EXTERNAL_REDIS_TLS
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
  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_DB 0
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_REDIS_PASSWORD="
  assert_key_absent "${env_file}" REDIS_DB
  assert_key_absent "${env_file}" CUBE_PROXY_REGISTRY_REDIS_DB
  assert_key_absent "${env_file}" CUBE_LCM_REDIS_DB
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
  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_DB 0
  assert_not_contains "${env_file}" "CUBE_SANDBOX_REDIS_PASSWORD="
}

test_external_redis_persists_unified_db() {
  local env_file="${TMP_DIR}/external-db.env"
  : > "${env_file}"
  # Seed stale per-component keys that old packages may have written.
  cat > "${env_file}" <<'EOF'
REDIS_DB=9
CUBE_PROXY_REGISTRY_REDIS_DB=9
CUBE_LCM_REDIS_DB=9
REDIS_URL=redis://old.example:6379/9
EOF

  persist_clean "${env_file}" \
    CUBE_EXTERNAL_REDIS_MASTER_NAME=sentinel-master \
    CUBE_EXTERNAL_REDIS_SENTINEL_NODES=10.0.0.1:26379 \
    CUBE_EXTERNAL_REDIS_PASSWORD=extsecret \
    CUBE_EXTERNAL_REDIS_DB=12

  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_DB 12
  assert_key_absent "${env_file}" REDIS_DB
  assert_key_absent "${env_file}" CUBE_PROXY_REGISTRY_REDIS_DB
  assert_key_absent "${env_file}" CUBE_LCM_REDIS_DB
  assert_key_absent "${env_file}" REDIS_URL
}

test_cubeops_start_redis_password_fallback() {
  local start_sh="${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh"
  # `-` (not `:-`): an unset password still falls back to the bundled ceuhvu123,
  # but an explicitly empty external password is preserved so managed Redis
  # without AuthToken ("no AUTH") is not sent a bogus AUTH. Mirrors the
  # up-cube-proxy / up-cube-lifecycle-manager fix.
  grep -Fq 'CUBE_SANDBOX_REDIS_PASSWORD-ceuhvu123' "${start_sh}" \
    || fail "cubeops-start.sh must fall back REDIS_PASSWORD to ceuhvu123 via - so unset still defaults"
  if grep -Fq 'CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123' "${start_sh}"; then
    fail "cubeops-start.sh must not use :- (an explicitly empty external password must stay empty)"
  fi
}

test_cubeops_start_derives_redis_db() {
  local start_sh="${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh"
  grep -Fq 'unset REDIS_URL' "${start_sh}" \
    || fail "cubeops-start.sh must unset REDIS_URL before selecting REDIS_DB"
  grep -Fq 'ignoring REDIS_URL; CubeOps now derives its Redis endpoint from CUBE_EXTERNAL_REDIS_*' \
    "${start_sh}" \
    || fail "cubeops-start.sh must log when ignoring a preserved REDIS_URL"
  grep -Fq 'normalize_redis_db "${CUBE_EXTERNAL_REDIS_DB:-${REDIS_DB:-0}}"' "${start_sh}" \
    || fail "cubeops-start.sh must validate CUBE_EXTERNAL_REDIS_DB with REDIS_DB fallback"
}

test_proxy_lcm_derive_unified_db() {
  grep -Fq '"${CUBE_EXTERNAL_REDIS_DB:-${CUBE_PROXY_REDIS_DB:-${CUBE_PROXY_REGISTRY_REDIS_DB:-0}}}"' \
    "${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh" \
    || fail "up-cube-proxy.sh must derive sandbox-routing Redis DB from CUBE_EXTERNAL_REDIS_DB"
  grep -Fq '__CUBE_PROXY_REDIS_DB__' \
    "${ONE_CLICK_DIR}/cubeproxy/global.conf.template" \
    || fail "global.conf.template must placeholder redis_index via __CUBE_PROXY_REDIS_DB__"
  grep -Fq '__CUBE_PROXY_REDIS_DB__' \
    "${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh" \
    || fail "up-cube-proxy.sh must render __CUBE_PROXY_REDIS_DB__ into global.conf"
  grep -Fq 'CUBE_PROXY_REGISTRY_REDIS_DB="${CUBE_PROXY_REDIS_DB}"' \
    "${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh" \
    || fail "up-cube-proxy.sh must use the agreed routing DB for registry"
  grep -Fq 'CUBE_PROXY_REDIS_DB (${CUBE_PROXY_REDIS_DB}) and CUBE_PROXY_REGISTRY_REDIS_DB (${CUBE_PROXY_REGISTRY_REDIS_DB}) conflict' \
    "${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh" \
    || fail "up-cube-proxy.sh must reject conflicting legacy Redis DB values"
  grep -Fq '"${CUBE_EXTERNAL_REDIS_DB:-${CUBE_LCM_REDIS_DB:-0}}"' \
    "${ONE_CLICK_DIR}/scripts/one-click/up-cube-lifecycle-manager.sh" \
    || fail "up-cube-lifecycle-manager.sh must prefer CUBE_EXTERNAL_REDIS_DB with LCM fallback"
  grep -Fq 'normalize_redis_db' "${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh" \
    || fail "up-cube-proxy.sh must validate Redis DB values"
  grep -Fq 'normalize_redis_db' "${ONE_CLICK_DIR}/scripts/one-click/up-cube-lifecycle-manager.sh" \
    || fail "up-cube-lifecycle-manager.sh must validate Redis DB values"
}

test_install_sh_calls_persist_helper() {
  grep -Fq 'persist_one_click_redis_runtime_env "${RUNTIME_ENV_FILE}"' \
    "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must persist Redis runtime env via persist_one_click_redis_runtime_env"
}

test_install_sh_patches_master_db_no() {
  grep -Fq 'one_click_patch_conf_redis_db' "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must patch conf db_no via one_click_patch_conf_redis_db"
  grep -Fq 'one_click_redis_db >/dev/null' "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must validate CUBE_EXTERNAL_REDIS_DB before destructive phase"
  # TC must get the same db_no as Master (progress snapshots share Redis).
  grep -Fq 'CubeTemplateCenter redis db_no=' "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must also patch CubeTemplateCenter conf db_no"
  grep -Fq 'one_click_patch_conf_redis_endpoint "${tc_cfg}" "CubeTemplateCenter" "${restore_bundled_redis}"' \
    "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must patch CubeTemplateCenter external Redis endpoint with restore_bundled"
  grep -Fq 'on every node that runs the control-plane' "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must warn about multi-node Redis DB distribution"
  grep -Fq 'removing legacy REDIS_URL' "${ONE_CLICK_DIR}/lib/common.sh" \
    || fail "persist_one_click_redis_runtime_env must log when stripping REDIS_URL"
}

test_one_click_patch_conf_redis_db_insert_indented() {
  local cfg="${TMP_DIR}/conf-no-db.yaml"
  cat > "${cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  password: "x"
EOF
  (
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=5 one_click_patch_conf_redis_db "${cfg}" >/dev/null
  )
  grep -qE '^  db_no: 5$' "${cfg}" \
    || fail "insert branch must keep db_no indented under redis:; got: $(cat "${cfg}")"
  if grep -qE '^db_no:' "${cfg}"; then
    fail "db_no must not be inserted at column 0"
  fi
}

test_one_click_patch_conf_redis_db_replace() {
  local cfg="${TMP_DIR}/conf-has-db.yaml"
  cat > "${cfg}" <<'EOF'
instance_db_config:
  db_no: 14
redis:
  nodes: "127.0.0.1:6379"
  db_no: 0
  password: "x"
EOF
  chmod 0644 "${cfg}"
  (
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=12 one_click_patch_conf_redis_db "${cfg}" >/dev/null
  )
  grep -qE '^  db_no: 12$' "${cfg}" \
    || fail "replace branch must rewrite db_no; got: $(cat "${cfg}")"
  grep -qE '^  db_no: 14$' "${cfg}" \
    || fail "replace branch must not rewrite db_no outside redis; got: $(cat "${cfg}")"
  local mode
  mode="$(stat -c '%a' "${cfg}" 2>/dev/null || stat -f '%OLp' "${cfg}")"
  [[ "${mode}" == "644" ]] \
    || fail "patch must preserve conf mode (expected 644, got ${mode})"
}

test_one_click_patch_conf_redis_db_handles_indented_block() {
  local cfg="${TMP_DIR}/conf-indented-redis.yaml"
  cat > "${cfg}" <<'EOF'
cache:
  redis:
    nodes: "127.0.0.1:6379"
  sibling:
    enabled: true
EOF
  (
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=6 one_click_patch_conf_redis_db "${cfg}" >/dev/null
  )
  grep -qE '^    db_no: 6$' "${cfg}" \
    || fail "indented redis block must receive nested db_no; got: $(cat "${cfg}")"
}

test_one_click_patch_conf_redis_db_fails_without_redis() {
  local cfg="${TMP_DIR}/conf-no-redis.yaml"
  cat > "${cfg}" <<'EOF'
instance_db_config:
  db_no: 9
EOF
  assert_fails bash -c '
    set -euo pipefail
    source "$1/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=4 one_click_patch_conf_redis_db "$2" >/dev/null
  ' _ "${ONE_CLICK_DIR}" "${cfg}"
  grep -qE '^  db_no: 9$' "${cfg}" \
    || fail "failed patch must leave original config untouched"
}

test_templatecenter_external_redis_endpoint_patch() {
  local cfg="${TMP_DIR}/templatecenter-redis.yaml"
  cat > "${cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  password: "local"
  db_no: 0
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.21
    CUBE_EXTERNAL_REDIS_PORT=6380
    CUBE_EXTERNAL_REDIS_PASSWORD=external
    CUBE_EXTERNAL_REDIS_MASTER_NAME=
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeTemplateCenter"
  )
  assert_not_contains "${cfg}" '127.0.0.1:6379'
  grep -qF 'nodes: "10.0.0.21:6380"' "${cfg}" \
    || fail "TemplateCenter must use external Redis host"
  grep -qF 'password: "external"' "${cfg}" \
    || fail "TemplateCenter must use external Redis password"
}

test_templatecenter_external_redis_sentinel_patch() {
  local cfg="${TMP_DIR}/templatecenter-sentinel.yaml"
  cat > "${cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  password: "local"
  db_no: 0
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=
    CUBE_EXTERNAL_REDIS_MASTER_NAME=mymaster
    CUBE_EXTERNAL_REDIS_SENTINEL_NODES=10.0.0.11:26379,10.0.0.12:26379
    CUBE_EXTERNAL_REDIS_PASSWORD=external
    CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD=sentinel-secret
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeTemplateCenter"
  )
  grep -qF 'nodes: ""' "${cfg}" \
    || fail "TemplateCenter Sentinel config must clear fixed nodes"
  grep -qF 'master_name: "mymaster"' "${cfg}" \
    || fail "TemplateCenter must use external Redis master name"
  grep -qF 'sentinel_nodes: "10.0.0.11:26379,10.0.0.12:26379"' "${cfg}" \
    || fail "TemplateCenter must use external Sentinel nodes"
  grep -qF 'sentinel_password: "sentinel-secret"' "${cfg}" \
    || fail "TemplateCenter must use external Sentinel password"
}

test_external_redis_tls_rendering() {
  local cfg="${TMP_DIR}/redis-tls.yaml"
  _fresh() { cat > "${cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  password: "local"
  db_no: 0
EOF
  }

  # external host + TLS on -> `tls: true` inserted (2-space indent) under redis:.
  _fresh
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    CUBE_EXTERNAL_REDIS_TLS=1
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  )
  grep -qE '^  tls: true$' "${cfg}" \
    || fail "TLS on must render 'tls: true' under redis:; got: $(cat "${cfg}")"

  # external host, TLS unset -> `tls: false` (CubeMaster must not dial plaintext
  # implicitly while other clients speak TLS).
  _fresh
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  )
  grep -qE '^  tls: false$' "${cfg}" || fail "TLS unset must render 'tls: false'"

  # re-run updates in place (idempotent: exactly one tls line, value refreshed).
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    CUBE_EXTERNAL_REDIS_TLS=1
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  )
  [[ "$(grep -cE '^  tls:' "${cfg}")" -eq 1 ]] || fail "re-run must keep exactly one tls: line"
  grep -qE '^  tls: true$' "${cfg}" || fail "re-run with TLS on must update to 'tls: true'"

  # restore-bundled -> `tls: false` (no stale TLS pointing at the plaintext local).
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=
    CUBE_EXTERNAL_REDIS_MASTER_NAME=
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster" 1
  )
  grep -qE '^  tls: false$' "${cfg}" || fail "restore_bundled must render 'tls: false'"
  unset -f _fresh

  # A two-space `tls:` key in another block must not be taken for redis.tls:
  # both the update and the insert stay inside the redis: block.
  cat > "${cfg}" <<'EOF'
auth:
  tls: true
redis:
  nodes: "127.0.0.1:6379"
  password: "local"
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  )
  awk '/^auth:/{a=1; next} /^[^[:space:]#]/{a=0} a && /^  tls: true$/{found=1} END{exit found?0:1}' "${cfg}" \
    || fail "another block's tls: must stay untouched; got: $(cat "${cfg}")"
  awk '/^redis:/{r=1; next} /^[^[:space:]#]/{r=0} r && /^  tls: false$/{found=1} END{exit found?0:1}' "${cfg}" \
    || fail "redis.tls must be rendered inside the redis: block; got: $(cat "${cfg}")"
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    CUBE_EXTERNAL_REDIS_TLS=1
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  )
  [[ "$(grep -cE '^  tls:' "${cfg}")" -eq 2 ]] || fail "re-run must keep one tls: line per block"
  awk '/^auth:/{a=1; next} /^[^[:space:]#]/{a=0} a && /^  tls: true$/{found=1} END{exit found?0:1}' "${cfg}" \
    || fail "re-run must not rewrite another block's tls:"
  awk '/^redis:/{r=1; next} /^[^[:space:]#]/{r=0} r && /^  tls: true$/{found=1} END{exit found?0:1}' "${cfg}" \
    || fail "re-run with TLS on must update redis.tls to true"

  # An indented redis: block with four-space children gets tls: at the
  # children's indentation, and a nested tls: deeper in the block is not taken
  # for redis.tls.
  cat > "${cfg}" <<'EOF'
cube:
  redis:
    nodes: "127.0.0.1:6379"
    password: "local"
    pool:
      tls: false
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    CUBE_EXTERNAL_REDIS_TLS=1
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  )
  grep -qxF '    tls: true' "${cfg}" \
    || fail "indented redis block must get tls: true at the children's indentation; got: $(cat "${cfg}")"
  grep -qxF '      tls: false' "${cfg}" \
    || fail "a nested tls: under redis.pool must stay untouched; got: $(cat "${cfg}")"
  [[ "$(one_click_conf_redis_tls "${cfg}")" == "true" ]] \
    || fail "one_click_conf_redis_tls must read back the rendered value"

  # A conf.yaml without a redis: block fails instead of silently leaving the
  # control plane on plaintext.
  printf 'scheduler:\n  tls: false\n' > "${cfg}"
  if (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
    CUBE_EXTERNAL_REDIS_PORT=6379
    CUBE_EXTERNAL_REDIS_TLS=1
    one_click_patch_conf_redis_endpoint "${cfg}" "CubeMaster"
  ) >/dev/null 2>&1; then
    fail "rendering redis.tls into a conf.yaml without a redis: block must fail"
  fi

  # Real shipped templates: exactly one tls line, inside the redis: block.
  local tpl real
  for tpl in cubemaster templatecenter; do
    real="${TMP_DIR}/real-${tpl}.yaml"
    cp "${ONE_CLICK_DIR}/../../configs/single-node/${tpl}.yaml" "${real}"
    (
      # shellcheck disable=SC1091
      source "${ONE_CLICK_DIR}/lib/common.sh"
      CUBE_EXTERNAL_REDIS_HOST=10.0.0.30
      CUBE_EXTERNAL_REDIS_PORT=6379
      CUBE_EXTERNAL_REDIS_PASSWORD=
      CUBE_EXTERNAL_REDIS_TLS=1
      one_click_patch_conf_redis_endpoint "${real}" "${tpl}"
    )
    [[ "$(grep -cE '^  tls:' "${real}")" -eq 1 ]] \
      || fail "${tpl}.yaml must end up with exactly one tls: line"
    awk '/^redis:/{r=1; next} /^[^[:space:]#]/{r=0} r && /^  tls: true$/{found=1} END{exit found?0:1}' "${real}" \
      || fail "${tpl}.yaml tls: true must sit inside the redis: block"
    grep -qF 'password: ""' "${real}" \
      || fail "${tpl}.yaml must keep an explicitly empty external Redis password"
  done
}

test_external_redis_tls_persist_and_scrub() {
  local env_file="${TMP_DIR}/external-tls.env"
  : > "${env_file}"

  # External TLS-only Redis without AuthToken: the switch is persisted and the
  # explicitly empty password stays empty for every client ("no AUTH").
  # Per-component TLS keys are derived at start and never persisted: they are
  # stripped like the legacy per-component DB keys.
  printf '%s\n' CUBE_PROXY_REDIS_SSL=1 CUBE_PROXY_REGISTRY_REDIS_SSL=1 \
    CUBE_LCM_REDIS_TLS=1 REDIS_TLS=1 >> "${env_file}"
  persist_clean "${env_file}" \
    CUBE_EXTERNAL_REDIS_HOST=10.0.0.8 \
    CUBE_EXTERNAL_REDIS_PASSWORD= \
    CUBE_EXTERNAL_REDIS_TLS=1
  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_TLS 1
  assert_value "${env_file}" CUBE_EXTERNAL_REDIS_PASSWORD ""
  assert_value "${env_file}" CUBE_PROXY_REDIS_PASSWORD ""
  local key
  for key in CUBE_PROXY_REDIS_SSL CUBE_PROXY_REGISTRY_REDIS_SSL CUBE_LCM_REDIS_TLS REDIS_TLS; do
    assert_key_absent "${env_file}" "${key}"
  done

  # Back to bundled Redis: the switch itself is dropped too, otherwise the
  # clients would use TLS against the plaintext local Redis.
  printf '%s\n' CUBE_PROXY_REDIS_SSL=1 CUBE_PROXY_REGISTRY_REDIS_SSL=1 \
    CUBE_LCM_REDIS_TLS=1 REDIS_TLS=1 >> "${env_file}"
  persist_clean "${env_file}"
  for key in CUBE_EXTERNAL_REDIS_TLS CUBE_PROXY_REDIS_SSL CUBE_PROXY_REGISTRY_REDIS_SSL \
    CUBE_LCM_REDIS_TLS REDIS_TLS; do
    assert_key_absent "${env_file}" "${key}"
  done
}

test_normalize_redis_tls() {
  local value got
  for value in 1 true; do
    got="$(normalize_redis_tls "${value}")"
    [[ "${got}" == "1" ]] || fail "normalize_redis_tls ${value} must be 1, got '${got}'"
  done
  for value in 0 false ""; do
    got="$(normalize_redis_tls "${value}")"
    [[ "${got}" == "0" ]] || fail "normalize_redis_tls '${value}' must be 0, got '${got}'"
  done
  for value in yes on TRUE 2; do
    if ( normalize_redis_tls "${value}" ) >/dev/null 2>&1; then
      fail "normalize_redis_tls must reject '${value}'"
    fi
  done
}

expect_no_ignored_tls_warning() {
  local out
  out="$(
    unset CUBE_EXTERNAL_REDIS_HOST CUBE_EXTERNAL_REDIS_MASTER_NAME CUBE_EXTERNAL_REDIS_TLS
    export "$@"
    one_click_warn_ignored_redis_tls 2>&1
  )"
  [[ -z "${out}" ]] || fail "no warning expected for '$*', got: ${out}"
}

# The bundled Redis is plaintext, so a TLS switch without an external endpoint
# is dropped; install.sh must say so instead of discarding it silently.
test_ignored_redis_tls_warns() {
  local out
  out="$(
    unset CUBE_EXTERNAL_REDIS_HOST CUBE_EXTERNAL_REDIS_MASTER_NAME
    CUBE_EXTERNAL_REDIS_TLS=1
    one_click_warn_ignored_redis_tls 2>&1
  )"
  grep -Fq 'WARNING: CUBE_EXTERNAL_REDIS_TLS is on' <<<"${out}" \
    || fail "TLS without an external Redis endpoint must warn, got: ${out}"

  expect_no_ignored_tls_warning CUBE_EXTERNAL_REDIS_TLS=1 CUBE_EXTERNAL_REDIS_HOST=redis.example.com
  expect_no_ignored_tls_warning CUBE_EXTERNAL_REDIS_TLS=1 CUBE_EXTERNAL_REDIS_MASTER_NAME=mymaster
  expect_no_ignored_tls_warning CUBE_EXTERNAL_REDIS_TLS=0

  grep -Eq '^one_click_warn_ignored_redis_tls$' "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must call one_click_warn_ignored_redis_tls"
}

test_redis_tls_switch_derivation() {
  local lcm="${ONE_CLICK_DIR}/scripts/one-click/up-cube-lifecycle-manager.sh"
  local proxy="${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh"
  local ops="${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh"
  local lcm_line proxy_line registry_line ops_line line var got
  lcm_line="$(grep -E '^CUBE_LCM_REDIS_TLS=' "${lcm}" || true)"
  proxy_line="$(grep -E '^CUBE_PROXY_REDIS_SSL=' "${proxy}" || true)"
  registry_line="$(grep -E '^CUBE_PROXY_REGISTRY_REDIS_SSL=' "${proxy}" || true)"
  ops_line="$(grep -E '^REDIS_TLS=' "${ops}" || true)"
  [[ -n "${lcm_line}" && -n "${proxy_line}" && -n "${registry_line}" && -n "${ops_line}" ]] \
    || fail "LCM / CubeProxy / CubeOps must each derive their Redis TLS switch"

  for line in "${lcm_line}" "${proxy_line}" "${ops_line}"; do
    var="${line%%=*}"
    # The single switch turns the client on, normalized to 1.
    got="$(CUBE_EXTERNAL_REDIS_TLS=true; eval "${line}"; printf '%s' "${!var}")"
    [[ "${got}" == "1" ]] || fail "${var} must follow CUBE_EXTERNAL_REDIS_TLS=true, got '${got}'"
    # Unset stays off.
    got="$(unset CUBE_EXTERNAL_REDIS_TLS; eval "${line}"; printf '%s' "${!var}")"
    [[ "${got}" == "0" ]] || fail "${var} must default off, got '${got}'"
    # A per-component value cannot switch one client on by itself.
    got="$(unset CUBE_EXTERNAL_REDIS_TLS; export "${var}=1"; eval "${line}"; printf '%s' "${!var}")"
    [[ "${got}" == "0" ]] || fail "${var}=1 without CUBE_EXTERNAL_REDIS_TLS must stay off, got '${got}'"
    # An invalid switch fails instead of leaving some clients on plaintext.
    if ( CUBE_EXTERNAL_REDIS_TLS=yes; eval "${line}" ) >/dev/null 2>&1; then
      fail "${var}: an invalid CUBE_EXTERNAL_REDIS_TLS must fail"
    fi
  done

  # The registry timer follows the data-plane switch, not a key of its own.
  # shellcheck disable=SC2034  # CUBE_PROXY_REDIS_SSL is read by the eval'd line
  got="$(CUBE_PROXY_REDIS_SSL=1; CUBE_PROXY_REGISTRY_REDIS_SSL=0; eval "${registry_line}"; printf '%s' "${CUBE_PROXY_REGISTRY_REDIS_SSL}")"
  [[ "${got}" == "1" ]] || fail "registry Redis TLS must follow CUBE_PROXY_REDIS_SSL, got '${got}'"
}

test_tls_intent_survives_upgrade_merge() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_tls_intent_survives_upgrade_merge needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local key found k
  for key in CUBE_EXTERNAL_REDIS_TLS CUBE_POSTGRES_SSL_MODE; do
    found=0
    for k in "${ONE_CLICK_DB_INTENT_KEYS[@]}"; do
      if [[ "${k}" == "${key}" ]]; then found=1; fi
    done
    [[ "${found}" -eq 1 ]] || fail "${key} must be in ONE_CLICK_DB_INTENT_KEYS"
    for k in "${ONE_CLICK_DB_ENGINE_INTENT_KEYS[@]}"; do
      [[ "${k}" != "${key}" ]] || fail "${key} must NOT be an engine intent key"
    done
  done

  local dotenv="${TMP_DIR}/tls-intent.env"
  cat > "${dotenv}" <<'EOF'
CUBE_EXTERNAL_REDIS_TLS=1
CUBE_POSTGRES_SSL_MODE=verify-full
EOF
  (
    unset CUBE_EXTERNAL_REDIS_TLS CUBE_POSTGRES_SSL_MODE CUBE_EXTERNAL_POSTGRES_HOST \
      CUBE_DATABASE_DRIVER CUBE_EXTERNAL_REDIS_DB
    snapshot_one_click_database_intent "${dotenv}"
    set -a
    # shellcheck disable=SC1090
    source "${dotenv}"
    set +a
    capture_one_click_database_dotenv_values
    # Simulate load_env_file of the merged .one-click.env pinning the old values.
    CUBE_EXTERNAL_REDIS_TLS=0
    CUBE_POSTGRES_SSL_MODE=require
    CUBE_DATABASE_DRIVER=postgres
    CUBE_EXTERNAL_POSTGRES_HOST=10.0.0.9
    apply_one_click_database_intent
    [[ "${CUBE_EXTERNAL_REDIS_TLS}" == "1" ]] \
      || fail "expected .env CUBE_EXTERNAL_REDIS_TLS=1 to win over preserved 0, got '${CUBE_EXTERNAL_REDIS_TLS}'"
    [[ "${CUBE_POSTGRES_SSL_MODE}" == "verify-full" ]] \
      || fail "expected .env CUBE_POSTGRES_SSL_MODE=verify-full to win over preserved require, got '${CUBE_POSTGRES_SSL_MODE}'"
    [[ "${CUBE_EXTERNAL_POSTGRES_HOST}" == "10.0.0.9" ]] \
      || fail "TLS-only intent must not scrub the preserved postgres host"
  ) || exit 1
}

test_one_click_redis_db_normalizes_leading_zeros() {
  local got
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=08 one_click_redis_db
  )"
  [[ "${got}" == "8" ]] || fail "expected leading-zero 08 → 8, got '${got}'"
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    normalize_redis_db '"7"' "quoted"
  )"
  [[ "${got}" == "7" ]] || fail "expected quoted \"7\" → 7, got '${got}'"
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    normalize_redis_db '7 # metrics' "comment"
  )"
  [[ "${got}" == "7" ]] || fail "expected trailing comment → 7, got '${got}'"
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    normalize_redis_db ' 5 ' "padded"
  )"
  [[ "${got}" == "5" ]] || fail "expected padded whitespace → 5, got '${got}'"
  assert_fails bash -c '
    set -euo pipefail
    source "$1/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=16 one_click_redis_db >/dev/null
  ' _ "${ONE_CLICK_DIR}"
  assert_fails bash -c '
    set -euo pipefail
    source "$1/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=999999999999999999999999 one_click_redis_db >/dev/null
  ' _ "${ONE_CLICK_DIR}"
}

test_legacy_redis_db_is_derived_on_upgrade() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_legacy_redis_db_is_derived_on_upgrade needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local env_file="${TMP_DIR}/legacy.env"
  local master_cfg="${TMP_DIR}/legacy-master.yaml"
  cat > "${env_file}" <<'EOF'
REDIS_DB=7
CUBE_PROXY_REGISTRY_REDIS_DB=7
CUBE_LCM_REDIS_DB=7
EOF
  cat > "${master_cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  db_no: 7
EOF
  local got
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    snapshot_one_click_database_intent /nonexistent
    derive_one_click_redis_db_from_legacy "${env_file}" "${master_cfg}"
    printf '%s' "${CUBE_EXTERNAL_REDIS_DB}"
  )"
  [[ "${got}" == "7" ]] \
    || fail "expected legacy Redis DB 7 to become CUBE_EXTERNAL_REDIS_DB, got '${got}'"
}

test_legacy_redis_db_conflict_prefers_master() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_legacy_redis_db_conflict_prefers_master needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local env_file="${TMP_DIR}/legacy-conflict.env"
  local master_cfg="${TMP_DIR}/legacy-conflict-master.yaml"
  cat > "${env_file}" <<'EOF'
REDIS_DB=5
CUBE_PROXY_REGISTRY_REDIS_DB=6
EOF
  cat > "${master_cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  db_no: 9
EOF
  local got
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    snapshot_one_click_database_intent /nonexistent
    derive_one_click_redis_db_from_legacy "${env_file}" "${master_cfg}"
    printf '%s' "${CUBE_EXTERNAL_REDIS_DB}"
  )"
  [[ "${got}" == "9" ]] \
    || fail "expected conflicting legacy DBs to prefer Master DB 9, got '${got}'"
}

test_legacy_master_redis_db_ambiguity_fails() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_legacy_master_redis_db_ambiguity_fails needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local env_file="${TMP_DIR}/legacy-ambiguous.env"
  local master_cfg="${TMP_DIR}/legacy-ambiguous-master.yaml"
  local err_file="${TMP_DIR}/legacy-ambiguous.err"
  cat > "${env_file}" <<'EOF'
REDIS_DB=7
EOF
  cat > "${master_cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  db_no: 7
  db_no: 8
EOF
  if bash -c '
    set -euo pipefail
    source "$1/lib/common.sh"
    unset CUBE_EXTERNAL_REDIS_DB
    snapshot_one_click_database_intent /nonexistent
    derive_one_click_redis_db_from_legacy "$2" "$3"
  ' _ "${ONE_CLICK_DIR}" "${env_file}" "${master_cfg}" 2>"${err_file}"; then
    fail "ambiguous CubeMaster redis.db_no must fail"
  fi
  grep -Fq "ambiguous redis.db_no in ${master_cfg}" "${err_file}" \
    || fail "ambiguous CubeMaster redis.db_no must report a clear error"
}

test_legacy_master_missing_redis_falls_back_to_env() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_legacy_master_missing_redis_falls_back_to_env needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local env_file="${TMP_DIR}/legacy-missing-redis.env"
  local master_cfg="${TMP_DIR}/legacy-missing-redis-master.yaml"
  local err_file="${TMP_DIR}/legacy-missing-redis.err"
  local got
  cat > "${env_file}" <<'EOF'
REDIS_DB=6
EOF
  cat > "${master_cfg}" <<'EOF'
server:
  port: 8080
EOF
  got="$(
    bash -c '
      set -euo pipefail
      source "$1/lib/common.sh"
      unset CUBE_EXTERNAL_REDIS_DB
      snapshot_one_click_database_intent /nonexistent
      derive_one_click_redis_db_from_legacy "$2" "$3"
      printf "%s" "${CUBE_EXTERNAL_REDIS_DB}"
    ' _ "${ONE_CLICK_DIR}" "${env_file}" "${master_cfg}" 2>"${err_file}"
  )"
  [[ "${got}" == "6" ]] \
    || fail "missing Master redis block must fall back to legacy env DB 6, got '${got}'"
  grep -Fq "WARNING: no redis.db_no found in ${master_cfg}; falling back to .one-click.env" "${err_file}" \
    || fail "missing Master redis block must log the fallback warning"
}

# Master conf db_no:0 must not promote a stale per-component REDIS_DB onto the
# whole stack (would abandon existing DB-0 route keys / metrics).
test_legacy_master_db_zero_ignores_nonzero_legacy() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_legacy_master_db_zero_ignores_nonzero_legacy needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local env_file="${TMP_DIR}/legacy-master-zero.env"
  local master_cfg="${TMP_DIR}/legacy-master-zero.yaml"
  local err_file="${TMP_DIR}/legacy-master-zero.err"
  local got
  cat > "${env_file}" <<'EOF'
REDIS_DB=2
EOF
  cat > "${master_cfg}" <<'EOF'
redis:
  nodes: "127.0.0.1:6379"
  db_no: 0
EOF
  got="$(
    bash -c '
      set -euo pipefail
      source "$1/lib/common.sh"
      unset CUBE_EXTERNAL_REDIS_DB
      snapshot_one_click_database_intent /nonexistent
      derive_one_click_redis_db_from_legacy "$2" "$3"
      printf "%s" "${CUBE_EXTERNAL_REDIS_DB}"
    ' _ "${ONE_CLICK_DIR}" "${env_file}" "${master_cfg}" 2>"${err_file}"
  )"
  [[ "${got}" == "0" ]] \
    || fail "Master db_no=0 must keep the stack on DB 0 when legacy REDIS_DB=2, got '${got}'"
  grep -Fq "legacy Redis DB 2 ignored; CubeMaster redis.db_no=0 keeps the stack on DB 0" "${err_file}" \
    || fail "Master db_no=0 + non-zero legacy must warn that the legacy key is ignored"
}

# Commented-out in env.example → merge re-appends old .one-click.env; without
# DB-intent snapshot/apply, upgrade cannot change CUBE_EXTERNAL_REDIS_DB.
# Needs bash 4+ associative arrays (same as the helpers under test; CI uses bash 4+).
test_redis_db_intent_survives_upgrade_merge() {
  if (( BASH_VERSINFO[0] < 4 )); then
    echo "SKIP: test_redis_db_intent_survives_upgrade_merge needs bash 4+ (got ${BASH_VERSION})"
    return 0
  fi
  local dotenv="${TMP_DIR}/redis-db-intent.env"
  cat > "${dotenv}" <<'EOF'
CUBE_EXTERNAL_REDIS_DB=5
EOF
  unset CUBE_EXTERNAL_REDIS_DB CUBE_EXTERNAL_POSTGRES_HOST CUBE_DATABASE_DRIVER
  snapshot_one_click_database_intent "${dotenv}"
  # Simulate load_env_file of .env then capture (shell-interpreted values).
  # shellcheck disable=SC1090
  set -a
  # shellcheck disable=SC1091
  source "${dotenv}"
  set +a
  capture_one_click_database_dotenv_values
  # Simulate load_env_file of merged .one-click.env pinning the old value,
  # while preserving an unrelated engine marker that must not be scrubbed.
  CUBE_EXTERNAL_REDIS_DB=12
  CUBE_DATABASE_DRIVER=postgres
  CUBE_EXTERNAL_POSTGRES_HOST=10.0.0.9
  apply_one_click_database_intent
  [[ "${CUBE_EXTERNAL_REDIS_DB}" == "5" ]] \
    || fail "expected .env CUBE_EXTERNAL_REDIS_DB=5 to win over preserved 12, got '${CUBE_EXTERNAL_REDIS_DB}'"
  [[ "${CUBE_EXTERNAL_POSTGRES_HOST}" == "10.0.0.9" ]] \
    || fail "redis-DB-only intent must not scrub preserved postgres host"
}

test_redis_db_in_db_intent_keys() {
  local found=0 key
  for key in "${ONE_CLICK_DB_INTENT_KEYS[@]}"; do
    if [[ "${key}" == "CUBE_EXTERNAL_REDIS_DB" ]]; then
      found=1
      break
    fi
  done
  [[ "${found}" -eq 1 ]] || fail "CUBE_EXTERNAL_REDIS_DB must be in ONE_CLICK_DB_INTENT_KEYS"
  found=0
  for key in "${ONE_CLICK_DB_ENGINE_INTENT_KEYS[@]}"; do
    if [[ "${key}" == "CUBE_EXTERNAL_REDIS_DB" ]]; then
      found=1
      break
    fi
  done
  [[ "${found}" -eq 0 ]] || fail "CUBE_EXTERNAL_REDIS_DB must NOT be in ONE_CLICK_DB_ENGINE_INTENT_KEYS"
}

test_local_redis_persists_default_password
test_local_redis_persists_custom_password
test_external_redis_persists_external_password
test_external_redis_persists_unified_db
test_cubeops_start_redis_password_fallback
test_cubeops_start_derives_redis_db
test_proxy_lcm_derive_unified_db
test_install_sh_calls_persist_helper
test_install_sh_patches_master_db_no
test_one_click_patch_conf_redis_db_insert_indented
test_one_click_patch_conf_redis_db_replace
test_one_click_patch_conf_redis_db_handles_indented_block
test_one_click_patch_conf_redis_db_fails_without_redis
test_templatecenter_external_redis_endpoint_patch
test_templatecenter_external_redis_sentinel_patch
test_external_redis_tls_rendering
test_external_redis_tls_persist_and_scrub
test_normalize_redis_tls
test_ignored_redis_tls_warns
test_redis_tls_switch_derivation
test_tls_intent_survives_upgrade_merge
test_one_click_redis_db_normalizes_leading_zeros
test_legacy_redis_db_is_derived_on_upgrade
test_legacy_redis_db_conflict_prefers_master
test_legacy_master_redis_db_ambiguity_fails
test_legacy_master_missing_redis_falls_back_to_env
test_legacy_master_db_zero_ignores_nonzero_legacy
test_redis_db_in_db_intent_keys
test_redis_db_intent_survives_upgrade_merge

echo "redis runtime env tests OK"
