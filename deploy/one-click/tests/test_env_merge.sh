#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the config-preserving env merge used by `install.sh --mode upgrade`
# (M3-1/M3-3). These exercise merge_env_three_way and the version/env helpers in
# lib/common.sh without touching the system.
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

# get_value FILE KEY -> prints raw RHS of the last active KEY= line.
get_value() {
  local file="$1" key="$2"
  sed -n "/^${key}=/{s/^${key}=//;p;}" "${file}" | tail -n 1
}

assert_value() {
  local file="$1" key="$2" expected="$3"
  local actual
  actual="$(get_value "${file}" "${key}")"
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

write_new_example() {
  cat > "$1" <<'EOF'
# sample env template
ONE_CLICK_DEPLOY_ROLE=control
CUBE_PVM_ENABLE=0
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
WEB_UI_IMAGE=registry/openresty:1.21.4.1-6
CUBE_PROXY_CERT_DIR=/usr/local/services/cubetoolbox/cubeproxy/certs
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
NEW_FEATURE_FLAG=on
# CUBE_SANDBOX_NODE_IP=10.0.0.10
EOF
}

test_preserves_user_customized_value() {
  local new="${TMP_DIR}/new1.example" old="${TMP_DIR}/old1.env"
  local out="${TMP_DIR}/out1.env" diff="${TMP_DIR}/diff1.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_PORT=3307
CUBE_SANDBOX_REDIS_PASSWORD=mysecret
ONE_CLICK_DEPLOY_ROLE=control
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_SANDBOX_MYSQL_PORT 3307
  assert_value "${out}" CUBE_SANDBOX_REDIS_PASSWORD mysecret
  # untouched key keeps new default
  assert_value "${out}" NEW_FEATURE_FLAG on
}

test_adds_new_keys_with_defaults() {
  local new="${TMP_DIR}/new2.example" old="${TMP_DIR}/old2.env"
  local out="${TMP_DIR}/out2.env" diff="${TMP_DIR}/diff2.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_PORT=3306
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" NEW_FEATURE_FLAG on
  assert_contains "${diff}" "[added]"
  assert_contains "${diff}" "NEW_FEATURE_FLAG=on"
}

test_three_way_adopts_new_default_for_untouched_key() {
  local new="${TMP_DIR}/new3.example" old="${TMP_DIR}/old3.env" base="${TMP_DIR}/base3.example"
  local out="${TMP_DIR}/out3.env" diff="${TMP_DIR}/diff3.txt"
  write_new_example "${new}"
  # baseline (old defaults) had the OLD image tag
  cat > "${base}" <<'EOF'
WEB_UI_IMAGE=registry/openresty:1.21.4.0-OLD
CUBE_SANDBOX_MYSQL_PORT=3306
EOF
  # user never touched WEB_UI_IMAGE -> still equals old default
  cat > "${old}" <<'EOF'
WEB_UI_IMAGE=registry/openresty:1.21.4.0-OLD
CUBE_SANDBOX_MYSQL_PORT=3306
EOF

  merge_env_three_way "${new}" "${old}" "${base}" "" "${out}" "${diff}" 2>/dev/null

  # adopts the NEW default since the user never customized it
  assert_value "${out}" WEB_UI_IMAGE "registry/openresty:1.21.4.1-6"
  assert_contains "${diff}" "three-way"
  assert_contains "${diff}" "[default-updated]"
}

test_three_way_keeps_customized_over_new_default() {
  local new="${TMP_DIR}/new4.example" old="${TMP_DIR}/old4.env" base="${TMP_DIR}/base4.example"
  local out="${TMP_DIR}/out4.env" diff="${TMP_DIR}/diff4.txt"
  write_new_example "${new}"
  cat > "${base}" <<'EOF'
WEB_UI_IMAGE=registry/openresty:1.21.4.0-OLD
EOF
  # user DID customize WEB_UI_IMAGE
  cat > "${old}" <<'EOF'
WEB_UI_IMAGE=registry/my-custom-openresty:9.9
EOF

  merge_env_three_way "${new}" "${old}" "${base}" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" WEB_UI_IMAGE "registry/my-custom-openresty:9.9"
  assert_contains "${diff}" "[preserved]"
}

test_preserves_shell_sensitive_values() {
  local new="${TMP_DIR}/new5.example" old="${TMP_DIR}/old5.env"
  local out="${TMP_DIR}/out5.env" diff="${TMP_DIR}/diff5.txt"
  write_new_example "${new}"
  # user customized a value containing '=' and a URL with ://@
  cat > "${old}" <<'EOF'
DATABASE_URL=mysql://u:p@host:3306/db2
WEIRD_KEY=a=b=c
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_PROXY_CERT_DIR "/usr/local/services/cubetoolbox/cubeproxy/certs"
  assert_value "${out}" DATABASE_URL "mysql://u:p@host:3306/db2"
  # WEIRD_KEY is old-only -> appended verbatim, value with '=' intact
  assert_value "${out}" WEIRD_KEY "a=b=c"

  # The merged file must remain valid shell that sources cleanly.
  (
    set -a
    # shellcheck disable=SC1090
    source "${out}"
    set +a
    [[ "${CUBE_PROXY_CERT_DIR}" == "/usr/local/services/cubetoolbox/cubeproxy/certs" ]] \
      || { echo "expansion failed: ${CUBE_PROXY_CERT_DIR}" >&2; exit 1; }
    [[ "${DATABASE_URL}" == "mysql://u:p@host:3306/db2" ]] || exit 1
    [[ "${WEIRD_KEY}" == "a=b=c" ]] || exit 1
  ) || fail "merged env did not source/expand correctly"
}

test_upsert_env_kv_preserves_shell_sensitive_values() {
  local env_file="${TMP_DIR}/upsert-sensitive.env"
  local secret=$'p@$$ word `tick` "quote" #hash;\\slash'

  upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_PASSWORD" "${secret}"

  assert_contains "${env_file}" 'CUBE_EXTERNAL_MYSQL_PASSWORD="'
  (
    unset CUBE_EXTERNAL_MYSQL_PASSWORD
    load_env_file "${env_file}"
    [[ "${CUBE_EXTERNAL_MYSQL_PASSWORD}" == "${secret}" ]] || {
      echo "expected '${secret}', got '${CUBE_EXTERNAL_MYSQL_PASSWORD:-}'" >&2
      exit 1
    }
  ) || fail "upsert_env_kv did not preserve shell-sensitive value"
}

test_upsert_env_kv_quotes_shell_metachar_only_values() {
  local env_file="${TMP_DIR}/upsert-metachar.env"
  local secret='CubeSandbox123;'

  upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_PASSWORD" "${secret}"

  assert_contains "${env_file}" 'CUBE_EXTERNAL_REDIS_PASSWORD="CubeSandbox123;'
  (
    unset CUBE_EXTERNAL_REDIS_PASSWORD
    load_env_file "${env_file}"
    [[ "${CUBE_EXTERNAL_REDIS_PASSWORD}" == "${secret}" ]] || {
      echo "expected '${secret}', got '${CUBE_EXTERNAL_REDIS_PASSWORD:-}'" >&2
      exit 1
    }
  ) || fail "shell metachar-only value should still be quoted safely"
}

test_upsert_env_kv_keeps_plain_scalars_readable() {
  local env_file="${TMP_DIR}/upsert-plain.env"

  cat > "${env_file}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
KEEP_ME=1
EOF

  upsert_env_kv "${env_file}" "ONE_CLICK_DEPLOY_ROLE" "compute"

  assert_value "${env_file}" ONE_CLICK_DEPLOY_ROLE compute
  assert_value "${env_file}" KEEP_ME 1
  [[ "$(read_env_key "${env_file}" ONE_CLICK_DEPLOY_ROLE)" == "compute" ]] \
    || fail "read_env_key should keep seeing plain scalar values"
}

test_remove_env_kv_drops_key() {
  local env_file="${TMP_DIR}/remove-sentinel.env"

  upsert_env_kv "${env_file}" "CUBE_SANDBOX_REDIS_HOST" "10.0.0.1"
  upsert_env_kv "${env_file}" "CUBE_SANDBOX_REDIS_MASTER_NAME" "mymaster"
  upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_MASTER_NAME" "mymaster"
  # remove_env_kv accepts multiple keys and strips them in one atomic rewrite.
  remove_env_kv "${env_file}" "CUBE_SANDBOX_REDIS_MASTER_NAME" "CUBE_PROXY_REDIS_MASTER_NAME"

  assert_value "${env_file}" CUBE_SANDBOX_REDIS_HOST 10.0.0.1
  if grep -q '^CUBE_SANDBOX_REDIS_MASTER_NAME=' "${env_file}"; then
    fail "CUBE_SANDBOX_REDIS_MASTER_NAME should be removed"
  fi
  if grep -q '^CUBE_PROXY_REDIS_MASTER_NAME=' "${env_file}"; then
    fail "CUBE_PROXY_REDIS_MASTER_NAME should be removed"
  fi
}

test_keeps_old_only_host_keys() {
  local new="${TMP_DIR}/new6.example" old="${TMP_DIR}/old6.env"
  local out="${TMP_DIR}/out6.env" diff="${TMP_DIR}/diff6.txt"
  write_new_example "${new}"
  # NODE_IP is commented in the template; it must survive as an active key.
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_NODE_IP=10.0.0.5
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.11
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_SANDBOX_NODE_IP 10.0.0.5
  assert_value "${out}" ONE_CLICK_CONTROL_PLANE_IP 10.0.0.11
  assert_contains "${out}" "preserved custom settings"
  assert_contains "${diff}" "[kept-extra]"
}

test_preserves_comments_and_structure() {
  local new="${TMP_DIR}/new7.example" old="${TMP_DIR}/old7.env"
  local out="${TMP_DIR}/out7.env" diff="${TMP_DIR}/diff7.txt"
  write_new_example "${new}"
  : > "${old}"

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_contains "${out}" "# sample env template"
  assert_contains "${out}" "# CUBE_SANDBOX_NODE_IP=10.0.0.10"
}

test_two_way_fallback_without_baseline() {
  local new="${TMP_DIR}/new8.example" old="${TMP_DIR}/old8.env"
  local out="${TMP_DIR}/out8.env" diff="${TMP_DIR}/diff8.txt"
  write_new_example "${new}"
  # untouched-by-user key equals OLD default; with no baseline we cannot tell,
  # so the old value must be kept (two-way: old wins).
  cat > "${old}" <<'EOF'
WEB_UI_IMAGE=registry/openresty:1.21.4.0-OLD
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" WEB_UI_IMAGE "registry/openresty:1.21.4.0-OLD"
  assert_contains "${diff}" "two-way-fallback"
}

test_two_way_migrates_legacy_cube_proxy_cert_dir_default() {
  local new="${TMP_DIR}/new_proxy_default.example" old="${TMP_DIR}/old_proxy_default.env"
  local out="${TMP_DIR}/out_proxy_default.env" diff="${TMP_DIR}/diff_proxy_default.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
ONE_CLICK_INSTALL_PREFIX=/usr/local/services/cubetoolbox
CUBE_PROXY_CERT_DIR="${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs"
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_not_contains "${out}" "ONE_CLICK_INSTALL_PREFIX="
  assert_value "${out}" CUBE_PROXY_CERT_DIR "/usr/local/services/cubetoolbox/cubeproxy/certs"
  assert_contains "${diff}" "[migrated-legacy]"
  assert_contains "${diff}" 'CUBE_PROXY_CERT_DIR: "${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs" -> /usr/local/services/cubetoolbox/cubeproxy/certs'
  (
    set -a
    # shellcheck disable=SC1090
    source "${out}"
    set +a
    [[ "${CUBE_PROXY_CERT_DIR}" == "/usr/local/services/cubetoolbox/cubeproxy/certs" ]] \
      || { echo "unexpected cert dir: ${CUBE_PROXY_CERT_DIR}" >&2; exit 1; }
  ) || fail "legacy CUBE_PROXY_CERT_DIR default was not migrated to fixed path"
}

test_two_way_migrates_single_quoted_legacy_cube_proxy_cert_dir_default() {
  local new="${TMP_DIR}/new_proxy_single_default.example" old="${TMP_DIR}/old_proxy_single_default.env"
  local out="${TMP_DIR}/out_proxy_single_default.env" diff="${TMP_DIR}/diff_proxy_single_default.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
ONE_CLICK_INSTALL_PREFIX=/usr/local/services/cubetoolbox
CUBE_PROXY_CERT_DIR='${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs'
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_not_contains "${out}" "ONE_CLICK_INSTALL_PREFIX="
  assert_value "${out}" CUBE_PROXY_CERT_DIR "/usr/local/services/cubetoolbox/cubeproxy/certs"
  assert_contains "${diff}" "[migrated-legacy]"
  assert_contains "${diff}" "CUBE_PROXY_CERT_DIR: '\${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs' -> /usr/local/services/cubetoolbox/cubeproxy/certs"
  (
    set -a
    # shellcheck disable=SC1090
    source "${out}"
    set +a
    [[ "${CUBE_PROXY_CERT_DIR}" == "/usr/local/services/cubetoolbox/cubeproxy/certs" ]] \
      || { echo "unexpected cert dir: ${CUBE_PROXY_CERT_DIR}" >&2; exit 1; }
  ) || fail "single-quoted legacy CUBE_PROXY_CERT_DIR default was not migrated to fixed path"
}

test_two_way_preserves_custom_cube_proxy_cert_dir() {
  local new="${TMP_DIR}/new_proxy_custom.example" old="${TMP_DIR}/old_proxy_custom.env"
  local out="${TMP_DIR}/out_proxy_custom.env" diff="${TMP_DIR}/diff_proxy_custom.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_PROXY_CERT_DIR=/custom/certs
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_PROXY_CERT_DIR "/custom/certs"
  assert_contains "${diff}" "[preserved]"
}

test_new_dotenv_overrides_take_priority() {
  local new="${TMP_DIR}/new9.example" old="${TMP_DIR}/old9.env" dotenv="${TMP_DIR}/new9.env"
  local out="${TMP_DIR}/out9.env" diff="${TMP_DIR}/diff9.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_PORT=3307
EOF
  # operator explicitly sets a different value in the new bundle .env
  cat > "${dotenv}" <<'EOF'
CUBE_SANDBOX_MYSQL_PORT=3399
EOF

  merge_env_three_way "${new}" "${old}" "" "${dotenv}" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_SANDBOX_MYSQL_PORT 3399
  assert_contains "${diff}" "[explicit]"
}

# --- persist_unified_dep_config: MANAGED freeze + DATABASE_URL precedence ---
# These guard the core "freeze the resolved MANAGED decision" invariant and the
# DATABASE_URL override path. Each case runs in a subshell so the exported
# CUBE_SANDBOX_* endpoint variables do not leak between tests.

test_persist_freezes_managed_0_for_external_mysql() {
  local out="${TMP_DIR}/persist_ext_mysql.env"
  : > "${out}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=db.example.com
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  )
  assert_value "${out}" CUBE_SANDBOX_MYSQL_MANAGED 0
  assert_value "${out}" CUBE_SANDBOX_MYSQL_HOST db.example.com
}

