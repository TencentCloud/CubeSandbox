#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
# Unit tests for the build-time shim capability declaration validation.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VALIDATOR="${ONE_CLICK_DIR}/../../CubeShim/scripts/check-shim-capabilities.py"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# write NAME CONTENT -> prints the path it wrote.
write() {
  local name="$1" content="$2"
  local path="${TMP_DIR}/${name}"
  printf '%s' "${content}" > "${path}"
  printf '%s\n' "${path}"
}

# expect_ok PATH
expect_ok() {
  local path="$1"
  if ! python3 "${VALIDATOR}" "${path}" >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    fail "expected acceptance, got rejection: $(cat "${TMP_DIR}/err")"
  fi
}

# expect_reject PATH PATTERN
expect_reject() {
  local path="$1" pattern="$2"
  if python3 "${VALIDATOR}" "${path}" >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    fail "expected rejection of ${path}, but it was accepted"
  fi
  grep -q -- "${pattern}" "${TMP_DIR}/err" \
    || fail "rejection of ${path} did not mention ${pattern}: $(cat "${TMP_DIR}/err")"
}

valid_doc='{"schema":1,"component":"cube-shim","version":"v0.8.0",
  "capabilities":{"update.snapshot_capture":1},"runtime":{}}'

# A real declaration is accepted, with and without a runtime section.
expect_ok "$(write valid.json "${valid_doc}")"
expect_ok "$(write no-runtime.json \
  '{"schema":1,"component":"cube-shim","version":"v0.8.0","capabilities":{"errors.coded":1}}')"

# Failures the build must catch rather than ship.
expect_reject "$(write empty.json '')" "is empty"
expect_reject "$(write whitespace.json '   
  ')" "is empty"
expect_reject "$(write not-json.json '{ not json')" "not valid JSON"
expect_reject "$(write array.json '[1,2,3]')" "not a JSON object"
expect_reject "$(write bad-schema.json \
  '{"schema":2,"component":"cube-shim","capabilities":{"errors.coded":1}}')" "schema"
expect_reject "$(write missing-schema.json \
  '{"component":"cube-shim","capabilities":{"errors.coded":1}}')" "schema"
expect_reject "$(write bad-component.json \
  '{"schema":1,"component":"cube-runtime","capabilities":{"errors.coded":1}}')" "component"
expect_reject "$(write no-caps.json \
  '{"schema":1,"component":"cube-shim","capabilities":{}}')" "no capabilities"
expect_reject "$(write caps-not-object.json \
  '{"schema":1,"component":"cube-shim","capabilities":["errors.coded"]}')" "no capabilities"

# A level of 0 is a meaningful runtime answer but never a build-time claim.
expect_reject "$(write zero-level.json \
  '{"schema":1,"component":"cube-shim","capabilities":{"errors.coded":0}}')" "positive integer"
# `true` would deserialize as int in Python without the explicit bool check.
expect_reject "$(write bool-level.json \
  '{"schema":1,"component":"cube-shim","capabilities":{"errors.coded":true}}')" "positive integer"
expect_reject "$(write string-level.json \
  '{"schema":1,"component":"cube-shim","capabilities":{"errors.coded":"1"}}')" "positive integer"
expect_reject "$(write empty-name.json \
  '{"schema":1,"component":"cube-shim","capabilities":{"":1}}')" "capability name"

# A non-object runtime section is malformed for the reader that parses it.
expect_reject "$(write bad-runtime.json \
  '{"schema":1,"component":"cube-shim","capabilities":{"errors.coded":1},"runtime":[]}')" "runtime"

# Missing file and wrong argument count are build errors too.
expect_reject "${TMP_DIR}/does-not-exist.json" "cannot read"
if python3 "${VALIDATOR}" >/dev/null 2>&1; then
  fail "expected usage error when no path is given"
fi

# The one-click wrapper must reject the same input.
# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"
assert_shim_capabilities "$(write wrapper-ok.json "${valid_doc}")"

if ( assert_shim_capabilities "$(write wrapper-bad.json '{ not json')" ) >/dev/null 2>&1; then
  fail "assert_shim_capabilities accepted an invalid declaration"
fi
if ( assert_shim_capabilities "${TMP_DIR}/nope.json" ) >/dev/null 2>&1; then
  fail "assert_shim_capabilities accepted a missing file"
fi

echo "PASS: shim capability declaration validation"
