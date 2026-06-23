#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cloud-free unit tests for the Tencent Cloud deployer's shared .env parsing
# (terraform/tencentcloud/lib-state-sync.sh: _env_value / _load_env_file). Both
# create.sh's load_saved_env and destroy.sh's preload depend on these to restore
# saved selections (region, instance types, credentials) byte-for-byte; a parse
# regression silently reuses a corrupted value (e.g. an instance type with a
# trailing "# comment" glued on) and drifts the next apply. These exercise every
# entry shape save_env_file / env.example can produce, without any cloud access.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_SYNC_LIB="$(cd "${SCRIPT_DIR}/../terraform/tencentcloud" && pwd)/lib-state-sync.sh"

# shellcheck source=../terraform/tencentcloud/lib-state-sync.sh
source "${STATE_SYNC_LIB}"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

assert_eq() {
  local expected="$1" actual="$2" msg="${3:-}"
  [[ "${expected}" == "${actual}" ]] || fail "expected '${expected}', got '${actual}' ${msg}"
}

# ---- _env_value: every shape save_env_file / env.example can write ----------
test_env_value_quoted() {
  # Plain single-quoted value (the common case).
  assert_eq "ap-guangzhou" "$(_env_value "TENCENTCLOUD_REGION='ap-guangzhou'")" "(plain)"
  # Empty value (unset password etc.).
  assert_eq "" "$(_env_value "TENCENTCLOUD_MYSQL_PASSWORD=''")" "(empty)"
  # Value with spaces (the OS image name) must survive intact.
  assert_eq "OpenCloudOS Server 9" "$(_env_value "TENCENTCLOUD_IMAGE_NAME='OpenCloudOS Server 9'")" "(spaces)"
  # A '#' INSIDE the quotes is part of the value (e.g. a password), not a comment.
  assert_eq 'p#ss' "$(_env_value "TENCENTCLOUD_CUBE_PASSWORD='p#ss'")" "(hash in value)"
}

test_env_value_inline_comment() {
  # env.example shape: quoted value followed by a trailing inline "# note". The
  # note must be dropped (this is exactly what create.sh corrupted before the fix).
  assert_eq "S5.2XLARGE16" \
    "$(_env_value "TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='S5.2XLARGE16'  # preferred type; actual per-node types are chosen at purchase time")" \
    "(env.example inline comment)"
  assert_eq "1" \
    "$(_env_value "TENCENTCLOUD_COMPUTE_NODE_COUNT='1'  # default 1; raise for a larger compute cluster")" \
    "(env.example node count)"
}

test_env_value_self_repairs_legacy() {
  # A legacy .env written by the buggy loader doubled the comment INSIDE the
  # quotes: KEY='value'  # note'  # note. Taking only the first quoted span
  # recovers the real value.
  local legacy="TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='S5.LARGE8'  # preference only; see terraform output compute_instance_types'  # preference only; see terraform output compute_instance_types"
  assert_eq "S5.LARGE8" "$(_env_value "${legacy}")" "(legacy self-repair)"
}

test_env_value_unquoted() {
  # Hand-edited unquoted value: a shell-style inline comment is stripped and the
  # value is trimmed; a bare value passes through unchanged.
  assert_eq "2" "$(_env_value "TENCENTCLOUD_COMPUTE_NODE_COUNT=2  # comment")" "(unquoted + comment)"
  assert_eq "value" "$(_env_value "K=value")" "(unquoted plain)"
}

# ---- _load_env_file: the shared preload loop --------------------------------
test_load_env_file_fills_only_unset() {
  local tmp
  tmp="$(mktemp)"
  cat >"${tmp}" <<'EOF'
# a comment line and a blank line below must be skipped

TENCENTCLOUD_REGION='ap-shanghai'
TENCENTCLOUD_IMAGE_NAME='OpenCloudOS Server 9'
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='S5.2XLARGE16'  # preferred type; actual per-node types are chosen at purchase time
EOF

  # An already-set var must win over the file (explicit override).
  local TENCENTCLOUD_REGION="ap-guangzhou"
  export TENCENTCLOUD_REGION
  unset TENCENTCLOUD_IMAGE_NAME TENCENTCLOUD_COMPUTE_INSTANCE_TYPE 2>/dev/null || true

  _load_env_file "${tmp}"

  assert_eq "ap-guangzhou" "${TENCENTCLOUD_REGION}" "(preset var wins)"
  assert_eq "OpenCloudOS Server 9" "${TENCENTCLOUD_IMAGE_NAME:-}" "(unset var filled)"
  assert_eq "S5.2XLARGE16" "${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-}" "(inline comment stripped on load)"

  rm -f "${tmp}"
  unset TENCENTCLOUD_REGION TENCENTCLOUD_IMAGE_NAME TENCENTCLOUD_COMPUTE_INSTANCE_TYPE 2>/dev/null || true
}

test_load_env_file_missing_is_noop() {
  # Must not error (set -e) when the .env does not exist yet (first run).
  _load_env_file "/nonexistent/path/.env" || fail "(missing file should be a no-op)"
}

test_env_value_quoted
test_env_value_inline_comment
test_env_value_self_repairs_legacy
test_env_value_unquoted
test_load_env_file_fills_only_unset
test_load_env_file_missing_is_noop

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} env-value test(s) failed" >&2
  exit 1
fi

echo "env value tests OK"