test_persist_freezes_managed_0_for_loopback_external_mysql() {
  # The regression this exists for: HOST=127.0.0.1 + MANAGED=0 must persist a
  # concrete MANAGED=0 so downstream readers do not recompute auto -> managed.
  local out="${TMP_DIR}/persist_loop_ext_mysql.env"
  : > "${out}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_MANAGED=0
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  )
  assert_value "${out}" CUBE_SANDBOX_MYSQL_MANAGED 0
}

test_persist_freezes_managed_1_for_bundled_mysql() {
  local out="${TMP_DIR}/persist_bundled_mysql.env"
  : > "${out}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  )
  assert_value "${out}" CUBE_SANDBOX_MYSQL_MANAGED 1
  # A bundled dependency must NOT freeze HOST/credentials (runtime defaults apply).
  assert_not_contains "${out}" "CUBE_SANDBOX_MYSQL_HOST="
}

test_persist_freezes_redis_managed() {
  local ext="${TMP_DIR}/persist_ext_redis.env"
  local bundled="${TMP_DIR}/persist_bundled_redis.env"
  : > "${ext}"
  : > "${bundled}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=cache.example.com
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${ext}" ""
  )
  assert_value "${ext}" CUBE_SANDBOX_REDIS_MANAGED 0
  assert_value "${ext}" CUBE_SANDBOX_REDIS_HOST cache.example.com
  assert_value "${ext}" CUBE_PROXY_REDIS_IP cache.example.com

  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${bundled}" ""
  )
  assert_value "${bundled}" CUBE_SANDBOX_REDIS_MANAGED 1
  assert_not_contains "${bundled}" "CUBE_SANDBOX_REDIS_HOST="
}

test_persist_explicit_database_url_wins() {
  # An explicit operator DATABASE_URL wins ONLY in external mysql mode (a fully
  # custom DSN can then be supplied). The host is external here.
  local out="${TMP_DIR}/persist_dburl_explicit.env"
  local operator="${TMP_DIR}/persist_dburl_operator.env"
  : > "${out}"
  cat > "${operator}" <<'EOF'
DATABASE_URL=mysql://opuser:oppass@opdb.example.com:3307/opdb
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=db.example.com
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" "${operator}"
  )
  assert_value "${out}" DATABASE_URL "mysql://opuser:oppass@opdb.example.com:3307/opdb"
}

test_persist_discards_stale_loopback_database_url_on_external() {
  # P1 regression (fslongjin third review): an existing external deployment very
  # likely still carries the pre-unification bundled default
  # `DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp` in its .env
  # (env.example shipped it ACTIVE for years and the old installer always
  # recomputed the value). On the first unified upgrade MySQL is external, so this
  # stale loopback DSN must NOT be honoured -- it would point CubeAPI at a dead
  # 127.0.0.1 while the real server is remote. It is discarded and DATABASE_URL is
  # reassembled from CUBE_SANDBOX_MYSQL_*.
  local out="${TMP_DIR}/persist_dburl_stale_loop.env"
  local operator="${TMP_DIR}/persist_dburl_stale_loop_op.env"
  : > "${out}"
  cat > "${operator}" <<'EOF'
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
EOF
  local warn="${TMP_DIR}/persist_dburl_stale_loop.err"
  (
    export CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=realpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" "${operator}"
  ) 2>"${warn}"
  assert_value "${out}" CUBE_SANDBOX_MYSQL_MANAGED 0
  # Rebuilt from the unified vars -> real host + real password, NOT the stale line.
  assert_value "${out}" DATABASE_URL "mysql://cube:realpass@10.0.0.20:3306/cube_mvp"
  assert_contains "${warn}" "ignoring the loopback DATABASE_URL"
}

test_persist_explicit_quoted_database_url_not_double_escaped() {
  # P2 regression (fslongjin third review): a hand-written explicit DATABASE_URL
  # in the operator .env is naturally quoted when it carries ? and & (query
  # params). read_env_key returns the raw RHS including the quotes; re-persisting
  # verbatim through upsert_env_kv would escape+re-wrap them, yielding a DSN with
  # literal quote characters CubeAPI cannot parse. The surrounding quote layer must
  # be stripped so the value round-trips cleanly.
  local out="${TMP_DIR}/persist_dburl_quoted.env"
  local operator="${TMP_DIR}/persist_dburl_quoted_op.env"
  local custom_dsn='mysql://opuser:oppass@opdb.example.com:3307/opdb?parseTime=true&charset=utf8mb4'
  : > "${out}"
  cat > "${operator}" <<EOF
DATABASE_URL="${custom_dsn}"
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=db.example.com
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" "${operator}"
  )
  # Sourcing the persisted env must yield the ORIGINAL DSN (no literal quotes).
  local sourced
  sourced="$(set -a; # shellcheck disable=SC1090
    source "${out}"; printf '%s' "${DATABASE_URL}")"
  [[ "${sourced}" == "${custom_dsn}" ]] \
    || fail "quoted explicit DATABASE_URL corrupted on persist: got '${sourced}'"
}

