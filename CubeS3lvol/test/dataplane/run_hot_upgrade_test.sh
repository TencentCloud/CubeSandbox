#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  T1 -- I/O continuity across a hot upgrade.
#
#  === What this proves, and why fio is the whole point ===
#
#  The claim under test is not "the upgrade finished" but "I/O never failed":
#  the host's block requests are paused while the target is replaced and then
#  complete on the *same* /dev/nvmeXnY, with no error and no sandbox teardown.
#  A run in which fio errored and the upgrade still "succeeded" must be red, so
#  every fio process is started before the upgrade and read back after it, and
#  err / io_errors are asserted per volume rather than summed into one number.
#
#  The mechanism is the one already proven by crash recovery: do not disconnect,
#  do not unload. rcow_upgrade.sh flushes and checkpoints online, then SIGKILLs
#  the target; rcow_start.sh rebuilds the same NQN/NSID/UUID grid and the kernel
#  reconnects. Anything that disconnects or unloads instead deletes the
#  namespace and the host tears the gendisk down -- which is the EIO this suite
#  exists to catch, so a failure here is a design violation, not a flake.
#
#  === What the pause window means, and what is printed ===
#
#  pause_window_ms is measured from just before rcow_upgrade.sh to after the
#  layout is verified live again. That deliberately *over*-states the true pause:
#  the online flush and checkpoint happen inside it but block nothing. It is an
#  upper bound, and a regression baseline -- a number that only grows is the
#  signal to look, not a pass/fail gate.
#
#  === The cross-SPDK case is not optional coverage ===
#
#  A hot upgrade across SPDK versions is allowed only if the nvmf model is
#  pinned (-d in rcow_create_subsystems) and there is a real test for
#  it. Same-binary self-upgrade cannot observe a firmware-revision change, so
#  --cross-spdk runs a second upgrade whose *new* binary comes from a second
#  build tree. Without the flag a SKIP is printed -- never silence, because a
#  silent fall-back to "same binary again" would look like coverage it is not.
#
#  Usage:
#    export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#    sudo -E ./test/dataplane/run_hot_upgrade_test.sh \
#             -e cos.ap-nanjing.myqcloud.com -b my-bucket -r ap-nanjing \
#             [--volumes N] [--cross-spdk /path/to/other/tree]
#
#  Needs root (nvme connect, /dev access), fio, and exclusive use of this
#  machine's nvme stack -- it connects all 32 controllers and owns the fixed RPC
#  socket, so it must not run concurrently with anything else.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

# Own lvstore, WAL image and run directory: a production instance on this host is
# untouched. The bstore.json / active_lvols paths are compiled into the module,
# as everywhere else, so they are pointed into RCOW_RUN_DIR for this run.
export RCOW_LVS_NAME=hotvs
export RCOW_WAL_IMG=/data/s3lvol_hot_wal.img
export RCOW_WAL_BDEV=hot_wal0
export RCOW_CAPACITY_GB=512
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=512
export RCOW_TGT_MEM_MB=4096
export RCOW_RUN_DIR=/var/tmp/rcow_hottest
export RCOW_LOG_DIR=/var/tmp/rcow_hottest/log
export RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"
export RCOW_ACTIVE_FILE=/var/tmp/rcow_hottest/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_hottest/bstore.json

TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"

ENDPOINT=""
BUCKET=""
REGION=""
VOLUMES=16
CROSS_SPDK=""

LVOL_GIB=1
# The upgrade happens while fio is mid-flight. 60 s is enough for the queues to
# be genuinely busy and for the checkpoint/flush path to have work, and short
# enough that both upgrades fit inside one fio runtime.
UPGRADE_AT_SEC=60
SECOND_AT_SEC=120
FIO_RUNTIME_SEC=300
# The window's upper bound includes SPDK cold start and the lvstore attach, so it
# is deliberately generous. Never lower it to make a run pass.
VERIFY_TIMEOUT_SEC=180

