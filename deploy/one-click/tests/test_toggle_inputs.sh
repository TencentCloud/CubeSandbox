#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the installer toggle mechanism (ONE_CLICK_TOGGLE_KEYS,
# snapshot_one_click_toggles, apply_one_click_toggles) in lib/common.sh.
# Toggle keys are on/off switches whose this-run value (process environment
# or bundle .env, presence implying explicit intent even for the default
# value) must survive the upgrade env merge, which otherwise preserves the
# old runtime value for keys whose .env value equals the env.example default.
# CUBE_PROXY_DNSMASQ_MODE additionally has a state-based stale-.env guard; see
# capture_one_click_toggle_dotenv_values in lib/common.sh.
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

# Assert that every entry in "$@" is present in ONE_CLICK_TOGGLE_KEYS, in
# order, with no extras — removing a key here silently resurrects the
# "cannot flip back to default on upgrade" bug for that key.
assert_toggle_keys() {
  local expected="${*}"
  local actual
  actual="${ONE_CLICK_TOGGLE_KEYS[*]}"
  [[ "${actual}" == "${expected}" ]] \
    || fail "expected ONE_CLICK_TOGGLE_KEYS='${expected}', got '${actual}'"
}

# Reset the toggle variables so leftovers from a previous scenario cannot
# leak into the next snapshot as process-env intent.
reset_toggle_vars() {
  unset ONE_CLICK_ENABLE_S3LVOL CUBE_PVM_ENABLE CUBE_PROXY_DNSMASQ_MODE ONE_CLICK_RUNTIME_ENV_FILE
}

# Simulate what install.sh does around the upgrade merge for the toggle keys:
# snapshot the intent, let the merged old-runtime value clobber the variable
# (as `load_env_file "${MERGED_ENV}"` would), then re-apply.
run_toggle_roundtrip() {
  local dotenv="$1"
  snapshot_one_click_toggles "${dotenv}"
  # Simulate load_env_file "${dotenv}": shell-interpret the values before the
  # dotenv snapshot is captured (quotes and trailing comments stripped).
  if [[ -f "${dotenv}" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "${dotenv}"
    set +a
  fi
  capture_one_click_toggle_dotenv_values
  ONE_CLICK_ENABLE_S3LVOL="${MERGED_S3LVOL:-}"
  CUBE_PVM_ENABLE="${MERGED_PVM:-}"
  CUBE_PROXY_DNSMASQ_MODE="${MERGED_DNSMASQ:-}"
  apply_one_click_toggles 2>/dev/null
}

test_toggle_keys_registry() {
  assert_toggle_keys ONE_CLICK_ENABLE_S3LVOL CUBE_PVM_ENABLE CUBE_PROXY_DNSMASQ_MODE
}

# Core regression: a .env value that equals the env.example default (0) must
# still count as explicit — this is what makes "disable on upgrade" work.
test_dotenv_default_value_is_explicit() {
  local dotenv="${TMP_DIR}/dotenv1.env"
  cat > "${dotenv}" <<'EOF'
ONE_CLICK_ENABLE_S3LVOL=0
CUBE_PVM_ENABLE=1
EOF
  reset_toggle_vars
  MERGED_S3LVOL=1
  MERGED_PVM=0
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "0" ]] \
    || fail "expected .env ONE_CLICK_ENABLE_S3LVOL=0 to disable the runtime 1"
  [[ "${CUBE_PVM_ENABLE}" == "1" ]] \
    || fail "expected .env CUBE_PVM_ENABLE=1 to win over the runtime 0"
}

# `VAR=x ./install.sh`: process-environment intent survives the merge even
# without any .env.
test_process_env_is_explicit() {
  local dotenv="${TMP_DIR}/absent.env"
  reset_toggle_vars
  ONE_CLICK_ENABLE_S3LVOL=0
  MERGED_S3LVOL=1
  MERGED_PVM=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "0" ]] \
    || fail "expected process-env ONE_CLICK_ENABLE_S3LVOL=0 to disable the runtime 1"
  [[ "${CUBE_PVM_ENABLE}" == "1" ]] \
    || fail "expected untouched CUBE_PVM_ENABLE to keep the merged value 1"
}

