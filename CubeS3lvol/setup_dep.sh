#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Set up this repository's build dependencies under deps/.
#
#  Run this once on a new machine, then build:
#
#      ./setup_dep.sh
#      make && make check-offline
#
#  Usage:
#    ./setup_dep.sh                 # everything that is missing
#    ./setup_dep.sh --check         # report only, change nothing
#    ./setup_dep.sh spdk            # just that one (spdk, aws)
#    ./setup_dep.sh --force aws     # rebuild even if it looks done
#    ./setup_dep.sh --jobs 8        # default is nproc
#    ./setup_dep.sh --print-stamp spdk|aws
#    ./setup_dep.sh --emit-builder-prebuilt   # image build only
#
#  Environment, for when the defaults are wrong:
#    AWS_BUILD_TYPE        CMake build type for the CRT. Debug by default,
#                          matching --enable-debug for SPDK; a release package
#                          wants AWS_BUILD_TYPE=RelWithDebInfo --force aws.
#    SPDK_CONFIGURE_ARGS   replaces SPDK's ./configure arguments outright
#    SPDK_COMMIT           the pinned upstream baseline
#
#  Two dependencies, installed under deps/ unless the builder image has a
#  matching prebuilt (stamped by pin + patches / CRT tags):
#    spdk  pinned commit, patched, configured, built -> /opt/s3lvol-spdk
#    aws   ten AWS CRT projects at pinned tags -> /opt/s3lvol-aws-{debug,relwithdebinfo}
#
#  === Why deps/ rather than a sibling directory ===
#
#  The build used to look for SPDK at ../spdk, which works only if whoever cloned
#  this repository also knew to clone SPDK next to it, at the right commit, with
#  the patches applied and configured the same way. None of that is discoverable
#  from the repository, and every part of it has to be right: a missing patch shows
#  up as an implicit declaration deep in a build log, a different ./configure shows
#  up as an undefined symbol at link time.
#
#  deps/ is git-ignored, so the tree stays clean, and mk/s3lvol.common.mk prefers
#  deps/spdk over ../spdk. An existing ../spdk still works and still wins if
#  SPDK_ROOT says so -- this is meant to remove a manual step, not to invalidate
#  the setup anyone already has.
#
#  === Why a pinned commit and not a tag or master ===
#
#  SPDK has no release that contains what this project needs, so the baseline is a
#  commit on master. Pinning it is what makes the build reproducible: master moves
#  several times a day, and the failure from building against a newer one is not
#  "SPDK is too new" but a patch that no longer applies, or a silently changed
#  behaviour in a function we do not own. Bumping the pin is a deliberate act with
#  a test run attached, which is exactly what it should be.
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPS_DIR="${SCRIPT_DIR}/deps"

# ---------------------------------------------------------------------------
# SPDK
#
# The commit is the *upstream* baseline, before this repository's patches. Note
# that a tree set up by hand may sit one commit ahead of it, because patches/
# can be applied with `git am` as well as `git apply`, and the former leaves a
# commit behind. So this is not the value `git describe` will print afterwards.
# ---------------------------------------------------------------------------
SPDK_REPO="${SPDK_REPO:-https://github.com/spdk/spdk.git}"
# Full SHA: GitHub will not fetch an abbreviated commit as a remote ref
# (`git fetch --depth 1 origin d64c4fa89` -> "couldn't find remote ref").
SPDK_COMMIT="${SPDK_COMMIT:-d64c4fa89233397460e2e4ff55a1c69b8e498598}"

# What ./configure was given. Deliberately short:
#
#   --enable-debug     asserts on. This project is pre-production and an assert
#                      firing in SPDK is worth far more than the cycles it costs.
#                      Turning it off is a release-packaging decision, not a
#                      developer-setup one, so it is not the default here.
#   --without-nvme-cuse avoids a libfuse3 dependency for a feature nothing here
#                      touches: CUSE exposes NVMe devices as character devices,
#                      and this target's only local bdev is aio.
#
# Everything else is left at SPDK's defaults, including CONFIG_SHARED=n, which is
# what makes the static linking in mk/s3lvol.common.mk possible.
SPDK_CONFIGURE_ARGS="${SPDK_CONFIGURE_ARGS:---enable-debug --without-nvme-cuse}"

JOBS="$(nproc 2>/dev/null || echo 4)"
MODE="setup"
FORCE=0
WANTED=""
PRINT_STAMP=""

S3LVOL_SPDK_PREBUILT="${S3LVOL_SPDK_PREBUILT:-/opt/s3lvol-spdk}"
S3LVOL_AWS_PREBUILT_DEBUG="${S3LVOL_AWS_PREBUILT_DEBUG:-/opt/s3lvol-aws-debug}"
S3LVOL_AWS_PREBUILT_RELWITHDEBINFO="${S3LVOL_AWS_PREBUILT_RELWITHDEBINFO:-/opt/s3lvol-aws-relwithdebinfo}"

while [ $# -gt 0 ]; do
	case "$1" in
	--check)   MODE="check" ;;
	--force)   FORCE=1 ;;
	--jobs)    shift; JOBS="${1:-}" ;;
	--jobs=*)  JOBS="${1#*=}" ;;
	--print-stamp)
		shift
		PRINT_STAMP="${1:-}"
		MODE="print-stamp"
		;;
	--print-stamp=*)
		PRINT_STAMP="${1#*=}"
		MODE="print-stamp"
		;;
	--emit-builder-prebuilt)
		MODE="emit-builder-prebuilt"
		;;
	-h|--help)
		sed -n '2,/^set -u/p' "${BASH_SOURCE[0]}" | sed 's/^#//;s/^ //'
		exit 0
		;;
	spdk|aws)  WANTED="${WANTED} $1" ;;
	-*)        echo "unknown option: $1" >&2; exit 1 ;;
	*)         echo "unknown dependency: $1 (known: spdk aws)" >&2; exit 1 ;;
	esac
	shift