# Max clat for a single fio command that straddles the window, tiered by volume
# count. A cold S3 round trip and the reconnect timer both scale with how much
# has to come back, so the small-fleet and large-fleet budgets are separate.
CLAT_MAX_MS_SMALL="${S3LVOL_HOT_CLAT_MAX_MS_SMALL:-30000}"
CLAT_MAX_MS_LARGE="${S3LVOL_HOT_CLAT_MAX_MS_LARGE:-60000}"
CLAT_TIER_VOLUMES=16

WORKDIR="$(mktemp -d /tmp/s3lvol_hot.XXXXXX)"
declare -a FIO_PIDS=()
declare -a VOL_NAMES=()
declare -a VOL_DEVS=()
T_START=0

PASS=0; FAIL=0; SKIP=0
# A skip that the environment forced (no fio) means the suite did not verify what
# it exists to verify, so it must not share an exit code with a real pass. A skip
# the operator chose (no --cross-spdk) is coverage not requested and does not.
SKIP_FORCED=0
pass() { PASS=$((PASS + 1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $*"; }
skip() { SKIP=$((SKIP + 1)); SKIP_FORCED=1; echo "  [SKIP] $*"; }
skip_opt() { SKIP=$((SKIP + 1)); echo "  [SKIP] $*"; }
info() { echo "  ---- $*"; }
# A prerequisite the machine cannot satisfy is "could not run", not a failure of
# the code under test: report it as a counted SKIP and leave on 2, the tree's
# distinct status for that (check_layering.sh), so run_all.sh and a human both
# see something other than a silent pass or a misleading red.
cannot_run() { skip "$1"; exit 2; }

usage()
{
	cat <<EOT
Usage: $0 -e <endpoint> -b <bucket> [-r region]
          [--volumes N] [--cross-spdk <build-tree>]

  -e  S3/COS endpoint, e.g. cos.ap-nanjing.myqcloud.com
  -b  bucket name
  -r  region (default taken from s3.cfg)
  --volumes N     how many volumes to create and drive (default ${VOLUMES})
  --cross-spdk D  second build tree whose s3lvol_tgt is the new binary for the
                  second upgrade; without it that upgrade is skipped

Credentials are read from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, or from
s3.cfg when those are unset.
EOT
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-e) ENDPOINT="$2"; shift 2 ;;
	-b) BUCKET="$2"; shift 2 ;;
	-r) REGION="$2"; shift 2 ;;
	--volumes) VOLUMES="$2"; shift 2 ;;
	--cross-spdk) CROSS_SPDK="$2"; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	*) usage >&2; exit 1 ;;
	esac
done

case "${VOLUMES}" in
''|*[!0-9]*) echo "--volumes must be a positive integer" >&2; exit 1 ;;
esac
[ "${VOLUMES}" -ge 1 ] || { echo "--volumes must be >= 1" >&2; exit 1; }

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

# The block device a volume resolves to, or nothing.
vol_dev()
{
	python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("device_path",""))' \
		"$(rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "$1")" 2>/dev/null)" \
		2>/dev/null
}

# Occurrences of a kernel message. Counting before and after, rather than slicing
# by line offset: the ring buffer is full on a long-lived host, so its length
# barely moves and `dmesg | tail -n +N` returns nothing.
dmesg_count()
{
	dmesg 2>/dev/null | grep -c "$1" || true
}

# The controllers this suite's connect created, one sysfs directory per line.
# Filtered by NQN prefix so a controller belonging to another stack on this host
# is never read or written.
our_controllers()
{
	local d
	for d in /sys/class/nvme/nvme*; do
		[ -r "${d}/subsysnqn" ] || continue
		case "$(cat "${d}/subsysnqn" 2>/dev/null)" in
		"${RCOW_NQN_PREFIX}"*) printf '%s\n' "${d}" ;;
		esac
	done
}

