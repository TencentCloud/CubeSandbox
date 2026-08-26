#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  A real filesystem on an activated volume, and snapshots of it.
#
#  === Why this suite exists, when run_snapshot_test.sh already covers snapshots ===
#
#  Everything else in test/dataplane reaches the volume with dd, fio or
#  blkdiscard: aligned, direct, one pattern at a known offset. A filesystem does
#  none of that. mkfs and mount issue small unaligned metadata writes, hundreds of
#  segments per request, and -- the part that turned out to matter -- an NVMe FLUSH
#  per journal commit.
#
#  That last one is not hypothetical. The first version of this stack implemented
#  SPDK_BDEV_IO_TYPE_FLUSH as spdk_blob_sync_md(), a blobstore *metadata*
#  operation, which blobstore asserts is only ever issued on its md_thread. A
#  FLUSH arrives on whichever nvmf poll group owns the qpair, never that thread,
#  so the target died the moment a host ran mkfs.xfs:
#
#      blob_verify_md_op: Assertion
#      `spdk_get_thread() == blob->bs->md_thread' failed.
#
#  Fifteen suites and 643 assertions passed over that bug, because not one of them
#  ever asked the volume to behave like a disk. This one does, and step [12] greps
#  for that assertion by name.
#
#  === What is actually asserted ===
#
#  The load-bearing claim is at file granularity, not block granularity:
#
#    a snapshot taken of a quiesced filesystem, mounted read-only elsewhere,
#    contains exactly the files the origin held at the moment it was taken --
#    same set, same contents -- and none of the changes made afterwards.
#
#  "Exactly" is a diff of two manifests (every file's md5, sorted), not a spot
#  check, and the manifest is asserted to be non-trivial first: comparing two
#  empty listings succeeds and proves nothing.
#
#  === Mounting a snapshot: what the host has to do. Measured, not assumed ===
#
#  This is the operational finding the suite exists to record, and the first two
#  versions of it guessed wrong.
#
#  A snapshot refuses writes at the bdev layer, but nvmf has no way to say so:
#  there is no write-protect bit in the namespace it reports (the note in
#  vbdev_s3lvol_lvol.c explains why, and run_snapshot_test.sh step [8] pins
#  blockdev --getro at 0 so that it breaks loudly if that ever changes). The host
#  therefore believes a snapshot is an ordinary writable disk.
#
#  XFS decides whether it may write during mount by asking the *block device*:
#  xlog_find_tail() calls xlog_clear_stale_blocks(), which writes, and skips it
#  only when xfs_readonly_buftarg() is true. With the host's flag at 0 that write
#  is attempted, the bdev rejects it, and the mount dies of an EIO that mount(8)
#  reports as the thoroughly misleading "can't read superblock":
#
#      XFS (nvme17n1): log recovery write I/O error at daddr 0x28 len 4096 error -5
#      XFS (nvme17n1): failed to locate log tail
#
#  The "recovery" there is not recovery of anything -- it happens with a perfectly
#  clean log, which is what took two attempts to work out.
#
#  blockdev --setro on the host is what closes the gap, and then XFS behaves:
#
#    snapshot taken of        --setro?  mount -o ro,nouuid
#    -----------------------  --------  -----------------------------------------
#    an unmounted filesystem  no        EIO, "can't read superblock"
#    an unmounted filesystem  yes       mounts, "Ending clean mount"
#    a frozen mounted fs      yes       refused honestly -- "recovery required on
#                                       read-only device, write access
#                                       unavailable"; takes -o norecovery
#
#  So: always --setro a snapshot before mounting it, and add -o norecovery when the
#  snapshot was taken of a live filesystem, because xfs_freeze does not cover the
#  log (xfs_quiesce_attr() forces the log and reclaims inodes; it writes no unmount
#  record) and a read-only device cannot replay one.
#
#  Steps [8] and [10] assert every row of that table, the two refusals included. A
#  refusal asserted without its reason is worth nothing -- any unrelated breakage
#  also fails to mount -- so each is matched against the specific kernel message.
#
#  nouuid throughout: a snapshot carries its origin's filesystem UUID, and XFS
#  refuses to mount a duplicate while the origin is mounted.
#
#  === Isolation ===
#
#  Own lvstore name, own WAL image, own run directory, so a production instance is
#  untouched. /data/cubelet/rcow/bstore.json and
#  /data/cubelet/rcow/active_lvols are shared
#  regardless -- their paths are compiled into the module (vbdev_s3lvol_active.c,
#  vbdev_s3lvol_bstore.c) -- so the test refuses to run when anything is already
#  recorded as active, and removes only its own entries.
#
#  dmesg gets I/O errors from steps [8] and [10] on purpose, the same way
#  run_snapshot_test.sh does when it proves a snapshot rejects writes.
#
#  Usage:
#    sudo -E ./test/dataplane/run_fs_test.sh
#
#  Needs root (target, nvme connect, mount), a readable /data/cubelet/cos.cfg,
#  nvme-cli and xfsprogs.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

