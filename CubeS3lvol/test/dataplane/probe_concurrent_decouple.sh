#!/usr/bin/env bash
# Does decoupling one export while another is being materialised corrupt it?
#
# What points here. The same export was decoupled three times on the same node,
# from the same objects, and read a different amount each time:
#
#   16:51:18  32 read(s),    139264 bytes,  4 served as zeroes   -> unmountable
#   17:11:18  74 read(s),  12701696 bytes,  4 served as zeroes   -> fine
#   17:11:57  13 read(s),   4272128 bytes,  0 served as zeroes   -> fine
#
# The manifest describes 4231168 bytes across ten chunks, so the last line is the
# shape a correct run has. The first read 3% of that and left nine of the ten
# chunks reading as zeroes, which is why the mount that followed failed on the
# root inode's checksum.
#
# The difference is not the export. It is what else was running. The failing run
# started at 16:51:16 while a 2048-cluster, 2.4 GB decouple of a *different*
# export had been going since 16:47:40 and did not finish until 16:53:46. The two
# clean runs were the only decouple on the node at the time.
#
# Concurrent decouples of one export now queue (9d3429e), but these were different
# exports, so nothing serialises them. This run reproduces that: a big export is
# put into materialisation, a small one is started underneath it, and the small
# one's bytes are then compared against the objects the manifest names.
#
# Verified per chunk rather than by mounting: a filesystem answers yes or no,
# while the chunk comparison says which chunk lost what, and that is what
# distinguishes a short read from a wrong offset.
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"
TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_SOCK="/var/run/s3lvol.sock"

SRC_LVS="ccsrc"
DST_LVS="ccdst"
SRC_WAL="/data/s3lvol_ccsrc.img"
DST_WAL="/data/s3lvol_ccdst.img"
NQN="nqn.2026-08.io.spdk:ccdec"
PORT=4433

# The big volume is what occupies the materialiser; the small one is the victim
# whose bytes are checked. The field case was 2048 clusters against 10.
BIG_GIB=2
BIG_FILL_MB=700
SMALL_GIB=1
SMALL_FILL_MB=4

# -x removes every namespace from the subsystem before the two decouples start, so
# nothing on the host can be reading, prefetching or writing while they run. It is
# the control for "is the host involved at all": if the corruption still happens
# with no block device in existence, the host is not part of it.
ENDPOINT=""; BUCKET=""; REGION="ap-nanjing"; NO_HOST=0
while getopts "e:b:r:x" opt; do
	case "${opt}" in
	e) ENDPOINT="${OPTARG}" ;;
	b) BUCKET="${OPTARG}" ;;
	r) REGION="${OPTARG}" ;;
	x) NO_HOST=1 ;;
	*) exit 1 ;;
	esac
done
[ -n "${ENDPOINT}" ] && [ -n "${BUCKET}" ] || { echo "need -e and -b" >&2; exit 1; }
[ -n "${AWS_ACCESS_KEY_ID:-}" ] || { echo "need credentials" >&2; exit 1; }

WORKDIR="$(mktemp -d /tmp/ccdec.XXXXXX)"
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
	echo "===== decouple accounting ====="
	grep -aE "Decoupling lvol|materialised|Releasing imported export" "${TGT_LOG}"
	echo "===== S3 client errors, if any ====="
	grep -aiE "GetObject|short|truncat|timeout|retry|curl|http=[45]" "${TGT_LOG}" \
		| tail -15
	echo "===== log: ${TGT_LOG} ====="
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null; sleep 2
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
truncate -s 512M "${SRC_WAL}"
truncate -s 512M "${DST_WAL}"

info "starting target"
"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_cos_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}")" >/dev/null
raw bdev_aio_create "$(printf '{"filename":"%s","name":"ccsrc_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"ccdst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null 2>&1
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":8,"wal_bdev":"ccsrc_wal0","journal_size_mb":64,"wal_size_mb":192}' \
	"${SRC_LVS}" "${BUCKET}")" >/dev/null || { echo "src lvstore failed"; exit 1; }

raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"CCDEC000000000000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

