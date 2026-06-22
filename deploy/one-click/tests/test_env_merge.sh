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
ONE_CLICK_INSTALL_PREFIX=/usr/local/services/cubetoolbox
ONE_CLICK_DEPLOY_ROLE=control
CUBE_PVM_ENABLE=0
CUBE_SANDBOX_MYSQL_PORT=3306
CUBE_SANDBOX_REDIS_PASSWORD=ceuhvu123
WEB_UI_IMAGE=registry/openresty:1.21.4.1-6
CUBE_PROXY_CERT_DIR="${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs"
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

  # ${} expansion in an untouched key is kept verbatim (not expanded/mangled)
  assert_contains "${out}" 'CUBE_PROXY_CERT_DIR="${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs"'
  assert_value "${out}" DATABASE_URL "mysql://u:p@host:3306/db2"
  # WEIRD_KEY is old-only -> appended verbatim, value with '=' intact
  assert_value "${out}" WEIRD_KEY "a=b=c"

  # The merged file must remain valid shell that sources cleanly and expands ${}
  # using the (preserved) ONE_CLICK_INSTALL_PREFIX line from the template itself.
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
MY_CUSTOM_KEEP=stays
EOF

  merge_env_three_way "${new}" "${old}" "" "" "${out}" "${diff}" 2>/dev/null

  # All 13 DEPRECATED_KEYS must be removed from the merged runtime env.
  for k in \
    AGENTHUB_DEEPSEEK_API_KEY OPENCLAW_DEEPSEEK_API_KEY \
    AGENTHUB_LLM_API_KEY OPENCLAW_LLM_API_KEY \
    AGENTHUB_LLM_PROVIDER OPENCLAW_LLM_PROVIDER \
    AGENTHUB_LLM_BASE_URL OPENCLAW_LLM_BASE_URL \
    AGENTHUB_LLM_MODEL OPENCLAW_DEFAULT_MODEL \
    AGENTHUB_LLM_CREDENTIAL_MODE \
    AGENTHUB_SECRET_KEY CUBE_API_DATABASE_URL; do
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

test_preserves_user_customized_value
test_adds_new_keys_with_defaults
test_three_way_adopts_new_default_for_untouched_key
test_three_way_keeps_customized_over_new_default
test_preserves_shell_sensitive_values
test_keeps_old_only_host_keys
test_preserves_comments_and_structure
test_two_way_fallback_without_baseline
test_new_dotenv_overrides_take_priority
test_version_lt
test_diff_report_redacts_secrets
test_drops_obsolete_agenthub_keys
test_non_utf8_env_fails_cleanly
test_read_helpers_reject_invalid_key
test_read_helpers
test_detect_existing_install
test_crlf_inputs_do_not_leak_carriage_returns
test_cidr_preflight_skip_conflict_param

echo "env merge tests OK"
