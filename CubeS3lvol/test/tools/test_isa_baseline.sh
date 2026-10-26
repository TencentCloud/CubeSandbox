#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Mock tests for the portable SPDK --target-arch pin and make_release ISA gate.
# Does not build or install SPDK.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SETUP="${ROOT}/setup_dep.sh"
# shellcheck source=../../mk/s3lvol_isa_gate.sh
. "${ROOT}/mk/s3lvol_isa_gate.sh"

fail() {
	echo "FAIL: $*" >&2
	echo "result: 0 passed, 1 failed"
	exit 1
}

got="$(S3LVOL_HOST_MACHINE=x86_64 "${SETUP}" --print-target-arch)"
[[ "${got}" == "haswell" ]] || fail "x86_64 default arch (got ${got})"

got="$(S3LVOL_HOST_MACHINE=x86_64 "${SETUP}" --print-configure-args)"
[[ "${got}" == *"--target-arch=haswell"* ]] \
	|| fail "x86_64 configure args must pin haswell (got ${got})"
[[ "${got}" != *"--target-arch=native"* ]] \
	|| fail "x86_64 default must not pass --target-arch=native (got ${got})"

got="$(S3LVOL_HOST_MACHINE=aarch64 "${SETUP}" --print-target-arch)"
[[ "${got}" == "armv8.2-a+crypto" ]] || fail "aarch64 default arch (got ${got})"

got="$(S3LVOL_HOST_MACHINE=aarch64 "${SETUP}" --print-configure-args)"
[[ "${got}" == *"--target-arch=armv8.2-a+crypto"* ]] \
	|| fail "aarch64 configure args must pin armv8.2-a+crypto (got ${got})"

got="$(SPDK_TARGET_ARCH=native S3LVOL_HOST_MACHINE=x86_64 \
	"${SETUP}" --print-configure-args)"
[[ "${got}" == *"--target-arch=native"* ]] \
	|| fail "SPDK_TARGET_ARCH=native must flow into configure args (got ${got})"

got="$(SPDK_CONFIGURE_ARGS='--enable-debug --without-nvme-cuse' \
	"${SETUP}" --print-configure-args)"
[[ "${got}" == "--enable-debug --without-nvme-cuse" ]] \
	|| fail "SPDK_CONFIGURE_ARGS must replace the default list outright (got ${got})"

tmp="$(mktemp -d)"
cleanup() { rm -rf "${tmp}"; }
trap cleanup EXIT

mkdir -p "${tmp}/ok/mk"
printf 'CONFIG_ARCH=haswell\n' >"${tmp}/ok/mk/config.mk"
s3lvol_verify_portable_isa "${tmp}/ok" \
	|| fail "haswell CONFIG_ARCH must pass the ISA gate"

mkdir -p "${tmp}/native/mk"
printf 'CONFIG_ARCH=native\n' >"${tmp}/native/mk/config.mk"
if s3lvol_verify_portable_isa "${tmp}/native" 2>/dev/null; then
	fail "CONFIG_ARCH=native must fail the ISA gate"
fi

mkdir -p "${tmp}/stamped"
printf 'native\n' >"${tmp}/stamped/.s3lvol-spdk-arch"
if s3lvol_verify_portable_isa "${tmp}/stamped" 2>/dev/null; then
	fail ".s3lvol-spdk-arch=native must fail the ISA gate"
fi
printf 'haswell\n' >"${tmp}/stamped/.s3lvol-spdk-arch"
s3lvol_verify_portable_isa "${tmp}/stamped" \
	|| fail ".s3lvol-spdk-arch=haswell must pass the ISA gate"

printf '#define RTE_COMPILE_TIME_CPUFLAGS RTE_CPUFLAG_AVX2,RTE_CPUFLAG_VPCLMULQDQ\n' \
	>"${tmp}/vpclmul.h"
if s3lvol_verify_portable_isa "${tmp}/ok" "${tmp}/vpclmul.h" 2>/dev/null; then
	fail "VPCLMULQDQ compile-time flags fixture must fail the ISA gate"
fi

printf '#define RTE_COMPILE_TIME_CPUFLAGS RTE_CPUFLAG_AVX2,RTE_CPUFLAG_VAES\n' \
	>"${tmp}/vaes.h"
if s3lvol_verify_portable_isa "${tmp}/ok" "${tmp}/vaes.h" 2>/dev/null; then
	fail "VAES compile-time flags fixture must fail the ISA gate"
fi

printf '#define RTE_COMPILE_TIME_CPUFLAGS RTE_CPUFLAG_AVX2,RTE_CPUFLAG_SSE4_2\n' \
	>"${tmp}/okflags.h"
s3lvol_verify_portable_isa "${tmp}/ok" "${tmp}/okflags.h" \
	|| fail "AVX2-only compile-time flags must pass the ISA gate"

echo "isa baseline tests OK"
echo "result: 14 passed, 0 failed"