live_controller_count()
{
	local d n=0
	while read -r d; do
		[ -n "${d}" ] || continue
		[ "$(cat "${d}/state" 2>/dev/null)" = "live" ] && n=$((n + 1))
	done < <(our_controllers)
	printf '%s' "${n}"
}

# Block-device names the host currently has. Set comparison across the upgrade is
# the detector for a namespace that was removed and rescanned: it does not depend
# on the kernel's exact wording, so a silent removal cannot hide the way it can
# from a dmesg grep alone.
block_dev_set()
{
	ls /dev/nvme*n* 2>/dev/null | sort | tr '\n' ' '
}

sleep_until()
{
	while [ "$(( $(date +%s) - T_START ))" -lt "$1" ]; do
		sleep 1
	done
}

cleanup()
{
	echo
	echo "=== cleanup ==="

	# Stop any fio still running before touching the devices; a leftover process
	# keeps the window open and its death would otherwise be reported against
	# the next suite.
	if [ "${#FIO_PIDS[@]}" -gt 0 ]; then
		kill "${FIO_PIDS[@]}" 2>/dev/null || true
	fi

	# rcow_stop.sh disconnects nvme and unloads; correct for teardown even
	# though it is exactly what the hot path must not do.
	"${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1 || true

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		rcow_load_credentials 2>/dev/null || true
		python3 "${PREFIX_RM}" -e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}" \
			-p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}"
		rm -rf "${RCOW_RUN_DIR}" "${WORKDIR}"
	else
		info "state kept for inspection: ${RCOW_WAL_IMG}, ${RCOW_RUN_DIR}, ${WORKDIR}"
	fi

	echo
	echo "=== result: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped ==="
	[ "${FAIL}" -eq 0 ] || exit 1
	# "It did not run" and "it passed" must not share an exit code. 2 is the
	# tree's "could not check" convention (check_layering.sh): the caller sees a
	# non-zero that is not a failure of the code under test.
	if [ "${SKIP_FORCED}" -eq 1 ]; then
		echo "some assertions could not run; this is not a pass" >&2
		exit 2
	fi
}
trap cleanup EXIT

# ==========================================================================
# [0] preconditions
# ==========================================================================
echo "=== [0] preconditions"

# Tree and build problems are red: the code under test is wrong or absent.
[ -x "${TGT_BIN}" ] || { echo "target not built: ${TGT_BIN}" >&2; exit 1; }
[ -x "${SCRIPTS}/rcow_start.sh" ] || { echo "rcow_start.sh missing" >&2; exit 1; }
[ -x "${SCRIPTS}/rcow_upgrade.sh" ] || {
	echo "rcow_upgrade.sh missing: the hot path is not in this tree" >&2; exit 1; }

# Machine prerequisites are "could not run": a counted skip, then exit 2.
[ "$(id -u)" -eq 0 ] || cannot_run "must run as root to connect nvme controllers"
command -v nvme >/dev/null || cannot_run "nvme-cli is required"
command -v truncate >/dev/null || cannot_run "truncate is required to size the WAL image"
command -v python3 >/dev/null || cannot_run "python3 is required"

if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
	"${ROOT}/test/tools/check_binary_fresh.sh" "${TGT_BIN}" || exit 1
fi

modprobe nvme_tcp 2>/dev/null || true
lsmod | grep -q nvme_tcp || cannot_run "nvme_tcp is not available; there is no loopback transport"

# The fixed RPC socket and the host's nvme stack are this suite's to own.
if [ -n "$(rcow_target_instances)" ]; then
	cannot_run "an s3lvol_tgt is already running; stop it first"
fi

[ -n "${ENDPOINT}" ] || ENDPOINT="$(rcow_cfg_get endpoint)"
[ -n "${REGION}" ]   || REGION="$(rcow_cfg_get region)"
[ -n "${BUCKET}" ]   || BUCKET="$(rcow_s3_buckets | head -1)"
[ -n "${REGION}" ] || REGION="us-east-1"