expose() {
	raw nvmf_subsystem_add_ns "$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
		"${NQN}" "$1")" 2>/dev/null \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('result',''))" 2>/dev/null
}
unexpose() {
	local nsid
	nsid="$(raw nvmf_get_subsystems 2>/dev/null | python3 -c "
import json, sys
nqn, bdev = sys.argv[1], sys.argv[2]
try: subs = json.load(sys.stdin).get('result', [])
except Exception: subs = []
for s in subs:
    if s.get('nqn') != nqn: continue
    for n in s.get('namespaces', []):
        if n.get('bdev_name') == bdev:
            print(n.get('nsid')); break
" "${NQN}" "$1" 2>/dev/null)"
	[ -n "${nsid}" ] && raw nvmf_subsystem_remove_ns \
		"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "${nsid}")" >/dev/null 2>&1
}
ctrl_of_nqn() {
	local c
	for c in /sys/class/nvme/nvme*; do
		[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && { basename "${c}"; return 0; }
	done
	return 1
}
wait_dev() {
	local nsid="$1" deadline=$(( $(date +%s) + 25 )) ctrl dev
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		ctrl="$(ctrl_of_nqn)" && {
			dev="/dev/${ctrl}n${nsid}"
			[ -b "${dev}" ] && { printf '%s' "${dev}"; return 0; }
		}
		sleep 0.2
	done
	return 1
}
export_and_wait() {
	local snap="$1" uuid
	uuid="$(rpc rcow_export_snapshot "$(printf '{"snapshot_name":"%s"}' "${snap}")" \
		2>&1 | tr -d '"[:space:]')"
	[ -n "${uuid}" ] || return 1
	local deadline=$(( $(date +%s) + 180 ))
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		case "$(rpc rcow_get_snapshot_status \
			"$(printf '{"export_uuid":"%s"}' "${uuid}")" 2>/dev/null)" in
		*DONE*) printf '%s' "${uuid}"; return 0 ;;
		esac
		sleep 1
	done
	return 1
}

# ---- two source volumes, exported.
info "building the big volume (${BIG_FILL_MB} MiB of data)"
rpc rcow_create_lvol "$(printf '{"lvol_name":"big","size_gib":%d}' "${BIG_GIB}")" >/dev/null
NSID_BIG="$(expose "${SRC_LVS}/big")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 3
BIG_DEV="$(wait_dev "${NSID_BIG}")" || { echo "big device missing"; exit 1; }
dd if=/dev/urandom of="${BIG_DEV}" bs=1M count="${BIG_FILL_MB}" \
	oflag=direct conv=fsync status=none

info "building the small volume with a real ext4"
rpc rcow_create_lvol "$(printf '{"lvol_name":"small","size_gib":%d}' "${SMALL_GIB}")" >/dev/null
NSID_SMALL="$(expose "${SRC_LVS}/small")"
SMALL_DEV="$(wait_dev "${NSID_SMALL}")" || { echo "small device missing"; exit 1; }
mkfs.ext4 -F -O metadata_csum,64bit "${SMALL_DEV}" >/dev/null 2>&1
mount "${SMALL_DEV}" "${MNT}" || { echo "mount of small failed"; exit 1; }
mkdir -p "${MNT}/data"
dd if=/dev/urandom of="${MNT}/data/blob.bin" bs=1M count="${SMALL_FILL_MB}" status=none
for i in $(seq 1 12); do echo "file ${i}" > "${MNT}/data/f${i}.txt"; done
sync
umount "${MNT}"
sync

rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
rpc rcow_checkpoint_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 5

rpc rcow_create_snapshot '{"lvol_name":"big","snapshot_name":"big-snap"}' >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"small","snapshot_name":"small-snap"}' >/dev/null
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 4

BIG_UUID="$(export_and_wait big-snap)" || { echo "big export failed"; exit 1; }
SMALL_UUID="$(export_and_wait small-snap)" || { echo "small export failed"; exit 1; }
info "big export   ${BIG_UUID}"
info "small export ${SMALL_UUID}"

# Record what the small export's chunks must contain, read through the export
# itself before any of this. Whatever a correct decouple produces has to match.
unexpose "${SRC_LVS}/big"
unexpose "${SRC_LVS}/small"
sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1 \
	|| { echo "unload of source failed"; exit 1; }
sleep 2
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":8,"wal_bdev":"ccdst_wal0","journal_size_mb":64,"wal_size_mb":192}' \
	"${DST_LVS}" "${BUCKET}")" >/dev/null || { echo "dst lvstore failed"; exit 1; }
info "destination lvstore ready"

