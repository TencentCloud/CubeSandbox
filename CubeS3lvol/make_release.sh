#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Build a release package: a directory (and a tarball) that runs on a machine
#  with no SPDK tree, no AWS CRT and no copy of this repository.
#
#  Usage:
#    ./make_release.sh                       # build, verify, tar
#    ./make_release.sh --version 1.2.0       # default is a git describe
#    ./make_release.sh --outdir /tmp/rel     # default is ./release
#    ./make_release.sh --no-tar              # leave the directory, skip the tarball
#    ./make_release.sh --skip-build           # package whatever is already built
#    ./make_release.sh --skip-smoke           # package, skip the runtime smoke tests
#
#  Both build types are recorded in VERSION, and a package that is not a release
#  build says so at the end. They default to the development setting, which is
#  the right default to build with and the wrong one to deploy:
#
#    AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --force aws
#    make S3LVOL_BUILD_TYPE=release && ./make_release.sh
#
#  Layout:
#
#    s3lvol-<version>/
#    ├── bin/s3lvol_tgt
#    ├── scripts/
#    │   ├── rcow_start.sh rcow_stop.sh rcow_recovery.sh rcow_common.sh
#    │   ├── rcow_purge.sh        delete an lvstore outright (irreversible)
#    │   ├── s3lvol_rpc.py        this repo's raw JSON-RPC client
#    │   ├── s3_prefix_rm.py      bucket cleanup, what rcow_purge.sh deletes with
#    │   ├── rpc.py               SPDK's, unmodified
#    │   └── python/spdk/...      what rpc.py imports
#    ├── VERSION
#    └── README.md
#
#  === What makes this packageable at all ===
#
#  Two properties, both established earlier and both worth restating because the
#  package silently stops working if either is lost:
#
#  1. bin/s3lvol_tgt links SPDK and DPDK statically and carries no RPATH. It is
#     checked below rather than assumed -- a single -L/-l reintroducing a shared
#     librte_* would produce a package that runs perfectly on the build machine.
#  2. scripts/rcow_*.sh detect their layout instead of being rewritten here. The
#     scripts that ship are byte-identical to the ones the test suite runs, which
#     is the only way their behaviour in the package follows from anything that
#     was tested.
#
#  === What is deliberately not in the package ===
#
#  - Python from spdk/python other than cli/ and rpc/: sma/ wants grpc and
#    spdkcli/ wants configshell. Shipping them would put imports in the package
#    that cannot succeed, and the first person to hit one has to work out whether
#    it matters.
#  - cos.cfg and the WAL image. Both are per-machine state: cos.cfg holds
#    credentials, and the WAL image's size fixes the journal layout, so a copy
#    from elsewhere is an lvstore whose log belongs to another node.
#  - the test suite. It needs a source tree, fio, and its own credentials.
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${SCRIPT_DIR}"

VERSION=""
OUTDIR="${REPO_ROOT}/release"
DO_TAR=1
DO_BUILD=1
DO_SMOKE=1

while [ $# -gt 0 ]; do
	case "$1" in
	--version)    shift; VERSION="${1:-}" ;;
	--version=*)  VERSION="${1#*=}" ;;
	--outdir)     shift; OUTDIR="${1:-}" ;;
	--outdir=*)   OUTDIR="${1#*=}" ;;
	--no-tar)     DO_TAR=0 ;;
	--skip-build) DO_BUILD=0 ;;
	--skip-smoke) DO_SMOKE=0 ;;
	-h|--help)
		sed -n '2,/^set -u/p' "${BASH_SOURCE[0]}" | sed 's/^#//;s/^ //'
		exit 0
		;;
	*) echo "unknown option: $1" >&2; exit 1 ;;
	esac
	shift
done

