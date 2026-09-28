#!/usr/bin/env bash
# Materialise probe: can the source reclaim a snapshot pinned by an export?
#
# This is the point of the whole path. A ref export names live objects on the
# source lvstore, so it pins the snapshot -- as long as an importer is still
# renewing, the snapshot cannot be deleted, whether or not anyone still needs
# the data. Materialise is the only way out: the source copies the data into
# objects the export owns, republishes a dense manifest (generation+1), and
# after that the export owes nobody anything and the snapshot can go.
#
# Checks, in causal order; none of them is optional:
#
#   [2] write a fresh lease by hand so the source believes an importer is
#       reading
#   [3] before materialising: the snapshot cannot be deleted (proof it is
#       pinned -- otherwise "deletable afterwards" proves nothing)
#   [4] materialise: the RPC succeeds, the manifest becomes dense, generation
#       advances, TTL goes to zero
#   [5] after materialising: the snapshot can be deleted -- the point of the
#       feature
#   [6] import after the source is unloaded: the data is still correct, and
#       no lease is taken
#
# Why [2] uses a hand-written lease rather than a real import: one process
# holds one blobstore, so source and destination lvstores cannot be loaded
# together. That does not weaken this probe -- snapshot delete decides by
# *lease* (export_pin_state), not by counting readers in this process, so a
# lease is exactly the signal a real importer would leave. The real import
# is [6], after the source is unloaded.
#
# [5] and [6] together are the full proof: [5] says the source got the space
# back; [6] says that was not by throwing the data away.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
GET_MANIFEST="${ROOT}/test/tools/s3_get_manifest.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=pmat_src
DST_LVS=pmat_dst
SRC_WAL=/tmp/pmat_src_wal.img
DST_WAL=/tmp/pmat_dst_wal.img
RPC_SOCK=/tmp/pmat.sock
TGT_LOG=/tmp/pmat_target.log
NQN="nqn.2026-09.io.spdk:pmat"
PORT="4475"
WORKDIR="$(mktemp -d /tmp/pmat.XXXXXX)"
FILL_MB=8

TGT_PID=""
CONNECTED=0
UUIDS=""
FAILED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }
ok()   { echo "     [PASS] $*"; }
bad()  { echo "     [FAIL] $*"; FAILED=$((FAILED + 1)); }
want() { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }
mfield() { python3 "${GET_MANIFEST}" -e "${EP}" -b "${BK}" -r "${RG}" -u "$1" \
	--field "$2" 2>/dev/null; }

cleanup()
{
	set +u
	[ "${CONNECTED}" = "1" ] && nvme disconnect -n "${NQN}" >/dev/null 2>&1
	[ -n "${TGT_PID}" ] && { kill "${TGT_PID}" 2>/dev/null; sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null; }
	rcow_load_credentials
	for p in "${SRC_LVS}" "${DST_LVS}"; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "${p}/" >/dev/null 2>&1
	done
	for u in ${UUIDS:-}; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "exports/${u}" >/dev/null 2>&1
	done
	rm -f "${SRC_WAL}" "${DST_WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo "===== log: ${TGT_LOG}"
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null
sleep 2
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
truncate -s 320M "${SRC_WAL}"
truncate -s 320M "${DST_WAL}"

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"; BK="$(rcow_s3_buckets|head -1)"; RG="$(rcow_cfg_get region)"
for p in "${SRC_LVS}" "${DST_LVS}"; do
	python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${p}/" >/dev/null 2>&1
done

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null 2>&1
raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PMAT00000000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

expose() { raw nvmf_subsystem_add_ns "$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
	"${NQN}" "$1")" 2>/dev/null | tr -d '[:space:]'; }
unexpose() { [ -n "$1" ] && raw nvmf_subsystem_remove_ns \
	"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "$1")" >/dev/null 2>&1; return 0; }
ctrl_of_nqn() { local c; for c in /sys/class/nvme/nvme*; do
	[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && { basename "${c}"; return 0; }
	done; return 1; }
wait_dev()
{
	local nsid="$1" deadline ctrl dev
	deadline=$(( $(date +%s) + 30 ))
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		if ctrl="$(ctrl_of_nqn)"; then
			dev="/dev/${ctrl}n${nsid}"
			[ -b "${dev}" ] && { printf '%s' "${dev}"; return 0; }
		fi
		sleep 0.3
	done
	return 1
}

# One target, two lvstores, both loaded at once. That is what makes the pin
# observable: the refusal to delete a pinned snapshot is checked against readers
# in this process, so the importing lvstore has to be one of them.
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BK}")" >/dev/null || { echo "create src"; exit 1; }

echo
info "[1] source: write, snapshot, zero-copy export"
rpc rcow_create_lvol '{"lvol_name":"v","size_gib":1}' >/dev/null || { echo "lvol"; exit 1; }
NSID_SRC="$(expose "${SRC_LVS}/v")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 2
DEV="$(wait_dev "${NSID_SRC}")" || { echo "src device"; exit 1; }

PAT="${WORKDIR}/pat.bin"
dd if=/dev/urandom of="${PAT}" bs=1M count="${FILL_MB}" status=none
PAT_MD5="$(md5sum "${PAT}" | cut -d' ' -f1)"
dd if="${PAT}" of="${DEV}" bs=1M count="${FILL_MB}" oflag=direct conv=fsync status=none
sync
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"v","snapshot_name":"s"}' >/dev/null || exit 1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 3

EXP="$(rpc rcow_export_snapshot '{"snapshot_name":"s"}' 2>&1 | tr -d '"[:space:]\n')"
[ -n "${EXP}" ] || { echo "export"; exit 1; }
UUIDS="${EXP}"
for _ in $(seq 90); do
	st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP}")" \
		2>/dev/null | tr -d '"[:space:]\n')"
	case "${st}" in *DONE*) break ;; esac
	sleep 1