done
# Order matters only in that spdk takes far longer, so aws failing on a missing
# cmake is worth learning before the twenty-minute build, not after. check_tools
# covers that, and this keeps the reporting order stable.
[ -n "${WANTED}" ] || WANTED="spdk aws"

# SPDK's genrpc.py needs python >= 3.9 and DPDK needs meson >= 0.57.2. The
# shared builder image keeps that toolchain in /opt/s3lvol-tools (a python3.9
# venv) instead of shadowing the system interpreter for the Go/Rust/kernel
# tracks; put it on PATH for this script only. Outside the builder the system
# python/meson are used as-is.
if [ -d /opt/s3lvol-tools/bin ]; then
	export PATH="/opt/s3lvol-tools/bin:${PATH}"
fi

if ! printf '%s' "${JOBS}" | grep -qE '^[0-9]+$' || [ "${JOBS}" -lt 1 ]; then
	echo "error: --jobs wants a positive integer, got '${JOBS}'" >&2
	exit 1
fi

log()  { printf '\033[0;32m[dep]\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33m[dep]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[0;31m[dep]\033[0m %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

# ---------------------------------------------------------------------------
# Preconditions
#
# Checked up front and all at once, rather than letting the first one fail
# fifteen minutes into a build. The package names differ per distribution, so
# what is printed is the command that is missing plus one suggestion, not a
# promise about the package manager.
# ---------------------------------------------------------------------------
check_tools()
{
	local missing="" t
	for t in git make gcc pkg-config python3 patch; do
		command -v "${t}" >/dev/null 2>&1 || missing="${missing} ${t}"
	done

	# cmake and ar only matter for the CRT, and g++ with them: aws-c-common's
	# CMakeLists does project(... C CXX), so a machine with only a C compiler
	# fails at configure time with a message about CXX rather than about the
	# missing package.
	case " ${WANTED} " in
	*" aws "*)
		for t in cmake ar g++; do
			command -v "${t}" >/dev/null 2>&1 || missing="${missing} ${t}"
		done
		# s2n-tls and aws-c-cal (built with USE_OPENSSL=ON) both need the
		# OpenSSL headers, not just the runtime that is on every machine.
		if ! pkg-config --exists openssl 2>/dev/null &&
		   [ ! -f /usr/include/openssl/ssl.h ]; then
			missing="${missing} openssl-devel(headers)"
		fi
		;;
	esac

	if [ -n "${missing}" ]; then
		err "missing tools:${missing}"
		err "on CentOS/TencentOS: yum install -y git make gcc gcc-c++ cmake \\"
		err "                binutils pkgconfig python3 patch openssl-devel"
		err "on Debian/Ubuntu:    apt install -y git make gcc g++ cmake \\"
		err "                         binutils pkg-config python3 patch libssl-dev"
		return 1
	fi
	return 0
}

# ---------------------------------------------------------------------------
# spdk
# ---------------------------------------------------------------------------
SPDK_DIR="${SPDK_DIR:-${DEPS_DIR}/spdk}"

digest_sha256()
{
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
	else
		openssl dgst -sha256 | awk '{print $NF}'
	fi
}

# Hash file contents only. `sha256sum FILE` / `openssl dgst FILE` embed the
# absolute path, and the stamp is computed from three directories that never
# agree (image build /tmp/s3lvol-dep, CI /workspace/CubeS3lvol, a developer
# checkout). Hashing the path made the prebuilt SPDK look stale everywhere.
file_content_digest()
{
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum < "$1" | awk '{print $1}'
	else
		openssl dgst -sha256 < "$1" | awk '{print $NF}'
	fi
}

spdk_patches_hash()
{
	local f
	{
		if [ -f "${SCRIPT_DIR}/patches/apply.sh" ]; then
			file_content_digest "${SCRIPT_DIR}/patches/apply.sh"
		fi
		for f in "${SCRIPT_DIR}"/patches/[0-9]*.patch; do
			[ -f "${f}" ] || continue
			file_content_digest "${f}"
		done
	} | digest_sha256
}

spdk_recipe_stamp()
{
	printf 'commit=%s\nconfigure=%s\npatches=%s\n' \
		"${SPDK_COMMIT}" "${SPDK_CONFIGURE_ARGS}" "$(spdk_patches_hash)" \
		| digest_sha256
}

spdk_stamp_file()
{
	echo "${1:-${SPDK_DIR}}/.s3lvol-spdk-stamp"
}

spdk_write_stamp()
{
	local dir="${1:-${SPDK_DIR}}"
	printf '%s\n' "$(spdk_recipe_stamp)" >"$(spdk_stamp_file "${dir}")" || return 1
	# slim_spdk_tree drops .git, so make_release.sh reads this pin instead.
	printf '%s\n' "${SPDK_COMMIT}" >"${dir}/.s3lvol-spdk-commit" || return 1
}

# Being "done" means the libraries this repository links are actually there, not
# that the directory exists. A checkout interrupted halfway, or configured but
# never built, would otherwise be reported as ready and then fail at link time
# with a message about aws-c-s3 or spdk_bdev that says nothing about the cause.
spdk_is_built()
{
	local dir="${1:-${SPDK_DIR}}"
	[ -f "${dir}/build/lib/libspdk_bdev.a" ] &&
	[ -f "${dir}/build/lib/libspdk_env_dpdk.a" ] &&
	[ -f "${dir}/dpdk/build/lib/librte_eal.a" ] &&
	[ -f "${dir}/include/spdk/blob.h" ]
}