log()  { printf '\033[0;32m[rel]\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33m[rel]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[0;31m[rel]\033[0m %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

# ---------------------------------------------------------------------------
# Version
#
# From git when it is not given, with -dirty appended by git describe itself when
# the tree has uncommitted changes. That suffix is the point: a package built from
# a modified tree cannot be traced back to a commit, and finding that out later
# from a deployed machine is much harder than seeing it in the filename now.
# ---------------------------------------------------------------------------
if [ -z "${VERSION}" ]; then
	VERSION="$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || true)"
	[ -n "${VERSION}" ] || VERSION="unknown"
fi

PKG_NAME="s3lvol-${VERSION}"
PKG_DIR="${OUTDIR}/${PKG_NAME}"

# ---------------------------------------------------------------------------
# Where SPDK is, for rpc.py and its library
#
# Resolved the same way the build does, so the rpc.py that ships comes from the
# SPDK the binary was linked against. Taking it from a different checkout would
# usually work and occasionally not, in the form of an RPC whose arguments have
# been renamed.
# ---------------------------------------------------------------------------
if [ -z "${SPDK_ROOT:-}" ]; then
	if [ -f "${REPO_ROOT}/deps/spdk/scripts/rpc.py" ]; then
		SPDK_ROOT="${REPO_ROOT}/deps/spdk"
	elif [ -f /opt/s3lvol-spdk/scripts/rpc.py ]; then
		SPDK_ROOT=/opt/s3lvol-spdk
	elif [ -f "${REPO_ROOT}/../spdk/scripts/rpc.py" ]; then
		SPDK_ROOT="$(cd "${REPO_ROOT}/../spdk" && pwd)"
	else
		die "no SPDK checkout found for rpc.py. Run ./setup_dep.sh, or set SPDK_ROOT"
	fi
fi

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"

# How the CRT inside the binary was compiled, as recorded by setup_dep.sh.
#
# Worth carrying into the package and worth a warning below, because the default
# for developer setup is Debug (see AWS_BUILD_TYPE in setup_dep.sh): that is the
# right default there and the wrong one here, and the binary is static, so once
# it is on another machine there is nothing left to inspect.
AWS_BUILT_AS="unknown"
AWS_BUILD_TYPE="${AWS_BUILD_TYPE:-Debug}"
_aws_bt="$(printf '%s' "${AWS_BUILD_TYPE}" | tr 'A-Z' 'a-z')"
_aws_prebuilt=""
case "${_aws_bt}" in
debug) _aws_prebuilt=/opt/s3lvol-aws-debug ;;
relwithdebinfo) _aws_prebuilt=/opt/s3lvol-aws-relwithdebinfo ;;
esac
if [ -n "${AWS_INSTALL_DIR:-}" ] && [ -f "${AWS_INSTALL_DIR}/.build_type" ]; then
	AWS_BUILT_AS="$(head -1 "${AWS_INSTALL_DIR}/.build_type" | tr -d '[:space:]')"
elif [ -f "${REPO_ROOT}/deps/aws/.build_type" ]; then
	AWS_BUILT_AS="$(head -1 "${REPO_ROOT}/deps/aws/.build_type" | tr -d '[:space:]')"
elif [ -n "${_aws_prebuilt}" ] && [ -f "${_aws_prebuilt}/.build_type" ]; then
	AWS_BUILT_AS="$(head -1 "${_aws_prebuilt}/.build_type" | tr -d '[:space:]')"
fi
[ -n "${AWS_BUILT_AS}" ] || AWS_BUILT_AS="unknown"
AWS_COMPONENTS_FILE=""
if [ -n "${AWS_INSTALL_DIR:-}" ] && [ -f "${AWS_INSTALL_DIR}/.aws-components" ]; then
	AWS_COMPONENTS_FILE="${AWS_INSTALL_DIR}/.aws-components"
elif [ -f "${REPO_ROOT}/deps/aws/.aws-components" ]; then
	AWS_COMPONENTS_FILE="${REPO_ROOT}/deps/aws/.aws-components"
elif [ -n "${_aws_prebuilt}" ] && [ -f "${_aws_prebuilt}/.aws-components" ]; then
	AWS_COMPONENTS_FILE="${_aws_prebuilt}/.aws-components"
fi
unset _aws_bt _aws_prebuilt