test_persist_managed_ignores_explicit_database_url() {
  # Regression guard: in managed (bundled) mode a stale explicit DATABASE_URL in
  # the operator .env must be IGNORED and DATABASE_URL reassembled from the
  # CUBE_SANDBOX_MYSQL_* vars. The bundled container is created with the custom
  # CUBE_SANDBOX_MYSQL_PASSWORD, so honouring the old default DATABASE_URL line
  # (env.example shipped `mysql://cube:cube_pass@...` active for years) would
  # point CubeAPI at the wrong password.
  local out="${TMP_DIR}/persist_dburl_managed_ignore.env"
  local operator="${TMP_DIR}/persist_dburl_managed_operator.env"
  : > "${out}"
  cat > "${operator}" <<'EOF'
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=custom_secret
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" "${operator}"
  )
  assert_value "${out}" CUBE_SANDBOX_MYSQL_MANAGED 1
  assert_value "${out}" DATABASE_URL "mysql://cube:custom_secret@127.0.0.1:3306/cube_mvp"
}

test_persist_assembles_database_url_from_unified_vars() {
  local out="${TMP_DIR}/persist_dburl_assembled.env"
  : > "${out}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=db.example.com
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    # password with URL metacharacters must be percent-encoded in DATABASE_URL
    export CUBE_SANDBOX_MYSQL_PASSWORD='p@ss:word/#%'
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  )
  assert_value "${out}" DATABASE_URL "mysql://cube:p%40ss%3Aword%2F%23%25@db.example.com:3306/cube_mvp"
}

test_persist_managed_database_url_pins_loopback_host() {
  # Regression: in managed (bundled) mode the container always binds 127.0.0.1
  # and conf.yaml is left at its 127.0.0.1 default, so the assembled DATABASE_URL
  # must use 127.0.0.1 even when *_HOST is a non-127.0.0.1 loopback literal.
  # Otherwise CubeAPI would point at 127.0.0.2 while CubeMaster/the container
  # stay on 127.0.0.1 and nothing listens at the DATABASE_URL endpoint.
  local out="${TMP_DIR}/persist_dburl_managed_loopback.env"
  : > "${out}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.2
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  )
  assert_value "${out}" CUBE_SANDBOX_MYSQL_MANAGED 1
  assert_value "${out}" DATABASE_URL "mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp"
}

# --- apply_deprecated_external_aliases: legacy CUBE_EXTERNAL_* compatibility ---
# Each case runs in a subshell that unsets the one-shot guard and every relevant
# variable so state cannot leak between cases.

test_aliases_maps_all_legacy_pairs() {
  local err="${TMP_DIR}/aliases_all.err"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_PORT CUBE_SANDBOX_MYSQL_USER \
      CUBE_SANDBOX_MYSQL_PASSWORD CUBE_SANDBOX_MYSQL_DB CUBE_SANDBOX_MYSQL_MANAGED \
      CUBE_SANDBOX_REDIS_HOST CUBE_SANDBOX_REDIS_PORT CUBE_SANDBOX_REDIS_PASSWORD \
      CUBE_SANDBOX_REDIS_MANAGED
    export CUBE_EXTERNAL_MYSQL_HOST=db.example.com
    export CUBE_EXTERNAL_MYSQL_PORT=3307
    export CUBE_EXTERNAL_MYSQL_USER=legacyuser
    export CUBE_EXTERNAL_MYSQL_PASSWORD=legacypass
    export CUBE_EXTERNAL_MYSQL_DB=legacydb
    export CUBE_EXTERNAL_REDIS_HOST=cache.example.com
    export CUBE_EXTERNAL_REDIS_PORT=6380
    export CUBE_EXTERNAL_REDIS_PASSWORD=legacyredispass
    apply_deprecated_external_aliases 2>/dev/null
    [[ "${CUBE_SANDBOX_MYSQL_HOST}" == "db.example.com" ]] || { echo "MYSQL_HOST=${CUBE_SANDBOX_MYSQL_HOST}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_PORT}" == "3307" ]] || { echo "MYSQL_PORT=${CUBE_SANDBOX_MYSQL_PORT}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_USER}" == "legacyuser" ]] || { echo "MYSQL_USER=${CUBE_SANDBOX_MYSQL_USER}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_PASSWORD}" == "legacypass" ]] || { echo "MYSQL_PASSWORD=${CUBE_SANDBOX_MYSQL_PASSWORD}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_DB}" == "legacydb" ]] || { echo "MYSQL_DB=${CUBE_SANDBOX_MYSQL_DB}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_REDIS_HOST}" == "cache.example.com" ]] || { echo "REDIS_HOST=${CUBE_SANDBOX_REDIS_HOST}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_REDIS_PORT}" == "6380" ]] || { echo "REDIS_PORT=${CUBE_SANDBOX_REDIS_PORT}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_REDIS_PASSWORD}" == "legacyredispass" ]] || { echo "REDIS_PASSWORD=${CUBE_SANDBOX_REDIS_PASSWORD}" >&2; exit 1; }
  ) 2>"${err}" || fail "legacy alias mapping incomplete: $(cat "${err}")"
}

test_aliases_emit_deprecation_warning() {
  local err="${TMP_DIR}/aliases_warn.err"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_MANAGED
    export CUBE_EXTERNAL_MYSQL_HOST=db.example.com
    apply_deprecated_external_aliases
  ) 2>"${err}" || true
  assert_contains "${err}" "deprecated"
  assert_contains "${err}" "CUBE_EXTERNAL_MYSQL_"
}

test_aliases_legacy_value_is_authoritative() {
  local err="${TMP_DIR}/aliases_priority.err"
  # Regression for the broken external upgrade path: a bundled-default (or any)
  # CUBE_SANDBOX_* value must NOT mask a still-set legacy CUBE_EXTERNAL_*. The
  # legacy value is authoritative and overwrites the new var, and the legacy key
  # is cleared afterwards so it cannot linger or re-apply.
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_MANAGED
    # The new var already carries the bundled loopback default; legacy must win.
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
    apply_deprecated_external_aliases 2>/dev/null
    [[ "${CUBE_SANDBOX_MYSQL_HOST}" == "10.0.0.20" ]] \
      || { echo "expected 10.0.0.20, got ${CUBE_SANDBOX_MYSQL_HOST}" >&2; exit 1; }
    [[ -z "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]] \
      || { echo "legacy key not cleared: ${CUBE_EXTERNAL_MYSQL_HOST}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_MANAGED}" == "0" ]] \
      || { echo "expected MANAGED=0, got ${CUBE_SANDBOX_MYSQL_MANAGED:-unset}" >&2; exit 1; }
  ) 2>"${err}" || fail "legacy value must be authoritative over the new var: $(cat "${err}")"
}

test_aliases_force_managed_0_for_legacy_host() {
  local err="${TMP_DIR}/aliases_managed.err"
  # A legacy loopback host must flip MANAGED to 0 (old semantics: any legacy host
  # meant external) when the operator has not set MANAGED explicitly.
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_MANAGED \
      CUBE_SANDBOX_REDIS_HOST CUBE_SANDBOX_REDIS_MANAGED
    export CUBE_EXTERNAL_MYSQL_HOST=127.0.0.1
    export CUBE_EXTERNAL_REDIS_HOST=127.0.0.1
    apply_deprecated_external_aliases 2>/dev/null
    [[ "${CUBE_SANDBOX_MYSQL_MANAGED}" == "0" ]] || { echo "MYSQL_MANAGED=${CUBE_SANDBOX_MYSQL_MANAGED:-unset}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_REDIS_MANAGED}" == "0" ]] || { echo "REDIS_MANAGED=${CUBE_SANDBOX_REDIS_MANAGED:-unset}" >&2; exit 1; }
  ) 2>"${err}" || fail "legacy host must force MANAGED=0: $(cat "${err}")"

  # An explicit MANAGED must NOT be overridden by the legacy-host auto-push.
  local err2="${TMP_DIR}/aliases_managed_explicit.err"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_HOST
    export CUBE_EXTERNAL_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_MANAGED=1
    apply_deprecated_external_aliases 2>/dev/null
    [[ "${CUBE_SANDBOX_MYSQL_MANAGED}" == "1" ]] \
      || { echo "expected MANAGED=1 preserved, got ${CUBE_SANDBOX_MYSQL_MANAGED}" >&2; exit 1; }
  ) 2>"${err2}" || fail "explicit MANAGED must survive legacy auto-push: $(cat "${err2}")"
}

test_merge_flips_external_to_bundled_with_managed_1() {
  # P2 regression (fslongjin third review): the documented "external -> bundled:
  # set *_MANAGED=1" upgrade path was a dead end. env.example ships *_MANAGED
  # COMMENTED, so an operator .env override never flowed through the template loop
  # and was silently dropped; even when honoured, the preserved remote HOST made
  # MANAGED=1 incompatible with a non-loopback host and install died. The merge
  # must now take the operator's *_MANAGED=1 AND reset the stale external HOST to
  # the bundled loopback default so the resolved config is self-consistent.
  local new="${TMP_DIR}/flip_new.example" old="${TMP_DIR}/flip_old.env"
  local dotenv="${TMP_DIR}/flip_dotenv.env"
  local merged="${TMP_DIR}/flip_merged.env" diff="${TMP_DIR}/flip_diff.txt"
  cat > "${new}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
# CUBE_SANDBOX_MYSQL_MANAGED=auto
CUBE_SANDBOX_REDIS_HOST=127.0.0.1
CUBE_SANDBOX_REDIS_PORT=6379
CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
# CUBE_SANDBOX_REDIS_MANAGED=auto
EOF
  # Old runtime: MySQL + Redis both frozen external at remote hosts.
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=realpass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
CUBE_SANDBOX_MYSQL_MANAGED=0
CUBE_SANDBOX_REDIS_HOST=10.0.0.21
CUBE_SANDBOX_REDIS_PORT=6379
CUBE_SANDBOX_REDIS_PASSWORD=realredispass
CUBE_SANDBOX_REDIS_MANAGED=0
EOF
  # Operator flips ONLY MySQL back to bundled; Redis is left external.
  cat > "${dotenv}" <<'EOF'
CUBE_SANDBOX_MYSQL_MANAGED=1
EOF
  merge_env_three_way "${new}" "${old}" "" "${dotenv}" "${merged}" "${diff}" 2>/dev/null

  # MySQL: forced bundled, HOST reset to the loopback default so MANAGED=1 is valid.
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_MANAGED 1
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_HOST 127.0.0.1
  # Redis: untouched -> stays external at its remote host.
  assert_value "${merged}" CUBE_SANDBOX_REDIS_MANAGED 0
  assert_value "${merged}" CUBE_SANDBOX_REDIS_HOST 10.0.0.21

  # The resolved MySQL decision must be self-consistent: sourcing the merged env
  # and calling mysql_is_managed must NOT die on a managed+non-loopback mismatch.
  (
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_MANAGED \
      CUBE_SANDBOX_REDIS_HOST CUBE_SANDBOX_REDIS_MANAGED
    set -a
    # shellcheck disable=SC1090
    source "${merged}"
    set +a
    mysql_is_managed || { echo "expected mysql managed" >&2; exit 1; }
    redis_is_external || { echo "expected redis external" >&2; exit 1; }
  ) || fail "flipped MySQL must resolve as managed without dying; Redis stays external"
}

