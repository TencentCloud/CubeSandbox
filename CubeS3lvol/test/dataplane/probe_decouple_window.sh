#!/usr/bin/env bash
# Does reading a volume while its decouple is still running return something a
# filesystem would reject?
#
# In the field a volume was exposed to the host 1.85 s before its decouple
# finished, and the mount that followed failed with "inode #2 checksum invalid",
# while re-importing the same export later mounted cleanly. The export's data is
# known good -- it was verified byte for byte against its S3 objects and mounts
# fine. That leaves the window: the device is readable before materialisation is
# complete.
#
# spdk_blob_materialize_cluster() is supposed to make this safe. Each cluster is
# copy-on-write: a reader either takes the old path (through the esnap to the
# export) or the new one (the local cluster), and both hold the same bytes. Nothing
# in the I/O path consults the decouple state, and materialise uses its own
# channel. So either the window really is safe and the field failure has another
# cause, or that reasoning has a hole in it.
#
# The experiment: build an export from a real ext4, import it, and from the moment
# the device appears read the superblock and root inode repeatedly while the
# decouple runs. Every sample is compared against what the same regions read after
# the decouple has finished. A single differing sample settles it.
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"
TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_SOCK="/var/run/s3lvol.sock"

SRC_LVS="dcwsrc"
DST_LVS="dcwdst"
SRC_WAL="/data/s3lvol_dcwsrc.img"
DST_WAL="/data/s3lvol_dcwdst.img"
NQN="nqn.2026-08.io.spdk:dcwin"
PORT=4431
# Big enough that materialising takes long enough to sample during, small enough
# to keep the run short. Every megabyte here is one 1 MiB chunk to fetch.
VOL_GIB=1
FILL_MB=4

ENDPOINT=""; BUCKET=""; REGION="ap-nanjing"
while getopts "e:b:r:" opt; do
	case "${opt}" in
	e) ENDPOINT="${OPTARG}" ;;
	b) BUCKET="${OPTARG}" ;;
	r) REGION="${OPTARG}" ;;
	*) exit 1 ;;
	esac
done
[ -n "${ENDPOINT}" ] && [ -n "${BUCKET}" ] || { echo "need -e and -b" >&2; exit 1; }
[ -n "${AWS_ACCESS_KEY_ID:-}" ] || { echo "need credentials" >&2; exit 1; }

WORKDIR="$(mktemp -d /tmp/dcw.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"
MNT="${WORKDIR}/mnt"
mkdir -p "${MNT}"

rpc()  { python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"; }
raw()  { python3 /tmp/rpc_raw.py "${RPC_SOCK}" "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }

CONNECTED=0
cleanup() {
	set +u
	[ "${CONNECTED}" = "1" ] && { umount "${MNT}" 2>/dev/null; \
		nvme disconnect -n "${NQN}" >/dev/null 2>&1; }
	[ -n "${TGT_PID:-}" ] && kill "${TGT_PID}" 2>/dev/null
	sleep 2
	kill -9 "${TGT_PID}" 2>/dev/null
	echo
	echo "===== decouple / materialise lines ====="
	grep -aE "Decoupling|materialised|no longer reads" "${TGT_LOG}" | tail -8
	echo "===== log: ${TGT_LOG} ====="
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null; sleep 2
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
truncate -s 320M "${SRC_WAL}"
truncate -s 320M "${DST_WAL}"

info "starting target"
"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_cos_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}")" >/dev/null

# One blobstore per process, so the two lvstores take turns rather than coexisting:
# the source is built and exported, then unloaded before the destination is made.
raw bdev_aio_create "$(printf '{"filename":"%s","name":"src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null 2>&1
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"src_wal0","journal_size_mb":64,"wal_size_mb":128}' \
	"${SRC_LVS}" "${BUCKET}")" >/dev/null || {
	echo "create_lvstore ${SRC_LVS} failed"; exit 1; }
info "source lvstore ready"

raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"DCW0000000000000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

expose() {
	raw nvmf_subsystem_add_ns "$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
		"${NQN}" "$1")" 2>/dev/null \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('result',''))" 2>/dev/null
}
ctrl_of_nqn() {
	local c
	for c in /sys/class/nvme/nvme*; do
		[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && { basename "${c}"; return 0; }
	done
	return 1
}
wait_dev() {
	local nsid="$1" deadline=$(( $(date +%s) + 20 )) ctrl dev
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		ctrl="$(ctrl_of_nqn)" && {
			dev="/dev/${ctrl}n${nsid}"
			[ -b "${dev}" ] && { printf '%s' "${dev}"; return 0; }
		}
		sleep 0.2
	done
	return 1
}

# ---- source: a real ext4 with real files, so the checks below are the same ones
# ---- a mount would make.
info "creating source volume and filling it"
rpc rcow_create_lvol "$(printf '{"lvol_name":"src","size_gib":%d}' "${VOL_GIB}")" >/dev/null
NSID_SRC="$(expose "${SRC_LVS}/src")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 3
SRC_DEV="$(wait_dev "${NSID_SRC}")" || { echo "source device never appeared"; exit 1; }
info "source device ${SRC_DEV}"

mkfs.ext4 -F -O metadata_csum,64bit "${SRC_DEV}" >/dev/null 2>&1 || {
	echo "mkfs failed"; exit 1; }
mount "${SRC_DEV}" "${MNT}" || { echo "mount failed"; exit 1; }
mkdir -p "${MNT}/data"
dd if=/dev/urandom of="${MNT}/data/blob.bin" bs=1M count="${FILL_MB}" status=none
# Few files, matching the volume this reproduces: it held fifteen. A sparse volume
# is the interesting shape here -- almost every cluster is a hole, and holes are
# the part of materialisation this run is aimed at.
for i in $(seq 1 12); do echo "file ${i}" > "${MNT}/data/f${i}.txt"; done
sync
umount "${MNT}"
sync

rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
rpc rcow_checkpoint_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 4

rpc rcow_create_snapshot '{"lvol_name":"src","snapshot_name":"src-snap"}' >/dev/null
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 3

EXP_UUID="$(rpc rcow_export_snapshot '{"snapshot_name":"src-snap"}' 2>&1 \
	| tr -d '"[:space:]')"
[ -n "${EXP_UUID}" ] || { echo "export failed"; exit 1; }
info "exported as ${EXP_UUID}"
for _ in $(seq 120); do
	st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP_UUID}")" \
		2>/dev/null | tr -d '"[:space:]')"
	case "${st}" in *DONE*) break ;; esac
	sleep 1