if [ -z "${AWS_ACCESS_KEY_ID:-}" ] || [ -z "${AWS_SECRET_ACCESS_KEY:-}" ]; then
	rcow_load_credentials 2>/dev/null ||
		cannot_run "no credentials: export AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY (with sudo, remember -E)"
fi
if [ -z "${ENDPOINT}" ] || [ -z "${BUCKET}" ]; then
	cannot_run "endpoint/bucket unknown: pass -e/-b or provide ${RCOW_S3_CFG}"
fi

HAVE_FIO=0
command -v fio >/dev/null 2>&1 && HAVE_FIO=1

rm -rf "${RCOW_RUN_DIR}"
mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}"
truncate -s 2G "${RCOW_WAL_IMG}" ||
	cannot_run "could not create the WAL image at ${RCOW_WAL_IMG}"

info "volumes ${VOLUMES}, endpoint ${ENDPOINT}, bucket ${BUCKET}, region ${REGION}"
info "fio: $([ "${HAVE_FIO}" -eq 1 ] && echo present || echo MISSING)"
[ "${HAVE_FIO}" -eq 1 ] || skip "fio is not installed: the I/O-continuity assertions cannot run"

if [ -n "${CROSS_SPDK}" ]; then
	CROSS_TGT="${CROSS_SPDK}/app/s3lvol_tgt/s3lvol_tgt"
	if [ ! -x "${CROSS_TGT}" ]; then
		cannot_run "--cross-spdk ${CROSS_SPDK} has no built s3lvol_tgt"
	fi
	info "cross-SPDK new binary: ${CROSS_TGT}"
	# Freshness still matters here: a stale binary in the second tree would not
	# carry the change under test. The check has to run from *that* tree --
	# check_binary_fresh.sh compares against the sources beside it, so this
	# repo's copy would compare the other binary against the wrong sources.
	if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
		if [ -x "${CROSS_SPDK}/test/tools/check_binary_fresh.sh" ]; then
			"${CROSS_SPDK}/test/tools/check_binary_fresh.sh" "${CROSS_TGT}" || exit 1
		else
			info "no check_binary_fresh.sh in ${CROSS_SPDK}: cannot tell whether that binary is stale"
		fi
	fi
fi

# ==========================================================================
# [1] bring the data plane up
# ==========================================================================
echo
echo "=== [1] bringing the data plane up"

if "${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1; then
	pass "rcow_start.sh brought the lvstore and the grid up"
else
	fail "rcow_start.sh failed"
	tail -25 "${WORKDIR}/start.log" | sed 's/^/       /'
	exit 1
fi

CONNECTED="$(our_controllers | wc -l)"
[ "${CONNECTED}" = "${RCOW_NUM_SUBSYS}" ] &&
	pass "the initiator is connected to all ${RCOW_NUM_SUBSYS} subsystems" ||
	fail "only ${CONNECTED} of ${RCOW_NUM_SUBSYS} controllers are present"

# ==========================================================================
# [2] N volumes, activated and reachable
# ==========================================================================
echo
echo "=== [2] creating and activating ${VOLUMES} volume(s)"

i=0
while [ "${i}" -lt "${VOLUMES}" ]; do
	name="$(printf 'hot%03d' "${i}")"
	if ! rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
			"${name}" "${LVOL_GIB}")" >/dev/null 2>"${WORKDIR}/create_${name}.err"; then
		fail "rcow_create_lvol ${name}"
		sed 's/^/       /' "${WORKDIR}/create_${name}.err"
		exit 1
	fi
	if ! rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" \
			>/dev/null 2>"${WORKDIR}/active_${name}.err"; then
		fail "rcow_active_bdev ${name}"
		sed 's/^/       /' "${WORKDIR}/active_${name}.err"
		exit 1
	fi
	VOL_NAMES+=("${name}")
	i=$((i + 1))
done
pass "${VOLUMES} volume(s) created and activated"

