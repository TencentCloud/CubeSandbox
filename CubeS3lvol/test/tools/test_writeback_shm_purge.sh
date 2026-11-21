#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The write-back ring of a purged lvstore has to go with it.
#
# The ring lives in /dev/shm and outlives the target. Once rcow_purge.sh has
# dropped the bstore.json entry, the next start creates instead of attaching,
# and create refuses a same-name ring that still holds records -- so a purge
# that leaves the ring behind turns into a target that fails at every start.
#
# Uses the real /dev/shm, because that is where rcow_writeback_shm_paths looks,
# with an lvstore name nobody else can have. No target, no S3: the purge runs
# with --keep-s3 --keep-wal, and RCOW_TGT_BIN points nowhere so no running
# target is ever mistaken for one holding this lvstore.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"
holder=""

RCOW_LVS_NAME="wbpurge-$$-${RANDOM}${RANDOM}"
RCOW_ACTIVE_FILE="${tmp}/active_lvols"
RCOW_BSTORE_FILE="${tmp}/bstore.json"
RCOW_WAL_IMG="${tmp}/wal_bdev.img"
RCOW_RPC_SOCK="${tmp}/no.sock"
RCOW_TGT_BIN="${tmp}/no-such-target"
export RCOW_LVS_NAME RCOW_ACTIVE_FILE RCOW_BSTORE_FILE RCOW_WAL_IMG \
	RCOW_RPC_SOCK RCOW_TGT_BIN
# shellcheck source=../../scripts/rcow_common.sh
source "${ROOT}/scripts/rcow_common.sh"

passed=0
failed=0
check() {
	local name="$1"
	shift
	if "$@"; then
		printf '\t[PASS] %s\n' "${name}"
		passed=$((passed + 1))
	else
		printf '\t[FAIL] %s\n' "${name}"
		failed=$((failed + 1))
	fi
}

echo "=== test_writeback_shm_purge ==="

if ! command -v flock >/dev/null 2>&1 || [ ! -d /dev/shm ] || [ ! -w /dev/shm ]; then
	# rc 2 with a summary line naming 0 failures is run_all.sh's "could not
	# run" signal, counted as a skip. Without the summary line run_suite()
	# reports "no summary line" and turns the whole run red.
	echo "SKIP: needs flock and a writable /dev/shm"
	printf '=== result: 0 passed, 0 failed ===\n'
	exit 2
fi

LEAF="$(rcow_writeback_shm_leaf "${RCOW_LVS_NAME}")"
DIR="/dev/shm/s3lvol-wb-$(id -u)"
RING="${DIR}/${LEAF}"
FLAT="/dev/shm/${LEAF}"
# A name the lock check may be pointed at without a ring behind it, and a file
# a planted symlink can point at, both inside this test's own names.
GHOST="${DIR}/${LEAF}.ghost"
DECOY="${tmp}/decoy"
made_dir=0
[ -d "${DIR}" ] || made_dir=1

cleanup() {
	[ -n "${holder}" ] && kill "${holder}" 2>/dev/null || true
	rm -f -- "${RING}" "${FLAT}" "${GHOST}"
	[ "${made_dir}" -eq 1 ] && rmdir -- "${DIR}" 2>/dev/null || true
	rm -rf "${tmp}"
}
trap cleanup EXIT

mkdir -p -- "${tmp}/bin"
printf '#!/bin/sh\nexit 1\n' >"${tmp}/bin/python3"
chmod +x "${tmp}/bin/python3"

[ "${made_dir}" -eq 0 ] || mkdir -m 0700 "${DIR}"

# --- listing ------------------------------------------------------------
check "no ring: nothing listed" [ -z "$(rcow_writeback_shm_paths)" ]

printf 'records' >"${RING}"
printf 'records' >"${FLAT}"
LISTED="$(rcow_writeback_shm_paths)"
check "the per-uid ring is listed" grep -qxF "${RING}" <<<"${LISTED}"
check "the legacy flat ring is listed" grep -qxF "${FLAT}" <<<"${LISTED}"

