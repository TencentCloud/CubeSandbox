#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for one-click external PostgreSQL (CUBE_DATABASE_DRIVER /
# CUBE_EXTERNAL_POSTGRES_*), mirroring Helm database.driver / postgres.*.
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

assert_contains() {
  if ! grep -Fq -- "$2" "$1"; then
    fail "expected $1 to contain: $2"
  fi
}

persist_db_clean() {
  local env_file="$1"
  shift
  (
    unset CUBE_DATABASE_DRIVER \
      CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_MYSQL_PORT \
      CUBE_EXTERNAL_MYSQL_USER CUBE_EXTERNAL_MYSQL_PASSWORD CUBE_EXTERNAL_MYSQL_DB \
      CUBE_EXTERNAL_POSTGRES_HOST CUBE_EXTERNAL_POSTGRES_PORT \
      CUBE_EXTERNAL_POSTGRES_USER CUBE_EXTERNAL_POSTGRES_PASSWORD CUBE_EXTERNAL_POSTGRES_DB \
      CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_PORT \
      CUBE_SANDBOX_MYSQL_USER CUBE_SANDBOX_MYSQL_PASSWORD CUBE_SANDBOX_MYSQL_DB \
      DATABASE_URL
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    if [[ "$#" -gt 0 ]]; then
      export "$@"
    fi
    persist_one_click_database_runtime_env "${env_file}"
  )
}

expect_validate_fail() {
  local label="$1"
  shift
  if (
    unset CUBE_DATABASE_DRIVER \
      CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_POSTGRES_HOST
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    export "$@"
    validate_one_click_database_config
  ) >/dev/null 2>&1; then
    fail "expected validate to fail: ${label}"
  fi
}

test_validate_rejects_bad_driver() {
  expect_validate_fail "bad driver" CUBE_DATABASE_DRIVER=sqlite
}

test_validate_postgres_requires_host() {
  expect_validate_fail "postgres without host" CUBE_DATABASE_DRIVER=postgres
}

test_validate_postgres_rejects_mysql_host() {
  expect_validate_fail "both hosts" \
    CUBE_DATABASE_DRIVER=postgres \
    CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com \
    CUBE_EXTERNAL_MYSQL_HOST=db.example.com
}

test_validate_mysql_rejects_postgres_host_without_driver() {
  expect_validate_fail "postgres host with mysql driver" \
    CUBE_DATABASE_DRIVER=mysql \
    CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com
}

test_validate_postgres_ok() {
  (
    unset CUBE_EXTERNAL_MYSQL_HOST
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    export CUBE_DATABASE_DRIVER=postgres
    export CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com
    validate_one_click_database_config
    [[ "${CUBE_DATABASE_DRIVER}" == "postgres" ]] || fail "driver not normalized"
  ) || fail "postgres validate should succeed"
}

test_persist_postgres_url_and_scrub_mysql() {
  local env_file="${TMP_DIR}/pg.env"
  : > "${env_file}"
  upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_HOST" "stale.mysql"
  upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_PASSWORD" "stale"

  persist_db_clean "${env_file}" \
    CUBE_DATABASE_DRIVER=postgres \
    CUBE_EXTERNAL_POSTGRES_HOST=10.0.0.20 \
    CUBE_EXTERNAL_POSTGRES_PORT=5432 \
    CUBE_EXTERNAL_POSTGRES_USER=cube \
    CUBE_EXTERNAL_POSTGRES_PASSWORD='p@ss:word/#' \
    CUBE_EXTERNAL_POSTGRES_DB=cube_mvp

  assert_value "${env_file}" CUBE_DATABASE_DRIVER postgres
  assert_value "${env_file}" CUBE_EXTERNAL_POSTGRES_HOST 10.0.0.20
  assert_value "${env_file}" DATABASE_URL \
    'postgresql://cube:p%40ss%3Aword%2F%23@10.0.0.20:5432/cube_mvp'
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_MYSQL_HOST="
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_MYSQL_PASSWORD="
}