# Test-specific, and exported so rcow_start.sh / rcow_stop.sh pick them up.
export RCOW_LVS_NAME=fsvs
export RCOW_WAL_IMG=/data/s3lvol_fs_wal.img
export RCOW_WAL_BDEV=fs_wal0
export RCOW_CAPACITY_GB=8
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_fstest
export RCOW_LOG_DIR=/var/tmp/rcow_fstest/log
export RCOW_COS_CFG="${RCOW_COS_CFG:-/data/cubelet/cos.cfg}"

# This suite's own registries, not the host's. They used to be the compiled-in
# /var/tmp paths, shared with every instance on the machine -- and cleanup below
# removes the active one outright, which is how a live instance lost its registry.
# The module resolves both through the environment now, and rcow_common.sh passes
# them on to the target.
export RCOW_ACTIVE_FILE=/var/tmp/rcow_fstest/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_fstest/bstore.json
ACTIVE_FILE="${RCOW_ACTIVE_FILE}"
BSTORE_FILE="${RCOW_BSTORE_FILE}"

ORIGIN=fs-a
SNAP_LIVE=fs-snap        # taken while the filesystem is frozen and mounted
SNAP_CLEAN=fs-snap-clean # taken after the filesystem has been unmounted
ORIGIN_GIB=2

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

WORKDIR=""
MNT_ORIGIN=""
MNT_LIVE=""
MNT_CLEAN=""
FROZEN=0
STARTED=0

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc()  { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }
jget() { python3 -c 'import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }

# Every file under a mount point with its md5, in a stable order. Paths stay
# relative ("./set1/big"), which is what makes an origin manifest and a snapshot
# manifest directly comparable even though the mount points differ.
fs_manifest()
{
	( cd "$1" && find . -type f -print0 | sort -z | xargs -0 md5sum )
}

# Directories too, so that a snapshot missing an empty directory is not reported
# as identical.
fs_listing()
{
	( cd "$1" && find . | sort )
}

# Force the next read to come from the device rather than the page cache. Without
# this, "the data is still there" would be answered out of memory by the same
# kernel that was just told what to write.
drop_caches()
{
	sync
	echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true
}

# Unmount, and say so if it needed force. A lazy unmount leaves the device busy,
# which then surfaces as an unrelated failure in the deactivate below, so the
# distinction is worth recording rather than hiding behind `|| true`.
unmount_quietly()
{
	local mnt="$1"

	mountpoint -q "${mnt}" 2>/dev/null || return 0
	if umount "${mnt}" 2>/dev/null; then
		return 0
	fi
	sync
	sleep 1
	umount "${mnt}" 2>/dev/null && return 0
	info "umount ${mnt} needed -l; something still holds it"
	umount -l "${mnt}" 2>/dev/null
	return 1
}

# Activate a volume and echo the block device the host ended up with. Prints
# nothing on failure; the caller decides what that means, because this runs in a
# command substitution and anything it did to PASS/FAIL would be lost with the
# subshell.
activate_and_resolve()
{
	local name="$1"

	rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" \
		>/dev/null 2>&1 || return 1
	rcow_verify_active 30 >/dev/null 2>&1 || return 1
	jget "$(rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "${name}")")" \
		device_path 2>/dev/null
}

# Occurrences of a kernel message, for before/after comparison across one mount
# attempt. Counting rather than slicing by line offset is deliberate: on a
# long-lived host the ring buffer is full, so its line count barely moves as
# messages arrive and `dmesg | tail -n +$N` reliably returns nothing. That mistake
# cost a run.
dmesg_count()
{
	dmesg | grep -c "$1" 2>/dev/null || true
}