spdk_tree_ready()
{
	local dir="${1:-${SPDK_DIR}}" stamp

	spdk_is_built "${dir}" || return 1
	if [ -f "$(spdk_stamp_file "${dir}")" ]; then
		stamp="$(tr -d '[:space:]' <"$(spdk_stamp_file "${dir}")")"
		[ "${stamp}" = "$(spdk_recipe_stamp)" ]
		return $?
	fi
	# Legacy checkout from before stamps existed: require a git worktree
	# whose patches still apply.
	[ -d "${dir}/.git" ] || return 1
	SPDK_ROOT="${dir}" "${SCRIPT_DIR}/patches/apply.sh" --check >/dev/null 2>&1
}

prebuilt_spdk_usable()
{
	[ "${FORCE}" -eq 0 ] || return 1
	spdk_tree_ready "${S3LVOL_SPDK_PREBUILT}"
}

spdk_check()
{
	if spdk_tree_ready "${SPDK_DIR}"; then
		echo "  spdk: built (${SPDK_DIR})"
		return 0
	fi
	if prebuilt_spdk_usable; then
		echo "  spdk: prebuilt (${S3LVOL_SPDK_PREBUILT}, stamp match)"
		return 0
	fi
	if [ -d "${SPDK_DIR}/.git" ]; then
		local head patched=no
		head="$(git -C "${SPDK_DIR}" rev-parse --short HEAD 2>/dev/null || echo '?')"
		if SPDK_ROOT="${SPDK_DIR}" "${SCRIPT_DIR}/patches/apply.sh" --check >/dev/null 2>&1; then
			patched=yes
		fi
		echo "  spdk: present but not ready (HEAD ${head}, patches applied: ${patched})"
		return 1
	fi
	echo "  spdk: absent (will clone ${SPDK_REPO} at ${SPDK_COMMIT})"
	return 1
}

# Static DPDK archives are what this repository links (see
# mk/s3lvol.common.mk), and SPDK's own default build produces them. This is
# checked rather than assumed, because a DPDK configured shared-only would let
# `make` here succeed and then fail at the link with "cannot find librte_eal.a".
spdk_fetch()
{
	if [ -d "${SPDK_DIR}/.git" ]; then
		log "spdk: reusing ${SPDK_DIR}"
	else
		if [ -d "${SPDK_DIR}" ] && [ -z "$(ls -A "${SPDK_DIR}" 2>/dev/null)" ]; then
			rmdir "${SPDK_DIR}" || return 1
		elif [ -e "${SPDK_DIR}" ]; then
			err "spdk: ${SPDK_DIR} exists but is not a git checkout."
			err "      Remove it and rerun, or set SPDK_DIR to a new path."
			return 1
		fi
		mkdir -p "$(dirname "${SPDK_DIR}")" || return 1
		# Prefer a one-commit fetch. That needs a ref the remote will
		# advertise -- a full SHA on GitHub, a tag, or a branch. An
		# abbreviated hash is not a remote ref and fails here; then
		# fall back to a normal clone (the pin is on master).
		log "spdk: fetching ${SPDK_REPO} at ${SPDK_COMMIT}"
		git init --quiet "${SPDK_DIR}" || return 1
		git -C "${SPDK_DIR}" remote add origin "${SPDK_REPO}" || return 1
		if git -C "${SPDK_DIR}" fetch --depth 1 origin "${SPDK_COMMIT}"; then
			git -C "${SPDK_DIR}" checkout --quiet --detach FETCH_HEAD || return 1
		else
			log "spdk: remote has no ref ${SPDK_COMMIT}; cloning"
			rm -rf "${SPDK_DIR}"
			git clone "${SPDK_REPO}" "${SPDK_DIR}" || return 1
		fi
	fi

	# Fetched only when the pinned commit is not already present, so that
	# re-running this offline works once the clone exists.
	if ! git -C "${SPDK_DIR}" cat-file -e "${SPDK_COMMIT}^{commit}" 2>/dev/null; then
		log "spdk: fetching ${SPDK_COMMIT}"
		git -C "${SPDK_DIR}" fetch --depth 1 origin "${SPDK_COMMIT}" || \
			git -C "${SPDK_DIR}" fetch origin "${SPDK_COMMIT}" || \
			git -C "${SPDK_DIR}" fetch --tags origin || return 1
	fi

	local head want
	head="$(git -C "${SPDK_DIR}" rev-parse HEAD 2>/dev/null || true)"
	want="$(git -C "${SPDK_DIR}" rev-parse "${SPDK_COMMIT}^{commit}" 2>/dev/null || true)"
	if [ -n "${want}" ] && [ "${head}" != "${want}" ]; then
		# Refuse to throw away local work. Someone may be carrying a fix
		# they have not upstreamed, and `git checkout --force` would take
		# it away silently.
		if [ -n "$(git -C "${SPDK_DIR}" status --porcelain 2>/dev/null)" ]; then
			err "spdk: ${SPDK_DIR} has uncommitted changes and is not at"
			err "      ${SPDK_COMMIT}. Refusing to touch it -- commit, stash"
			err "      or remove the directory, then rerun."
			return 1
		fi
		log "spdk: checking out ${SPDK_COMMIT}"
		git -C "${SPDK_DIR}" checkout --quiet --detach "${SPDK_COMMIT}" || return 1
	fi

	# --depth 1 on the submodules: DPDK's history is larger than SPDK's own and
	# nothing here needs it. The pin lives in SPDK's gitlink, so this stays
	# reproducible.
	log "spdk: updating submodules (dpdk, isa-l and friends)"
	git -C "${SPDK_DIR}" submodule update --init --recursive --depth 1 || return 1
	return 0
}

spdk_patch()
{
	log "spdk: applying patches/"
	SPDK_ROOT="${SPDK_DIR}" "${SCRIPT_DIR}/patches/apply.sh" || return 1
	return 0
}