test_persist_mysql_scrubs_postgres() {
  local env_file="${TMP_DIR}/mysql.env"
  : > "${env_file}"
  upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_HOST" "stale.pg"
  upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_PASSWORD" "stale"

  persist_db_clean "${env_file}" \
    CUBE_DATABASE_DRIVER=mysql \
    CUBE_EXTERNAL_MYSQL_HOST=10.0.0.21 \
    CUBE_EXTERNAL_MYSQL_PORT=3306 \
    CUBE_EXTERNAL_MYSQL_USER=cube \
    CUBE_EXTERNAL_MYSQL_PASSWORD=secret \
    CUBE_EXTERNAL_MYSQL_DB=cube_mvp

  assert_value "${env_file}" CUBE_DATABASE_DRIVER mysql
  assert_value "${env_file}" DATABASE_URL \
    'mysql://cube:secret@10.0.0.21:3306/cube_mvp'
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_POSTGRES_HOST="
}

test_persist_local_mysql_default() {
  local env_file="${TMP_DIR}/local.env"
  : > "${env_file}"

  persist_db_clean "${env_file}"

  assert_value "${env_file}" CUBE_DATABASE_DRIVER mysql
  assert_value "${env_file}" DATABASE_URL \
    'mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp'
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_POSTGRES_HOST="
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_MYSQL_HOST="
}

test_skip_local_mysql_helper() {
  (
    unset CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_POSTGRES_HOST CUBE_DATABASE_DRIVER
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    one_click_skip_local_mysql && fail "default must not skip"
    export CUBE_DATABASE_DRIVER=postgres
    one_click_skip_local_mysql && fail "bare driver=postgres must not skip"
    export CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com
    one_click_skip_local_mysql || fail "postgres host must skip"
  ) || fail "skip helper checks failed"
}

test_contract_wiring() {
  local install_sh="${ONE_CLICK_DIR}/install.sh"
  local up_support="${ONE_CLICK_DIR}/scripts/one-click/up-support.sh"
  local env_example="${ONE_CLICK_DIR}/env.example"
  local cfg="${ONE_CLICK_DIR}/../../configs/single-node/cubemaster.yaml"
  assert_contains "${env_example}" "CUBE_DATABASE_DRIVER=mysql"
  assert_contains "${env_example}" "CUBE_EXTERNAL_POSTGRES_HOST"
  # Single DRIVER knob (not duplicated inside each engine block).
  local driver_lines
  driver_lines="$(grep -cE '^#?[[:space:]]*CUBE_DATABASE_DRIVER=' "${env_example}" || true)"
  [[ "${driver_lines}" -eq 1 ]] || fail "env.example should declare CUBE_DATABASE_DRIVER once (got ${driver_lines})"
  assert_contains "${install_sh}" "CUBE_EXTERNAL_POSTGRES_HOST"
  assert_contains "${install_sh}" 'patch_conf_external_instance_db "${cfg}"'
  assert_contains "${install_sh}" 'patch_conf_external_instance_db "${tc_cfg}"'
  assert_contains "${ONE_CLICK_DIR}/lib/common.sh" "patch_cubemaster_instance_db_config"
  assert_contains "${install_sh}" "apply_one_click_database_intent"
  assert_contains "${install_sh}" "capture_one_click_database_dotenv_values"
  assert_contains "${install_sh}" "PGCONNECT_TIMEOUT"
  assert_contains "${up_support}" "CUBE_EXTERNAL_POSTGRES_HOST"
  assert_contains "${cfg}" 'driver: "mysql"'
}


# Simulate install.sh: snapshot presence+process env, source .env (quotes
# stripped), capture shell-interpreted dotenv values, then apply after merge.
run_db_intent_roundtrip() {
  local env_file="$1"
  snapshot_one_click_database_intent "${env_file}"
  if [[ -f "${env_file}" ]]; then
    # shellcheck disable=SC1090
    set -a
    # shellcheck disable=SC1091
    source "${env_file}"
    set +a
    capture_one_click_database_dotenv_values
  fi
}

