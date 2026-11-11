#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the installer desired-state mechanism (ONE_CLICK_TOGGLE_KEYS,
# snapshot_one_click_toggles, apply_one_click_toggles) in lib/common.sh.
# These on/off switches and selectors must preserve this-run intent (process
# environment or bundle .env, except copied template defaults for protected
# keys) across the upgrade env merge.
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
  unset \
    ONE_CLICK_ENABLE_S3LVOL \
    ONE_CLICK_ENABLE_JFS \
    ONE_CLICK_ENABLE_OBSERVABILITY \
    CUBE_LOGS_BACKEND \
    CUBE_SANDBOX_MINIO_ENABLED \
    CUBE_PVM_ENABLE
}

# Simulate what install.sh does around the upgrade merge for the toggle keys:
# snapshot the intent, let the merged old-runtime value clobber the variable
# (as `load_env_file "${MERGED_ENV}"` would), then re-apply.
run_toggle_roundtrip() {
  local dotenv="$1"
  snapshot_one_click_toggles "${dotenv}"
  ONE_CLICK_ENABLE_S3LVOL="${MERGED_S3LVOL:-}"
  ONE_CLICK_ENABLE_OBSERVABILITY="${MERGED_OBSERVABILITY:-}"
  CUBE_LOGS_BACKEND="${MERGED_LOGS_BACKEND:-}"
  CUBE_SANDBOX_MINIO_ENABLED="${MERGED_MINIO:-}"
  CUBE_PVM_ENABLE="${MERGED_PVM:-}"
  apply_one_click_toggles 2>/dev/null
}

test_toggle_keys_registry() {
  assert_toggle_keys \
    ONE_CLICK_ENABLE_S3LVOL \
    ONE_CLICK_ENABLE_JFS \
    ONE_CLICK_ENABLE_OBSERVABILITY \
    CUBE_LOGS_BACKEND \
    CUBE_SANDBOX_MINIO_ENABLED \
    CUBE_PVM_ENABLE
}

# Some toggles still accept their template default as explicit intent. The
# observability default is protected so a copied template cannot disable it.
test_dotenv_default_value_is_explicit() {
  local dotenv="${TMP_DIR}/dotenv1.env"
  cat > "${dotenv}" <<'EOF'
ONE_CLICK_ENABLE_S3LVOL=0
ONE_CLICK_ENABLE_OBSERVABILITY=0
CUBE_SANDBOX_MINIO_ENABLED=0
CUBE_PVM_ENABLE=1
EOF
  reset_toggle_vars
  MERGED_S3LVOL=1
  MERGED_OBSERVABILITY=1
  MERGED_MINIO=1
  MERGED_PVM=0
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "0" ]] \
    || fail "expected .env ONE_CLICK_ENABLE_S3LVOL=0 to disable the runtime 1"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "1" ]] \
    || fail "copied observability default must not disable the runtime 1"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "0" ]] \
    || fail "expected .env CUBE_SANDBOX_MINIO_ENABLED=0 to disable the runtime 1"
  [[ "${CUBE_PVM_ENABLE}" == "1" ]] \
    || fail "expected .env CUBE_PVM_ENABLE=1 to win over the runtime 0"
}

# `VAR=x ./install.sh`: process-environment intent survives the merge even
# without any .env.
test_process_env_is_explicit() {
  local dotenv="${TMP_DIR}/absent.env"
  reset_toggle_vars
  ONE_CLICK_ENABLE_S3LVOL=0
  ONE_CLICK_ENABLE_OBSERVABILITY=0
  CUBE_SANDBOX_MINIO_ENABLED=0
  MERGED_S3LVOL=1
  MERGED_OBSERVABILITY=1
  MERGED_MINIO=1
  MERGED_PVM=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "0" ]] \
    || fail "expected process-env ONE_CLICK_ENABLE_S3LVOL=0 to disable the runtime 1"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "0" ]] \
    || fail "expected process-env ONE_CLICK_ENABLE_OBSERVABILITY=0 to disable the runtime 1"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "0" ]] \
    || fail "expected process-env CUBE_SANDBOX_MINIO_ENABLED=0 to disable the runtime 1"
  [[ "${CUBE_PVM_ENABLE}" == "1" ]] \
    || fail "expected untouched CUBE_PVM_ENABLE to keep the merged value 1"
}

# Both channels set: .env wins, mirroring the toggle channel order
# (.env file > process environment > defaults).
test_dotenv_beats_process_env() {
  local dotenv="${TMP_DIR}/dotenv2.env"
  cat > "${dotenv}" <<'EOF'
ONE_CLICK_ENABLE_S3LVOL=1
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_SANDBOX_MINIO_ENABLED=1
EOF
  reset_toggle_vars
  ONE_CLICK_ENABLE_S3LVOL=0
  ONE_CLICK_ENABLE_OBSERVABILITY=0
  CUBE_SANDBOX_MINIO_ENABLED=0
  MERGED_S3LVOL=0
  MERGED_OBSERVABILITY=0
  MERGED_MINIO=0
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "1" ]] \
    || fail "expected .env ONE_CLICK_ENABLE_S3LVOL=1 to beat the process-env 0"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "1" ]] \
    || fail "expected .env ONE_CLICK_ENABLE_OBSERVABILITY=1 to beat the process-env 0"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "0" ]] \
    || fail "template-default MinIO must not override explicit process-env 0"
}