# --------------------------------------------------------------------------
cleanup()
{
	echo ""
	echo "=== cleanup"

	# Thawing first: a frozen filesystem makes umount block forever, and this
	# runs on the failure path too, where the freeze may still be in effect.
	if [ "${FROZEN}" -eq 1 ] && [ -n "${MNT_ORIGIN}" ]; then
		xfs_freeze -u "${MNT_ORIGIN}" 2>/dev/null && info "origin thawed"
		FROZEN=0
	fi

	[ -n "${MNT_CLEAN}" ]  && unmount_quietly "${MNT_CLEAN}"
	[ -n "${MNT_LIVE}" ]   && unmount_quietly "${MNT_LIVE}"
	[ -n "${MNT_ORIGIN}" ] && unmount_quietly "${MNT_ORIGIN}"

	if rcow_target_alive; then
		for name in "${SNAP_CLEAN}" "${SNAP_LIVE}" "${ORIGIN}"; do
			rpc rcow_deactive_bdev \
				"$(printf '{"device_name":"%s"}' "${name}")" \
				>/dev/null 2>&1
		done

		# Delete the lvstore rather than unload it: that removes the objects
		# and the bstore.json entry together. After a failure keep both --
		# they are the evidence.
		if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			rpc rcow_delete_lvstore \
				"$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
				>/dev/null 2>&1 || info "delete_lvstore failed"
		fi
	fi

	[ "${STARTED}" -eq 1 ] && "${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		ENDPOINT="$(rcow_cfg_get cos_endpoint)"
		REGION="$(rcow_cfg_get region)"
		BUCKET="$(rcow_cos_buckets | head -1)"
		rcow_load_credentials
		python3 "${PREFIX_RM}" -e "${ENDPOINT}" -b "${BUCKET}" \
			-r "${REGION}" -p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}" "${ACTIVE_FILE}" "${ACTIVE_FILE}.replay"
		python3 - "${BSTORE_FILE}" "${RCOW_LVS_NAME}" <<'PY'
import json, sys
p = sys.argv[1]
try:
    d = json.load(open(p))
except Exception:
    raise SystemExit
if d.pop(sys.argv[2], None) is not None:
    json.dump(d, open(p, "w"), indent=2)
PY
		[ -n "${WORKDIR}" ] && rm -rf "${WORKDIR}"
	else
		info "state kept: ${RCOW_WAL_IMG}, ${ACTIVE_FILE}, ${RCOW_LOG}, ${WORKDIR}"
	fi

	echo ""
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# ==========================================================================
echo "=== [0] preconditions"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -x "${ROOT}/app/s3lvol_tgt/s3lvol_tgt" ] || { echo "target not built" >&2; exit 1; }
[ -r "${RCOW_COS_CFG}" ] || { echo "no COS config at ${RCOW_COS_CFG}" >&2; exit 1; }

for tool in nvme mkfs.xfs xfs_freeze xfs_repair mountpoint md5sum python3; do
	command -v "${tool}" >/dev/null 2>&1 || {
		echo "${tool} is required (nvme-cli, xfsprogs)" >&2; exit 1; }
done

# Usually not loaded on a fresh boot, and mount's autoload happens too late to
# give a clear message.
if ! grep -qw xfs /proc/filesystems; then
	modprobe xfs 2>/dev/null || true
	grep -qw xfs /proc/filesystems || {
		echo "the kernel has no xfs support" >&2; exit 1; }
fi

if [ -n "$(rcow_target_instances)" ]; then
	echo "an s3lvol_tgt is already running; stop it first" >&2
	exit 1
fi

# The run directory holds the target log, which is appended to and which step [12]
# greps. Keeping a previous run's would report its asserts against this run and,
# worse, report the run that caused them as green.
rm -rf "${RCOW_RUN_DIR}"
mkdir -p "${RCOW_RUN_DIR}"

WORKDIR="$(mktemp -d /tmp/rcow_fstest.XXXXXX)"
MNT_ORIGIN="${WORKDIR}/origin"
MNT_LIVE="${WORKDIR}/snap_live"
MNT_CLEAN="${WORKDIR}/snap_clean"
mkdir -p "${MNT_ORIGIN}" "${MNT_LIVE}" "${MNT_CLEAN}"

rm -f "${RCOW_WAL_IMG}"
truncate -s 1G "${RCOW_WAL_IMG}"
pass "clean slate; xfsprogs $(mkfs.xfs -V 2>&1 | awk '{print $3}'), kernel $(uname -r)"