# Both channels set: .env wins, mirroring install.sh's documented channel
# order (CLI flags > .env file > process environment > defaults).
test_dotenv_beats_process_env() {
  local dotenv="${TMP_DIR}/dotenv2.env"
  cat > "${dotenv}" <<'EOF'
ONE_CLICK_ENABLE_S3LVOL=1
EOF
  reset_toggle_vars
  ONE_CLICK_ENABLE_S3LVOL=0
  MERGED_S3LVOL=0
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "1" ]] \
    || fail "expected .env ONE_CLICK_ENABLE_S3LVOL=1 to beat the process-env 0"
}

# No intent anywhere: the merged (old runtime) value is left untouched.
test_no_intent_keeps_merged_value() {
  local dotenv="${TMP_DIR}/dotenv3.env"
  echo "CUBE_SANDBOX_MYSQL_PORT=3307" > "${dotenv}"
  reset_toggle_vars
  MERGED_S3LVOL=1
  MERGED_PVM=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "1" ]] \
    || fail "expected no-intent run to keep the merged ONE_CLICK_ENABLE_S3LVOL=1"
  [[ "${CUBE_PVM_ENABLE}" == "1" ]] \
    || fail "expected no-intent run to keep the merged CUBE_PVM_ENABLE=1"
}

# A commented-out or absent key in .env is not intent: the snapshot must key
# off an active KEY= line, not a template comment.
test_commented_dotenv_line_is_not_intent() {
  local dotenv="${TMP_DIR}/dotenv4.env"
  cat > "${dotenv}" <<'EOF'
# ONE_CLICK_ENABLE_S3LVOL=0
CUBE_PVM_ENABLE=0
EOF
  reset_toggle_vars
  MERGED_S3LVOL=1
  MERGED_PVM=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "1" ]] \
    || fail "commented .env line must not count as intent"
  [[ "${CUBE_PVM_ENABLE}" == "0" ]] \
    || fail "active .env CUBE_PVM_ENABLE=0 must count as intent"
}

# CUBE_PROXY_DNSMASQ_MODE must survive the upgrade merge through either
# channel. Regression: without this key in the registry, an upgrade reset it
# to the env.example default (networkmanager) and hosts without resolvectl /
# NetworkManager failed the DNS preflight.
test_dnsmasq_mode_roundtrip() {
  local dotenv="${TMP_DIR}/dotenv-dnsmasq.env"
  local runtime="${TMP_DIR}/runtime-dnsmasq.env"

  # Fresh install (no previous runtime env): .env stays authoritative and wins
  # over the process environment, matching the documented precedence.
  echo "CUBE_PROXY_DNSMASQ_MODE=networkmanager" > "${dotenv}"
  reset_toggle_vars
  CUBE_PROXY_DNSMASQ_MODE=standalone
  MERGED_DNSMASQ=standalone
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "networkmanager" ]] \
    || fail "fresh install: .env networkmanager must win over process-env standalone"

  # Process-environment value survives when .env has no active line.
  reset_toggle_vars
  CUBE_PROXY_DNSMASQ_MODE=standalone
  MERGED_DNSMASQ=networkmanager
  run_toggle_roundtrip "${TMP_DIR}/absent-dnsmasq.env"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "standalone" ]] \
    || fail "expected process-env CUBE_PROXY_DNSMASQ_MODE=standalone to survive the merge"

  # Upgrade where the runtime already holds standalone: a stale full-copy .env
  # with the old template default must not override it.
  echo "CUBE_PROXY_DNSMASQ_MODE=standalone" > "${runtime}"
  reset_toggle_vars
  export ONE_CLICK_RUNTIME_ENV_FILE="${runtime}"
  CUBE_PROXY_DNSMASQ_MODE=standalone
  MERGED_DNSMASQ=networkmanager
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "standalone" ]] \
    || fail "stale .env default must not override process-env standalone on a standalone runtime"

  reset_toggle_vars
  export ONE_CLICK_RUNTIME_ENV_FILE="${runtime}"
  MERGED_DNSMASQ=standalone
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "standalone" ]] \
    || fail "stale .env default must not override a preserved standalone"

  # Explicit networkmanager in .env still works when the runtime is not
  # non-default (documented .env > process-env precedence is preserved).
  echo "CUBE_PROXY_DNSMASQ_MODE=networkmanager" > "${runtime}"
  reset_toggle_vars
  export ONE_CLICK_RUNTIME_ENV_FILE="${runtime}"
  CUBE_PROXY_DNSMASQ_MODE=standalone
  MERGED_DNSMASQ=standalone
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "networkmanager" ]] \
    || fail "explicit .env networkmanager must win on a default-mode runtime"

  unset ONE_CLICK_RUNTIME_ENV_FILE
}

