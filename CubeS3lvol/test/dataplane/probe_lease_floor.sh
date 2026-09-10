#!/usr/bin/env bash
# Lease cadence probe: does a ref import renew at the fixed cadence, and does
# the lease object appear immediately?
#
# ttl_sec is deliberately supplied and then waited out: it is a compatibility
# field and must affect neither export validity nor the 20-second lease cadence.
# The first lease must still be written immediately rather than after one tick.
#
# One lvstore is not enough: importing into the same lvstore degrades to a
# local clone and never takes the esnap path. Two are used (unload after
# the source exports, then create the destination), matching
# probe_esnap_snapshot.sh.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=please_src
DST_LVS=please_dst
SRC_WAL=/tmp/please_src_wal.img
DST_WAL=/tmp/please_dst_wal.img
RPC_SOCK=/tmp/please.sock
TGT_LOG=/tmp/please_target.log
WORKDIR="$(mktemp -d /tmp/please.XXXXXX)"

# A short deprecated value makes accidental TTL behavior easy to detect.
SHORT_TTL=6

TGT_PID=""
UUIDS=""
FAILED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }
ok()   { echo "     [PASS] $*"; }
bad()  { echo "     [FAIL] $*"; FAILED=$((FAILED + 1)); }
want() { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }

cleanup()
{
	set +u
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
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config failed"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null 2>&1

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BK}")" >/dev/null || { echo "create src failed"; exit 1; }

echo
info "[1] a snapshot exported with ttl_sec=${SHORT_TTL}"
rpc rcow_create_lvol '{"lvol_name":"v","size_gib":1}' >/dev/null || { echo "lvol"; exit 1; }
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"v","snapshot_name":"s"}' >/dev/null \
	|| { echo "snapshot"; exit 1; }
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 2

EXP="$(rpc rcow_export_snapshot "$(printf '{"snapshot_name":"s","ttl_sec":%d}' \
	"${SHORT_TTL}")" 2>&1 | tr -d '"[:space:]\n')"
[ -n "${EXP}" ] || { echo "export failed"; exit 1; }
UUIDS="${EXP}"
for _ in $(seq 60); do
	st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP}")" \
		2>/dev/null | tr -d '"[:space:]\n')"
	case "${st}" in *DONE*) break ;; esac
	sleep 1
done
info "exported ${EXP} (status ${st:-?})"

rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1
sleep 2
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BK}")" >/dev/null || { echo "create dst failed"; exit 1; }

# Wait it out deliberately: the export must remain importable.
echo
info "[2] waiting past ttl_sec, then importing the still-valid export"
sleep "$((SHORT_TTL + 2))"

MARK="$(wc -l <"${TGT_LOG}")"
rpc rcow_import_lvol "$(printf '{"lvol_name":"i","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import failed"; exit 1; }

INTERVAL="$(tail -n "+${MARK}" "${TGT_LOG}" \
	| grep -aoE 'renewing [0-9]+ lease\(s\) every [0-9]+ second' \
	| grep -aoE 'every [0-9]+' | grep -aoE '[0-9]+' | head -1)"
info "renew interval reported: ${INTERVAL:-<none>} s"
want "a ref export renews at the fixed cadence" "${INTERVAL:-0}" "20"

# The lease has to exist now, not one period from now. Checked at 5 s: comfortably
# inside the 20 s period, so a pass means it was written at start rather than by
# the first tick.
echo
info "[3] the lease object appears without waiting for the first tick"
sleep 5
if python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" \
		-p "${SRC_LVS}/meta/exports/${EXP}.lease" --list 2>/dev/null \
		| grep -q "${EXP}"; then
	ok "the lease was written at import, ${INTERVAL:-?} s before the first tick"
else
	bad "no lease object 5 s after import: the window the floor opened is real"
fi

# And renew_s in the body is what the source turns into a grace period, so it has
# to say the floor too -- an honest 20 gives 60 s, a stale 1 would give 3.
RENEW_S="$(python3 - <<PY 2>"${WORKDIR}/renew_s.err"
import json, os, sys
sys.path.insert(0, "${ROOT}/test/tools")
# s3_prefix_rm's Client, not s3_bucket's -- both exist and only this one has
# _base_path(). s3_get_manifest.py imports the same one.
from s3_prefix_rm import Client
c = Client("${EP}", "${BK}", "${RG}", False,
           os.environ["AWS_ACCESS_KEY_ID"], os.environ["AWS_SECRET_ACCESS_KEY"])
key = "${SRC_LVS}/meta/exports/${EXP}.lease"
st, body = c.request("GET", c._base_path() + "/" + key)
if st != 200:
    sys.stderr.write("GET %s -> HTTP %d\n" % (key, st))
    sys.exit(1)
print(json.loads(body).get("renew_s", ""))
PY
)"
if [ -n "${RENEW_S}" ]; then
	want "the lease reports that cadence, so the source's grace is 3x it" \
		"${RENEW_S}" "20"
else
	# Not downgraded to a note: renew_s is the number the source multiplies by
	# three, so an unread one leaves the actual hazard unverified.
	bad "could not read renew_s back: $(tail -1 "${WORKDIR}/renew_s.err")"
fi

echo
echo "===== SUMMARY"
if [ "${FAILED}" = "0" ]; then
	echo "  importing after the deprecated ttl_sec still renews every 20 seconds;"
	echo "  the lease is written at import."
else
	echo "  ${FAILED} failure(s) -- see [FAIL] above"
fi
exit "${FAILED}"