spdk_build()
{
	# Ubuntu 20.04's gcc-9 on aarch64 does not ship arm_sve.h; ISA-L's SVE
	# sources include it. gcc-10 does. Leave CC/CXX alone if the caller set them.
	if [ "$(uname -m)" = aarch64 ] && command -v gcc-10 >/dev/null 2>&1; then
		export CC="${CC:-gcc-10}"
		export CXX="${CXX:-g++-10}"
		log "spdk: using ${CC} (ISA-L SVE needs arm_sve.h)"
	fi

	if [ ! -f "${SPDK_DIR}/mk/config.mk" ] || [ "${FORCE}" -eq 1 ]; then
		log "spdk: ./configure ${SPDK_CONFIGURE_ARGS}"
		# Not word-split by accident: the arguments are a list, and the
		# variable exists so that a caller can pass a different list.
		# shellcheck disable=SC2086
		(cd "${SPDK_DIR}" && ./configure ${SPDK_CONFIGURE_ARGS}) || {
			err "spdk: ./configure failed. It names the library it could not"
			err "      find; SPDK's own scripts/pkgdep.sh installs the lot:"
			err "        ${SPDK_DIR}/scripts/pkgdep.sh"
			return 1
		}
	else
		log "spdk: already configured (mk/config.mk exists; --force to redo)"
	fi

	log "spdk: make -j${JOBS} (ten to thirty minutes on a first build)"
	make -C "${SPDK_DIR}" -j"${JOBS}" || return 1

	spdk_is_built || {
		err "spdk: the build reported success but the libraries this project"
		err "      links are not all there. Expected under ${SPDK_DIR}:"
		err "        build/lib/libspdk_bdev.a, build/lib/libspdk_env_dpdk.a,"
		err "        dpdk/build/lib/librte_eal.a"
		return 1
	}
	return 0
}

setup_spdk()
{
	if [ "${FORCE}" -eq 0 ]; then
		if spdk_tree_ready "${SPDK_DIR}"; then
			log "spdk: already set up at ${SPDK_DIR}, nothing to do (--force to rebuild)"
			return 0
		fi
		# Image prebuilt is only a substitute when we would otherwise
		# populate deps/spdk. An explicit SPDK_DIR is left alone.
		if [ "${SPDK_DIR}" = "${DEPS_DIR}/spdk" ] && prebuilt_spdk_usable; then
			log "spdk: using builder prebuilt at ${S3LVOL_SPDK_PREBUILT}"
			return 0
		fi
		if spdk_is_built "${SPDK_DIR}"; then
			warn "spdk: built, but stamp/patches do not match -- applying and rebuilding"
		fi
	fi

	spdk_fetch  || return 1
	spdk_patch  || return 1
	spdk_build  || return 1
	spdk_write_stamp "${SPDK_DIR}" || return 1
	log "spdk: ready at ${SPDK_DIR}"
	return 0
}

# ---------------------------------------------------------------------------
# aws-c-s3 (the AWS CRT)
#
# Ten separate CMake projects, each pinned to a tag, built in dependency order
# into one prefix, and then archived into a single libaws.a. Derived from the
# aws_build.sh and ar.sh that were being run by hand.
#
# === Why the order is a list and not a loop over a set ===
#
# Each project finds the previous ones through CMAKE_PREFIX_PATH pointing at the
# prefix they were just installed into, so this is a topological order, not a
# preference. Building aws-c-io before aws-c-cal fails at configure time. The
# order below is the one that works; the dependency each entry adds is noted so
# that it can be re-derived rather than trusted.
#
# === Why one libaws.a and not ten -l flags ===
#
# The ten are interdependent, so linking them by name means either getting the
# order right on the link line or wrapping them in --start-group. A single
# archive makes the question go away, and mk/s3lvol.common.mk already prefers
# lib64/libaws.a when it is there. `ar -M` is what merges them: it can ADDLIB
# whole archives, which plain `ar q` cannot.
# ---------------------------------------------------------------------------
AWS_DIR="${AWS_DIR:-${DEPS_DIR}/aws}"          # prefix: include/, lib64/
AWS_SRC="${AWS_SRC:-${DEPS_DIR}/aws-src}"      # the ten checkouts

# How the CRT is compiled.
#
# Debug by default, which is the same call as --enable-debug for SPDK above and
# for the same reason: this is a pre-production developer setup, and being able
# to walk a stack through the CRT -- where every S3 request is signed, sent and
# parsed -- is worth more right now than the cycles it costs. CMake's Debug is
# `-g` with no -O and no -DNDEBUG, so asserts inside the CRT stay live too.
#
# It is a variable and not a constant because the trade-off inverts for a
# release package. Then:
#
#     AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --force aws
#
# Note it must not be left empty either: CMake's *empty* build type is neither
# optimised nor NDEBUG *nor* -g, and the CRT's CMakeLists does not supply a
# default, so an unset value silently means "no optimisation and no symbols" --
# the worst of both. That was what the hand-written script did.
AWS_BUILD_TYPE="${AWS_BUILD_TYPE:-Debug}"

# Checked here rather than left to CMake, because CMake accepts an unknown build
# type without a word and then compiles with CMAKE_C_FLAGS_<TYPO> -- which does
# not exist, so: no flags. A typo would produce a working but unoptimised,
# symbol-less libaws.a. Matching is case-insensitive, as CMake upper-cases the
# value before looking the flags up.
case "$(printf '%s' "${AWS_BUILD_TYPE}" | tr 'A-Z' 'a-z')" in
debug | release | relwithdebinfo | minsizerel) ;;
*)
	echo "error: AWS_BUILD_TYPE wants Debug, Release, RelWithDebInfo or" \
	     "MinSizeRel, got '${AWS_BUILD_TYPE}'" >&2
	exit 1
	;;
