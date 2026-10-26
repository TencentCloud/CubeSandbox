#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Refuse to test a binary that is older than the code it was built from.
#
#  Usage: check_binary_fresh.sh <binary>
#
#  This exists because of a specific failure, not as a general precaution. The
#  top-level Makefile used to leave app/s3lvol_tgt out of every target, so a
#  plain `make` refreshed the libraries and left the binary alone. A full
#  dataplane regression then passed -- 39/39, 32/32, 32/32, 55/55 -- against a
#  binary that predated the fix it was supposed to be proving. Nothing in the
#  output hinted at it; the only tell was a log line naming a function that had
#  been renamed hours earlier.
#
#  A green run against a stale binary is worse than a red one: it is evidence
#  pointing the wrong way, and it is spent deciding the code is correct. The
#  Makefile is fixed, but the Makefile is not the only way to arrive here --
#  `make shared`, an interrupted build, or a rebuild of the wrong tree all end
#  the same way. So the check lives with the tests, where the wrong conclusion
#  would actually be drawn.
#
#  Compares mtimes only. That is enough for the failure being prevented, and it
#  has no false negatives that matter: touching a file without changing it costs
#  one rebuild, whereas missing a real change costs a wrong conclusion.

set -u

BIN="${1:-}"

if [ -z "${BIN}" ]; then
	echo "usage: $(basename "$0") <binary>" >&2
	exit 2
fi

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SELF_DIR}/../.." && pwd)"

if [ ! -x "${BIN}" ]; then
	echo "[FAIL] ${BIN} does not exist or is not executable" >&2
	echo "       build it first: make -C ${REPO_ROOT} AWS_INSTALL_DIR=<prefix>" >&2
	exit 1
fi

# Everything the binary is linked from. app/ is included because s3lvol_tgt.c
# itself is part of it; test/ is not, since it does not end up in the binary.
SEARCH_DIRS=""
for d in lib include module app; do
	[ -d "${REPO_ROOT}/${d}" ] && SEARCH_DIRS="${SEARCH_DIRS} ${REPO_ROOT}/${d}"
done

# -quit after the first hit: this runs before every dataplane test and there is
# no reason to enumerate the whole tree once an answer exists.
STALE="$(find ${SEARCH_DIRS} -type f \( -name '*.c' -o -name '*.h' \) \
	-newer "${BIN}" -print -quit 2>/dev/null)"

if [ -n "${STALE}" ]; then
	echo "[FAIL] ${BIN} is older than the source it is built from" >&2
	echo "       newer: ${STALE#${REPO_ROOT}/}" >&2
	echo "" >&2
	echo "       Testing this binary would report on code that is not in it." >&2
	echo "       Rebuild: make -C ${REPO_ROOT} AWS_INSTALL_DIR=<prefix>" >&2
	echo "" >&2
	echo "       Set S3LVOL_SKIP_FRESH_CHECK=1 to run anyway (bisecting an" >&2
	echo "       old binary on purpose is the one good reason)." >&2
	exit 1
fi

exit 0