# No intent anywhere: the merged (old runtime) value is left untouched.
test_no_intent_keeps_merged_value() {
  local dotenv="${TMP_DIR}/dotenv3.env"
  echo "CUBE_SANDBOX_MYSQL_PORT=3307" > "${dotenv}"
  reset_toggle_vars
  MERGED_S3LVOL=1
  MERGED_OBSERVABILITY=1
  MERGED_MINIO=0
  MERGED_PVM=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "1" ]] \
    || fail "expected no-intent run to keep the merged ONE_CLICK_ENABLE_S3LVOL=1"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "1" ]] \
    || fail "expected no-intent run to keep the merged ONE_CLICK_ENABLE_OBSERVABILITY=1"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "0" ]] \
    || fail "expected no-intent run to keep the merged CUBE_SANDBOX_MINIO_ENABLED=0"
  [[ "${CUBE_PVM_ENABLE}" == "1" ]] \
    || fail "expected no-intent run to keep the merged CUBE_PVM_ENABLE=1"
}

# A commented-out or absent key in .env is not intent: the snapshot must key
# off an active KEY= line, not a template comment.
test_commented_dotenv_line_is_not_intent() {
  local dotenv="${TMP_DIR}/dotenv4.env"
  cat > "${dotenv}" <<'EOF'
# ONE_CLICK_ENABLE_S3LVOL=0
# ONE_CLICK_ENABLE_OBSERVABILITY=0
# CUBE_SANDBOX_MINIO_ENABLED=0
CUBE_PVM_ENABLE=0
EOF
  reset_toggle_vars
  MERGED_S3LVOL=1
  MERGED_OBSERVABILITY=1
  MERGED_MINIO=1
  MERGED_PVM=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${ONE_CLICK_ENABLE_S3LVOL}" == "1" ]] \
    || fail "commented .env line must not count as intent"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "1" ]] \
    || fail "commented observability .env line must not count as intent"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "1" ]] \
    || fail "commented MinIO .env line must not count as intent"
  [[ "${CUBE_PVM_ENABLE}" == "0" ]] \
    || fail "active .env CUBE_PVM_ENABLE=0 must count as intent"
}

test_logs_backend_can_switch_back_to_default() {
  local dotenv="${TMP_DIR}/logs-backend.env"
  cat > "${dotenv}" <<'EOF'
CUBE_LOGS_BACKEND=loki
EOF
  reset_toggle_vars
  CUBE_LOGS_BACKEND=loki
  MERGED_LOGS_BACKEND=elasticsearch
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_LOGS_BACKEND}" == "loki" ]] \
    || fail "expected explicit process-env Loki selection to replace the merged Elasticsearch backend"
}

test_legacy_es_endpoint_beats_new_template_default() {
  local resolved
  resolved="$(resolve_one_click_logs_backend_selection \
    loki http://127.0.0.1:9200 0 0)"
  [[ "${resolved}" == "elasticsearch" ]] \
    || fail "expected a legacy ES endpoint to beat the newly introduced Loki template default"

  resolved="$(resolve_one_click_logs_backend_selection \
    loki http://127.0.0.1:9200 0 1)"
  [[ "${resolved}" == "loki" ]] \
    || fail "expected an explicit Loki selection to beat the legacy ES endpoint"
}

test_observability_intent_rotates_and_clears_values() {
  local dotenv="${TMP_DIR}/observability-intent.env"
  cat > "${dotenv}" <<'EOF'
CUBE_LOGS_ES_ENDPOINT=https://new-es.example:9200
CUBE_LOGS_ES_PASSWORD='new password'
CUBE_LOKI_RETENTION_PERIOD=
EOF
  unset CUBE_LOGS_ES_ENDPOINT CUBE_LOGS_ES_PASSWORD CUBE_LOKI_RETENTION_PERIOD
  snapshot_one_click_observability_intent "${dotenv}"
  load_env_file "${dotenv}" >/dev/null 2>&1
  capture_one_click_observability_dotenv_values

  CUBE_LOGS_ES_ENDPOINT=http://old-es:9200
  CUBE_LOGS_ES_PASSWORD=old-password
  CUBE_LOKI_RETENTION_PERIOD=30d
  apply_one_click_observability_intent

  [[ "${CUBE_LOGS_ES_ENDPOINT}" == "https://new-es.example:9200" ]] \
    || fail "expected this-run Elasticsearch endpoint to replace the merged runtime value"
  [[ "${CUBE_LOGS_ES_PASSWORD}" == "new password" ]] \
    || fail "expected quoted this-run password to be shell-interpreted and preserved"
  [[ -z "${CUBE_LOKI_RETENTION_PERIOD}" ]] \
    || fail "expected an explicit empty value to clear merged retention"
  one_click_observability_intent_declared CUBE_LOKI_RETENTION_PERIOD \
    || fail "expected an empty observability value to remain declared intent"
}