# ==========================================================================
echo ""
echo "=== [1] bringing the data plane up"

if "${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1; then
	STARTED=1
	pass "rcow_start.sh succeeded"
else
	fail "rcow_start.sh failed"
	tail -25 "${WORKDIR}/start.log" | sed 's/^/       /'
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [2] create and activate the origin volume"

rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
	"${ORIGIN}" "${ORIGIN_GIB}")" >/dev/null || {
	fail "rcow_create_lvol ${ORIGIN}"; exit 1; }
pass "${ORIGIN} created (${ORIGIN_GIB} GiB)"

A_DEV="$(activate_and_resolve "${ORIGIN}")"
if [ -n "${A_DEV}" ] && [ -b "${A_DEV}" ]; then
	pass "${ORIGIN} is activated and reachable as ${A_DEV}"
else
	fail "${ORIGIN} did not become a block device (got '${A_DEV}')"
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [3] mkfs.xfs"
#
# -K skips mkfs's discard of the whole device. Not because discard is broken --
# run_snapshot_test.sh asserts it reclaims S3 objects -- but because a 2 GiB unmap
# here would dominate the runtime of a test that is about something else.

if mkfs.xfs -f -K "${A_DEV}" >"${WORKDIR}/mkfs.log" 2>&1; then
	pass "mkfs.xfs succeeded"
	info "$(grep -m1 '^data' "${WORKDIR}/mkfs.log" | tr -s ' ')"
else
	fail "mkfs.xfs failed"
	sed 's/^/       /' "${WORKDIR}/mkfs.log"
	exit 1
fi

# Not a formality: mkfs reporting success while the superblock never reached the
# device is exactly what a broken flush path looks like from here. blkid reads the
# device again, after the cache was dropped.
drop_caches
FSTYPE="$(blkid -o value -s TYPE "${A_DEV}" 2>/dev/null || echo)"
[ "${FSTYPE}" = "xfs" ] && pass "the device identifies as xfs after a cache drop" \
	|| fail "blkid reports '${FSTYPE}', not xfs"

if grep -qiE 'error|cannot' "${WORKDIR}/mkfs.log"; then
	fail "mkfs.xfs reported problems"
	grep -inE 'error|cannot' "${WORKDIR}/mkfs.log" | head -5 | sed 's/^/       /'
else
	pass "mkfs.xfs logged no errors"
fi

# ==========================================================================
echo ""
echo "=== [4] mount and write the dataset"

if mount -t xfs "${A_DEV}" "${MNT_ORIGIN}" 2>"${WORKDIR}/mount.err"; then
	pass "mounted read-write at ${MNT_ORIGIN}"
else
	fail "mount failed: $(tr -d '\n' <"${WORKDIR}/mount.err")"
	exit 1
fi

# A few sizes spanning the interesting boundaries -- below a 4 KiB block, below and
# above the 1 MiB chunk, and one large enough to cross several clusters -- plus
# enough small files that the directory and inode metadata is non-trivial.
mkdir -p "${MNT_ORIGIN}/set1/nested" "${MNT_ORIGIN}/set1/empty_dir"
for spec in "tiny 1 1K" "block 1 4K" "sub_chunk 1 256K" "chunk 1 1M" \
	    "multi 5 1M" "big 24 1M"; do
	set -- ${spec}
	dd if=/dev/urandom of="${MNT_ORIGIN}/set1/$1" bs="$3" count="$2" \
		status=none || fail "writing set1/$1 failed"
done
for i in $(seq -w 1 40); do
	dd if=/dev/urandom of="${MNT_ORIGIN}/set1/nested/s${i}" bs=4K count=1 \
		status=none
done
printf 'written before the snapshot\n' >"${MNT_ORIGIN}/set1/marker.txt"

# The step that makes the host issue NVMe FLUSH commands in bulk.
sync

FILES1="$(find "${MNT_ORIGIN}" -type f | wc -l)"
BYTES1="$(du -sk "${MNT_ORIGIN}" | cut -f1)"
if [ "${FILES1}" -ge 45 ]; then
	pass "${FILES1} files, ${BYTES1} KiB written and synced"