test_merge_managed_auto_keeps_external_host() {
  # Complement: *_MANAGED=auto does NOT flip external -> bundled. auto is
  # host-driven and the upgrade preserves the external HOST, so the dependency
  # stays external (matches the docs). Only *_MANAGED=1 forces bundled.
  local new="${TMP_DIR}/auto_new.example" old="${TMP_DIR}/auto_old.env"
  local dotenv="${TMP_DIR}/auto_dotenv.env"
  local merged="${TMP_DIR}/auto_merged.env" diff="${TMP_DIR}/auto_diff.txt"
  cat > "${new}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
# CUBE_SANDBOX_MYSQL_MANAGED=auto
EOF
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=realpass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
CUBE_SANDBOX_MYSQL_MANAGED=0
EOF
  cat > "${dotenv}" <<'EOF'
CUBE_SANDBOX_MYSQL_MANAGED=auto
EOF
  merge_env_three_way "${new}" "${old}" "" "${dotenv}" "${merged}" "${diff}" 2>/dev/null
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_MANAGED auto
  # HOST preserved (not reset), so auto resolves to external.
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_HOST 10.0.0.20
  (
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_MANAGED
    set -a
    # shellcheck disable=SC1090
    source "${merged}"
    set +a
    mysql_is_external || { echo "expected mysql external under auto+remote host" >&2; exit 1; }
  ) || fail "auto + preserved remote host must resolve as external"
}

test_external_upgrade_migrates_endpoint_end_to_end() {
  # End-to-end regression for the broken external upgrade path. An old
  # .one-click.env from a pre-unification install carries BOTH the real external
  # endpoint on CUBE_EXTERNAL_* AND a bundled-default CUBE_SANDBOX_* set (the
  # password is the untouched cube_pass/ceuhvu123). Running the full
  # merge -> alias -> persist pipeline must keep HOST, password, DATABASE_URL and
  # CUBE_PROXY_REDIS_* pointed at the ORIGINAL external address -- NOT silently
  # rewrite them to 127.0.0.1 + the defaults (the pre-fix behaviour).
  local new="${TMP_DIR}/ext_up_new.example" old="${TMP_DIR}/ext_up_old.env"
  local merged="${TMP_DIR}/ext_up_merged.env" diff="${TMP_DIR}/ext_up_diff.txt"
  local runtime="${TMP_DIR}/ext_up_runtime.env"

  # New env.example ships the unified CUBE_SANDBOX_* set at its bundled defaults.
  cat > "${new}" <<'EOF'
# unified deps template
ONE_CLICK_DEPLOY_ROLE=control
CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
# CUBE_SANDBOX_MYSQL_MANAGED=auto
CUBE_SANDBOX_REDIS_HOST=127.0.0.1
CUBE_SANDBOX_REDIS_PORT=6379
CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
# CUBE_SANDBOX_REDIS_MANAGED=auto
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
EOF

  # Old runtime: real endpoint on the legacy set, untouched bundled defaults on
  # the new set (exactly the "real address on CUBE_EXTERNAL_*, cube_pass on
  # CUBE_SANDBOX_*" situation the reviewer described).
  cat > "${old}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
CUBE_EXTERNAL_MYSQL_PORT=3306
CUBE_EXTERNAL_MYSQL_USER=cube
CUBE_EXTERNAL_MYSQL_PASSWORD=realmysqlpass
CUBE_EXTERNAL_MYSQL_DB=cube_mvp
CUBE_EXTERNAL_REDIS_HOST=10.0.0.21
CUBE_EXTERNAL_REDIS_PORT=6379
CUBE_EXTERNAL_REDIS_PASSWORD=realredispass
CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
CUBE_SANDBOX_REDIS_HOST=127.0.0.1
CUBE_SANDBOX_REDIS_PORT=6379
CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${merged}" "${diff}" 2>/dev/null

  # After the merge the unified set already reflects the external endpoint...
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_HOST 10.0.0.20
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_PASSWORD realmysqlpass
  assert_value "${merged}" CUBE_SANDBOX_REDIS_HOST 10.0.0.21
  assert_value "${merged}" CUBE_SANDBOX_REDIS_PASSWORD realredispass
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_MANAGED 0
  assert_value "${merged}" CUBE_SANDBOX_REDIS_MANAGED 0
  # ...and the legacy keys are dropped, not left to linger.
  assert_not_contains "${merged}" "CUBE_EXTERNAL_MYSQL_HOST="
  assert_not_contains "${merged}" "CUBE_EXTERNAL_REDIS_HOST="
  assert_contains "${diff}" "[migrated-external]"

  # Now replay install.sh's remaining steps: source the merged env, apply the
  # alias resolver, and persist the unified config into the runtime env.
  cp -f "${merged}" "${runtime}"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_PORT CUBE_SANDBOX_MYSQL_USER \
      CUBE_SANDBOX_MYSQL_PASSWORD CUBE_SANDBOX_MYSQL_DB CUBE_SANDBOX_MYSQL_MANAGED \
      CUBE_SANDBOX_REDIS_HOST CUBE_SANDBOX_REDIS_PORT CUBE_SANDBOX_REDIS_PASSWORD \
      CUBE_SANDBOX_REDIS_MANAGED \
      CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_MYSQL_PORT CUBE_EXTERNAL_MYSQL_USER \
      CUBE_EXTERNAL_MYSQL_PASSWORD CUBE_EXTERNAL_MYSQL_DB \
      CUBE_EXTERNAL_REDIS_HOST CUBE_EXTERNAL_REDIS_PORT CUBE_EXTERNAL_REDIS_PASSWORD
    set -a
    # shellcheck disable=SC1090
    source "${merged}"
    set +a
    apply_deprecated_external_aliases 2>/dev/null
    persist_unified_dep_config "${runtime}" ""
  )

  # The persisted runtime env must still point at the external address, with the
  # real password threaded through DATABASE_URL and the cube-proxy Redis vars.
  assert_value "${runtime}" CUBE_SANDBOX_MYSQL_HOST 10.0.0.20
  assert_value "${runtime}" CUBE_SANDBOX_MYSQL_MANAGED 0
  assert_value "${runtime}" DATABASE_URL "mysql://cube:realmysqlpass@10.0.0.20:3306/cube_mvp"
  assert_value "${runtime}" CUBE_SANDBOX_REDIS_HOST 10.0.0.21
  assert_value "${runtime}" CUBE_SANDBOX_REDIS_MANAGED 0
  assert_value "${runtime}" CUBE_PROXY_REDIS_IP 10.0.0.21
  assert_value "${runtime}" CUBE_PROXY_REDIS_PORT 6379
  assert_value "${runtime}" CUBE_PROXY_REDIS_PASSWORD realredispass
  # The broken behaviour would have produced these; they must NOT appear.
  [[ "$(get_value "${runtime}" DATABASE_URL)" != *127.0.0.1* ]] \
    || fail "DATABASE_URL still points at 127.0.0.1 after external upgrade"
  [[ "$(get_value "${runtime}" DATABASE_URL)" != *cube_pass* ]] \
    || fail "DATABASE_URL still uses the default password after external upgrade"
  assert_not_contains "${runtime}" "CUBE_EXTERNAL_MYSQL_HOST="
  assert_not_contains "${runtime}" "CUBE_EXTERNAL_REDIS_HOST="
}

test_runtime_external_migration_complete_detection() {
  # runtime_external_migration_complete is the P1 signal: true (0) when the OLD
  # runtime env carries NO active CUBE_EXTERNAL_* key.
  local with_legacy="${TMP_DIR}/mig_with_legacy.env"
  local without_legacy="${TMP_DIR}/mig_without_legacy.env"
  local commented="${TMP_DIR}/mig_commented.env"
  cat > "${with_legacy}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.99
CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
EOF
  cat > "${without_legacy}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.99
CUBE_SANDBOX_MYSQL_MANAGED=0
EOF
  # A commented legacy line must NOT count as still-configured.
  cat > "${commented}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.99
# CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
EOF

  if runtime_external_migration_complete "${with_legacy}"; then
    fail "runtime env with active CUBE_EXTERNAL_* must be reported as NOT migrated"
  fi
  runtime_external_migration_complete "${without_legacy}" \
    || fail "runtime env without CUBE_EXTERNAL_* must be reported as migrated"
  runtime_external_migration_complete "${commented}" \
    || fail "commented CUBE_EXTERNAL_* must not count as still-configured"
  runtime_external_migration_complete "${TMP_DIR}/does_not_exist.env" \
    || fail "missing runtime env must count as migrated (nothing legacy to migrate)"
}