esac

# name|tag|repo|extra cmake flags
#
# Tags rather than commits, because these are released libraries with real
# version numbers -- unlike SPDK, where the baseline had to be a point on master.
#
# s2n-tls is the TLS implementation aws-c-io uses on Linux; without it aws-c-io
# configures but has no TLS, and every https:// request fails at handshake.
AWS_COMPONENTS="
s2n-tls|v1.5.25|https://github.com/aws/s2n-tls.git|
aws-c-common|v0.12.4|https://github.com/awslabs/aws-c-common.git|
aws-checksums|v0.2.6|https://github.com/awslabs/aws-checksums.git|
aws-c-cal|v0.9.2|https://github.com/awslabs/aws-c-cal.git|-DUSE_OPENSSL=ON
aws-c-io|v0.22.0|https://github.com/awslabs/aws-c-io.git|
aws-c-compression|v0.3.1|https://github.com/awslabs/aws-c-compression.git|
aws-c-http|v0.10.4|https://github.com/awslabs/aws-c-http.git|
aws-c-sdkutils|v0.2.4|https://github.com/awslabs/aws-c-sdkutils.git|
aws-c-auth|v0.9.1|https://github.com/awslabs/aws-c-auth.git|
aws-c-s3|v0.8.7|https://github.com/awslabs/aws-c-s3.git|
"

# CMake flags that are always passed. Part of the AWS recipe stamp so a change
# here invalidates the builder prebuilt the same way a tag bump does.
AWS_CMAKE_FIXED_FLAGS="BUILD_SHARED_LIBS=OFF
CMAKE_POSITION_INDEPENDENT_CODE=ON
BUILD_TESTING=OFF"

aws_recipe_stamp()
{
	{
		printf '%s\n' "${AWS_COMPONENTS}" | awk -F'|' 'NF && $1 != "" { print $1 "|" $2 "|" $4 }'
		printf '%s\n' "${AWS_CMAKE_FIXED_FLAGS}"
	} | digest_sha256
}

aws_prefix_stamp()
{
	printf 'type=%s\nrecipe=%s\n' "${AWS_BUILD_TYPE}" "$(aws_recipe_stamp)" | digest_sha256
}

aws_prebuilt_dir()
{
	case "$(printf '%s' "${AWS_BUILD_TYPE}" | tr 'A-Z' 'a-z')" in
	debug) echo "${S3LVOL_AWS_PREBUILT_DEBUG}" ;;
	relwithdebinfo) echo "${S3LVOL_AWS_PREBUILT_RELWITHDEBINFO}" ;;
	*) echo "" ;;
	esac
}

# The order ar.sh used. Not the build order and it does not need to be: ar -M
# merges object files, and the linker resolves between them afterwards.
AWS_ARCHIVE_LIBS="aws-c-auth aws-c-cal aws-c-common aws-c-compression \
aws-checksums aws-c-http aws-c-io aws-c-s3 aws-c-sdkutils s2n"

