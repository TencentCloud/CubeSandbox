#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

RCOW_LVS_NAME=bstore-recovery-test
RCOW_ACTIVE_FILE="${tmp}/active"
RCOW_BSTORE_FILE="${tmp}/bstore.json"
# shellcheck source=../../scripts/rcow_common.sh
source "${ROOT}/scripts/rcow_common.sh"

passed=0
failed=0
expect_status() {
	local name="$1" want="$2" got=0
	shift 2
	"$@" >/dev/null 2>&1 || got=$?
	if [ "${got}" -eq "${want}" ]; then
		printf '\t[PASS] %s\n' "${name}"
		passed=$((passed + 1))
	else
		printf '\t[FAIL] %s: got %s, want %s\n' "${name}" "${got}" "${want}"
		failed=$((failed + 1))
	fi
}

echo "=== test_bstore_recovery ==="
expect_status "absent registry" 1 rcow_bstore_entry target

printf '%s\n' '{"other":{"bs_name":"bstore_other"}}' >"${RCOW_BSTORE_FILE}"
expect_status "valid registry missing entry" 2 rcow_bstore_entry target

printf '%s\n' '{"target":' >"${RCOW_BSTORE_FILE}"
expect_status "truncated registry is corrupt" 3 rcow_bstore_entry target

printf '%s\n' '{"target":{"ns_name":"bucket"}}' >"${RCOW_BSTORE_FILE}"
expect_status "malformed entry is corrupt" 3 rcow_bstore_entry target

printf '%s\n' \
	'{"target":{"bs_name":"bstore_1","ns_name":"bucket","wal_bdev":"wal0"}}' \
	>"${RCOW_BSTORE_FILE}"
entry="$(rcow_bstore_entry target)"
if [ "${entry}" = "bucket wal0" ]; then
	printf '\t[PASS] valid entry\n'
	passed=$((passed + 1))
else
	printf '\t[FAIL] valid entry: %s\n' "${entry}"
	failed=$((failed + 1))
fi

printf '=== result: %d passed, %d failed ===\n' "${passed}" "${failed}"
[ "${failed}" -eq 0 ]