done
want "the export is a reference one to begin with" "$(mfield "${EXP}" layout)" "ref"
want "at generation 0" "$(mfield "${EXP}" generation)" "0"
info "export ${EXP}"

echo
info "[2] a fresh lease, so the source believes an importer is reading"
# Written by hand rather than by importing, because one process holds one
# blobstore -- the destination lvstore cannot be loaded next to the source. That
# is fine for what is being tested here: the delete path decides by *lease*
# (export_pin_state), not by counting readers in this process, so a lease is
# exactly the signal a real importer would leave.
#
# The importer side of the same story is [6], after this lvstore is unloaded.
rcow_load_credentials
python3 "${ROOT}/test/tools/s3_put_lease.py" "${EP}" "${BK}" "${RG}" \
	"${SRC_LVS}/meta/exports/${EXP}.lease" 20 0 >"${WORKDIR}/lease.log" 2>&1 \
	&& ok "an importer's lease was written" \
	|| bad "could not write the lease: $(tail -1 "${WORKDIR}/lease.log")"
# The source reads the lease on its own poller; give it a turn before asking.
sleep 8

echo
info "[3] before materialising: the snapshot is pinned"
# Asserted first, and this is not ceremony: without it "deletable afterwards"
# would prove nothing, since it might have been deletable all along.
DEL="$(rpc --raw rcow_delete_lvol "$(printf '{"lvol_name":"s","lvs_name":"%s"}' \
	"${SRC_LVS}")" 2>&1)"
info "delete reply: ${DEL}"
sleep 2
if rpc rcow_get_lvstores 2>/dev/null | grep -q '"s"'; then
	ok "the snapshot survived: the reference export still pins it"
else
	bad "the snapshot was deleted while a reference export names it: ${DEL}"
fi

echo
info "[4] materialise it"
MARK="$(wc -l <"${TGT_LOG}")"
MAT="$(rpc --raw rcow_materialise_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
	"${EXP}" "${SRC_LVS}")" 2>&1)"
info "reply: ${MAT}"
if printf '%s' "${MAT}" | grep -qiE 'error|refus'; then
	bad "rcow_materialise_export failed: ${MAT}"
	echo; echo "===== SUMMARY"; echo "  ${FAILED} failure(s)"; exit "${FAILED}"
fi
ok "rcow_materialise_export succeeded"

want "the manifest is now a copy" "$(mfield "${EXP}" layout)" "dense"
want "at generation 1" "$(mfield "${EXP}" generation)" "1"
# expires_at was only ever a promise to hold a snapshot for somebody.
want "with no deadline left" "$(mfield "${EXP}" expires_at)" "0"

echo
info "[5] after materialising: the snapshot can go"
# The point of the whole exercise.
DEL="$(rpc --raw rcow_delete_lvol "$(printf '{"lvol_name":"s","lvs_name":"%s"}' \
	"${SRC_LVS}")" 2>&1)"
info "delete reply: ${DEL}"
for _ in $(seq 60); do
	rpc rcow_get_lvstores 2>/dev/null | grep -q '"s"' || break
	sleep 1
done
if rpc rcow_get_lvstores 2>/dev/null | grep -q '"s"'; then
	bad "the snapshot is still there after materialising: ${DEL}"
else
	ok "the snapshot was deleted -- the export no longer pins it"
fi

echo
info "[6] and an importer reads the copies, with the source gone"
# The snapshot is deleted and its objects with it, so the reference the manifest
# used to hold is unresolvable. An import now has to work entirely off the
# copies -- which is where the write side and the read side meet.
#
# Imported after unloading the source rather than alongside it, because one
# process holds one blobstore. That also makes it the stronger test: the
# exporting lvstore is not merely quiet, it is gone.
unexpose "${NSID_SRC}"; sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1 \
	|| bad "could not unload the source"
sleep 2

if rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
		"${DST_LVS}" "${BK}")" >/dev/null 2>&1; then
	IMP="$(rpc --raw rcow_import_lvol "$(printf '{"lvol_name":"i","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
		"${EXP}" "${DST_LVS}")" 2>&1)"
	info "import reply: ${IMP}"

	NSID_DST="$(expose "${DST_LVS}/i")"
	sleep 2
	DEV_I="$(wait_dev "${NSID_DST}")"
	if [ -n "${DEV_I}" ] && [ -b "${DEV_I}" ]; then
		sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true
		want "it reads the right data out of the copies" \
			"$(dd if="${DEV_I}" bs=1M count="${FILL_MB}" iflag=direct \
				status=none 2>"${WORKDIR}/read.err" | md5sum \
				| cut -d' ' -f1)" \
			"${PAT_MD5}"
	else
		bad "the imported volume never became a device"
	fi

	# A copied export pins nothing, so an import of one takes no lease. Said
	# explicitly because the absence is the point: this is what "the source
	# owes nobody anything" looks like from the other side.
	if tail -n "+${MARK}" "${TGT_LOG}" | grep -q 'renewing .* lease'; then
		bad "the import took a lease on an export that pins nothing"
	else
		ok "and takes no lease, there being nothing left to pin"
	fi
else
	bad "could not create the destination lvstore"
fi

echo
echo "===== SUMMARY"
if [ "${FAILED}" = "0" ]; then
	echo "  the source reclaimed the snapshot: after materialising, the"
	echo "  export holds its own copies, the snapshot can be deleted, and"
	echo "  the export can still be imported with the right data."
else
	echo "  ${FAILED} failure(s) -- see [FAIL] above"
fi
exit "${FAILED}"