# --- a ring a process holds is refused, and not removed -----------------
# exec, so the pid in $! is the one holding the lock. With `flock FILE cmd` the
# fd is inherited by cmd and outlives a kill of flock itself.
( exec 9<>"${RING}"; flock -x 9; exec sleep 30 ) &
holder=$!
for _ in $(seq 50); do
	flock -n "${RING}" true 2>/dev/null || break
	sleep 0.1
done
rc=0
rcow_remove_unheld_writeback_shm >/dev/null 2>&1 || rc=$?
check "a held ring makes the removal fail" [ "${rc}" -ne 0 ]
check "and the held ring is still there" [ -e "${RING}" ]
kill "${holder}" 2>/dev/null || true
wait "${holder}" 2>/dev/null || true
holder=""

# --- a ring name that cannot be derived fails the removal ---------------
# Not a silent no-op: every caller reads success as "the ring is gone".
rc=0
(
	rcow_writeback_shm_leaf() { return 1; }
	rcow_remove_unheld_writeback_shm
) >/dev/null 2>&1 || rc=$?
check "an underivable ring name makes the removal fail" [ "${rc}" -ne 0 ]
check "and touches nothing" [ -e "${RING}" ]

# --- rcow_purge.sh takes the ring with the rest of the lvstore ----------
printf '{"%s":{"bs_name":"b","ns_name":"n","wal_bdev":"w"}}\n' \
	"${RCOW_LVS_NAME}" >"${RCOW_BSTORE_FILE}"
[ -e "${FLAT}" ] || printf 'records' >"${FLAT}"

DRY="$(bash "${ROOT}/scripts/rcow_purge.sh" --dry-run --keep-s3 --keep-wal 2>&1)"
# The "(N used)" suffix is produced only by the listing block, so this cannot be
# satisfied by a header line that prints the path unconditionally.
check "--dry-run shows the ring with its size" \
	grep -qE "write-back shm: ${RING} \([0-9]" <<<"${DRY}"
check "--dry-run leaves it alone" [ -e "${RING}" ]

rc=0
OUT="$(bash "${ROOT}/scripts/rcow_purge.sh" --yes --keep-s3 --keep-wal 2>&1)" || rc=$?
check "the purge succeeds" [ "${rc}" -eq 0 ]
check "it says it removed the ring" grep -qF "removed the write-back ring" <<<"${OUT}"
check "the per-uid ring is gone" [ ! -e "${RING}" ]
check "the legacy flat ring is gone" [ ! -e "${FLAT}" ]
check "and the bstore entry with it" \
	python3 -c 'import json,sys; sys.exit(sys.argv[2] in json.load(open(sys.argv[1])))' \
	"${RCOW_BSTORE_FILE}" "${RCOW_LVS_NAME}"

rc=0
bash "${ROOT}/scripts/rcow_purge.sh" --yes --keep-s3 --keep-wal >/dev/null 2>&1 || rc=$?
check "a second purge with nothing left is still clean" [ "${rc}" -eq 0 ]

# --- a held ring is refused before anything is deleted ------------------
# The preflight is what makes this safe: without it the purge deletes the S3
# prefix, the bstore entry and the WAL first and only then fails on the flock,
# leaving a target it could not see serving an lvstore whose objects are gone.
printf 'records' >"${RING}"
printf '{"%s":{"bs_name":"b","ns_name":"n","wal_bdev":"w"}}\n' \
	"${RCOW_LVS_NAME}" >"${RCOW_BSTORE_FILE}"
( exec 9<>"${RING}"; flock -x 9; exec sleep 30 ) &
holder=$!
for _ in $(seq 50); do
	flock -n "${RING}" true 2>/dev/null || break
	sleep 0.1