else
	fail "only ${FILES1} files made it; the comparisons below would prove nothing"
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [5] freeze, snapshot, thaw"
#
# Freezing is what a real orchestrator has to do to snapshot a mounted volume: it
# waits for in-flight transactions, flushes data, metadata and the superblock, so
# the snapshot holds a filesystem that is coherent rather than caught mid-update.
# What it does *not* do on this kernel is cover the log -- see the header, and step
# [8], which is where that shows up.

if xfs_freeze -f "${MNT_ORIGIN}" 2>"${WORKDIR}/freeze.err"; then
	FROZEN=1
	pass "the origin filesystem is frozen"
else
	fail "xfs_freeze -f failed: $(tr -d '\n' <"${WORKDIR}/freeze.err")"
	exit 1
fi

# Captured while frozen: this is the exact state the snapshot must contain.
MANIFEST1="${WORKDIR}/manifest1.txt"
LISTING1="${WORKDIR}/listing1.txt"
fs_manifest "${MNT_ORIGIN}" >"${MANIFEST1}"
fs_listing  "${MNT_ORIGIN}" >"${LISTING1}"

SNAP_RC=0
rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${ORIGIN}" "${SNAP_LIVE}")" >"${WORKDIR}/snap.json" \
	2>"${WORKDIR}/snap.err" || SNAP_RC=1

# Thawing unconditionally, before judging the snapshot: a filesystem left frozen
# makes every later umount -- including the one in cleanup -- block forever.
if xfs_freeze -u "${MNT_ORIGIN}" 2>/dev/null; then
	FROZEN=0
	pass "the origin filesystem is thawed again"
else
	fail "xfs_freeze -u failed; the mount is stuck frozen"
fi

if [ "${SNAP_RC}" -eq 0 ]; then
	pass "snapshot ${SNAP_LIVE} taken while frozen"
else
	fail "rcow_create_snapshot failed: $(tr -d '\n' <"${WORKDIR}/snap.err")"
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [6] change the origin -- the snapshot must not follow"
#
# Three kinds of change, because they reach the snapshot differently: new files
# allocate clusters the snapshot never had, a deletion frees clusters it still
# references, and an in-place overwrite is the one that has to take the
# copy-on-write path rather than rewriting a shared cluster.

mkdir -p "${MNT_ORIGIN}/set2"
for i in 1 2 3; do
	dd if=/dev/urandom of="${MNT_ORIGIN}/set2/after${i}" bs=1M count=2 status=none
done
rm -f "${MNT_ORIGIN}/set1/chunk"
dd if=/dev/urandom of="${MNT_ORIGIN}/set1/big" bs=1M count=1 conv=notrunc \
	status=none
printf 'written after the snapshot\n' >"${MNT_ORIGIN}/set1/marker.txt"
sync

MANIFEST2="${WORKDIR}/manifest2.txt"
fs_manifest "${MNT_ORIGIN}" >"${MANIFEST2}"

if cmp -s "${MANIFEST1}" "${MANIFEST2}"; then
	fail "the origin is unchanged after the writes; step [9] would pass vacuously"
	exit 1
else
	pass "the origin moved on ($(comm -13 <(cut -d' ' -f1 "${MANIFEST1}" | sort) \
		<(cut -d' ' -f1 "${MANIFEST2}" | sort) | wc -l) new checksums)"
fi

# Push everything acknowledged so far into S3, so that the snapshot's data has to
# come back from the object store rather than from the WAL overlay. A snapshot
# that is right in memory and wrong in S3 is the more likely of the two failures.
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 && pass "the lvstore flushed to S3" \
	|| info "flush_lvstore failed (continuing)"

# ==========================================================================
echo ""
echo "=== [7] activate the snapshot"

S_DEV="$(activate_and_resolve "${SNAP_LIVE}")"
if [ -n "${S_DEV}" ] && [ -b "${S_DEV}" ] && [ "${S_DEV}" != "${A_DEV}" ]; then
	pass "${SNAP_LIVE} is activated as its own device ${S_DEV}"
else
	fail "${SNAP_LIVE} did not become its own block device (got '${S_DEV}')"
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [8] mounting the snapshot of a live filesystem"
#
# Three rows of the table in the header, in the order an operator discovers them:
# the plain mount fails confusingly, --setro turns that into an honest refusal, and
# norecovery is what actually works. All three are asserted, because the failure to
# guard against is somebody reading the first line alone and concluding that
# snapshots cannot be mounted.

drop_caches