test_second_upgrade_ignores_leftover_legacy_env() {
  # P1 regression (fslongjin second review): a deployment already migrated to the
  # unified vars must NOT be rolled back to a stale endpoint by leftover
  # CUBE_EXTERNAL_* the operator forgot to delete from their .env. When the OLD
  # runtime env has no CUBE_EXTERNAL_* (migration complete), install.sh sets
  # ONE_CLICK_EXTERNAL_MIGRATION_COMPLETE=1 and the leftover legacy vars must be
  # DISCARDED, not applied -- the freshly merged unified endpoint must win.
  local err="${TMP_DIR}/second_upgrade.err"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_PORT CUBE_SANDBOX_MYSQL_USER CUBE_SANDBOX_MYSQL_DB \
      CUBE_SANDBOX_REDIS_HOST CUBE_SANDBOX_REDIS_PORT CUBE_SANDBOX_REDIS_PASSWORD
    # The merged unified config already points at the new endpoint...
    export CUBE_SANDBOX_MYSQL_HOST=10.0.0.99
    export CUBE_SANDBOX_MYSQL_PASSWORD=newpass
    export CUBE_SANDBOX_MYSQL_MANAGED=0
    # ...but stale legacy vars linger in the process env (from the operator .env).
    export CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
    export CUBE_EXTERNAL_MYSQL_PASSWORD=oldpass
    # Migration already complete on a prior install.
    export ONE_CLICK_EXTERNAL_MIGRATION_COMPLETE=1
    apply_deprecated_external_aliases 2>/dev/null
    [[ "${CUBE_SANDBOX_MYSQL_HOST}" == "10.0.0.99" ]] \
      || { echo "HOST rolled back to ${CUBE_SANDBOX_MYSQL_HOST}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_PASSWORD}" == "newpass" ]] \
      || { echo "PASSWORD rolled back to ${CUBE_SANDBOX_MYSQL_PASSWORD}" >&2; exit 1; }
    # Leftover legacy vars must be cleared so they cannot re-apply downstream.
    [[ -z "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]] \
      || { echo "leftover legacy host not cleared" >&2; exit 1; }
    [[ -z "${CUBE_EXTERNAL_MYSQL_PASSWORD:-}" ]] \
      || { echo "leftover legacy password not cleared" >&2; exit 1; }
  ) 2>"${err}" || fail "second upgrade must ignore leftover legacy vars: $(cat "${err}")"

  # And the operator gets a distinct warning that the stale lines were discarded.
  local warn="${TMP_DIR}/second_upgrade_warn.err"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    export CUBE_SANDBOX_MYSQL_HOST=10.0.0.99
    export CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
    export ONE_CLICK_EXTERNAL_MIGRATION_COMPLETE=1
    apply_deprecated_external_aliases
  ) 2>"${warn}" || true
  assert_contains "${warn}" "already migrated"
}

test_persist_preserves_custom_runtime_database_url() {
  # P2 regression (fslongjin second review): in external mode, a custom runtime
  # DATABASE_URL (extra query params) with no explicit operator DSN must be
  # PRESERVED verbatim, not silently rebuilt from CUBE_SANDBOX_MYSQL_* (which
  # would drop the params) and NOT double-quoted by a read-then-re-upsert.
  local out="${TMP_DIR}/persist_custom_runtime_dburl.env"
  local custom_dsn='mysql://cube:realpass@10.0.0.20:3306/cube_mvp?parseTime=true&charset=utf8mb4'
  : > "${out}"
  # Seed the runtime env exactly as a prior install would: upsert_env_kv quotes
  # the value because it carries shell metacharacters (? &). This is the on-disk
  # form the next upgrade actually sees.
  upsert_env_kv "${out}" DATABASE_URL "${custom_dsn}"
  local seeded_line
  seeded_line="$(get_value "${out}" DATABASE_URL)"
  [[ "${seeded_line}" == "\"${custom_dsn}\"" ]] \
    || fail "test setup: expected quoted seed, got ${seeded_line}"

  local warn="${TMP_DIR}/persist_custom_runtime_dburl.err"
  (
    export CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=realpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    # No operator .env DSN -> the custom runtime value must be preserved.
    persist_unified_dep_config "${out}" ""
  ) 2>"${warn}"
  # The line must be preserved byte-for-byte (still single-quoted layer, NOT
  # re-quoted into \"...\"). Sourcing it must yield the original DSN.
  assert_value "${out}" DATABASE_URL "\"${custom_dsn}\""
  local sourced
  sourced="$(set -a; # shellcheck disable=SC1090
    source "${out}"; printf '%s' "${DATABASE_URL}")"
  [[ "${sourced}" == "${custom_dsn}" ]] \
    || fail "sourced DATABASE_URL corrupted: got '${sourced}'"
  assert_contains "${warn}" "custom DATABASE_URL"
}

test_persist_rebuilds_plain_runtime_database_url() {
  # Complement to the P2 test: a PLAIN auto-generated runtime DATABASE_URL (no
  # query/fragment) must NOT be preserved -- it is rebuilt from CUBE_SANDBOX_MYSQL_*
  # so a credential change still propagates on upgrade.
  local out="${TMP_DIR}/persist_plain_runtime_dburl.env"
  cat > "${out}" <<'EOF'
DATABASE_URL=mysql://cube:oldpass@10.0.0.20:3306/cube_mvp
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=newpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  )
  assert_value "${out}" DATABASE_URL "mysql://cube:newpass@10.0.0.20:3306/cube_mvp"
}

test_persist_discards_stale_loopback_runtime_database_url_when_host_remote() {
  # Review finding (2026-08-04): the runtime-DSN preserve heuristic must not keep a
  # loopback-hosted custom DSN when MySQL now resolves at a remote host. An
  # operator who moved a loopback-external deployment onto a remote server (updated
  # CUBE_SANDBOX_MYSQL_HOST, no explicit .env DATABASE_URL) but whose prior install
  # persisted a query-bearing loopback DSN into the runtime env would otherwise
  # keep pointing CubeAPI at a dead 127.0.0.1. The stale loopback DSN must be
  # discarded and rebuilt from CUBE_SANDBOX_MYSQL_* (dropping the query params is
  # acceptable -- the endpoint was unreachable anyway).
  local out="${TMP_DIR}/persist_stale_loop_runtime.env"
  local custom_dsn='mysql://cube:realpass@127.0.0.1:3306/cube_mvp?parseTime=true&charset=utf8mb4'
  : > "${out}"
  upsert_env_kv "${out}" DATABASE_URL "${custom_dsn}"
  local warn="${TMP_DIR}/persist_stale_loop_runtime.err"
  (
    export CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=realpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" ""
  ) 2>"${warn}"
  # Rebuilt at the remote host, NOT the preserved loopback DSN.
  assert_value "${out}" DATABASE_URL "mysql://cube:realpass@10.0.0.20:3306/cube_mvp"
  assert_contains "${warn}" "discarding the loopback DATABASE_URL"

  # Complement: a loopback DSN kept in loopback-external mode (host stays local)
  # must still be PRESERVED verbatim -- the discard is scoped to a remote resolve.
  local out2="${TMP_DIR}/persist_loop_runtime_kept.env"
  : > "${out2}"
  upsert_env_kv "${out2}" DATABASE_URL "${custom_dsn}"
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=realpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_MYSQL_MANAGED=0
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out2}" ""
  ) 2>/dev/null
  assert_value "${out2}" DATABASE_URL "\"${custom_dsn}\""
}

test_external_alias_map_matches_shell_array() {
  # Guard the "KEEP IN SYNC" contract between the shell array
  # ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS (scripts/common/validation.sh, the
  # canonical old:new definition) and the Python EXTERNAL_ALIAS_MAP embedded in
  # merge_env_three_way's heredoc (lib/common.sh). If one side gains/loses a
  # legacy key without the other, the upgrade merge either misses a key (dropping
  # the operator's custom endpoint) or fails to strip a stale key from disk.
  # This test fails loudly on any divergence.
  local shell_pairs="${TMP_DIR}/alias_shell_pairs.txt"
  local py_pairs="${TMP_DIR}/alias_py_pairs.txt"

  # The shell array is already sourced (via lib/common.sh -> validation.sh).
  printf '%s\n' "${ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS[@]}" | sort > "${shell_pairs}"

  # Extract the Python tuple entries from lib/common.sh and render them in the
  # same old:new form. Anchored to the EXTERNAL_ALIAS_MAP = ( ... ) block so
  # unrelated ("a", "b") tuples elsewhere cannot leak in.
  python3 - "${ONE_CLICK_DIR}/lib/common.sh" > "${py_pairs}" <<'PY'
import re, sys
src = open(sys.argv[1], encoding="utf-8").read()
# Non-greedy up to the closing paren that sits alone at the start of a line
# (lib/common.sh formats the map with ")" on its own line). A bare "\)" would
# stop at the first ")" *inside* a tuple and truncate the capture.
m = re.search(r"EXTERNAL_ALIAS_MAP\s*=\s*\(\n(.*?)\n\)", src, re.S)
if not m:
    sys.stderr.write("EXTERNAL_ALIAS_MAP not found in lib/common.sh\n")
    sys.exit(1)
pairs = re.findall(r'\(\s*"([^"]+)"\s*,\s*"([^"]+)"\s*\)', m.group(1))
for line in sorted("%s:%s" % (o, n) for o, n in pairs):
    print(line)
PY

  if ! diff -u "${shell_pairs}" "${py_pairs}" >/dev/null 2>&1; then
    echo "--- shell (ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS) vs python (EXTERNAL_ALIAS_MAP) ---" >&2
    diff -u "${shell_pairs}" "${py_pairs}" >&2 || true
    fail "EXTERNAL_ALIAS_MAP (lib/common.sh) is out of sync with ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS (validation.sh)"
  fi
  # Sanity: both sides are non-empty (a bad regex returning nothing must not pass).
  [[ -s "${shell_pairs}" ]] || fail "shell alias pairs came back empty"
  [[ -s "${py_pairs}" ]] || fail "python alias pairs came back empty"
}

test_version_lt() {
  version_lt 1.0.0 2.0.0 || fail "1.0.0 < 2.0.0 should be true"
  version_lt v0.2.2 v0.2.3 || fail "v0.2.2 < v0.2.3 should be true"
  ! version_lt 2.0.0 1.0.0 || fail "2.0.0 < 1.0.0 should be false"
  ! version_lt 1.2.3 1.2.3 || fail "equal versions should not be <"
  # non-semver / SHA-like inputs are intentionally not comparable by version_lt:
  # the upgrade downgrade guard must not block on legacy labels.
  ! version_lt a1b2c3d e5f6a7b || fail "git SHA-like versions should not compare as <"
  ! version_lt 1.0.0 a1b2c3d || fail "mixed semver/SHA-like inputs should not compare as <"
}