# The namespaces were added after the connect, so this is the AEN hot-plug path;
# it also gives every one of them a block device to fio.
if rcow_verify_active 60; then
	pass "every volume resolved to an openable block device"
else
	fail "some volumes never became block devices"
fi

for name in "${VOL_NAMES[@]}"; do
	dev="$(vol_dev "${name}")"
	if [ -n "${dev}" ] && [ -b "${dev}" ]; then
		VOL_DEVS+=("${dev}")
		info "${name} -> ${dev}"
	else
		fail "${name} has no block device (got '${dev}')"
		VOL_DEVS+=("")
	fi
done

# ==========================================================================
# [3] fio on every volume, started before the upgrade and read after it
# ==========================================================================
echo
echo "=== [3] starting fio on ${VOLUMES} volume(s), runtime ${FIO_RUNTIME_SEC}s"

if [ "${HAVE_FIO}" -eq 1 ]; then
	mkdir -p "${WORKDIR}/fio"
	for idx in "${!VOL_NAMES[@]}"; do
		name="${VOL_NAMES[${idx}]}"
		dev="${VOL_DEVS[${idx}]}"
		[ -b "${dev}" ] || continue
		fio --name="${name}" --filename="${dev}" \
			--ioengine=libaio --direct=1 \
			--rw=randrw --bs=4k --iodepth=32 \
			--time_based --runtime="${FIO_RUNTIME_SEC}" \
			--continue_on_error=none --error_dump=1 \
			--output-format=json --output="${WORKDIR}/fio/${name}.json" \
			>"${WORKDIR}/fio/${name}.stdout" 2>&1 &
		FIO_PIDS+=($!)
	done
	[ "${#FIO_PIDS[@]}" -gt 0 ] && pass "${#FIO_PIDS[@]} fio job(s) running" \
		|| fail "no fio job could be started"
	T_START="$(date +%s)"
	# Let the queues fill before the first upgrade; without this the window lands
	# on an idle device and proves nothing about I/O in flight.
	sleep 10
else
	skip "no fio: the per-volume err/io_errors/clat assertions are not run"
fi