test_copied_template_preserves_external_backends() {
  local dotenv="${TMP_DIR}/copied.env" old="${TMP_DIR}/old.env"
  local out="${TMP_DIR}/merged.env" diff="${TMP_DIR}/merged.diff"
  cp "${ONE_CLICK_DIR}/env.example" "${dotenv}"
  cat > "${old}" <<'EOF'
CUBE_SANDBOX_MINIO_ENABLED=0
CUBE_S3_ENDPOINT=https://external-s3.example
CUBE_S3_BUCKET=existing-bucket
CUBE_LOGS_BACKEND=elasticsearch
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_LOGS_ES_ENDPOINT=https://external-es.example:9200
CUBE_LOGS_ES_INDEX=existing-logs
CUBE_LOGS_ES_USER=existing-user
CUBE_LOGS_ES_PASSWORD=existing-password
EOF
  reset_toggle_vars
  unset CUBE_LOGS_ES_ENDPOINT CUBE_LOGS_ES_INDEX CUBE_LOGS_ES_USER CUBE_LOGS_ES_PASSWORD
  snapshot_one_click_toggles "${dotenv}"
  snapshot_one_click_observability_intent "${dotenv}"
  load_env_file "${dotenv}" >/dev/null 2>&1
  capture_one_click_observability_dotenv_values
  merge_env_three_way "${ONE_CLICK_DIR}/env.example" "${old}" "" "${dotenv}" "${out}" "${diff}" 2>/dev/null
  load_env_file "${out}" >/dev/null 2>&1
  apply_one_click_toggles 2>/dev/null
  apply_one_click_observability_intent
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "0" ]] || fail "copied template re-enabled MinIO"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "1" ]] || fail "copied template disabled observability"
  [[ -z "${ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT[ONE_CLICK_ENABLE_OBSERVABILITY]+x}" ]] || fail "copied observability default counted as explicit"
  [[ "${CUBE_S3_ENDPOINT}" == "https://external-s3.example" ]] || fail "external S3 endpoint changed"
  [[ "${CUBE_LOGS_BACKEND}" == "elasticsearch" ]] || fail "copied template switched ES to Loki"
  [[ "${CUBE_LOGS_ES_ENDPOINT}" == "https://external-es.example:9200" ]] || fail "ES endpoint changed"
  [[ "${CUBE_LOGS_ES_INDEX}" == "existing-logs" && "${CUBE_LOGS_ES_USER}" == "existing-user" && "${CUBE_LOGS_ES_PASSWORD}" == "existing-password" ]] || fail "ES index or credentials changed"
  [[ -z "${ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT[CUBE_LOGS_BACKEND]+x}" ]] || fail "copied Loki default counted as explicit"

  reset_toggle_vars
  CUBE_SANDBOX_MINIO_ENABLED=1
  CUBE_LOGS_BACKEND=loki
  ONE_CLICK_ENABLE_OBSERVABILITY=0
  MERGED_MINIO=0
  MERGED_LOGS_BACKEND=elasticsearch
  MERGED_OBSERVABILITY=1
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "1" && "${CUBE_LOGS_BACKEND}" == "loki" ]] || fail "explicit process defaults must still work"
  [[ "${ONE_CLICK_ENABLE_OBSERVABILITY}" == "0" ]] || fail "explicit process default must still disable observability"

  printf 'CUBE_SANDBOX_MINIO_ENABLED=0\nCUBE_LOGS_BACKEND=elasticsearch\n' > "${dotenv}"
  reset_toggle_vars
  CUBE_SANDBOX_MINIO_ENABLED=1
  CUBE_LOGS_BACKEND=loki
  MERGED_MINIO=1
  MERGED_LOGS_BACKEND=loki
  run_toggle_roundtrip "${dotenv}"
  [[ "${CUBE_SANDBOX_MINIO_ENABLED}" == "0" && "${CUBE_LOGS_BACKEND}" == "elasticsearch" ]] || fail "non-default dotenv must override process environment"
}

test_toggle_keys_registry
test_dotenv_default_value_is_explicit
test_process_env_is_explicit
test_dotenv_beats_process_env
test_no_intent_keeps_merged_value
test_commented_dotenv_line_is_not_intent
test_logs_backend_can_switch_back_to_default
test_legacy_es_endpoint_beats_new_template_default
test_observability_intent_rotates_and_clears_values
test_copied_template_preserves_external_backends

echo "test_toggle_inputs: all tests passed"