# And how this repository's own code was compiled. Same reasoning, and the same
# default: S3LVOL_BUILD_TYPE is debug, which means -O0 on the whole data path.
#
# Read off the stamp mk/s3lvol.common.mk maintains, because its file name *is*
# the build type -- so it doubles as a record of what the .o files in the tree
# were last compiled with, which is the question being asked here.
S3LVOL_BUILT_AS="unknown"
for f in "${REPO_ROOT}"/.build-*; do
	[ -e "${f}" ] || continue
	S3LVOL_BUILT_AS="${f##*/.build-}"
done

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
if [ "${DO_BUILD}" -eq 1 ]; then
	log "building ${PKG_NAME}"
	make -C "${REPO_ROOT}" >/dev/null || die "build failed. Run 'make' to see why"
else
	log "--skip-build: packaging the existing binary"
fi
[ -x "${TGT_BIN}" ] || die "no target binary at ${TGT_BIN} (drop --skip-build?)"

# ---------------------------------------------------------------------------
# Verify the binary is actually self-contained
#
# Before copying anything, because a package that cannot run is worse than no
# package: it gets deployed, and the failure surfaces as a missing library on a
# machine nobody has a toolchain on.
#
# ldd is not sufficient on its own -- dropping DPDK's RTE_INIT constructors
# produces a binary that links, has a clean ldd, starts, and then cannot find a
# mempool ops named "ring" -- so the smoke test below runs the thing.
# ---------------------------------------------------------------------------
verify_binary()
{
	local bad n

	bad="$(ldd "${TGT_BIN}" 2>/dev/null | grep -E 'librte_|libspdk_' || true)"
	if [ -n "${bad}" ]; then
		err "the target still depends on shared SPDK/DPDK libraries:"
		printf '%s\n' "${bad}" | sed 's/^/    /' >&2
		err "these resolve out of the build machine's SPDK tree, so the package"
		err "would not run anywhere else. See mk/s3lvol.common.mk (dpdk_link_args)."
		return 1
	fi

	n="$(readelf -d "${TGT_BIN}" 2>/dev/null | grep -cE 'RPATH|RUNPATH' || true)"
	if [ "${n}" -ne 0 ]; then
		err "the target carries an RPATH/RUNPATH:"
		readelf -d "${TGT_BIN}" | grep -E 'RPATH|RUNPATH' | sed 's/^/    /' >&2
		err "that is a path on this machine; it must not be baked into a release."
		return 1
	fi

	# Anything left is a system library, which is what the package expects to
	# find on the target machine. Listed rather than judged: the set that is
	# acceptable depends on the deployment image, and this is the information
	# needed to decide.
	log "system libraries the package will need:"
	ldd "${TGT_BIN}" 2>/dev/null | awk '{print $1}' | grep -E '^lib' | sort |
		sed 's/^/    /'
	return 0
}

verify_binary || die "refusing to package a binary that is not self-contained"

# ---------------------------------------------------------------------------
# Assemble
# ---------------------------------------------------------------------------
rm -rf "${PKG_DIR}"
mkdir -p "${PKG_DIR}/bin" "${PKG_DIR}/scripts" || die "cannot create ${PKG_DIR}"

log "bin/s3lvol_tgt"
install -m 0755 "${TGT_BIN}" "${PKG_DIR}/bin/s3lvol_tgt" || die "install failed"

log "scripts/ (start, stop, recovery, purge, common)"
for f in rcow_common.sh rcow_start.sh rcow_stop.sh rcow_recovery.sh rcow_purge.sh; do
	[ -f "${REPO_ROOT}/scripts/${f}" ] || die "missing scripts/${f}"
	install -m 0755 "${REPO_ROOT}/scripts/${f}" "${PKG_DIR}/scripts/${f}" ||
		die "install failed for ${f}"
done

log "scripts/s3lvol_rpc.py (this repo's raw RPC client)"
install -m 0755 "${REPO_ROOT}/test/tools/s3lvol_rpc.py" \
	"${PKG_DIR}/scripts/s3lvol_rpc.py" || die "install failed"

