# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Sourced by make_release.sh and the ISA baseline tests. Not shipped in the
#  release package -- scripts/ is what runs on a deployed node.
#
#  A package built with SPDK's default --target-arch=native follows the build
#  host CPU. On a recent Intel laptop that is tigerlake (VAES + VPCLMULQDQ);
#  DPDK EAL then refuses to start on cloud VMs that do not expose those flags.
#  Refuse that here, without starting the target: builder --skip-smoke would
#  otherwise ship it.

s3lvol_read_spdk_target_arch() {
	local root="$1" conf arch

	if [ -f "${root}/.s3lvol-spdk-arch" ]; then
		tr -d '[:space:]' <"${root}/.s3lvol-spdk-arch"
		return 0
	fi
	conf="${root}/mk/config.mk"
	if [ -f "${conf}" ]; then
		arch="$(sed -n 's/^CONFIG_ARCH=\(.*\)$/\1/p' "${conf}" | head -1 | tr -d '[:space:]')"
		if [ -n "${arch}" ]; then
			printf '%s\n' "${arch}"
			return 0
		fi
	fi
	return 1
}

# DPDK compile-time required-flag names. A naive `strings` of s3lvol_tgt also
# hits EAL's full CPU-flag name table, so only scan headers / fixtures that
# list RTE_COMPILE_TIME_CPUFLAGS (or an equivalent required-flag list).
s3lvol_isa_token_forbidden() {
	# RTE_CPUFLAG_VPCLMULQDQ / RTE_CPUFLAG_VAES use an underscore prefix;
	# do not require a non-identifier boundary before the name.
	grep -Eiq 'VPCLMULQDQ|([^A-Za-z]|^)VAES([^A-Za-z]|$)' "$1"
}

s3lvol_verify_portable_isa() {
	local root="$1"
	local arch f
	shift

	arch="$(s3lvol_read_spdk_target_arch "${root}" 2>/dev/null || true)"
	if [ -z "${arch}" ]; then
		printf '%s\n' "cannot determine SPDK CONFIG_ARCH under ${root} (expected .s3lvol-spdk-arch or mk/config.mk)" >&2
		return 1
	fi
	if [ "${arch}" = "native" ]; then
		printf '%s\n' "SPDK was configured with --target-arch=native; a release package would only run on the build CPU. Rebuild with the default (haswell on x86_64, armv8.2-a+crypto on aarch64) or set SPDK_TARGET_ARCH." >&2
		return 1
	fi

	for f in \
		"${root}/include/rte_build_config.h" \
		"${root}/dpdk/build/rte_build_config.h" \
		"${root}/dpdk/config/rte_config.h" \
		"$@"; do
		[ -n "${f}" ] && [ -f "${f}" ] || continue
		if s3lvol_isa_token_forbidden "${f}"; then
			printf '%s\n' "${f} lists VPCLMULQDQ or VAES as a compile-time CPU flag; that is the tigerlake-native signature and will fail on cloud VMs that do not expose those instructions." >&2
			return 1
		fi
	done
	return 0
}