test_diff_report_redacts_secrets() {
  local new="${TMP_DIR}/new_sec.example" old="${TMP_DIR}/old_sec.env"
  local out="${TMP_DIR}/out_sec.env" diff="${TMP_DIR}/diff_sec.txt"
  write_new_example "${new}"
  # User customized the secret values -> they appear in [preserved]/[kept-extra].
  # Use neutral secret-bearing var names (matched by the redaction regex via
  # API_KEY/TOKEN) that are NOT in the obsolete deny-list, so they are preserved.
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_REDIS_PASSWORD=topsecret123
DATABASE_URL=mysql://u:p@host:3306/realdb
CUSTOM_THIRD_PARTY_API_KEY=sk-custom-secret
MY_SERVICE_AUTH_TOKEN=tok-openclaw-secret
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  # The diff report must NOT leak plaintext secrets...
  assert_contains "${diff}" "CUBE_SANDBOX_REDIS_PASSWORD=***REDACTED***"
  assert_contains "${diff}" "DATABASE_URL=***REDACTED***"
  assert_contains "${diff}" "CUSTOM_THIRD_PARTY_API_KEY=***REDACTED***"
  assert_contains "${diff}" "MY_SERVICE_AUTH_TOKEN=***REDACTED***"
  assert_not_contains "${diff}" "topsecret123"
  assert_not_contains "${diff}" "realdb"
  assert_not_contains "${diff}" "sk-custom-secret"
  assert_not_contains "${diff}" "tok-openclaw-secret"

  # ...but the merged runtime env MUST keep the real values intact.
  assert_value "${out}" CUBE_SANDBOX_REDIS_PASSWORD topsecret123
  assert_value "${out}" DATABASE_URL "mysql://u:p@host:3306/realdb"
  assert_value "${out}" CUSTOM_THIRD_PARTY_API_KEY "sk-custom-secret"
  assert_value "${out}" MY_SERVICE_AUTH_TOKEN "tok-openclaw-secret"
}

test_drops_obsolete_agenthub_keys() {
  local new="${TMP_DIR}/new_obs.example" old="${TMP_DIR}/old_obs.env"
  local out="${TMP_DIR}/out_obs.env" diff="${TMP_DIR}/diff_obs.txt"
  write_new_example "${new}"
  # Old runtime carries the now-obsolete AgentHub LLM env vars plus a legit
  # custom key. Only the obsolete ones must be dropped.
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_PORT=3306
AGENTHUB_DEEPSEEK_API_KEY=sk-agenthub-secret
OPENCLAW_DEEPSEEK_API_KEY=sk-openclaw-secret
AGENTHUB_LLM_API_KEY=sk-llm-secret
OPENCLAW_LLM_API_KEY=sk-openclaw-llm-secret
AGENTHUB_LLM_PROVIDER=deepseek
OPENCLAW_LLM_PROVIDER=openai
AGENTHUB_LLM_BASE_URL=https://api.example.com
OPENCLAW_LLM_BASE_URL=https://api.openclaw.example.com
AGENTHUB_LLM_MODEL=custom-model
OPENCLAW_DEFAULT_MODEL=deepseek/deepseek-v4-flash
AGENTHUB_LLM_CREDENTIAL_MODE=egress
AGENTHUB_SECRET_KEY=base64key
CUBE_API_DATABASE_URL=mysql://old:pass@host:3306/db
ONE_CLICK_INSTALL_PREFIX=/opt/cube
ONE_CLICK_TOOLBOX_ROOT=/opt/cube
MY_CUSTOM_KEEP=stays
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  # All DEPRECATED_KEYS must be removed from the merged runtime env.
  for k in \
    AGENTHUB_DEEPSEEK_API_KEY OPENCLAW_DEEPSEEK_API_KEY \
    AGENTHUB_LLM_API_KEY OPENCLAW_LLM_API_KEY \
    AGENTHUB_LLM_PROVIDER OPENCLAW_LLM_PROVIDER \
    AGENTHUB_LLM_BASE_URL OPENCLAW_LLM_BASE_URL \
    AGENTHUB_LLM_MODEL OPENCLAW_DEFAULT_MODEL \
    AGENTHUB_LLM_CREDENTIAL_MODE \
    AGENTHUB_SECRET_KEY CUBE_API_DATABASE_URL \
    ONE_CLICK_INSTALL_PREFIX ONE_CLICK_TOOLBOX_ROOT; do
    if grep -q "^${k}=" "${out}"; then
      fail "obsolete key ${k} should have been dropped from ${out}"
    fi
  done
  assert_not_contains "${out}" "sk-agenthub-secret"
  assert_not_contains "${out}" "sk-llm-secret"
  # The report records the drops without leaking secrets.
  assert_contains "${diff}" "[dropped] obsolete keys removed on upgrade:"
  assert_not_contains "${diff}" "sk-agenthub-secret"
  # A non-obsolete custom key is still preserved verbatim.
  assert_value "${out}" MY_CUSTOM_KEEP stays
}

test_migrates_custom_cube_proxy_image_tag() {
  local new="${TMP_DIR}/new_proxy_img.example" old="${TMP_DIR}/old_proxy_img.env"
  local out="${TMP_DIR}/out_proxy_img.env" diff="${TMP_DIR}/diff_proxy_img.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_PROXY_IMAGE_TAG=my.reg/cube-proxy:custom
CUBE_PROXY_BASE_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
CUBE_SANDBOX_MYSQL_PORT=3306
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_SANDBOX_CUBE_PROXY_IMAGE "my.reg/cube-proxy:custom"
  if grep -q "^CUBE_PROXY_IMAGE_TAG=" "${out}"; then
    fail "CUBE_PROXY_IMAGE_TAG should have been dropped after migration"
  fi
  if grep -q "^CUBE_PROXY_BASE_IMAGE=" "${out}"; then
    fail "CUBE_PROXY_BASE_IMAGE should have been dropped on upgrade"
  fi
  assert_contains "${diff}" "[migrated-legacy]"
  assert_contains "${diff}" "CUBE_PROXY_IMAGE_TAG: my.reg/cube-proxy:custom -> CUBE_SANDBOX_CUBE_PROXY_IMAGE=my.reg/cube-proxy:custom"
  assert_contains "${diff}" "[dropped]"
}

test_drops_default_cube_proxy_image_tag_without_migration() {
  local new="${TMP_DIR}/new_proxy_def.example" old="${TMP_DIR}/old_proxy_def.env"
  local out="${TMP_DIR}/out_proxy_def.env" diff="${TMP_DIR}/diff_proxy_def.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_PROXY_IMAGE_TAG=cube-proxy:one-click
CUBE_PROXY_BASE_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
CUBE_SANDBOX_MYSQL_PORT=3306
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  if grep -q "^CUBE_PROXY_IMAGE_TAG=" "${out}"; then
    fail "default CUBE_PROXY_IMAGE_TAG should have been dropped"
  fi
  if grep -q "^CUBE_PROXY_BASE_IMAGE=" "${out}"; then
    fail "CUBE_PROXY_BASE_IMAGE should have been dropped"
  fi
  if grep -q "^CUBE_SANDBOX_CUBE_PROXY_IMAGE=" "${out}"; then
    fail "default cube-proxy:one-click must not migrate to CUBE_SANDBOX_CUBE_PROXY_IMAGE"
  fi
  assert_contains "${diff}" "[dropped]"
}

test_keeps_existing_cube_sandbox_cube_proxy_image_over_legacy_tag() {
  local new="${TMP_DIR}/new_proxy_keep.example" old="${TMP_DIR}/old_proxy_keep.env"
  local out="${TMP_DIR}/out_proxy_keep.env" diff="${TMP_DIR}/diff_proxy_keep.txt"
  write_new_example "${new}"
  cat > "${old}" <<'EOF'
CUBE_PROXY_IMAGE_TAG=my.reg/cube-proxy:stale
CUBE_SANDBOX_CUBE_PROXY_IMAGE=my.reg/cube-proxy:already-set
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  assert_value "${out}" CUBE_SANDBOX_CUBE_PROXY_IMAGE "my.reg/cube-proxy:already-set"
  if grep -q "^CUBE_PROXY_IMAGE_TAG=" "${out}"; then
    fail "CUBE_PROXY_IMAGE_TAG should have been dropped"
  fi
  # Must not rewrite an already-present override from the stale IMAGE_TAG.
  assert_not_contains "${diff}" "CUBE_PROXY_IMAGE_TAG: my.reg/cube-proxy:stale ->"
}

test_non_utf8_env_fails_cleanly() {
  local new="${TMP_DIR}/new_bad_utf8.example" old="${TMP_DIR}/old_bad_utf8.env"
  local out="${TMP_DIR}/out_bad_utf8.env" diff="${TMP_DIR}/diff_bad_utf8.txt" err="${TMP_DIR}/bad_utf8.err"
  write_new_example "${new}"
  printf 'CUBE_SANDBOX_MYSQL_PORT=3307\nBAD=\xff\n' > "${old}"

  if merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>"${err}"; then
    fail "merge_env_three_way should reject non-UTF-8 input"
  fi
  assert_contains "${err}" "env merge input is not valid UTF-8"
  assert_contains "${err}" "${old}"
  [[ ! -e "${out}" || ! -s "${out}" ]] || fail "merged env should not be written for invalid UTF-8 input"
}

test_read_helpers_reject_invalid_key() {
  local f="${TMP_DIR}/inv.env"
  cat > "${f}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
EOF
  if ( read_env_key "${f}" 'bad/key' ) >/dev/null 2>&1; then
    fail "read_env_key should reject an invalid key name"
  fi
  if ( read_version_field "${f}" 'bad.field' ) >/dev/null 2>&1; then
    fail "read_version_field should reject an invalid field name"
  fi
}

test_read_helpers() {
  local f="${TMP_DIR}/ver.txt"
  cat > "${f}" <<'EOF'
release_version=v0.5.0
git_commit=abc123
EOF
  [[ "$(read_version_field "${f}" release_version)" == "v0.5.0" ]] || fail "read_version_field"
  [[ "$(read_version_field "${f}" missing)" == "" ]] || fail "read_version_field missing"

  local e="${TMP_DIR}/role.env"
  cat > "${e}" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=compute
EOF
  [[ "$(read_env_key "${e}" ONE_CLICK_DEPLOY_ROLE)" == "compute" ]] || fail "read_env_key"
}

test_detect_existing_install() {
  local d="${TMP_DIR}/inst"
  mkdir -p "${d}"
  ! detect_existing_install "${d}" || fail "should not detect install without .one-click.env"
  : > "${d}/.one-click.env"
  detect_existing_install "${d}" || fail "should detect install with .one-click.env"
}