# Cross-checked against run_snapshot_test.sh step [8], which pins the same value
# from the other direction. If nvmf ever reports write protection this flips, and
# the whole step below becomes unnecessary -- so it is asserted, not assumed.
S_RO="$(blockdev --getro "${S_DEV}" 2>/dev/null || echo '?')"
if [ "${S_RO}" = "0" ]; then
	pass "the host sees the snapshot as writable (nvmf reports no write-protect bit)"
else
	fail "blockdev --getro says '${S_RO}'; nvmf grew nsattr.cwp support?"
	info "good news if so, but vbdev_s3lvol_lvol.c and run_snapshot_test.sh"
	info "document the opposite and need updating together with this step"
fi

EIO_BEFORE="$(dmesg_count 'log recovery write I/O error')"
if mount -t xfs -o ro,nouuid "${S_DEV}" "${MNT_LIVE}" \
		2>"${WORKDIR}/mount_plain.err"; then
	fail "a plain read-only mount succeeded, which the header says it cannot"
	info "if the kernel or nvmf changed, this is an improvement -- rewrite the"
	info "table in the header from what it now does"
	unmount_quietly "${MNT_LIVE}"
else
	pass "plain -o ro,nouuid is refused: $(tr -d '\n' <"${WORKDIR}/mount_plain.err")"

	# The reason matters more than the refusal: any unrelated breakage also
	# fails to mount, and would be quietly accepted here as "expected".
	if [ "$(dmesg_count 'log recovery write I/O error')" -gt "${EIO_BEFORE}" ]; then
		pass "for the documented reason -- XFS wrote to the log because the host"
		pass "flag says writable, and the snapshot rejected the write"
	else
		fail "but not for the documented reason: no new 'log recovery write I/O"
		fail "error' in dmesg, so something else is wrong with the snapshot"
		dmesg | grep -i 'XFS (' | tail -6 | sed 's/^/       /'
	fi
fi

# The one thing the host can do about a device that is read-only without saying so.
RO_BEFORE="$(dmesg_count 'recovery required on read-only device')"
if blockdev --setro "${S_DEV}" && [ "$(blockdev --getro "${S_DEV}")" = "1" ]; then
	pass "blockdev --setro marks it read-only host-side"
else
	fail "blockdev --setro ${S_DEV} did not take"
fi

if mount -t xfs -o ro,nouuid "${S_DEV}" "${MNT_LIVE}" \
		2>"${WORKDIR}/mount_setro.err"; then
	fail "it mounted without norecovery; the frozen snapshot's log was clean after"
	fail "all, so the header's claim about xfs_freeze needs revisiting"
	unmount_quietly "${MNT_LIVE}"
elif [ "$(dmesg_count 'recovery required on read-only device')" -gt "${RO_BEFORE}" ]; then
	pass "and XFS now refuses honestly: recovery required, write access"
	pass "unavailable -- no more EIO pretending the superblock is unreadable"
else
	fail "the refusal did not name the reason: expected 'recovery required on"
	fail "read-only device' in dmesg"
	dmesg | grep -i 'XFS (' | tail -6 | sed 's/^/       /'
fi

if mount -t xfs -o ro,norecovery,nouuid "${S_DEV}" "${MNT_LIVE}" \
		2>"${WORKDIR}/mount_live.err"; then
	pass "-o ro,norecovery,nouuid mounts it: the supported way to read a snapshot"
	pass "of a live filesystem"
else
	fail "even with norecovery the snapshot would not mount: \
$(tr -d '\n' <"${WORKDIR}/mount_live.err")"
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [9] the snapshot equals the origin as it was at the freeze"

SNAP_MANIFEST="${WORKDIR}/manifest_live.txt"
SNAP_LISTING="${WORKDIR}/listing_live.txt"
fs_manifest "${MNT_LIVE}" >"${SNAP_MANIFEST}"
fs_listing  "${MNT_LIVE}" >"${SNAP_LISTING}"

if diff -u "${MANIFEST1}" "${SNAP_MANIFEST}" >"${WORKDIR}/manifest.diff"; then
	pass "every one of the ${FILES1} files matches the origin byte for byte"
else
	fail "the snapshot's contents differ from the origin at freeze time"
	head -20 "${WORKDIR}/manifest.diff" | sed 's/^/       /'
fi

if diff -u "${LISTING1}" "${SNAP_LISTING}" >"${WORKDIR}/listing.diff"; then
	pass "the directory tree is identical, empty directories included"
