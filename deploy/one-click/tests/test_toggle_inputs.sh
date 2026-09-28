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
  unset ONE_CLICK_ENABLE_S3LVOL CUBE_PVM_ENABLE
}

# Simulate what install.sh does around the upgrade merge for the toggle keys:
# snapshot the intent, let the merged old-runtime value clobber the variable
# (as `load_env_file "${MERGED_ENV}"` would), then re-apply.
run_toggle_roundtrip() {
  local dotenv="$1"
  snapshot_one_click_toggles "${dotenv}"
  ONE_CLICK_ENABLE_S3LVOL="${MERGED_S3LVOL:-}"
  CUBE_PVM_ENABLE="${MERGED_PVM:-}"
  apply_one_click_toggles 2>/dev/null
}

test_toggle_keys_registry() {
  assert_toggle_keys ONE_CLICK_ENABLE_S3LVOL CUBE_PVM_ENABLE
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

test_toggle_keys_registry
test_dotenv_default_value_is_explicit
test_process_env_is_explicit
test_dotenv_beats_process_env
test_no_intent_keeps_merged_value
test_commented_dotenv_line_is_not_intent

echo "test_toggle_inputs: all tests passed"