test_crlf_inputs_do_not_leak_carriage_returns() {
  local new="${TMP_DIR}/new10.example" old="${TMP_DIR}/old10.env"
  local out="${TMP_DIR}/out10.env" diff="${TMP_DIR}/diff10.txt"
  write_new_example "${new}"
  # old runtime written with CRLF line endings
  printf 'CUBE_SANDBOX_MYSQL_PORT=3307\r\nCUSTOM_ONLY=keepme\r\n' > "${old}"

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  # No carriage returns should survive into the merged output
  if grep -q $'\r' "${out}"; then
    fail "merged output contains carriage returns"
  fi
  assert_value "${out}" CUBE_SANDBOX_MYSQL_PORT 3307
  assert_value "${out}" CUSTOM_ONLY keepme
}

test_cidr_preflight_skip_conflict_param() {
  # skip=1 must still enforce format validation (invalid format -> die)
  if ( check_cidr_preflight "not-a-cidr" 1 ) >/dev/null 2>&1; then
    fail "invalid CIDR should fail format validation even with skip=1"
  fi
  # misaligned network address -> die even with skip=1
  if ( check_cidr_preflight "10.0.0.5/16" 1 ) >/dev/null 2>&1; then
    fail "misaligned CIDR should fail even with skip=1"
  fi
  # valid + skip=1 -> passes without touching host interfaces/routes
  ( check_cidr_preflight "10.123.0.0/16" 1 ) >/dev/null 2>&1 \
    || fail "valid CIDR with skip=1 should pass"
}

# --- _host_is_loopback: loopback classification (validation.sh) ---
# Guards the bundled-vs-external default. Run in a subshell since it only reads
# its argument; assertions cover the special-name arms, the 127.0.0.0/8 range,
# the octet range-check, and the "127."-prefixed hostname false-positive trap.
test_host_is_loopback_classification() {
  # Loopback: empty, the special names, and any in-range 127.x.y.z literal.
  local h
  for h in "" localhost ::1 0:0:0:0:0:0:0:1 127.0.0.1 127.0.0.2 127.1.2.3 127.255.255.255; do
    _host_is_loopback "${h}" || fail "expected '${h}' to be loopback"
  done
  # Non-loopback: real remote addresses, an out-of-range octet, a "127."-prefixed
  # hostname (not a dotted-quad), and an IPv6 form other than the two accepted.
  for h in 10.0.0.20 db.example.com 128.0.0.1 126.0.0.1 127.999.0.1 127.0.0 127.0.0.1.5 127.foo.example.com "::2"; do
    if _host_is_loopback "${h}"; then
      fail "expected '${h}' NOT to be loopback"
    fi
  done
}

# --- _dep_is_managed: MANAGED resolution across all accepted spellings ---
# _dep_is_managed calls die() on error, so exercise the failure arms in a
# subshell and key off the exit status; success arms are asserted by return code.
test_dep_is_managed_string_aliases() {
  # auto / unset -> managed iff host is loopback.
  _dep_is_managed "" 127.0.0.1 T || fail "auto+loopback should be managed"
  _dep_is_managed auto 127.0.0.1 T || fail "auto+loopback should be managed"
  if _dep_is_managed auto db.example.com T; then fail "auto+remote should be unmanaged"; fi

  # Truthy spellings force managed; require a loopback host (asserted here).
  local v
  for v in 1 true yes on TRUE Yes On; do
    _dep_is_managed "${v}" 127.0.0.1 T || fail "MANAGED=${v}+loopback should be managed"
  done
  # Falsy spellings force unmanaged regardless of host.
  for v in 0 false no off FALSE No Off; do
    if _dep_is_managed "${v}" 127.0.0.1 T; then fail "MANAGED=${v} should be unmanaged"; fi
  done

  # Truthy + non-loopback host is a hard error (die -> non-zero in a subshell).
  if ( _dep_is_managed 1 db.example.com T ) >/dev/null 2>&1; then
    fail "MANAGED=1 with a remote host must fail"
  fi
  # An unrecognized spelling is a hard error.
  if ( _dep_is_managed maybe 127.0.0.1 T ) >/dev/null 2>&1; then
    fail "an invalid MANAGED spelling must fail"
  fi
}

# --- dsn_component_has_metachar: shared DATABASE_URL injection guard ---
# The single-source helper (validation.sh) used by up.sh and cube-api-start.sh
# to refuse assembling a mysql:// DSN from a credential carrying a URL/shell
# metacharacter. Cover the clean-component pass and every guarded character,
# including whitespace matched via [:space:] (the arm most easily broken by a
# well-meaning "backslash cleanup").
test_dsn_component_has_metachar() {
  # Clean components (the bundled defaults plus a benign custom password) must
  # NOT be flagged.
  local c
  for c in cube cube_pass 127.0.0.1 3306 cube_mvp "P4ssw0rd-ok.value"; do
    if dsn_component_has_metachar "${c}"; then
      fail "expected clean component '${c}' to pass the DSN metachar guard"
    fi
  done
  # Each guarded character must be flagged. Whitespace is covered by a literal
  # space and a tab (both must match via [:space:]).
  local bad
  for bad in 'a@b' 'a:b' 'a/b' 'a#b' 'a%b' 'a"b' 'a`b' 'a$b' "a'b" 'a\b' 'a b' "$(printf 'a\tb')"; do
    if ! dsn_component_has_metachar "${bad}"; then
      fail "expected component '${bad}' to be flagged by the DSN metachar guard"
    fi
  done
}

# --- remove_env_kv: delete active key, no-op when absent, preserve 0600 ---
test_remove_env_kv_deletes_and_preserves_mode() {
  local f="${TMP_DIR}/remove_kv.env"
  cat > "${f}" <<'EOF'
KEEP_ME=1
CUBE_EXTERNAL_MYSQL_PASSWORD=legacysecret
ALSO_KEEP=2
EOF
  chmod 0600 "${f}"

  remove_env_kv "${f}" CUBE_EXTERNAL_MYSQL_PASSWORD
  assert_not_contains "${f}" "CUBE_EXTERNAL_MYSQL_PASSWORD="
  assert_not_contains "${f}" "legacysecret"
  assert_value "${f}" KEEP_ME 1
  assert_value "${f}" ALSO_KEEP 2
  [[ "$(stat -c '%a' "${f}")" == "600" ]] || fail "remove_env_kv must preserve 0600 mode"
}

test_remove_env_kv_noop_when_key_absent() {
  local f="${TMP_DIR}/remove_kv_noop.env"
  cat > "${f}" <<'EOF'
KEEP_ME=1
ALSO_KEEP=2
EOF
  chmod 0600 "${f}"
  local before
  before="$(cat "${f}")"

  remove_env_kv "${f}" CUBE_EXTERNAL_MYSQL_PASSWORD
  [[ "$(cat "${f}")" == "${before}" ]] || fail "remove_env_kv must be a no-op when the key is absent"
  [[ "$(stat -c '%a' "${f}")" == "600" ]] || fail "no-op remove_env_kv must preserve 0600 mode"
}

test_remove_env_kv_deletes_multiple_keys_in_one_pass() {
  local f="${TMP_DIR}/remove_kv_multi.env"
  cat > "${f}" <<'EOF'
KEEP_ME=1
CUBE_EXTERNAL_MYSQL_HOST=10.0.0.1
CUBE_EXTERNAL_MYSQL_PASSWORD=legacysecret
ALSO_KEEP=2
CUBE_EXTERNAL_REDIS_HOST=10.0.0.2
EOF
  chmod 0600 "${f}"

  remove_env_kv "${f}" CUBE_EXTERNAL_MYSQL_HOST CUBE_EXTERNAL_MYSQL_PASSWORD CUBE_EXTERNAL_REDIS_HOST
  assert_not_contains "${f}" "CUBE_EXTERNAL_MYSQL_HOST="
  assert_not_contains "${f}" "CUBE_EXTERNAL_MYSQL_PASSWORD="
  assert_not_contains "${f}" "CUBE_EXTERNAL_REDIS_HOST="
  assert_not_contains "${f}" "legacysecret"
  assert_value "${f}" KEEP_ME 1
  assert_value "${f}" ALSO_KEEP 2
  [[ "$(stat -c '%a' "${f}")" == "600" ]] || fail "multi-key remove_env_kv must preserve 0600 mode"
}

test_remove_env_kv_deletes_indented_and_crlf_lines() {
  local f="${TMP_DIR}/remove_kv_crlf.env"
  printf 'KEEP_ME=1\r\n  CUBE_EXTERNAL_MYSQL_PASSWORD=secret\r\nALSO_KEEP=2\r\n' > "${f}"
  chmod 0600 "${f}"

  remove_env_kv "${f}" CUBE_EXTERNAL_MYSQL_PASSWORD
  assert_not_contains "${f}" "CUBE_EXTERNAL_MYSQL_PASSWORD"
  assert_not_contains "${f}" "secret"
  assert_value "${f}" KEEP_ME 1
  assert_value "${f}" ALSO_KEEP 2
  # Surviving lines must be CR-stripped so the file has consistent LF endings.
  if grep -q $'\r' "${f}"; then
    fail "remove_env_kv must strip trailing CR from surviving lines"
  fi
}

test_remove_env_kv_noop_when_no_keys_given() {
  local f="${TMP_DIR}/remove_kv_nokeys.env"
  cat > "${f}" <<'EOF'
KEEP_ME=1
ALSO_KEEP=2
EOF
  chmod 0600 "${f}"
  local before
  before="$(cat "${f}")"

  remove_env_kv "${f}"
  [[ "$(cat "${f}")" == "${before}" ]] || fail "remove_env_kv must be a no-op when no keys are given"
}