else
	fail "the snapshot's directory tree differs"
	head -20 "${WORKDIR}/listing.diff" | sed 's/^/       /'
fi

# The three isolation checks spelled out, so that a failure names which kind of
# change leaked through instead of pointing at a diff.
[ ! -d "${MNT_LIVE}/set2" ] \
	&& pass "files created after the snapshot are absent from it" \
	|| fail "the snapshot shows set2/, which was created after it was taken"

[ -f "${MNT_LIVE}/set1/chunk" ] \
	&& pass "a file deleted from the origin is still in the snapshot" \
	|| fail "deleting from the origin removed the file from the snapshot too"

SNAP_MARKER="$(cat "${MNT_LIVE}/set1/marker.txt" 2>/dev/null || echo '<unreadable>')"
[ "${SNAP_MARKER}" = "written before the snapshot" ] \
	&& pass "an overwritten file reads its pre-snapshot contents" \
	|| fail "the snapshot's marker.txt says '${SNAP_MARKER}'"

unmount_quietly "${MNT_LIVE}"

# ==========================================================================
echo ""
echo "=== [10] the snapshot of an unmounted filesystem, which mounts normally"
#
# The contrast that makes step [8] precise. Same volume, same RPC; the only
# difference is that the filesystem was unmounted first, which writes an unmount
# record and leaves the log clean. That is the row of the table where a snapshot
# mounts with nothing but -o ro,nouuid -- provided --setro, because a clean log
# does not spare the host from XFS's write to xlog_clear_stale_blocks().

if unmount_quietly "${MNT_ORIGIN}"; then
	pass "the origin unmounted cleanly"
else
	fail "the origin could not be unmounted"
fi

rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${ORIGIN}" "${SNAP_CLEAN}")" >/dev/null 2>"${WORKDIR}/snap2.err" \
	&& pass "snapshot ${SNAP_CLEAN} taken with the filesystem unmounted" \
	|| { fail "rcow_create_snapshot ${SNAP_CLEAN}: \
$(tr -d '\n' <"${WORKDIR}/snap2.err")"; }

C_DEV="$(activate_and_resolve "${SNAP_CLEAN}")"
if [ -n "${C_DEV}" ] && [ -b "${C_DEV}" ]; then
	pass "${SNAP_CLEAN} is ${C_DEV}"
else
	fail "${SNAP_CLEAN} did not become a block device (got '${C_DEV}')"
fi

if [ -b "${C_DEV}" ]; then
	drop_caches

	# Worth having separately from the file comparison: metadata can be
	# inconsistent while every file still reads back correctly, and that is the
	# failure that surfaces weeks later instead of here. It belongs to this
	# snapshot and not to the frozen one -- with a dirty log, -n declines to
	# replay it and reports the free-space counters it could not reconcile,
	# which is a complaint about the log rather than about the snapshot.
	if xfs_repair -n "${C_DEV}" >"${WORKDIR}/repair.log" 2>&1; then
		pass "xfs_repair -n finds the snapshot consistent"
	else
		fail "xfs_repair -n reports problems with the snapshot"
		grep -vE '^\s*- (agno|scan|process|setting|check|traversing|traversal|moving|zero|found|using)' \
			"${WORKDIR}/repair.log" | head -15 | sed 's/^/       /'
	fi

	# The trap, asserted so that nobody has to fall into it twice: a clean log is
	# not sufficient. Without --setro this fails exactly like the frozen snapshot
	# did, for a reason that has nothing to do with the log.
	EIO_BEFORE="$(dmesg_count 'log recovery write I/O error')"
	if mount -t xfs -o ro,nouuid "${C_DEV}" "${MNT_CLEAN}" \
			2>"${WORKDIR}/mount_clean_norw.err"; then
		fail "it mounted before --setro; then XFS no longer writes during mount"
		info "on a device it believes is writable, and the header is out of date"
		unmount_quietly "${MNT_CLEAN}"
	elif [ "$(dmesg_count 'log recovery write I/O error')" -gt "${EIO_BEFORE}" ]; then
		pass "a clean log is not enough: without --setro XFS still writes during"
		pass "mount, and is still refused"
	else
		fail "the mount failed for an undocumented reason"
		dmesg | grep -i 'XFS (' | tail -6 | sed 's/^/       /'
	fi

	CLEAN_BEFORE="$(dmesg_count 'Ending clean mount')"
	blockdev --setro "${C_DEV}" || fail "blockdev --setro ${C_DEV} failed"

	if mount -t xfs -o ro,nouuid "${C_DEV}" "${MNT_CLEAN}" \
			2>"${WORKDIR}/mount_clean.err"; then
		pass "with --setro it mounts read-only, no norecovery and no warnings"

		[ "$(dmesg_count 'Ending clean mount')" -gt "${CLEAN_BEFORE}" ] \
			&& pass "and XFS called it a clean mount, so nothing was replayed" \
			|| info "XFS did not log 'Ending clean mount' for it"

		fs_manifest "${MNT_CLEAN}" >"${WORKDIR}/manifest_clean.txt"
		if diff -u "${MANIFEST2}" "${WORKDIR}/manifest_clean.txt" \
				>"${WORKDIR}/clean.diff"; then
			pass "and holds exactly what the origin held when it was unmounted"
		else
			fail "its contents differ from the origin at unmount time"
			head -20 "${WORKDIR}/clean.diff" | sed 's/^/       /'
		fi
		unmount_quietly "${MNT_CLEAN}"
	else
		fail "mounting ${SNAP_CLEAN} failed even with --setro: \
$(tr -d '\n' <"${WORKDIR}/mount_clean.err")"
		dmesg | grep -i 'XFS (' | tail -6 | sed 's/^/       /'
	fi