# ==========================================================================
# [4] the hot upgrade, and the layout-continuity assertions
# ==========================================================================
# One function for both upgrades: the only difference between the same-binary
# mechanism test and the cross-SPDK case is which binary the *new* target runs.
do_upgrade()
{
	local label="$1" new_bin="$2"
	local before after start end start_env

	before="$(block_dev_set)"
	printf '%s' "${before}" >"${WORKDIR}/blockdevs-${label}-before.txt"

	# The verbatim get_bdev listing captured before the stop; rcow_verify_active
	# --expect compares the live view against it byte for byte.
	rpc rcow_get_bdev '{}' >"${WORKDIR}/layout-${label}-before.json" 2>/dev/null || {
		fail "${label}: rcow_get_bdev before the upgrade"
		return 1
	}

	dmesg_count 'Buffer I/O error' >"${WORKDIR}/dmesg_ioerr_before.txt"
	dmesg_count 'removing namespace' >"${WORKDIR}/dmesg_nsrm_before.txt"

	info "${label}: upgrading (new binary ${new_bin})"
	start="$(date +%s)"

	if ! "${SCRIPTS}/rcow_upgrade.sh" >"${WORKDIR}/hot_stop_${label}.log" 2>&1; then
		fail "${label}: rcow_upgrade.sh failed"
		tail -20 "${WORKDIR}/hot_stop_${label}.log" | sed 's/^/       /'
		return 1
	fi
	pass "${label}: rcow_upgrade.sh stopped the target without unloading"

	# The stop is also the step that records the layout for the comparison
	# below; an entry-less snapshot would make that comparison vacuous.
	grep -q '"device_name"' "${RCOW_HOT_SNAPSHOT}" 2>/dev/null &&
		pass "${label}: rcow_upgrade.sh recorded the layout snapshot" ||
		fail "${label}: no layout snapshot at ${RCOW_HOT_SNAPSHOT}"

	# RCOW_TGT_BIN for this invocation only; rcow_common.sh takes a pre-set value.
	# rcow_start.sh's freshness check runs *this* tree's checker against whatever
	# binary it is handed, so it means nothing for the cross-SPDK one: section [0]
	# already ran that tree's own checker against it when the tree has one, and
	# this tree's sources being newer says nothing about it either way. Nothing is
	# set in the other case, so an operator's own skip survives.
	start_env=(RCOW_TGT_BIN="${new_bin}")
	[ "${label}" != "crossspdk" ] || start_env+=(S3LVOL_SKIP_FRESH_CHECK=1)
	if ! env "${start_env[@]}" "${SCRIPTS}/rcow_start.sh" \
			>"${WORKDIR}/hot_start_${label}.log" 2>&1; then
		fail "${label}: rcow_start.sh failed after the hot stop"
		tail -20 "${WORKDIR}/hot_start_${label}.log" | sed 's/^/       /'
		return 1
	fi
	pass "${label}: rcow_start.sh rebuilt the lvstore and replayed the volumes"

	if rcow_verify_active "${VERIFY_TIMEOUT_SEC}" \
			--expect "${WORKDIR}/layout-${label}-before.json"; then
		pass "${label}: layout is bit-for-bit identical after the upgrade"
	else
		fail "${label}: layout changed across the upgrade"
	fi

	end="$(date +%s)"
	info "pause_window_ms=$(( (end - start) * 1000 ))"

	# All 32 controllers back to live. Fewer than 32 means one was deleted --
	# the gendisk is gone and the volumes hashed to it are unrecoverable.
	local live
	live="$(live_controller_count)"
	[ "${live}" = "${RCOW_NUM_SUBSYS}" ] &&
		pass "${label}: all ${RCOW_NUM_SUBSYS} controllers are live" ||
		fail "${label}: only ${live} of ${RCOW_NUM_SUBSYS} controllers are live"

	# A removed-and-rescanned namespace changes the device set even if the
	# volumes that survived happen to line up again. Independent of kernel
	# wording, unlike the dmesg grep below.
	after="$(block_dev_set)"
	[ "${after}" = "${before}" ] &&
		pass "${label}: the host's block-device set is unchanged" ||
		fail "${label}: the block-device set changed: '${before}' -> '${after}'"

	local ioerr nsrm
	ioerr="$(dmesg_count 'Buffer I/O error')"
	nsrm="$(dmesg_count 'removing namespace')"
	[ "${ioerr}" -le "$(cat "${WORKDIR}/dmesg_ioerr_before.txt")" ] &&
		pass "${label}: no new 'Buffer I/O error' in dmesg" ||
		fail "${label}: dmesg gained 'Buffer I/O error' during the upgrade"
	[ "${nsrm}" -le "$(cat "${WORKDIR}/dmesg_nsrm_before.txt")" ] &&
		pass "${label}: no new namespace-removal line in dmesg" ||
		fail "${label}: dmesg gained a namespace-removal line during the upgrade"

	return 0
}

echo
echo "=== [4] hot upgrade #1 (same binary)"
sleep_until "${UPGRADE_AT_SEC}"
do_upgrade same "${TGT_BIN}" || true

# ==========================================================================
# [5] hot upgrade #2 (cross-SPDK), or a loud skip
# ==========================================================================
echo
if [ -n "${CROSS_SPDK}" ]; then
	echo "=== [5] hot upgrade #2 (new binary from ${CROSS_SPDK})"
	sleep_until "${SECOND_AT_SEC}"
	do_upgrade crossspdk "${CROSS_TGT}" || true
else
	echo "=== [5] hot upgrade #2: skipped, no --cross-spdk"
	skip_opt "cross-SPDK coverage not exercised: pass --cross-spdk <tree>.
