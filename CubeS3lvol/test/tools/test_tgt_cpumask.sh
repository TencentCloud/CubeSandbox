#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Offline tests for the default s3lvol_tgt CPU mask (last two allowed CPUs).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELPER="${ROOT}/scripts/rcow_cpumask.sh"

fail() {
	echo "FAIL: $*" >&2
	echo "result: 0 passed, 1 failed"
	exit 1
}

[[ -f "${HELPER}" ]] || fail "missing ${HELPER}"
# shellcheck disable=SC1090
source "${HELPER}"

want_mask() {
	local list="$1" expect="$2" got
	got="$(rcow_default_tgt_cpumask "${list}" 2>/dev/null)" \
		|| fail "rcow_default_tgt_cpumask ${list} failed"
	[[ "${got}" == "${expect}" ]] \
		|| fail "rcow_default_tgt_cpumask ${list}: got ${got}, want ${expect}"
}

want_fail() {
	local list="$1"
	if rcow_default_tgt_cpumask "${list}" >/dev/null 2>&1; then
		fail "rcow_default_tgt_cpumask ${list} should fail"
	fi
}

want_mask "0" "0x1"
want_mask "0,1" "0x3"
want_mask "0-1" "0x3"
want_mask "0-3" "0xc"
want_mask "0-7" "0xc0"
want_mask "0-15" "0xc000"
want_mask "6,7" "0xc0"
want_mask "0,2,4-6" "0x60"
want_mask "" "0x1"
want_mask "0-127" "0xc000000000000000"
want_mask "62-65" "0xc000000000000000"
want_fail "64-127"
want_fail "64,65"

live="$(rcow_default_tgt_cpumask 2>/dev/null)"
[[ "${live}" == 0x* ]] || fail "live mask must be hex, got ${live}"

echo "result: 14 passed, 0 failed"
