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
      CUBE_SANDBOX_REDIS_PASSWORD REDIS_URL
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
  grep -Fq 'CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123' "${start_sh}" \
    || fail "cubeops-start.sh must fall back REDIS_PASSWORD to ceuhvu123"
}

test_cubeops_start_derives_redis_db() {
  local start_sh="${ONE_CLICK_DIR}/scripts/systemd/cubeops-start.sh"
  grep -Fq 'unset REDIS_URL' "${start_sh}" \
    || fail "cubeops-start.sh must unset REDIS_URL before selecting REDIS_DB"
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
  grep -Fq 'one_click_patch_conf_redis_endpoint "${tc_cfg}" "CubeTemplateCenter"' \
    "${ONE_CLICK_DIR}/install.sh" \
    || fail "install.sh must patch CubeTemplateCenter external Redis endpoint"
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

test_one_click_redis_db_normalizes_leading_zeros() {
  local got
  got="$(
    unset CUBE_EXTERNAL_REDIS_DB
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    CUBE_EXTERNAL_REDIS_DB=08 one_click_redis_db
  )"
  [[ "${got}" == "8" ]] || fail "expected leading-zero 08 → 8, got '${got}'"
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
test_one_click_redis_db_normalizes_leading_zeros
test_legacy_redis_db_is_derived_on_upgrade
test_legacy_redis_db_conflict_prefers_master
test_legacy_master_redis_db_ambiguity_fails
test_legacy_master_missing_redis_falls_back_to_env
test_redis_db_in_db_intent_keys
test_redis_db_intent_survives_upgrade_merge

echo "redis runtime env tests OK"