# Where CMake decides to put libraries varies (lib on Debian, lib64 on RHEL and
# its derivatives), and it is decided per project by GNUInstallDirs. So it is
# discovered rather than assumed -- guessing wrong here means an -L to nowhere
# and a link failure that says nothing about the cause.
aws_libdir()
{
	local dir="${1:-${AWS_DIR}}"
	if [ -d "${dir}/lib64" ] && ls "${dir}"/lib64/*.a >/dev/null 2>&1; then
		echo "${dir}/lib64"
	elif [ -d "${dir}/lib" ] && ls "${dir}"/lib/*.a >/dev/null 2>&1; then
		echo "${dir}/lib"
	else
		echo ""
	fi
}

# The build type the prefix was installed with, or "" when it cannot be told --
# which is the case for a prefix built before this stamp existed, or by hand.
# "" is deliberately not treated as a mismatch: rebuilding ten CMake projects
# because of a missing file would be a poor trade for a guess.
aws_stamp_read()
{
	local dir="${1:-${AWS_DIR}}"
	[ -f "${dir}/.build_type" ] || { echo ""; return 0; }
	head -1 "${dir}/.build_type" 2>/dev/null | tr -d '[:space:]'
}

aws_write_stamps()
{
	local dir="${1:-${AWS_DIR}}"
	printf '%s\n' "${AWS_BUILD_TYPE}" >"${dir}/.build_type" || return 1
	printf '%s\n' "$(aws_prefix_stamp)" >"${dir}/.s3lvol-aws-stamp" || return 1
	# slim_aws_prefix drops aws-src/.git; make_release.sh reads the pins here.
	printf '%s\n' "${AWS_COMPONENTS}" | awk -F'|' 'NF && $1 != "" { printf "%s %s\n", $1, $2 }' \
		>"${dir}/.aws-components" || return 1
}

aws_prefix_ready()
{
	local dir="$1" stamp how

	aws_is_built "${dir}" || return 1
	if [ -f "${dir}/.s3lvol-aws-stamp" ]; then
		stamp="$(tr -d '[:space:]' <"${dir}/.s3lvol-aws-stamp")"
		[ "${stamp}" = "$(aws_prefix_stamp)" ]
		return $?
	fi
	how="$(aws_stamp_read "${dir}")"
	[ -z "${how}" ] || [ "${how}" = "${AWS_BUILD_TYPE}" ]
}

prebuilt_aws_usable()
{
	local dir

	[ "${FORCE}" -eq 0 ] || return 1
	dir="$(aws_prebuilt_dir)"
	[ -n "${dir}" ] || return 1
	aws_prefix_ready "${dir}"
}

aws_path_is_managed()
{
	case "$1" in
	"${DEPS_DIR}/aws"|"${DEPS_DIR}/aws-src") return 0 ;;
	"${S3LVOL_AWS_PREBUILT_DEBUG}"|"${S3LVOL_AWS_PREBUILT_RELWITHDEBINFO}") return 0 ;;
	/tmp/s3lvol-aws-src) return 0 ;;
	esac
	return 1
}

# Undo the install so that "is this component's .a present" is once again the
# only thing deciding what gets rebuilt. Used when the build type changes: the
# resume logic is per-component and keyed on the artefact, so the artefacts have
# to go, or nine of the ten would keep their old flags.
#
# The build directories go too. CMake would reconfigure them correctly from the
# command line alone, but they are worthless once the flags change and leaving
# them means the next failure is read against stale logs.
aws_clear_prefix()
{
	local libdir name

	# Both paths are derived from known prefixes and neither is allowed to
	# be bare, because what follows is rm -rf.
	aws_path_is_managed "${AWS_DIR}" || die "refusing to clear '${AWS_DIR}'"
	aws_path_is_managed "${AWS_SRC}" || die "refusing to clear '${AWS_SRC}'"

	libdir="$(aws_libdir)"
	if [ -n "${libdir}" ]; then
		log "aws: removing previously installed libraries from ${libdir}"
		rm -f "${libdir}"/*.a || return 1
	fi
	for name in $(printf '%s\n' "${AWS_COMPONENTS}" | cut -d'|' -f1); do
		[ -n "${name}" ] || continue
		rm -rf "${AWS_SRC}/${name}/build" || return 1
	done
	rm -f "${AWS_DIR}/.build_type" "${AWS_DIR}/.s3lvol-aws-stamp"
	return 0
}

# As with SPDK: "done" means the artefact this repository links is present, plus
# the header it includes. A prefix left half-built by an interrupted run would
# otherwise be reported ready.
aws_is_built()
{
	local dir="${1:-${AWS_DIR}}" libdir
	libdir="$(aws_libdir "${dir}")"
	[ -n "${libdir}" ] &&
	[ -f "${libdir}/libaws.a" ] &&
	[ -f "${dir}/include/aws/s3/s3_client.h" ]
}

aws_check()
{
	local libdir n how dir
	if aws_prefix_ready "${AWS_DIR}"; then
		libdir="$(aws_libdir "${AWS_DIR}")"
		n="$(ar t "${libdir}/libaws.a" 2>/dev/null | wc -l)"
		how="$(aws_stamp_read "${AWS_DIR}")"
		echo "  aws:  built (${libdir}/libaws.a, ${n} objects, ${how:-build type unrecorded})"
		return 0
	fi
	if prebuilt_aws_usable; then
		dir="$(aws_prebuilt_dir)"
		libdir="$(aws_libdir "${dir}")"
		n="$(ar t "${libdir}/libaws.a" 2>/dev/null | wc -l)"
		echo "  aws:  prebuilt (${dir}, ${n} objects, ${AWS_BUILD_TYPE})"
		return 0
	fi
	if aws_is_built "${AWS_DIR}"; then
		libdir="$(aws_libdir "${AWS_DIR}")"
		n="$(ar t "${libdir}/libaws.a" 2>/dev/null | wc -l)"
		how="$(aws_stamp_read "${AWS_DIR}")"
		echo "  aws:  built (${libdir}/libaws.a, ${n} objects, ${how:-build type unrecorded})"
		echo "        wanted ${AWS_BUILD_TYPE}; ./setup_dep.sh aws rebuilds it"
		return 1
	fi
	if [ -d "${AWS_SRC}" ]; then
		echo "  aws:  present but not finished (sources at ${AWS_SRC})"
		return 1
	fi
	# An install elsewhere is worth naming, but it is not a substitute: the
	# build deliberately does not fall back to system prefixes (see the AWS
	# section of mk/s3lvol.common.mk -- the one on this machine carries a
	# local modification). It counts only when it is asked for by name.
	local d
	for d in /usr/local/aws /opt/aws /usr/local; do
		if [ -f "${d}/include/aws/s3/s3_client.h" ]; then
			echo "  aws:  absent under deps/, though there is an install at ${d}"
			echo "        (not used automatically: make AWS_INSTALL_DIR=${d} to"
			echo "         use it anyway, or ./setup_dep.sh aws to build ours)"
			return 1
		fi
	done
	echo "  aws:  absent (will build 10 CRT projects into ${AWS_DIR})"
	return 1
}

# One component: clone at its tag, configure, build, install into the prefix.
#
# Each is skipped when its own library is already installed, so an interrupted
# run resumes instead of starting over -- ten CMake projects is long enough for
# that to matter.
aws_build_one()
{
	local name="$1" tag="$2" repo="$3" extra="$4"
	local src="${AWS_SRC}/${name}"
	local libname libdir

	# s2n-tls installs libs2n.a, everything else installs lib<name>.a.
	case "${name}" in
	s2n-tls) libname="libs2n.a" ;;
	*)       libname="lib${name}.a" ;;
	esac

	libdir="$(aws_libdir)"
	if [ -n "${libdir}" ] && [ -f "${libdir}/${libname}" ] && [ "${FORCE}" -eq 0 ]; then
		log "aws: ${name} ${tag} already installed"
		return 0
	fi

	if [ -d "${src}/.git" ]; then
		# Left as it is when it is already at the right tag. Re-cloning ten
		# repositories to rebuild one is not worth the network.
		local at
		at="$(git -C "${src}" describe --tags --exact-match 2>/dev/null || true)"
		if [ "${at}" != "${tag}" ]; then
			log "aws: ${name} is at '${at:-unknown}', want ${tag}"
			git -C "${src}" fetch --tags origin || return 1
			git -C "${src}" checkout --quiet --detach "${tag}" || return 1
		fi
	else
		log "aws: cloning ${name} ${tag}"
		# --depth 1 with --branch <tag>: only this tag's tree is needed, and
		# these repositories are pinned by tag, so there is nothing to lose.
		git clone --quiet --depth 1 --branch "${tag}" "${repo}" "${src}" || return 1
	fi

	log "aws: building ${name} ${tag}"
	# BUILD_SHARED_LIBS=OFF is explicit rather than relying on the default:
	# this project links the CRT statically (see mk/s3lvol.common.mk), and a
	# shared build would leave libaws.a absent with no other complaint.
	#
	# BUILD_TESTING=OFF because several of these pull in extra dependencies for
	# their test suites, and a failure there would stop a build we only want the
	# library from.
	#
	# CMAKE_POSITION_INDEPENDENT_CODE=ON so the archive can also go into the .so
	# this repository builds; without it, linking libs3lvol_bdev.so against
	# libaws.a fails on relocations.
	#
	# CMAKE_BUILD_TYPE is always passed explicitly -- see AWS_BUILD_TYPE.
	#
	# shellcheck disable=SC2086
	cmake -S "${src}" -B "${src}/build" \
		-DCMAKE_INSTALL_PREFIX="${AWS_DIR}" \
		-DCMAKE_PREFIX_PATH="${AWS_DIR}" \
		-DCMAKE_BUILD_TYPE="${AWS_BUILD_TYPE}" \
		-DBUILD_SHARED_LIBS=OFF \
		-DBUILD_TESTING=OFF \
		-DCMAKE_POSITION_INDEPENDENT_CODE=ON \
		${extra} >"${src}/build.configure.log" 2>&1 || {
		err "aws: cmake configure failed for ${name}"
		err "     last 20 lines of ${src}/build.configure.log:"
		tail -20 "${src}/build.configure.log" >&2
		return 1
	}

	cmake --build "${src}/build" --target install -j "${JOBS}" \
		>"${src}/build.make.log" 2>&1 || {
		err "aws: build failed for ${name}"
		err "     last 20 lines of ${src}/build.make.log:"
		tail -20 "${src}/build.make.log" >&2
		return 1
	}
	return 0
}

# Merge the ten into libaws.a.
#
# Done in a temporary directory and moved into place at the end, so that an
# interrupted merge cannot leave a truncated libaws.a that aws_is_built would
# then accept.
aws_archive()
{
	local libdir tmp missing="" l
	libdir="$(aws_libdir)"
	[ -n "${libdir}" ] || { err "aws: no lib/ or lib64/ under ${AWS_DIR}"; return 1; }

	for l in ${AWS_ARCHIVE_LIBS}; do
		[ -f "${libdir}/lib${l}.a" ] || missing="${missing} lib${l}.a"
	done
	if [ -n "${missing}" ]; then
		err "aws: cannot archive, these are not installed:${missing}"
		return 1
	fi

	log "aws: archiving 10 libraries into libaws.a"
	tmp="$(mktemp -d "${libdir}/.ar.XXXXXX")" || return 1

	{
		echo "CREATE ${tmp}/libaws.a"
		for l in ${AWS_ARCHIVE_LIBS}; do
			echo "ADDLIB ${libdir}/lib${l}.a"
		done
		echo "SAVE"
		echo "END"
	} | ar -M || { rm -rf "${tmp}"; err "aws: ar -M failed"; return 1; }

	if [ ! -s "${tmp}/libaws.a" ]; then
		rm -rf "${tmp}"
		err "aws: ar -M produced nothing"
		return 1
	fi
	mv -f "${tmp}/libaws.a" "${libdir}/libaws.a" || { rm -rf "${tmp}"; return 1; }
	rm -rf "${tmp}"

	# The one symbol worth checking by name: it is what lib/s3bsdev calls, and
	# an archive that merged but lost aws-c-s3 would otherwise only show up as
	# an undefined reference at the very end of this project's build.
	if ! nm -g --defined-only "${libdir}/libaws.a" 2>/dev/null |
	     grep -q "aws_s3_client_new"; then
		err "aws: libaws.a does not define aws_s3_client_new -- the merge"
		err "     dropped aws-c-s3. Rerun with --force aws."
		return 1
	fi
	return 0
}

setup_aws()
{
	local line name tag repo extra libdir n installed prebuilt

	if [ "${FORCE}" -eq 0 ]; then
		if aws_prefix_ready "${AWS_DIR}"; then
			installed="$(aws_stamp_read "${AWS_DIR}")"
			log "aws: already set up at ${AWS_DIR} (${installed:-unrecorded}), nothing to do (--force to rebuild)"
			return 0
		fi
		if [ "${AWS_DIR}" = "${DEPS_DIR}/aws" ] && prebuilt_aws_usable; then
			prebuilt="$(aws_prebuilt_dir)"
			log "aws: using builder prebuilt at ${prebuilt} (${AWS_BUILD_TYPE})"
			return 0
		fi
		if aws_is_built "${AWS_DIR}"; then
			installed="$(aws_stamp_read "${AWS_DIR}")"
			log "aws: installed as ${installed:-unrecorded}, wanted ${AWS_BUILD_TYPE} -- rebuilding"
			aws_clear_prefix || return 1
		fi
	else
		if aws_is_built "${AWS_DIR}" || [ -d "${AWS_SRC}" ]; then
			aws_clear_prefix || return 1
		fi
	fi

	mkdir -p "${AWS_SRC}" || return 1

	# Read with IFS rather than cut, so that a component with no extra flags
	# does not need a placeholder.
	printf '%s\n' "${AWS_COMPONENTS}" | while IFS='|' read -r name tag repo extra; do
		[ -n "${name}" ] || continue
		aws_build_one "${name}" "${tag}" "${repo}" "${extra}" || exit 1
	done || return 1

	aws_archive || return 1

	# Last, so that an interrupted run leaves no stamp rather than a wrong one:
	# absent means "unknown, keep what is there", which is recoverable, while a
	# wrong value would make a half-rebuilt prefix look settled.
	aws_write_stamps "${AWS_DIR}" || return 1

	libdir="$(aws_libdir "${AWS_DIR}")"
	n="$(ar t "${libdir}/libaws.a" 2>/dev/null | wc -l)"
	log "aws: ready at ${AWS_DIR} (libaws.a, ${n} objects, ${AWS_BUILD_TYPE})"
	return 0
}

# ---------------------------------------------------------------------------
# Builder-image install: compile into /opt, then drop VCS, objects and docs.
# ---------------------------------------------------------------------------
slim_spdk_tree()
{
	local root="$1" gitpath

	[ -n "${root}" ] && [ -d "${root}" ] || return 1
	find "${root}" -name .git -print0 2>/dev/null |
		while IFS= read -r -d '' gitpath; do
			rm -rf "${gitpath}"
		done
	find "${root}" -type f -name '*.o' -delete 2>/dev/null || true
	find "${root}" -type f \( -name '*.so' -o -name '*.so.*' \) -delete 2>/dev/null || true
	rm -rf "${root}/test" "${root}/doc" "${root}/docs" "${root}/examples" \
		"${root}/app" "${root}/build/examples" 2>/dev/null || true
	if [ -d "${root}/dpdk" ]; then
		find "${root}/dpdk" -mindepth 1 -maxdepth 1 ! -name build \
			-exec rm -rf {} + 2>/dev/null || true
		if [ -d "${root}/dpdk/build" ]; then
			find "${root}/dpdk/build" -mindepth 1 -maxdepth 1 ! -name lib \
				-exec rm -rf {} + 2>/dev/null || true
		fi
	fi
	return 0
}

slim_aws_prefix()
{
	local root="$1"

	[ -n "${root}" ] && [ -d "${root}" ] || return 1
	# Keep include/, lib or lib64 with libaws.a, and the stamps.
	find "${root}" -mindepth 1 -maxdepth 1 \
		! -name include ! -name lib ! -name lib64 \
		! -name .build_type ! -name .s3lvol-aws-stamp \
		! -name .aws-components \
		-exec rm -rf {} + 2>/dev/null || true
	return 0
}

emit_builder_prebuilt()
{
	local saved_force="${FORCE}"

	FORCE=0
	SPDK_DIR="${S3LVOL_SPDK_PREBUILT}"
	mkdir -p "${S3LVOL_SPDK_PREBUILT}" || return 1
	setup_spdk || return 1
	slim_spdk_tree "${S3LVOL_SPDK_PREBUILT}" || return 1
	spdk_write_stamp "${S3LVOL_SPDK_PREBUILT}" || return 1
	chmod -R a+rX "${S3LVOL_SPDK_PREBUILT}" || return 1

	AWS_SRC=/tmp/s3lvol-aws-src
	mkdir -p "${AWS_SRC}" || return 1

	AWS_DIR="${S3LVOL_AWS_PREBUILT_DEBUG}"
	AWS_BUILD_TYPE=Debug
	mkdir -p "${AWS_DIR}" || return 1
	setup_aws || return 1
	slim_aws_prefix "${AWS_DIR}" || return 1
	chmod -R a+rX "${AWS_DIR}" || return 1

	# Reconfigure the shared source tree for the second prefix. The Debug
	# cmake caches would otherwise keep CMAKE_INSTALL_PREFIX and flags.
	find "${AWS_SRC}" -mindepth 2 -maxdepth 2 -type d -name build \
		-exec rm -rf {} + 2>/dev/null || true

	AWS_DIR="${S3LVOL_AWS_PREBUILT_RELWITHDEBINFO}"
	AWS_BUILD_TYPE=RelWithDebInfo
	mkdir -p "${AWS_DIR}" || return 1
	setup_aws || return 1
	slim_aws_prefix "${AWS_DIR}" || return 1
	chmod -R a+rX "${AWS_DIR}" || return 1

	rm -rf "${AWS_SRC}"
	FORCE="${saved_force}"
	log "builder prebuilt ready:"
	log "    ${S3LVOL_SPDK_PREBUILT}"
	log "    ${S3LVOL_AWS_PREBUILT_DEBUG}"
	log "    ${S3LVOL_AWS_PREBUILT_RELWITHDEBINFO}"
	return 0
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
if [ "${MODE}" = "print-stamp" ]; then
	case "${PRINT_STAMP}" in
	spdk) printf '%s\n' "$(spdk_recipe_stamp)" ;;
	aws)  printf '%s\n' "$(aws_recipe_stamp)" ;;
	*)
		echo "error: --print-stamp wants 'spdk' or 'aws', got '${PRINT_STAMP}'" >&2
		exit 1
		;;
	esac
	exit 0
fi

if [ "${MODE}" = "emit-builder-prebuilt" ]; then
	check_tools || exit 1
	emit_builder_prebuilt || exit 1
	exit 0
fi

if [ "${MODE}" = "check" ]; then
	echo "dependencies under ${DEPS_DIR}:"
	rc=0
	for d in ${WANTED}; do
		"${d}_check" || rc=1
	done
	echo ""
	if [ "${rc}" -eq 0 ]; then
		echo "all set. Build with: make"
	else
		echo "run ./setup_dep.sh to fix the above"
	fi
	exit "${rc}"
fi

check_tools || exit 1

failed=""
for d in ${WANTED}; do
	"setup_${d}" || failed="${failed} ${d}"
done

echo ""
if [ -n "${failed}" ]; then
	err "failed:${failed}"
	exit 1
fi

log "done. Next:"
log "    make                # library + s3lvol_tgt"
log "    make check-offline  # 335 assertions, no credentials, no root"
