#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for optional reactor-mask argument construction.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMMON="${ROOT}/scripts/rcow_common.sh"
START="${ROOT}/scripts/rcow_start.sh"

fail() {
	echo "FAIL: $*" >&2
	echo "result: 0 passed, 1 failed"
	exit 1
}

[[ -f "${COMMON}" ]] || fail "missing ${COMMON}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

RCOW_LVS_NAME=cpumask-test
RCOW_ACTIVE_FILE="${tmp}/active_lvols"
RCOW_BSTORE_FILE="${tmp}/bstore.json"
unset RCOW_TGT_CPUMASK
# shellcheck source=../../scripts/rcow_common.sh
source "${COMMON}"

rcow_set_tgt_cpumask_args
[[ "${#RCOW_TGT_CPUMASK_ARGS[@]}" -eq 0 ]] \
	|| fail "unset mask must not add -m"

RCOW_TGT_CPUMASK=""
rcow_set_tgt_cpumask_args
[[ "${#RCOW_TGT_CPUMASK_ARGS[@]}" -eq 0 ]] \
	|| fail "empty mask must not add -m"

RCOW_TGT_CPUMASK=0x30
rcow_set_tgt_cpumask_args
[[ "${#RCOW_TGT_CPUMASK_ARGS[@]}" -eq 2 ]] \
	|| fail "hex mask must add exactly two arguments"
[[ "${RCOW_TGT_CPUMASK_ARGS[0]}" == "-m" &&
   "${RCOW_TGT_CPUMASK_ARGS[1]}" == "0x30" ]] \
	|| fail "hex mask was not preserved"

RCOW_TGT_CPUMASK='[4,9]'
rcow_set_tgt_cpumask_args
[[ "${#RCOW_TGT_CPUMASK_ARGS[@]}" -eq 2 ]] \
	|| fail "CPU list must remain one mask argument"
[[ "${RCOW_TGT_CPUMASK_ARGS[0]}" == "-m" &&
   "${RCOW_TGT_CPUMASK_ARGS[1]}" == "[4,9]" ]] \
	|| fail "CPU list was not preserved"

grep -Fq '${RCOW_TGT_CPUMASK_ARGS[@]+"${RCOW_TGT_CPUMASK_ARGS[@]}"}' "${START}" \
	|| fail "rcow_start.sh must expand the optional mask argument array safely"
if grep -Eq -- '-m[[:space:]]+"\$\{RCOW_TGT_CPUMASK\}"' "${START}"; then
	fail "rcow_start.sh still passes the mask unconditionally"
fi

echo "rcow CPU mask tests OK"
echo "result: 9 passed, 0 failed"