test_merge_flips_external_to_bundled_with_managed_1_env_host() {
  # Review finding 1 (primary path): the MANAGED=1 HOST reset must not be defeated
  # by a stale non-loopback CUBE_SANDBOX_MYSQL_HOST left in the operator .env. An
  # operator who followed the unified docs (HOST=10.0.0.20 in .env) then flips to
  # bundled by adding MANAGED=1 must end up with a self-consistent MANAGED=1 +
  # loopback HOST -- the template loop must NOT re-apply the .env HOST as an
  # override.
  local new="${TMP_DIR}/flip2_new.example" old="${TMP_DIR}/flip2_old.env"
  local dotenv="${TMP_DIR}/flip2_dotenv.env"
  local merged="${TMP_DIR}/flip2_merged.env" diff="${TMP_DIR}/flip2_diff.txt"
  cat > "${new}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
# CUBE_SANDBOX_MYSQL_MANAGED=auto
EOF
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_MYSQL_USER=cube
CUBE_SANDBOX_MYSQL_PASSWORD=realpass
CUBE_SANDBOX_MYSQL_DB=cube_mvp
CUBE_SANDBOX_MYSQL_MANAGED=0
EOF
  # Operator .env carries BOTH the stale remote HOST and the flip to MANAGED=1.
  cat > "${dotenv}" <<'EOF'
CUBE_SANDBOX_MYSQL_HOST=10.0.0.20
CUBE_SANDBOX_MYSQL_MANAGED=1
EOF
  merge_env_three_way "${new}" "${old}" "" "${dotenv}" "${merged}" "${diff}" 2>/dev/null
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_MANAGED 1
  # HOST reset to the loopback default DESPITE the explicit .env override.
  assert_value "${merged}" CUBE_SANDBOX_MYSQL_HOST 127.0.0.1
  (
    unset CUBE_SANDBOX_MYSQL_HOST CUBE_SANDBOX_MYSQL_MANAGED
    set -a
    # shellcheck disable=SC1090
    source "${merged}"
    set +a
    mysql_is_managed || { echo "expected mysql managed" >&2; exit 1; }
  ) || fail "MANAGED=1 flip must reset a stale .env HOST and resolve without dying"
}

test_aliases_reset_host_when_force_managed_with_legacy_host() {
  # Review finding 1 (first-migration alias path): on a true first migration the
  # alias loop maps a still-set legacy CUBE_EXTERNAL_MYSQL_HOST onto
  # CUBE_SANDBOX_MYSQL_HOST -- overwriting the loopback reset merge applied for a
  # MANAGED=1 flip. If the operator force-set MANAGED=1, the mapped non-loopback
  # host must be reset to the bundled default so MANAGED=1 stays self-consistent
  # and mysql_is_managed does not die.
  local err="${TMP_DIR}/aliases_force_reset.err"
  (
    unset ONE_CLICK_EXTERNAL_ALIASES_APPLIED
    unset CUBE_SANDBOX_MYSQL_HOST
    export CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
    export CUBE_SANDBOX_MYSQL_MANAGED=1
    apply_deprecated_external_aliases 2>/dev/null
    [[ "${CUBE_SANDBOX_MYSQL_MANAGED}" == "1" ]] \
      || { echo "expected MANAGED=1 preserved, got ${CUBE_SANDBOX_MYSQL_MANAGED}" >&2; exit 1; }
    [[ "${CUBE_SANDBOX_MYSQL_HOST}" == "127.0.0.1" ]] \
      || { echo "expected HOST reset to 127.0.0.1, got ${CUBE_SANDBOX_MYSQL_HOST}" >&2; exit 1; }
    mysql_is_managed || { echo "expected mysql managed after reset" >&2; exit 1; }
  ) 2>"${err}" || fail "force-managed + legacy host must reset HOST: $(cat "${err}")"
}

test_persist_explicit_database_url_with_inline_comment() {
  # Review finding 2: an explicit DATABASE_URL with a trailing inline comment (or
  # quotes + comment) must be persisted as the shell would source it -- the
  # comment stripped, the value clean -- so the installer and a later
  # `source .one-click.env` agree. Previously the raw RHS (comment included) was
  # re-quoted, corrupting the DSN.
  local out="${TMP_DIR}/persist_dburl_comment.env"
  local operator="${TMP_DIR}/persist_dburl_comment_op.env"
  local want='mysql://opuser:oppass@opdb.example.com:3306/mydb?parseTime=true'
  : > "${out}"
  cat > "${operator}" <<EOF
DATABASE_URL="${want}"  # prod
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=db.example.com
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" "${operator}"
  )
  local sourced
  sourced="$(set -a; # shellcheck disable=SC1090
    source "${out}"; printf '%s' "${DATABASE_URL}")"
  [[ "${sourced}" == "${want}" ]] \
    || fail "quoted DATABASE_URL + inline comment corrupted on persist: got '${sourced}'"

  # Bare (unquoted) value with a trailing inline comment.
  local out2="${TMP_DIR}/persist_dburl_comment2.env"
  local operator2="${TMP_DIR}/persist_dburl_comment2_op.env"
  local want2='mysql://opuser:oppass@opdb.example.com:3306/mydb'
  : > "${out2}"
  cat > "${operator2}" <<EOF
DATABASE_URL=${want2}  # legacy note
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=db.example.com
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=cube_pass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out2}" "${operator2}"
  )
  sourced="$(set -a; # shellcheck disable=SC1090
    source "${out2}"; printf '%s' "${DATABASE_URL}")"
  [[ "${sourced}" == "${want2}" ]] \
    || fail "bare DATABASE_URL + inline comment corrupted on persist: got '${sourced}'"
}

test_persist_discards_stale_bundled_default_on_loopback_external() {
  # Review finding 3: in loopback-external mode (HOST=127.0.0.1 + MANAGED=0), a
  # leftover bundled-default DATABASE_URL (env.example shipped
  # `mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp` active for years) must NOT be
  # honoured when the operator configured a custom CUBE_SANDBOX_MYSQL_PASSWORD --
  # it would silently point CubeAPI at the wrong (default) credentials. It is
  # discarded and DATABASE_URL is rebuilt from CUBE_SANDBOX_MYSQL_*.
  local out="${TMP_DIR}/persist_dburl_stale_loop_ext.env"
  local operator="${TMP_DIR}/persist_dburl_stale_loop_ext_op.env"
  local warn="${TMP_DIR}/persist_dburl_stale_loop_ext.err"
  : > "${out}"
  cat > "${operator}" <<'EOF'
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=realpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_MYSQL_MANAGED=0
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out}" "${operator}"
  ) 2>"${warn}"
  # Rebuilt from the unified vars -> the custom password, NOT the stale default.
  assert_value "${out}" DATABASE_URL "mysql://cube:realpass@127.0.0.1:3306/cube_mvp"
  assert_contains "${warn}" "bundled default credentials"

  # A deliberate loopback DSN with NON-default credentials must be preserved.
  local out2="${TMP_DIR}/persist_dburl_loop_custom.env"
  local operator2="${TMP_DIR}/persist_dburl_loop_custom_op.env"
  : > "${out2}"
  cat > "${operator2}" <<'EOF'
DATABASE_URL=mysql://myuser:mypass@127.0.0.1:3306/mydb
EOF
  (
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    export CUBE_SANDBOX_MYSQL_PORT=3306
    export CUBE_SANDBOX_MYSQL_USER=cube
    export CUBE_SANDBOX_MYSQL_PASSWORD=realpass
    export CUBE_SANDBOX_MYSQL_DB=cube_mvp
    export CUBE_SANDBOX_MYSQL_MANAGED=0
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    export CUBE_SANDBOX_REDIS_PORT=6379
    export CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
    persist_unified_dep_config "${out2}" "${operator2}"
  )
  assert_value "${out2}" DATABASE_URL "mysql://myuser:mypass@127.0.0.1:3306/mydb"
}

test_preserves_user_customized_value
test_adds_new_keys_with_defaults
test_three_way_adopts_new_default_for_untouched_key
test_three_way_keeps_customized_over_new_default
test_preserves_shell_sensitive_values
test_upsert_env_kv_preserves_shell_sensitive_values
test_upsert_env_kv_quotes_shell_metachar_only_values
test_upsert_env_kv_keeps_plain_scalars_readable
test_remove_env_kv_drops_key
test_keeps_old_only_host_keys
test_preserves_comments_and_structure
test_two_way_fallback_without_baseline
test_two_way_migrates_legacy_cube_proxy_cert_dir_default
test_two_way_migrates_single_quoted_legacy_cube_proxy_cert_dir_default
test_two_way_preserves_custom_cube_proxy_cert_dir
test_new_dotenv_overrides_take_priority
test_persist_freezes_managed_0_for_external_mysql
test_persist_freezes_managed_0_for_loopback_external_mysql
test_persist_freezes_managed_1_for_bundled_mysql
test_persist_freezes_redis_managed
test_persist_explicit_database_url_wins
test_persist_discards_stale_loopback_database_url_on_external
test_persist_explicit_quoted_database_url_not_double_escaped
test_persist_managed_ignores_explicit_database_url
test_persist_assembles_database_url_from_unified_vars
test_persist_managed_database_url_pins_loopback_host
test_merge_flips_external_to_bundled_with_managed_1
test_merge_flips_external_to_bundled_with_managed_1_env_host
test_merge_managed_auto_keeps_external_host
test_aliases_maps_all_legacy_pairs
test_aliases_emit_deprecation_warning
test_aliases_legacy_value_is_authoritative
test_aliases_force_managed_0_for_legacy_host
test_aliases_reset_host_when_force_managed_with_legacy_host
test_persist_explicit_database_url_with_inline_comment
test_persist_discards_stale_bundled_default_on_loopback_external
test_external_upgrade_migrates_endpoint_end_to_end
test_runtime_external_migration_complete_detection
test_second_upgrade_ignores_leftover_legacy_env
test_persist_preserves_custom_runtime_database_url
test_persist_rebuilds_plain_runtime_database_url
test_persist_discards_stale_loopback_runtime_database_url_when_host_remote
test_external_alias_map_matches_shell_array
test_version_lt
test_diff_report_redacts_secrets
test_drops_obsolete_agenthub_keys
test_migrates_custom_cube_proxy_image_tag
test_drops_default_cube_proxy_image_tag_without_migration
test_keeps_existing_cube_sandbox_cube_proxy_image_over_legacy_tag
test_non_utf8_env_fails_cleanly
test_read_helpers_reject_invalid_key
test_read_helpers
test_detect_existing_install
test_crlf_inputs_do_not_leak_carriage_returns
test_cidr_preflight_skip_conflict_param
test_host_is_loopback_classification
test_dep_is_managed_string_aliases
test_dsn_component_has_metachar
test_remove_env_kv_deletes_and_preserves_mode
test_remove_env_kv_noop_when_key_absent
test_remove_env_kv_deletes_multiple_keys_in_one_pass
test_remove_env_kv_deletes_indented_and_crlf_lines
test_remove_env_kv_noop_when_no_keys_given

echo "env merge tests OK"