Without it this is only a same-binary self-upgrade and says nothing about a
firmware-revision change"
fi

# ==========================================================================
# [6] the fio results, per volume
# ==========================================================================
echo
echo "=== [6] fio results across the upgrade(s)"

if [ "${HAVE_FIO}" -eq 0 ]; then
	skip "no fio: err / io_errors / clat not available"
else
	# Wait for the time-based jobs to finish on their own, but never forever: a
	# device that went away leaves fio blocked, and hanging here would hide that
	# behind the teardown.
	WAIT_DEADLINE=$(( $(date +%s) + FIO_RUNTIME_SEC ))
	while :; do
		alive=0
		for pid in "${FIO_PIDS[@]}"; do
			kill -0 "${pid}" 2>/dev/null && alive=$((alive + 1))
		done
		[ "${alive}" -eq 0 ] && break
		[ "$(date +%s)" -ge "${WAIT_DEADLINE}" ] && break
		sleep 2
	done

	if [ "${CLAT_TIER_VOLUMES}" -lt "${VOLUMES}" ]; then
		CLAT_MAX_MS="${CLAT_MAX_MS_LARGE}"
	else
		CLAT_MAX_MS="${CLAT_MAX_MS_SMALL}"
	fi
	info "clat threshold for ${VOLUMES} volume(s): ${CLAT_MAX_MS} ms"

	for name in "${VOL_NAMES[@]}"; do
		json="${WORKDIR}/fio/${name}.json"
		if [ ! -s "${json}" ]; then
			# fio with --continue_on_error=none exits on the first error; a
			# missing report is itself a failure, not a skip.
			fail "${name}: fio produced no report (it may have exited on I/O error)"
			continue
		fi
		if python3 - "${json}" "${name}" "${CLAT_MAX_MS}" <<-'PY'
import json, sys
path, name, clat_max_ms = sys.argv[1], sys.argv[2], int(sys.argv[3])
try:
    d = json.load(open(path))
except Exception as exc:
    print("%s: report is not JSON (%s)" % (name, exc))
    sys.exit(1)
jobs = d.get("jobs", [])
if not jobs:
    print("%s: fio report has no jobs" % name)
    sys.exit(1)
err = sum(j.get("error", 0) for j in jobs)
ioerr = sum(j.get("read", {}).get("io_errors", 0) +
            j.get("write", {}).get("io_errors", 0) for j in jobs)
clat = max(max(j.get("read", {}).get("clat_ns", {}).get("max", 0) or 0,
               j.get("write", {}).get("clat_ns", {}).get("max", 0) or 0)
           for j in jobs)
clat_ms = clat / 1e6
bad = []
if err != 0:
    bad.append("fio error=%d" % err)
if ioerr != 0:
    bad.append("io_errors=%d" % ioerr)
if clat_ms > clat_max_ms:
    bad.append("max clat %.0f ms > %d ms" % (clat_ms, clat_max_ms))
if bad:
    print("%s: %s" % (name, "; ".join(bad)))
    sys.exit(1)
print("%s: err 0, io_errors 0, max clat %.0f ms" % (name, clat_ms))
PY
		then
			pass "${name}: fio clean, clat within ${CLAT_MAX_MS} ms"
		else
			fail "${name}: fio did not complete cleanly"
		fi
	done
fi

# ==========================================================================
# [7] target log
# ==========================================================================
echo
echo "=== [7] target log"

# rcow_target_alive resolves RCOW_TGT_BIN, which after the cross-SPDK upgrade
# names the other tree's binary -- a live target read as gone. The instance scan
# carries the basename fallback for exactly that case.
if [ -n "$(rcow_target_instances)" ]; then
	pass "the target is running at the end of the run"
else
	fail "the target is not running at the end of the run"
fi

if grep -qE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" 2>/dev/null; then
	fail "assertion or fault in the target log"
	grep -nE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no asserts and no faults in the recovered target's log"
fi