fi

# ==========================================================================
echo ""
echo "=== [11] the origin survived all of it"
#
# A fresh read-write mount replays the log and re-reads every inode from the
# device, so this also proves the origin's metadata is intact -- and it needs
# writes, which the origin, unlike its snapshots, must accept.

drop_caches
if mount -t xfs "${A_DEV}" "${MNT_ORIGIN}" 2>"${WORKDIR}/remount.err"; then
	pass "the origin mounted again read-write"
else
	fail "remounting the origin failed: $(tr -d '\n' <"${WORKDIR}/remount.err")"
fi

if mountpoint -q "${MNT_ORIGIN}"; then
	fs_manifest "${MNT_ORIGIN}" >"${WORKDIR}/manifest2_again.txt"
	if diff -u "${MANIFEST2}" "${WORKDIR}/manifest2_again.txt" \
			>"${WORKDIR}/origin.diff"; then
		pass "the origin still holds exactly what was written after the snapshot"
	else
		fail "the origin's contents changed across the remount"
		head -20 "${WORKDIR}/origin.diff" | sed 's/^/       /'
	fi

	printf 'still writable\n' >"${MNT_ORIGIN}/set2/post_remount.txt" 2>/dev/null \
		&& sync && pass "the origin is still writable with two snapshots of it" \
		|| fail "writing to the origin after the remount failed"
fi

# ==========================================================================
echo ""
echo "=== [12] target log"

if rcow_target_alive; then
	pass "the target is still running"
else
	fail "the target is no longer running"
	rcow_log_tail 30
fi

# Named explicitly, because this is the assertion the suite was written for: a
# FLUSH from the host reaching a blobstore metadata call would abort here and
# nowhere else in the tree (HANDOFF 7.17).
if grep -q 'blob_verify_md_op' "${RCOW_LOG}" 2>/dev/null; then
	fail "blobstore's md_thread assertion fired: a metadata call was made from an"
	fail "I/O thread. See ${RCOW_LOG}"
	grep -n 'blob_verify_md_op' "${RCOW_LOG}" | head -3 | sed 's/^/       /'
else
	pass "no md_thread assertion: the FLUSH path stayed off blob metadata"
fi

if grep -qE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" 2>/dev/null; then
	fail "the target log has an assertion or a fault"
	grep -nE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no asserts and no faults in the target log"
fi

# The exception list is the one run_snapshot_test.sh explains: a 404 is how this
# code asks whether a key exists, and aws-c-s3 logs every non-2xx at ERROR.
LOG_ERRORS="$(grep -inE 'error|failed' "${RCOW_LOG}" 2>/dev/null | \
	grep -viE 'already|not found|read-only|is not a snapshot|meta/owner' | \
	grep -viE 'response status=404' || true)"
if [ -n "${LOG_ERRORS}" ]; then
	info "errors mentioned in the log (expected ones filtered out):"
	echo "${LOG_ERRORS}" | head -8 | sed 's/^/       /'
else
	pass "no unexpected errors in the target log"
fi