done
rc=0
OUT="$(bash "${ROOT}/scripts/rcow_purge.sh" --yes --keep-s3 --keep-wal 2>&1)" || rc=$?
check "a purge with a held ring exits non-zero" [ "${rc}" -ne 0 ]
check "it refuses before deleting anything" grep -q 'Nothing was deleted' <<<"${OUT}"
check "and leaves the held ring in place" [ -e "${RING}" ]
check "and leaves the bstore entry alone" \
	python3 -c 'import json,sys; sys.exit(sys.argv[2] not in json.load(open(sys.argv[1])))' \
	"${RCOW_BSTORE_FILE}" "${RCOW_LVS_NAME}"
kill "${holder}" 2>/dev/null || true
wait "${holder}" 2>/dev/null || true
holder=""

# --- a ring that cannot be located fails the purge ----------------------
# Not "absent": a leaf that cannot be derived used to read as "no ring", skip
# the removal and still exit 0, reporting an lvstore as gone with its ring in
# place. Faked with a python3 that fails, which is what derives the name.
rc=0
OUT="$(PATH="${tmp}/bin:${PATH}" bash "${ROOT}/scripts/rcow_purge.sh" \
	--yes --keep-s3 --keep-wal 2>&1)" || rc=$?
check "a ring that cannot be located fails the purge" [ "${rc}" -ne 0 ]
check "and says so rather than reporting it absent" \
	grep -q 'could not be located' <<<"${OUT}"
check "and leaves the ring in place" [ -e "${RING}" ]

# --- a failure earlier in the purge leaves the ring alone ---------------
# The ring is the only step a re-run cannot undo. With the bstore entry still in
# place the next start *attaches* and replays the ring, so removing it after a
# failed step would silently drop the writes that replay was going to recover --
# on an lvstore that is otherwise intact.
#
# The failure is injected by making the activation registry a non-empty
# directory: `rm -f` on one fails for root too, so this does not depend on
# permission bits the test may be running above.
printf 'records' >"${RING}"
printf '{"%s":{"bs_name":"b","ns_name":"n","wal_bdev":"w"}}\n' \
	"${RCOW_LVS_NAME}" >"${RCOW_BSTORE_FILE}"
rm -f -- "${RCOW_ACTIVE_FILE}"
mkdir -p -- "${RCOW_ACTIVE_FILE}/blocker"
rc=0
OUT="$(bash "${ROOT}/scripts/rcow_purge.sh" --yes --keep-s3 --keep-wal 2>&1)" || rc=$?
rm -rf -- "${RCOW_ACTIVE_FILE}"
check "a purge with a failed step exits non-zero" [ "${rc}" -ne 0 ]
check "and keeps the ring for the recovery to replay" [ -e "${RING}" ]
check "and says it left it in place" \
	grep -q 'leaving the write-back ring in place' <<<"${OUT}"
check "and does not claim to have removed it" \
	bash -c '! grep -q "removed the write-back ring" <<<"$1"' _ "${OUT}"

# --- the lock check does not create the file it checks ------------------
# `flock -n PATH true` opens with O_CREAT, as bash's <> redirection does, so a
# ring that went away between the listing and the check was re-created as a
# zero-length 0644 file. writeback_unlink() refuses exactly that (st_size <
# S3_WB_HEADER_SIZE, and a non-zero group/other mode) with -EACCES, so the
# check left behind the start failure this purge exists to clear.
rm -f -- "${GHOST}"
rc=0
rcow_open_locked_writeback_shm "${GHOST}" || rc=$?
check "locking a path that is gone reports no ring" [ "${rc}" -eq 2 ]
check "and does not create it" [ ! -e "${GHOST}" ]
flock -n "${GHOST}" true 2>/dev/null || true
check "which flock's own path form would have done" [ -f "${GHOST}" ]
rm -f -- "${GHOST}"