# rcow_purge.sh deletes an lvstore's objects with this, and looks for it beside
# itself. Pure stdlib, so it works on a host with no aws-cli and no boto3 -- which
# is the point, since cleaning up after a crashed target is exactly when nothing
# else is available.
log "scripts/s3_prefix_rm.py (bucket cleanup, used by rcow_purge.sh)"
install -m 0755 "${REPO_ROOT}/test/tools/s3_prefix_rm.py" \
	"${PKG_DIR}/scripts/s3_prefix_rm.py" || die "install failed"

log "scripts/rpc.py + scripts/python/spdk (from ${SPDK_ROOT})"
install -m 0755 "${SPDK_ROOT}/scripts/rpc.py" "${PKG_DIR}/scripts/rpc.py" ||
	die "install failed"

# Only what rpc.py reaches. cli/ and rpc/ plus the two top-level modules; see
# the header for why sma/ and spdkcli/ are left out.
mkdir -p "${PKG_DIR}/scripts/python/spdk"
for item in __init__.py version.py cli rpc; do
	[ -e "${SPDK_ROOT}/python/spdk/${item}" ] ||
		die "missing ${SPDK_ROOT}/python/spdk/${item}"
	cp -r "${SPDK_ROOT}/python/spdk/${item}" "${PKG_DIR}/scripts/python/spdk/" ||
		die "copy failed for ${item}"
done
# __pycache__ is per-interpreter and would be stale on the target machine.
find "${PKG_DIR}/scripts/python" -name '__pycache__' -type d -prune -exec rm -rf {} + \
	2>/dev/null || true