test_upgrade_intent_clears_opposite_mysql() {
  local env_file="${TMP_DIR}/intent.env"
  cat > "${env_file}" <<'EOF'
CUBE_DATABASE_DRIVER=postgres
CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com
CUBE_EXTERNAL_POSTGRES_PASSWORD=secret
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    # Simulate upgrade merge re-injecting the previous MySQL marker.
    export CUBE_DATABASE_DRIVER=mysql
    export CUBE_EXTERNAL_MYSQL_HOST=stale.mysql
    export CUBE_EXTERNAL_MYSQL_PASSWORD=stale
    export CUBE_EXTERNAL_POSTGRES_HOST=""
    apply_one_click_database_intent
    [[ "${CUBE_DATABASE_DRIVER}" == "postgres" ]] || fail "driver not restored"
    [[ "${CUBE_EXTERNAL_POSTGRES_HOST}" == "pg.example.com" ]] || fail "postgres host not restored"
    [[ -z "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]] || fail "stale mysql host not cleared"
  ) || fail "upgrade intent mysql→postgres failed"
}

test_upgrade_intent_keeps_undeclared_mysql_password() {
  local env_file="${TMP_DIR}/partial.env"
  cat > "${env_file}" <<'EOF'
CUBE_DATABASE_DRIVER=mysql
CUBE_EXTERNAL_MYSQL_HOST=db.example.com
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    export CUBE_EXTERNAL_MYSQL_HOST=old.mysql
    export CUBE_EXTERNAL_MYSQL_PASSWORD='keep-me'
    export CUBE_EXTERNAL_POSTGRES_HOST=stale.pg
    apply_one_click_database_intent
    [[ "${CUBE_EXTERNAL_MYSQL_HOST}" == "db.example.com" ]] || fail "host from .env not applied"
    [[ "${CUBE_EXTERNAL_MYSQL_PASSWORD}" == "keep-me" ]] || fail "undeclared password must stay merge-preserved"
    [[ -z "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]] || fail "opposite postgres host not cleared"
  ) || fail "upgrade intent keep password failed"
}

test_upgrade_intent_process_env_fills_gap() {
  local env_file="${TMP_DIR}/driver-only.env"
  cat > "${env_file}" <<'EOF'
CUBE_DATABASE_DRIVER=postgres
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    export CUBE_EXTERNAL_POSTGRES_HOST=from-cli.example.com
    run_db_intent_roundtrip "${env_file}"
    # Merge left stale mysql; CLI host must survive alongside dotenv driver.
    export CUBE_EXTERNAL_MYSQL_HOST=stale.mysql
    export CUBE_EXTERNAL_POSTGRES_HOST=""
    apply_one_click_database_intent
    [[ "${CUBE_DATABASE_DRIVER}" == "postgres" ]] || fail "driver from .env"
    [[ "${CUBE_EXTERNAL_POSTGRES_HOST}" == "from-cli.example.com" ]] || fail "process env host not applied"
    [[ -z "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]] || fail "opposite mysql not cleared"
  ) || fail "upgrade intent process env gap failed"
}

test_upgrade_intent_bundled_via_empty_host() {
  local env_file="${TMP_DIR}/bundled.env"
  cat > "${env_file}" <<'EOF'
CUBE_DATABASE_DRIVER=mysql
CUBE_EXTERNAL_MYSQL_HOST=
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    export CUBE_EXTERNAL_MYSQL_HOST=stale.mysql
    export CUBE_EXTERNAL_POSTGRES_HOST=stale.pg
    apply_one_click_database_intent
    [[ -z "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]] || fail "empty .env host must clear mysql"
    [[ -z "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]] || fail "postgres host should clear for mysql driver"
  ) || fail "upgrade intent bundled failed"
}

test_upgrade_intent_quoted_password_survives() {
  local env_file="${TMP_DIR}/quoted.env"
  cat > "${env_file}" <<'EOF'
CUBE_DATABASE_DRIVER=postgres
CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com
CUBE_EXTERNAL_POSTGRES_PASSWORD='p@ss word#'
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    export CUBE_EXTERNAL_POSTGRES_PASSWORD='corrupted-from-merge'
    apply_one_click_database_intent
    [[ "${CUBE_EXTERNAL_POSTGRES_PASSWORD}" == 'p@ss word#' ]]       || fail "quoted .env password must round-trip without quote chars (got: ${CUBE_EXTERNAL_POSTGRES_PASSWORD})"
  ) || fail "upgrade intent quoted password failed"
}

test_upgrade_intent_host_implies_driver() {
  local env_file="${TMP_DIR}/host-only.env"
  cat > "${env_file}" <<'EOF'
CUBE_EXTERNAL_MYSQL_HOST=db2.example.com
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    # Stale merge-preserved postgres driver must not scrub the new MySQL host.
    export CUBE_DATABASE_DRIVER=postgres
    export CUBE_EXTERNAL_POSTGRES_HOST=stale.pg
    export CUBE_EXTERNAL_MYSQL_HOST=""
    apply_one_click_database_intent
    [[ "${CUBE_DATABASE_DRIVER}" == "mysql" ]] || fail "host must imply mysql driver"
    [[ "${CUBE_EXTERNAL_MYSQL_HOST}" == "db2.example.com" ]] || fail "mysql host not restored"
    [[ -z "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]] || fail "stale postgres host not cleared"
  ) || fail "upgrade intent host implies driver failed"
}

test_upgrade_intent_rejects_driver_host_conflict() {
  local env_file="${TMP_DIR}/conflict.env"
  cat > "${env_file}" <<'EOF'
CUBE_DATABASE_DRIVER=mysql
CUBE_EXTERNAL_POSTGRES_HOST=pg.example.com
EOF
  if (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    apply_one_click_database_intent
  ) >/dev/null 2>&1; then
    fail "expected die on DRIVER=mysql + POSTGRES_HOST"
  fi
}

test_upgrade_intent_noop_without_dotenv_keys() {
  local env_file="${TMP_DIR}/empty.env"
  : > "${env_file}"
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    run_db_intent_roundtrip "${env_file}"
    export CUBE_EXTERNAL_MYSQL_HOST=keep.mysql
    apply_one_click_database_intent
    [[ "${CUBE_EXTERNAL_MYSQL_HOST}" == "keep.mysql" ]] || fail "should preserve when .env has no DB keys"
  ) || fail "upgrade intent noop failed"
}

test_persist_postgres_skips_empty_host() {
  local env_file="${TMP_DIR}/empty-pg-host.env"
  cat > "${env_file}" <<'EOF'
DATABASE_URL=postgresql://stale@old:5432/db
CUBE_EXTERNAL_POSTGRES_HOST=stale.pg
EOF
  persist_db_clean "${env_file}" CUBE_DATABASE_DRIVER=postgres
  # Empty host → bundled MySQL markers (safe for later control-role reuse).
  assert_value "${env_file}" CUBE_DATABASE_DRIVER mysql
  assert_not_contains "${env_file}" "CUBE_EXTERNAL_POSTGRES_HOST="
  assert_value "${env_file}" DATABASE_URL 'mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp'
}


test_patch_conf_postgres_preserves_cube_ops_addr() {
  local cfg="${TMP_DIR}/cubemaster-pg.conf.yaml"
  local ops_addr='http://127.0.0.1:3010'
  cat > "${cfg}" <<EOF
common:
  cube_ops_addr: "${ops_addr}"
instance_db_config:
  driver: "mysql"
  addr: "127.0.0.1:3306"
  user: "cube"
  pwd: "cube_pass"
  db_name: "cube_mvp"
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    patch_cubemaster_instance_db_config "${cfg}" "postgres" \
      "10.0.0.20:5432" "pguser" "pg/pass|x" "cube_mvp"
  ) || fail "patch postgres failed"
  grep -Fq 'cube_ops_addr: "http://127.0.0.1:3010"' "${cfg}" \
    || fail "cube_ops_addr must not be clobbered"
  grep -Fq 'driver: "postgres"' "${cfg}" || fail "driver not patched"
  grep -Fq 'addr: "10.0.0.20:5432"' "${cfg}" || fail "addr not patched"
  grep -Fq 'user: "pguser"' "${cfg}" || fail "user not patched"
  grep -Fq 'pwd: "pg/pass|x"' "${cfg}" || fail "pwd not patched"
  grep -Fq 'db_name: "cube_mvp"' "${cfg}" || fail "db_name not patched"
}

# Shipped templates: CubeMaster has the driver field, CubeTemplateCenter does
# not; the password exercises escape_sed.
apply_external_db_to_shipped_templates() {
  local master="${TMP_DIR}/shipped-cubemaster.yaml"
  local tc="${TMP_DIR}/shipped-templatecenter.yaml"
  local repo_root
  repo_root="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"
  cp "${repo_root}/configs/single-node/cubemaster.yaml" "${master}"
  cp "${repo_root}/configs/single-node/templatecenter.yaml" "${tc}"
  (( $# )) || fail "apply_external_db_to_shipped_templates needs at least one VAR=value"
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    unset CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_POSTGRES_HOST
    export "$@"
    patch_conf_external_instance_db "${master}"
    patch_conf_external_instance_db "${tc}"
  ) || fail "patch_conf_external_instance_db failed"
  printf '%s\n%s\n' "${master}" "${tc}"
}

assert_instance_db() {
  local file="$1" driver="$2" addr="$3" user="$4" pwd="$5" db_name="$6"
  grep -Fq "driver: \"${driver}\"" "${file}" || fail "${file}: driver want ${driver}"
  grep -Fq "addr: \"${addr}\"" "${file}" || fail "${file}: addr want ${addr}"
  grep -Fq "user: \"${user}\"" "${file}" || fail "${file}: user want ${user}"
  grep -Fq "pwd: \"${pwd}\"" "${file}" || fail "${file}: pwd not written intact"
  grep -Fq "db_name: \"${db_name}\"" "${file}" || fail "${file}: db_name want ${db_name}"
  local driver_lines
  driver_lines="$(grep -cE '^[[:space:]]*driver:' "${file}")"
  [[ "${driver_lines}" -eq 1 ]] || fail "${file}: expected one driver line, got ${driver_lines}"
  grep -qE '^  driver: "' "${file}" || fail "${file}: driver must stay indented under instance_db_config"
}

test_external_postgres_patches_templatecenter() {
  local files master tc
  files="$(apply_external_db_to_shipped_templates \
    CUBE_EXTERNAL_POSTGRES_HOST=pg.example.internal \
    CUBE_EXTERNAL_POSTGRES_PORT=5432 \
    CUBE_EXTERNAL_POSTGRES_USER=pguser \
    CUBE_EXTERNAL_POSTGRES_PASSWORD='pg/pass|x' \
    CUBE_EXTERNAL_POSTGRES_DB=cube_ext
  )" || fail "postgres template patch setup failed"
  master="${files%%$'\n'*}"
  tc="${files#*$'\n'}"
  assert_instance_db "${master}" postgres "pg.example.internal:5432" pguser 'pg/pass|x' cube_ext
  assert_instance_db "${tc}" postgres "pg.example.internal:5432" pguser 'pg/pass|x' cube_ext
  assert_not_contains "${tc}" "127.0.0.1:__CUBE_SANDBOX_MYSQL_PORT__"
  assert_not_contains "${tc}" "__CUBE_SANDBOX_MYSQL_USER__"
  grep -Fq 'cube_ops_addr: "__CUBEMASTER_CUBE_OPS_ADDR__"' "${master}" \
    || fail "cube_ops_addr must survive external postgres patch"
  grep -Fq 'nodes: "127.0.0.1:__CUBE_SANDBOX_REDIS_PORT__"' "${tc}" \
    || fail "templatecenter redis endpoint must be left to the redis patcher"
}

test_external_mysql_patches_templatecenter() {
  local files master tc
  files="$(apply_external_db_to_shipped_templates \
    CUBE_EXTERNAL_MYSQL_HOST=mysql.example.internal \
    CUBE_EXTERNAL_MYSQL_PORT=3306 \
    CUBE_EXTERNAL_MYSQL_USER=extuser \
    CUBE_EXTERNAL_MYSQL_PASSWORD=ext-secret \
    CUBE_EXTERNAL_MYSQL_DB=cube_ext
  )" || fail "mysql template patch setup failed"
  master="${files%%$'\n'*}"
  tc="${files#*$'\n'}"
  assert_instance_db "${master}" mysql "mysql.example.internal:3306" extuser ext-secret cube_ext
  assert_instance_db "${tc}" mysql "mysql.example.internal:3306" extuser ext-secret cube_ext
  assert_not_contains "${tc}" "127.0.0.1:__CUBE_SANDBOX_MYSQL_PORT__"
}

test_external_db_patch_noop_without_host() {
  local tc="${TMP_DIR}/noop-templatecenter.yaml"
  local repo_root before
  repo_root="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"
  cp "${repo_root}/configs/single-node/templatecenter.yaml" "${tc}"
  before="$(cat "${tc}")"
  (
    unset CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_POSTGRES_HOST
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    patch_conf_external_instance_db "${tc}"
  ) || fail "noop patch failed"
  [[ "$(cat "${tc}")" == "${before}" ]] || fail "unset external hosts must leave conf unchanged"
}

test_external_db_patch_missing_file_ok() {
  (
    unset CUBE_EXTERNAL_MYSQL_HOST
    export CUBE_EXTERNAL_POSTGRES_HOST=pg.example.internal
    export CUBE_EXTERNAL_POSTGRES_PORT=5432
    export CUBE_EXTERNAL_POSTGRES_USER=pguser
    export CUBE_EXTERNAL_POSTGRES_PASSWORD=secret
    export CUBE_EXTERNAL_POSTGRES_DB=cube_ext
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    patch_conf_external_instance_db "${TMP_DIR}/no-such-templatecenter.yaml"
  ) || fail "missing conf must not fail the patch (older packages omit TemplateCenter)"
}

# A `driver:` outside instance_db_config must survive: the probe and the
# rewrite are both confined to the block, so a future plugin-style entry with
# its own driver key cannot be clobbered while the real one stays unset.
test_external_db_patch_ignores_foreign_driver() {
  local cfg="${TMP_DIR}/decoy-driver.yaml" driver_lines
  cat > "${cfg}" <<'EOF'
instance_db_config:
  addr: "127.0.0.1:3306"
  user: "cube"
  pwd: "cube_pass"
  db_name: "cube_mvp"
volume_plugins:
  driver: "keep-me"
EOF
  (
    unset CUBE_EXTERNAL_MYSQL_HOST
    export CUBE_EXTERNAL_POSTGRES_HOST=pg.example.internal
    export CUBE_EXTERNAL_POSTGRES_PORT=5432
    export CUBE_EXTERNAL_POSTGRES_USER=pguser
    export CUBE_EXTERNAL_POSTGRES_PASSWORD=secret
    export CUBE_EXTERNAL_POSTGRES_DB=cube_ext
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    patch_conf_external_instance_db "${cfg}"
  ) || fail "decoy-driver patch failed"
  grep -Fq 'driver: "keep-me"' "${cfg}" \
    || fail "driver outside instance_db_config must not be rewritten"
  grep -Fq 'driver: "postgres"' "${cfg}" \
    || fail "in-block driver must be rewritten to the external engine"
  grep -Fq 'addr: "pg.example.internal:5432"' "${cfg}" || fail "addr not patched"
  driver_lines="$(grep -cE '^[[:space:]]*driver:' "${cfg}")"
  [[ "${driver_lines}" -eq 2 ]] \
    || fail "expected the foreign and the in-block driver only (got ${driver_lines})"
}

# Without an instance_db_config block there is nothing to point at the external
# endpoint; failing loudly beats leaving the component on the bundled endpoint.
test_external_db_patch_requires_instance_block() {
  local cfg="${TMP_DIR}/no-block.yaml"
  printf '%s\n' 'log:' '  level: "info"' > "${cfg}"
  if (
    unset CUBE_EXTERNAL_MYSQL_HOST
    export CUBE_EXTERNAL_POSTGRES_HOST=pg.example.internal
    export CUBE_EXTERNAL_POSTGRES_PORT=5432
    export CUBE_EXTERNAL_POSTGRES_USER=pguser
    export CUBE_EXTERNAL_POSTGRES_PASSWORD=secret
    export CUBE_EXTERNAL_POSTGRES_DB=cube_ext
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    patch_conf_external_instance_db "${cfg}"
  ); then
    fail "a conf without instance_db_config must fail the patch"
  fi
}

# The patch rewrites conf.yaml port placeholders, so it must run after both
# generators; pin the call order, not just the call presence.
test_control_plane_patch_order() {
  local cm tc patch
  cm="$(grep -n '^  generate_cubemaster_config_ports$' "${ONE_CLICK_DIR}/install.sh" | cut -d: -f1)"
  tc="$(grep -n '^  generate_templatecenter_config$' "${ONE_CLICK_DIR}/install.sh" | cut -d: -f1)"
  patch="$(grep -n '^  patch_cubemaster_external_deps$' "${ONE_CLICK_DIR}/install.sh" | cut -d: -f1)"
  [[ -n "${cm}" && -n "${tc}" && -n "${patch}" ]] \
    || fail "install.sh must call generate_cubemaster_config_ports, generate_templatecenter_config and patch_cubemaster_external_deps"
  [[ "${cm}" -lt "${tc}" && "${tc}" -lt "${patch}" ]] \
    || fail "config generation must precede patching (cm=${cm} tc=${tc} patch=${patch})"
}

test_patch_conf_mysql_preserves_cube_ops_addr() {
  local cfg="${TMP_DIR}/cubemaster-mysql.conf.yaml"
  cat > "${cfg}" <<'EOF'
common:
  cube_ops_addr: "http://10.1.2.3:3010"
instance_db_config:
  driver: "postgres"
  addr: "10.0.0.20:5432"
  user: "pg"
  pwd: "x"
  db_name: "cube_mvp"
EOF
  (
    # shellcheck disable=SC1091
    source "${ONE_CLICK_DIR}/lib/common.sh"
    patch_cubemaster_instance_db_config "${cfg}" "mysql" \
      "10.0.0.30:3306" "cube" "cube_pass" "cube_mvp"
  ) || fail "patch mysql failed"
  grep -Fq 'cube_ops_addr: "http://10.1.2.3:3010"' "${cfg}" \
    || fail "cube_ops_addr must not be clobbered by mysql patch"
  grep -Fq 'driver: "mysql"' "${cfg}" || fail "driver not patched to mysql"
  grep -Fq 'addr: "10.0.0.30:3306"' "${cfg}" || fail "mysql addr not patched"
}

test_skip_local_mysql_predicate_consistency() {
  local helper up_support quickcheck postcheck
  helper="$(sed -n '/^one_click_skip_local_mysql()/,/^}/p' "${ONE_CLICK_DIR}/lib/common.sh")"
  echo "${helper}" | grep -Fq 'CUBE_EXTERNAL_MYSQL_HOST' \
    || fail "helper must key off MYSQL host"
  echo "${helper}" | grep -Fq 'CUBE_EXTERNAL_POSTGRES_HOST' \
    || fail "helper must key off POSTGRES host"
  echo "${helper}" | grep -q 'CUBE_DATABASE_DRIVER' \
    && fail "helper must not key off bare DRIVER"
  up_support="${ONE_CLICK_DIR}/scripts/one-click/up-support.sh"
  quickcheck="${ONE_CLICK_DIR}/scripts/one-click/quickcheck.sh"
  postcheck="${ONE_CLICK_DIR}/scripts/systemd/mysql-postcheck.sh"
  for f in "${up_support}" "${quickcheck}" "${postcheck}"; do
    grep -Fq 'CUBE_EXTERNAL_MYSQL_HOST' "${f}" || fail "${f}: missing MYSQL host check"
    grep -Fq 'CUBE_EXTERNAL_POSTGRES_HOST' "${f}" || fail "${f}: missing POSTGRES host check"
  done
  # skip assignment must not reference bare DRIVER anymore
  ! grep -E 'skip_local_mysql=1|SKIP_LOCAL_MYSQL=1' -A1 -B3 "${up_support}" "${quickcheck}" \
    | grep -E 'CUBE_DATABASE_DRIVER|DATABASE_DRIVER' \
    || fail "up-support/quickcheck skip still references DRIVER"
  ! grep -E 'CUBE_DATABASE_DRIVER.*postgres' "${postcheck}" \
    || fail "mysql-postcheck still references bare DRIVER"
}


test_validate_rejects_bad_driver
test_validate_postgres_requires_host
test_validate_postgres_rejects_mysql_host
test_validate_mysql_rejects_postgres_host_without_driver
test_validate_postgres_ok
test_persist_postgres_url_and_scrub_mysql
test_persist_mysql_scrubs_postgres
test_persist_local_mysql_default
test_skip_local_mysql_helper
test_contract_wiring
test_upgrade_intent_clears_opposite_mysql
test_upgrade_intent_keeps_undeclared_mysql_password
test_upgrade_intent_process_env_fills_gap
test_upgrade_intent_bundled_via_empty_host
test_upgrade_intent_quoted_password_survives
test_upgrade_intent_host_implies_driver
test_upgrade_intent_rejects_driver_host_conflict
test_upgrade_intent_noop_without_dotenv_keys
test_persist_postgres_skips_empty_host
test_patch_conf_postgres_preserves_cube_ops_addr
test_patch_conf_mysql_preserves_cube_ops_addr
test_external_postgres_patches_templatecenter
test_external_mysql_patches_templatecenter
test_external_db_patch_noop_without_host
test_external_db_patch_missing_file_ok
test_external_db_patch_ignores_foreign_driver
test_external_db_patch_requires_instance_block
test_control_plane_patch_order
test_skip_local_mysql_predicate_consistency

echo "external postgres tests OK"