# --- a symlink at the ring's name is unlinked, not followed -------------
# /dev/shm is mode 1777 and the legacy flat leaf is derivable from the lvstore
# name, so any local user can plant one before root runs the purge. Following
# it had root open a file of their choosing, and take its lock: a symlink to a
# file somebody holds made the purge fail with "refusing to discard live
# write-back shm" for a ring that was never there, which is a local user
# denying the purge at will. writeback_unlink() uses O_NOFOLLOW for the same
# reason. The link is still unlinked -- the next create fails on it either way
# (openat() with O_NOFOLLOW gives ELOOP) -- but only the link, never its target.
rm -f -- "${RING}" "${FLAT}"
printf 'records' >"${RING}"
printf 'not a ring' >"${DECOY}"
( exec 9<>"${DECOY}"; flock -x 9; exec sleep 30 ) &
holder=$!
for _ in $(seq 50); do
	flock -n "${DECOY}" true 2>/dev/null || break
	sleep 0.1
done
ln -s "${DECOY}" "${FLAT}"
rc=0
OUT="$(rcow_remove_unheld_writeback_shm 2>&1)" || rc=$?
check "a planted symlink does not fail the removal" [ "${rc}" -eq 0 ]
check "it is not mistaken for a ring in use" \
	bash -c '! grep -q "refusing to discard live" <<<"$1"' _ "${OUT}"
check "the symlink itself is gone" bash -c '[ ! -L "$1" ]' _ "${FLAT}"
check "what it pointed at is untouched" [ -s "${DECOY}" ]
check "and the real ring went with it" [ ! -e "${RING}" ]
kill "${holder}" 2>/dev/null || true
wait "${holder}" 2>/dev/null || true
holder=""

# --- no flock: refused before anything is deleted -----------------------
# The removal needs flock as much as the preflight does. Skipping the preflight
# when it is missing meant the purge deleted the S3 prefix, the bstore entry and
# the WAL first, and only then ran a `flock -n` that exits 127 -- read as "held",
# so a ring nobody holds was reported live after everything else was gone.
# Faked with a PATH that has every command the purge needs except flock.
NOLOCK_BIN="${tmp}/nolock-bin"
mkdir -p -- "${NOLOCK_BIN}"
for c in python3 du stat rm id cut tr sed grep cat mkdir rmdir ls date \
	dirname basename; do
	p="$(command -v "${c}" 2>/dev/null)" && ln -sf "${p}" "${NOLOCK_BIN}/${c}"
done

printf 'records' >"${RING}"
printf '{"%s":{"bs_name":"b","ns_name":"n","wal_bdev":"w"}}\n' \
	"${RCOW_LVS_NAME}" >"${RCOW_BSTORE_FILE}"
rc=0
OUT="$(PATH="${NOLOCK_BIN}" "${BASH}" "${ROOT}/scripts/rcow_purge.sh" \
	--yes --keep-s3 --keep-wal 2>&1)" || rc=$?
check "a purge with no flock exits non-zero" [ "${rc}" -ne 0 ]
check "it says nothing was deleted" grep -q 'nothing was deleted' <<<"${OUT}"
check "it does not claim the ring is held" \
	bash -c '! grep -q "refusing to discard live" <<<"$1"' _ "${OUT}"
check "the ring is still there" [ -e "${RING}" ]
check "and the bstore entry with it" \
	python3 -c 'import json,sys; sys.exit(sys.argv[2] not in json.load(open(sys.argv[1])))' \
	"${RCOW_BSTORE_FILE}" "${RCOW_LVS_NAME}"

# flock is only needed when there is a ring to lock, so its absence must not
# fail a purge that has none -- otherwise this refusal becomes the new way an
# lvstore cannot be discarded.
rm -f -- "${RING}"
rc=0
PATH="${NOLOCK_BIN}" "${BASH}" "${ROOT}/scripts/rcow_purge.sh" \
	--yes --keep-s3 --keep-wal >/dev/null 2>&1 || rc=$?
check "but no flock and no ring is still a clean purge" [ "${rc}" -eq 0 ]

printf '=== result: %d passed, %d failed ===\n' "${passed}" "${failed}"
[ "${failed}" -eq 0 ]