# ---------------------------------------------------------------------------
# VERSION: what this package is, and what it was built from
#
# Written because "which build is on that machine" is asked at the worst possible
# time. The SPDK commit and the CRT versions are in here as well: the binary is
# static, so nothing on the machine can be inspected to find out afterwards.
# ---------------------------------------------------------------------------
{
	echo "name:        ${PKG_NAME}"
	echo "version:     ${VERSION}"
	echo "built:       $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	echo "built_on:    $(uname -sr) $(uname -m)"
	echo "s3lvol_git:  $(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || echo unknown)"
	echo "s3lvol_build_type: ${S3LVOL_BUILT_AS}"
	echo "spdk_root:   ${SPDK_ROOT}"
	spdk_git="$(git -C "${SPDK_ROOT}" rev-parse HEAD 2>/dev/null || true)"
	if [ -z "${spdk_git}" ] && [ -f "${SPDK_ROOT}/.s3lvol-spdk-commit" ]; then
		spdk_git="$(head -1 "${SPDK_ROOT}/.s3lvol-spdk-commit" | tr -d '[:space:]')"
	fi
	[ -n "${spdk_git}" ] || spdk_git="unknown"
	spdk_describe="$(git -C "${SPDK_ROOT}" describe --tags 2>/dev/null || true)"
	[ -n "${spdk_describe}" ] || spdk_describe="${spdk_git}"
	echo "spdk_git:    ${spdk_git}"
	echo "spdk_describe: ${spdk_describe}"
	echo "aws_crt_build_type: ${AWS_BUILT_AS}"
	if [ -d "${REPO_ROOT}/deps/aws-src" ]; then
		echo "aws_crt:"
		for d in "${REPO_ROOT}"/deps/aws-src/*; do
			[ -d "${d}/.git" ] || continue
			printf '  %-20s %s\n' "$(basename "${d}")" \
				"$(git -C "${d}" describe --tags 2>/dev/null || echo unknown)"
		done
	elif [ -n "${AWS_COMPONENTS_FILE}" ]; then
		echo "aws_crt:"
		while read -r name tag; do
			[ -n "${name}" ] || continue
			printf '  %-20s %s\n' "${name}" "${tag}"
		done < "${AWS_COMPONENTS_FILE}"
	fi
	echo "glibc_built_against: $(ldd --version 2>/dev/null | head -1)"
} > "${PKG_DIR}/VERSION"

# ---------------------------------------------------------------------------
# README: what someone who only has this directory needs
#
# The text lives in README.md at the repo root, not inline here: it is a document
# people read in the checkout, and the package must ship that same file rather
# than a second copy maintained inside this script.
# ---------------------------------------------------------------------------
cp "${REPO_ROOT}/README.md" "${PKG_DIR}/README.md" ||
	die "copying README.md into the package failed"

# ---------------------------------------------------------------------------
# Smoke test
#
# Run from the package directory with an emptied environment, because that is the
# thing being tested: whether it works without this repository, without an SPDK
# tree, and without anything the build machine happens to have exported.
#
# --wait-for-rpc so nothing is created and no S3 credentials are needed: the
# target comes up, DPDK initialises, the RPC server listens, and it exits. That
# covers the failure this cannot afford to ship -- EAL unable to find its mempool
# ops -- without touching a bucket.
# ---------------------------------------------------------------------------
smoke_test()
{
	local sock log rc=0 pid
	sock="$(mktemp -u /tmp/s3lvol_smoke.XXXXXX.sock)"
	log="$(mktemp /tmp/s3lvol_smoke.XXXXXX.log)"

	log "smoke test: starting bin/s3lvol_tgt with an emptied environment"

	# Backgrounded here rather than inside a subshell, so that $! is the
	# target's pid. Written as ( cd ... & echo $! ) it is the subshell's,
	# which exits immediately -- the kill then succeeds against nothing and
	# leaves a target running. Measured: the first run of this script left one
	# behind.
	#
	# The binary is named by absolute path rather than run from PKG_DIR, since
	# nothing about it depends on the working directory.
	#
	# --disable-cpumask-locks because this is a build-time check, not a
	# deployment: an s3lvol_tgt already serving on the same core would
	# otherwise make it fail with "Cannot create lock on core 0", which says
	# nothing about the package. The smoke test does no I/O, so sharing a
	# core with whatever is running is harmless.
	env -i PATH=/usr/bin:/usr/sbin:/bin LD_LIBRARY_PATH=/nonexistent \
		"${PKG_DIR}/bin/s3lvol_tgt" -m 0x1 --no-huge -s 512 \
		--disable-cpumask-locks \
		-r "${sock}" --wait-for-rpc >"${log}" 2>&1 &
	pid=$!

	sleep 8

	if grep -q "Reactor started" "${log}" 2>/dev/null; then
		log "smoke test: EAL initialised and a reactor is running"
	else
		err "smoke test FAILED -- the target did not reach a running reactor."
		err "This is what a package that links but cannot run looks like."
		err "log (${log}):"
		sed 's/^/    /' "${log}" >&2
		rc=1
	fi

	# Proves the RPC server is listening, which is what the scripts talk to.
	if [ -S "${sock}" ]; then
		log "smoke test: RPC socket created"
	else
		err "smoke test: no RPC socket at ${sock}"
		rc=1
	fi

	# Waited for, not just signalled: returning while it is still shutting down
	# would leave the next thing to run into a socket that is about to vanish.
	kill "${pid}" 2>/dev/null
	for _ in 1 2 3 4 5; do
		kill -0 "${pid}" 2>/dev/null || break
		sleep 1
	done
	if kill -0 "${pid}" 2>/dev/null; then
		warn "smoke test: target${pid} ignored SIGTERM, sending SIGKILL"
		kill -9 "${pid}" 2>/dev/null
	fi
	wait "${pid}" 2>/dev/null || true

	if kill -0 "${pid}" 2>/dev/null; then
		err "smoke test: could not stop target ${pid} -- kill it by hand"
		rc=1
	fi

	rm -f "${sock}"
	[ "${rc}" -eq 0 ] && rm -f "${log}"
	return "${rc}"
}

# Checked separately because it is a different failure: the target can be fine
# while rpc.py cannot import its library, and that only shows up when a script
# tries to create a subsystem.
smoke_test_rpc_py()
{
	log "smoke test: rpc.py can import its library from scripts/python"
	if (cd "${PKG_DIR}/scripts" &&
	    env -i PATH=/usr/bin:/bin PYTHONPATH="${PKG_DIR}/scripts/python" \
		python3 ./rpc.py --help >/dev/null 2>&1); then
		return 0
	fi
	err "smoke test FAILED -- rpc.py cannot run from the package."
	err "output:"
	(cd "${PKG_DIR}/scripts" &&
	 env -i PATH=/usr/bin:/bin PYTHONPATH="${PKG_DIR}/scripts/python" \
		python3 ./rpc.py --help 2>&1 | head -15 | sed 's/^/    /') >&2
	return 1
}

# And that the scripts agree they are in a package. If this branch went wrong they
# would look for the binary in app/s3lvol_tgt/, which does not exist here.
smoke_test_layout()
{
	local out
	log "smoke test: the scripts detect the package layout"
	out="$(cd "${PKG_DIR}/scripts" && bash -c '
		source ./rcow_common.sh >/dev/null 2>&1
		echo "${RCOW_LAYOUT}|${RCOW_TGT_BIN}|${RCOW_SPDK_RPC_PY}|${RCOW_RPC_PY}"
	' 2>/dev/null)"

	case "${out}" in
	"package|${PKG_DIR}/bin/s3lvol_tgt|${PKG_DIR}/scripts/rpc.py|${PKG_DIR}/scripts/s3lvol_rpc.py")
		return 0
		;;
	esac
	err "smoke test FAILED -- wrong paths resolved inside the package:"
	err "    got:  ${out}"
	err "    want: package|${PKG_DIR}/bin/s3lvol_tgt|${PKG_DIR}/scripts/rpc.py|${PKG_DIR}/scripts/s3lvol_rpc.py"
	return 1
}

if [ "${DO_SMOKE}" -eq 1 ]; then
	smoke_test_layout   || die "package is broken"
	smoke_test_rpc_py   || die "package is broken"
	smoke_test          || die "package is broken"
else
	log "--skip-smoke: skipping the runtime smoke tests"
fi

# ---------------------------------------------------------------------------
# Tarball
# ---------------------------------------------------------------------------
if [ "${DO_TAR}" -eq 1 ]; then
	log "tarball"
	# --owner/--group so the archive does not carry the build machine's uids,
	# and -C so it unpacks into s3lvol-<version>/ rather than over the cwd.
	tar -czf "${OUTDIR}/${PKG_NAME}.tar.gz" \
		--owner=root --group=root \
		-C "${OUTDIR}" "${PKG_NAME}" || die "tar failed"
fi

echo ""
log "${PKG_NAME}"
log "  dir:  ${PKG_DIR}"
[ "${DO_TAR}" -eq 1 ] && log "  tar:  ${OUTDIR}/${PKG_NAME}.tar.gz ($(du -h "${OUTDIR}/${PKG_NAME}.tar.gz" | cut -f1))"
log "  size: $(du -sh "${PKG_DIR}" | cut -f1)"
log "  built: s3lvol=${S3LVOL_BUILT_AS} crt=${AWS_BUILT_AS}"

# Said out loud rather than left in VERSION, because both defaults are the debug
# one and shipping that is a decision, not a typo to be discovered later.
rel_note=""
case "${S3LVOL_BUILT_AS}" in
release) ;;
*) rel_note="s3lvol's own code (${S3LVOL_BUILT_AS}: -O0, SPDK_DEBUGLOG compiled in)" ;;
esac
case "${AWS_BUILT_AS}" in
Release | RelWithDebInfo | MinSizeRel) ;;
*)
	[ -z "${rel_note}" ] || rel_note="${rel_note} and "
	rel_note="${rel_note}the AWS CRT (${AWS_BUILT_AS})"
	;;
esac

if [ -n "${rel_note}" ]; then
	echo ""
	warn "this package is not a release build: ${rel_note}."
	warn "Those are the development defaults and they are fine to test with."
	warn "For something meant to be deployed:"
	warn "    AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --force aws"
	warn "    make S3LVOL_BUILD_TYPE=release && ./make_release.sh"
	warn "(both are recorded in VERSION; the binary is static, so there is"
	warn " nothing left to inspect once it is on another machine)"
fi
echo ""
log "on the target machine:"
log "    tar xzf ${PKG_NAME}.tar.gz && cd ${PKG_NAME}"
log "    less README.md            # cos.cfg and the WAL image are not included"
log "    scripts/rcow_start.sh"