# Reference: import the small export WITHOUT decoupling, so reads go straight to
# the objects, and take a per-chunk fingerprint.
raw rcow_import_lvol "$(printf '{"lvol_name":"ref","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${SMALL_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "ref import failed"; exit 1; }
NSID_REF="$(expose "${DST_LVS}/ref")"
REF_DEV="$(wait_dev "${NSID_REF}")" || { echo "ref device missing"; exit 1; }
info "reference (esnap, no decouple): ${REF_DEV}"
for off in $(seq 0 63); do
	dd if="${REF_DEV}" bs=1M count=1 skip="${off}" iflag=direct 2>/dev/null \
		| md5sum | cut -c1-32
done > "${WORKDIR}/ref.txt"
info "reference fingerprint taken over the first 64 MiB"

if [ "${NO_HOST}" = "1" ]; then
	unexpose "${DST_LVS}/ref"
	nvme disconnect -n "${NQN}" >/dev/null 2>&1
	CONNECTED=0
	sleep 2
	info "control run: no namespace is exposed while the decouples run"
fi

# ---- the experiment.
info "starting the BIG decouple, then the small one underneath it"
raw rcow_import_lvol "$(printf '{"lvol_name":"vbig","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${BIG_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "big import failed"; exit 1; }

# Wait until the big one is demonstrably running, so the small one really does
# land underneath it rather than after it.
for _ in $(seq 100); do
	n="$(raw rcow_get_decouple 2>/dev/null | python3 -c "
import json,sys
try: rows=json.load(sys.stdin).get('result',[])
except Exception: rows=[]
print(len([r for r in rows if r.get('lvol_name')=='vbig' and not r.get('queued')]))
" 2>/dev/null)"
	[ "${n}" = "1" ] && break
	sleep 0.1
done
info "big decouple is running; importing the small one with decouple:true"

raw rcow_import_lvol "$(printf '{"lvol_name":"vsmall","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${SMALL_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "small import failed"; exit 1; }

# Both must finish before anything is judged.
for _ in $(seq 600); do
	left="$(raw rcow_get_decouple 2>/dev/null | python3 -c "
import json,sys
try: rows=json.load(sys.stdin).get('result',[])
except Exception: rows=[]
print(len(rows))
" 2>/dev/null)"
	[ "${left}" = "0" ] && break
	sleep 1
done
info "both decouples finished"
sleep 3

NSID_SMALL2="$(expose "${DST_LVS}/vsmall")"
if [ "${CONNECTED}" != "1" ]; then
	nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
	CONNECTED=1
	sleep 3
fi
SMALL2_DEV="$(wait_dev "${NSID_SMALL2}")" || { echo "vsmall device missing"; exit 1; }
info "materialised small volume: ${SMALL2_DEV}"
for off in $(seq 0 63); do
	dd if="${SMALL2_DEV}" bs=1M count=1 skip="${off}" iflag=direct 2>/dev/null \
		| md5sum | cut -c1-32
done > "${WORKDIR}/got.txt"

echo
echo "================ RESULT ================"
DIFF_COUNT="$(paste "${WORKDIR}/ref.txt" "${WORKDIR}/got.txt" \
	| awk '$1 != $2 { n++ } END { print n + 0 }')"
if [ "${DIFF_COUNT}" -eq 0 ]; then
	echo "chunks differing from the export: 0 -- concurrent decouple is clean"
	echo "The field failure is not explained by concurrency alone."
else
	echo "chunks differing from the export: ${DIFF_COUNT} of 64 -- REPRODUCED"
	echo
	echo "  chunk  through-export      after-concurrent-decouple"
	paste "${WORKDIR}/ref.txt" "${WORKDIR}/got.txt" | awk '
		$1 != $2 { printf "  %5d  %-18s %s\n", NR - 1, substr($1,1,16), substr($2,1,16) }' \
		| head -20
fi

echo
info "and the filesystem's verdict"
e2fsck -fn "${SMALL2_DEV}" 2>&1 | head -10 | sed 's/^/     /'
if mount -o ro "${SMALL2_DEV}" "${MNT}" 2>/dev/null; then
	echo "     mount: OK, $(ls "${MNT}/data" 2>/dev/null | wc -l) entries under data/"
	umount "${MNT}"
else
	echo "     mount: FAILED -- same symptom as the field"
fi
