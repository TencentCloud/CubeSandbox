#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Apply this repo's SPDK patches to the SPDK checkout we build against.
#
#  Idempotent: a patch that is already applied is skipped, so this is safe to run
#  from a build script or by hand, as often as you like.
#
#  Usage:
#    patches/apply.sh                # SPDK at deps/spdk, else ../spdk
#    SPDK_ROOT=/path/to/spdk patches/apply.sh
#    patches/apply.sh --check            # report only, change nothing, non-zero if
#                                        # anything is missing
#    patches/apply.sh --reverse          # take them back out
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
if [ -z "${SPDK_ROOT:-}" ]; then
	if [ -f "${REPO_ROOT}/deps/spdk/include/spdk/blob.h" ]; then
		SPDK_ROOT="${REPO_ROOT}/deps/spdk"
	else
		SPDK_ROOT="$(cd "${REPO_ROOT}/../spdk" 2>/dev/null && pwd || true)"
	fi
fi

MODE="apply"
case "${1:-}" in
--check)   MODE="check" ;;
--reverse) MODE="reverse" ;;
"")        ;;
*)         echo "usage: $0 [--check|--reverse]" >&2; exit 1 ;;
esac

if [ -z "${SPDK_ROOT}" ] || [ ! -f "${SPDK_ROOT}/include/spdk/blob.h" ]; then
	echo "error: no SPDK checkout at '${SPDK_ROOT:-../spdk}'; set SPDK_ROOT" >&2
	exit 1
fi

# git apply is what decides whether a patch is present, and it needs a work tree.
if ! git -C "${SPDK_ROOT}" rev-parse --git-dir >/dev/null 2>&1; then
	echo "error: ${SPDK_ROOT} is not a git checkout; these patches are managed" >&2
	echo "       with git apply, so a tarball will not do" >&2
	exit 1
fi

applied=0
missing=0
failed=0

for patch in "${SCRIPT_DIR}"/[0-9]*.patch; do
	[ -e "${patch}" ] || continue
	name="$(basename "${patch}")"

	if git -C "${SPDK_ROOT}" apply --reverse --check "${patch}" >/dev/null 2>&1; then
		# Reverses cleanly, so it is already in.
		case "${MODE}" in
		reverse)
			if git -C "${SPDK_ROOT}" apply --reverse "${patch}"; then
				echo "removed  ${name}"
			else
				echo "FAILED to remove ${name}" >&2
				failed=$((failed + 1))
			fi
			;;
		*)
			echo "present  ${name}"
			applied=$((applied + 1))
			;;
		esac
		continue
	fi

	if [ "${MODE}" = "reverse" ]; then
		echo "absent   ${name}"
		continue
	fi
	if [ "${MODE}" = "check" ]; then
		echo "MISSING  ${name}"
		missing=$((missing + 1))
		continue
	fi

	if ! git -C "${SPDK_ROOT}" apply --check "${patch}" >/dev/null 2>&1; then
		# Neither applied nor applicable: the checkout has moved under us. Say so
		# instead of leaving a half-patched tree.
		echo "FAILED   ${name} does not apply to ${SPDK_ROOT}" >&2
		echo "         SPDK is at $(git -C "${SPDK_ROOT}" describe --tags 2>/dev/null || echo unknown)" >&2
		echo "         see patches/README.md for how to refresh it" >&2
		failed=$((failed + 1))
		continue
	fi

	if git -C "${SPDK_ROOT}" apply "${patch}"; then
		echo "applied  ${name}"
		applied=$((applied + 1))
	else
		echo "FAILED   ${name}" >&2
		failed=$((failed + 1))
	fi
done

if [ "${failed}" -gt 0 ]; then
	exit 1
fi
if [ "${MODE}" = "check" ] && [ "${missing}" -gt 0 ]; then
	echo
	echo "${missing} patch(es) are not applied. Run: patches/apply.sh" >&2
	exit 1
fi
if [ "${MODE}" = "apply" ] && [ "${applied}" -gt 0 ]; then
	echo
	echo "note: SPDK has to be rebuilt for these to take effect:"
	echo "      make -C ${SPDK_ROOT} -j\$(nproc)"
fi
exit 0