done
info "export status: ${st:-unknown}"

# The source is done with; its namespace goes away with it so the host stops
# holding the device open, and only then can the destination take the blobstore.
SRC_NSID="$(raw nvmf_get_subsystems 2>/dev/null | python3 -c "
import json, sys
nqn = sys.argv[1]
try: subs = json.load(sys.stdin).get('result', [])
except Exception: subs = []
for s in subs:
    if s.get('nqn') != nqn: continue
    for n in s.get('namespaces', []):
        if n.get('bdev_name', '').endswith('/src'):
            print(n.get('nsid')); break
" "${NQN}" 2>/dev/null)"
[ -n "${SRC_NSID}" ] && raw nvmf_subsystem_remove_ns \
	"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "${SRC_NSID}")" >/dev/null 2>&1
sleep 2

rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1 \
	|| { echo "unload of source failed"; exit 1; }
sleep 2
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"dst_wal0","journal_size_mb":64,"wal_size_mb":128}' \
	"${DST_LVS}" "${BUCKET}")" >/dev/null || {
	echo "create_lvstore ${DST_LVS} failed"; exit 1; }
info "destination lvstore ready"

# ---- the experiment: import with decouple, then read while it runs.
info "importing with decouple:true and sampling during materialisation"
raw rcow_import_lvol "$(printf '{"lvol_name":"imp","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>"${WORKDIR}/import.err" || {
	echo "import failed"; cat "${WORKDIR}/import.err"; exit 1; }

NSID_IMP="$(expose "${DST_LVS}/imp")"
IMP_DEV="$(wait_dev "${NSID_IMP}")" || { echo "imported device never appeared"; exit 1; }
info "imported device ${IMP_DEV} (decouple may still be running)"

# The two regions a mount reads first and trusts absolutely: the superblock lives
# at byte 1024, the root inode in group 0's inode table. Both are inside the first
# megabyte, which is also the first cluster to be materialised -- so if the window
# is unsafe at all, it is unsafe here.
SAMPLES=0
DIFFS=0
declare -A SEEN
while :; do
	busy="$(raw rcow_get_decouple 2>/dev/null | python3 -c "
import json,sys
try: rows=json.load(sys.stdin).get('result',[])
except Exception: rows=[]
print(len([r for r in rows if r.get('lvol_name')=='imp']))
" 2>/dev/null)"
	h="$(dd if="${IMP_DEV}" bs=4096 count=256 iflag=direct 2>/dev/null | md5sum | cut -c1-32)"
	SAMPLES=$((SAMPLES + 1))
	SEEN["${h}"]=1
	[ "${busy}" = "0" ] && break
	[ "${SAMPLES}" -gt 400 ] && break
	sleep 0.05
done
info "sampled the first MiB ${SAMPLES} time(s) during/after decouple"

# Settle, then take the authoritative reading.
sleep 3
FINAL="$(dd if="${IMP_DEV}" bs=4096 count=256 iflag=direct 2>/dev/null | md5sum | cut -c1-32)"
DISTINCT="${#SEEN[@]}"
info "distinct first-MiB contents observed: ${DISTINCT}"
for h in "${!SEEN[@]}"; do
	if [ "${h}" != "${FINAL}" ]; then
		DIFFS=$((DIFFS + 1))
		info "  differs from settled value: ${h}"
	fi
done
info "settled value: ${FINAL}"

echo
if [ "${DIFFS}" -eq 0 ]; then
	echo "RESULT: the window is consistent -- every sample matched the settled"
	echo "        content, so reading during materialisation returned the same"
	echo "        bytes throughout. The field failure needs another explanation."
else
	echo "RESULT: the window is NOT consistent -- ${DIFFS} of ${DISTINCT} distinct"
	echo "        readings differed from the settled content. A mount landing here"
	echo "        can read a superblock or root inode that does not match its"
	echo "        checksum, which is exactly what was reported."
fi

# And the filesystem's own verdict, which is the one that matters.
echo
info "fsck and mount after the decouple settled"
e2fsck -fn "${IMP_DEV}" 2>&1 | head -8 | sed 's/^/     /'
if mount -o ro "${IMP_DEV}" "${MNT}" 2>/dev/null; then
	echo "     mount: OK, $(ls "${MNT}/data" | wc -l) entries under data/"
	umount "${MNT}"
else
	echo "     mount: FAILED"
fi
