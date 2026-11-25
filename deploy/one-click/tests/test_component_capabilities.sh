#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for the build-time validation of component capability declarations.
# Those files are what Cubelet uses to pick a capture path and to decide whether a
# guest can freeze a filesystem; an unreadable one degrades silently at runtime, so
# the build has to reject it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VALIDATOR="${ONE_CLICK_DIR}/../../CubeShim/scripts/check-component-capabilities.py"

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

# expect_ok PATH [COMPONENT]
expect_ok() {
  local path="$1" component="${2:-}"
  if ! python3 "${VALIDATOR}" "${path}" ${component:+"${component}"} \
      >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    fail "expected acceptance, got rejection: $(cat "${TMP_DIR}/err")"
  fi
}

# expect_reject PATH PATTERN [COMPONENT]
expect_reject() {
  local path="$1" pattern="$2" component="${3:-}"
  if python3 "${VALIDATOR}" "${path}" ${component:+"${component}"} \
      >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    fail "expected rejection of ${path}, but it was accepted"
  fi
  grep -q -- "${pattern}" "${TMP_DIR}/err" \
    || fail "rejection of ${path} did not mention ${pattern}: $(cat "${TMP_DIR}/err")"
}

shim_doc='{"schema":1,"component":"cube-shim","version":"v0.8.0",
  "capabilities":{"update.snapshot_capture":1}}'
agent_doc='{"schema":1,"component":"cube-agent","version":"v0.8.0",
  "capabilities":{"guest.fs_freeze":1}}'

# Both components' real shapes are accepted, with and without a pinned name.
expect_ok "$(write shim.json "${shim_doc}")"
expect_ok "$(write shim.json "${shim_doc}")" cube-shim
expect_ok "$(write agent.json "${agent_doc}")"
expect_ok "$(write agent.json "${agent_doc}")" cube-agent

# The component name can be pinned, which is how each build path calls it.
expect_reject "$(write agent.json "${agent_doc}")" "component" cube-shim
expect_reject "$(write shim.json "${shim_doc}")" "component" cube-agent

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
expect_reject "$(write no-component.json \
  '{"schema":1,"capabilities":{"errors.coded":1}}')" "no component name"
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

# Missing file and wrong argument count are build errors too.
expect_reject "${TMP_DIR}/does-not-exist.json" "cannot read"
if python3 "${VALIDATOR}" >/dev/null 2>&1; then
  fail "expected usage error when no path is given"
fi

# The wrapper used by the build scripts must reject the same input and report
# through the project's own die() so the failure reads like every other one.
# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"
assert_component_capabilities "$(write wrapper-ok.json "${shim_doc}")" cube-shim
assert_component_capabilities "$(write wrapper-agent.json "${agent_doc}")" cube-agent

if ( assert_component_capabilities "$(write wrapper-bad.json '{ not json')" ) >/dev/null 2>&1; then
  fail "assert_component_capabilities accepted an invalid declaration"
fi
if ( assert_component_capabilities "$(write wrapper-wrong.json "${agent_doc}")" cube-shim ) >/dev/null 2>&1; then
  fail "assert_component_capabilities accepted a mismatched component name"
fi
if ( assert_component_capabilities "${TMP_DIR}/nope.json" ) >/dev/null 2>&1; then
  fail "assert_component_capabilities accepted a missing file"
fi

echo "PASS: component capability declaration validation"