# A plain `cp env.example .env` must not count as intent: the template ships
# CUBE_PROXY_DNSMASQ_MODE commented out (the code default is networkmanager),
# so an old runtime `standalone` survives an upgrade.
test_dnsmasq_mode_template_copy_is_not_intent() {
  local dotenv="${TMP_DIR}/dotenv-dnsmasq-template.env"
  printf '# CUBE_PROXY_DNSMASQ_MODE=networkmanager\n' > "${dotenv}"
  reset_toggle_vars
  MERGED_DNSMASQ=standalone
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "standalone" ]] \
    || fail "commented template line must not override a preserved standalone"

  reset_toggle_vars
  MERGED_DNSMASQ=standalone
  run_toggle_roundtrip "${TMP_DIR}/absent-dnsmasq2.env"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "standalone" ]] \
    || fail "no-intent upgrade must keep the runtime CUBE_PROXY_DNSMASQ_MODE=standalone"
}

# .env values must be replayed shell-interpreted: load_env_file strips quotes
# and trailing comments, and check_dns_preflight validates the mode strictly
# (replaying the raw RHS would be rejected as an unsupported mode).
test_dnsmasq_mode_dotenv_is_shell_interpreted() {
  local dotenv="${TMP_DIR}/dotenv-dnsmasq-quoted.env"
  printf 'CUBE_PROXY_DNSMASQ_MODE="standalone"  # no NetworkManager\n' > "${dotenv}"
  reset_toggle_vars
  MERGED_DNSMASQ=networkmanager
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_PROXY_DNSMASQ_MODE}" == "standalone" ]] \
    || fail "quoted/annotated .env CUBE_PROXY_DNSMASQ_MODE must be replayed shell-interpreted (got '${CUBE_PROXY_DNSMASQ_MODE}')"
}

# Keep the template line commented: an active default would make the common
# `cp env.example .env` flow silently reset a preserved standalone mode.
test_env_example_keeps_dnsmasq_mode_commented() {
  local example="${ONE_CLICK_DIR}/env.example"
  if grep -q '^CUBE_PROXY_DNSMASQ_MODE=' "${example}"; then
    fail "env.example must keep CUBE_PROXY_DNSMASQ_MODE commented out"
  fi
  grep -q '^# CUBE_PROXY_DNSMASQ_MODE=' "${example}" \
    || fail "env.example should document CUBE_PROXY_DNSMASQ_MODE as a commented example"
}

test_toggle_keys_registry
test_dotenv_default_value_is_explicit
test_process_env_is_explicit
test_dotenv_beats_process_env
test_no_intent_keeps_merged_value
test_commented_dotenv_line_is_not_intent
test_dnsmasq_mode_roundtrip
test_dnsmasq_mode_dotenv_is_shell_interpreted
test_dnsmasq_mode_template_copy_is_not_intent
test_env_example_keeps_dnsmasq_mode_commented

echo "test_toggle_inputs: all tests passed"
