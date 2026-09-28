#!/usr/bin/env bash
# Visibility probe for snapshot chain depth.
#
# Why it exists: export_build_chain() walks the local snapshot chain and returns
# -E2BIG once depth exceeds S3LVOL_DEFAULT_MAX_CHAIN_DEPTH (32). export_snapshot
# then **silently** falls back to a whole-volume copy. The copy is a correct
# fallback; the problem is that the resulting dense export is never reaped
# (the reaper only collects REF), so the cost is permanent: one registry entry
# plus a full-volume replica.
#
# Until that point nothing tells you the chain is growing. The log has a single
# "exporting by copying instead" line that is almost indistinguishable from a
# normal export -- so this can already have happened in production with no
# record of it.
#
# Snapshots belong to the user; creating one must not be refused because the
# chain is long. So: warn at a soft threshold (24) and report chain_depth via
# rcow_get_lvstores, so the control plane can act before the cliff rather than
# discover it afterwards.
#
# Checks:
#   [2] chain_depth increases by 1 on every snapshot, and the RPC actually
#       reports it (not a constant 0 or a stuck value)
#   [3] a warning appears at the soft threshold of 24, and not before
#   [4] past the hard limit of 32 the warning uses stronger wording
#   [5] deleting an intermediate snapshot actually shortens the chain -- that
#       is the action the warning recommends, so it has to work
#   [6] an export within depth 32 is still a ref (zero-copy was not broken)
#
# [5] is the one that matters most. The warning tells the user to delete an
# intermediate snapshot; if that does nothing, the warning is sending them on
# a wild goose chase. Deleting an intermediate snapshot is blobstore merging
# the deleted layer into its only clone (allowed only when clone_count == 1):
# metadata only, zero S3 I/O.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
GET_MANIFEST="${ROOT}/test/tools/s3_get_manifest.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

LVS=pchain
WAL=/tmp/pchain_wal.img
RPC_SOCK=/tmp/pchain.sock
TGT_LOG=/tmp/pchain_target.log
WORKDIR="$(mktemp -d /tmp/pchain.XXXXXX)"

# Soft threshold 24, hard limit 32: same as include/s3lvol/s3_types.h.
SOFT=24
HARD=32
# Build to 34 layers: two past the hard limit so [4]'s stronger warning fires.
BUILD_TO=34

TGT_PID=""
FAILED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }
ok()   { echo "     [PASS] $*"; }
bad()  { echo "     [FAIL] $*"; FAILED=$((FAILED + 1)); }
want() { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }

# chain_depth of one lvol, taken from rcow_get_lvstores JSON.
#
# --raw prints the JSON-RPC result member itself: a top-level lvstore array,
# not {"result": [...]}.
depth_of()
{
	raw rcow_get_lvstores '' 2>/dev/null | python3 -c '
import json, sys
want = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if isinstance(d, dict):
    d = d.get("result") or []
for lvs in d:
    for l in (lvs.get("lvols") or []):
        if l.get("name") == want:
            print(l.get("chain_depth", ""))
            sys.exit(0)
' "$1"
}

cleanup()
{
	set +u
	[ -n "${TGT_PID}" ] && { kill "${TGT_PID}" 2>/dev/null; sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null; }
	rcow_load_credentials
	python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
		-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
		-p "${LVS}/" >/dev/null 2>&1
	for u in ${UUIDS:-}; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "exports/${u}" >/dev/null 2>&1
	done
	rm -f "${WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo "===== log: ${TGT_LOG}"
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null
sleep 2
rm -f "${RPC_SOCK}" "${WAL}"
truncate -s 320M "${WAL}"

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"; BK="$(rcow_s3_buckets|head -1)"; RG="$(rcow_cfg_get region)"
python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${LVS}/" >/dev/null 2>&1

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"pc_wal0","block_size":4096}' \
	"${WAL}")" >/dev/null 2>&1

echo
info "[1] one volume, about to grow its snapshot chain"
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"pc_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${LVS}" "${BK}")" >/dev/null || { echo "create lvstore"; exit 1; }
rpc rcow_create_lvol '{"lvol_name":"v","size_gib":1}' >/dev/null || { echo "lvol"; exit 1; }
D0="$(depth_of v)"
want "an unparented volume has chain depth 1" "${D0}" "1"

echo
info "[2] each snapshot should increase chain depth by 1"
MARK="$(wc -l <"${TGT_LOG}")"
MISMATCH=0
SOFT_FIRST=""
for i in $(seq 1 "${BUILD_TO}"); do
	rpc rcow_create_snapshot "$(printf '{"lvol_name":"v","snapshot_name":"s%d"}' "$i")" \
		>/dev/null 2>&1 || { bad "snapshot s${i} failed"; break; }
	# The volume itself plus i snapshots = i+1 layers.
	D="$(depth_of v)"
	EXPECT=$((i + 1))
	if [ "${D}" != "${EXPECT}" ]; then
		bad "after ${i} snapshot(s), v's chain depth is '${D}', want ${EXPECT}"
		MISMATCH=$((MISMATCH + 1))
		[ "${MISMATCH}" -ge 3 ] && break
	fi
	[ "${EXPECT}" = "${SOFT}" ] && SOFT_FIRST="$(wc -l <"${TGT_LOG}")"
done
[ "${MISMATCH}" = "0" ] && ok "chain depth stepped up to $(depth_of v)"
want "final chain depth" "$(depth_of v)" "$((BUILD_TO + 1))"

echo
info "[3] soft threshold ${SOFT}: quiet before it, a warning at it"
NEW="$(tail -n "+${MARK}" "${TGT_LOG}")"
# The warning text includes the depth, so the first one can be pinned to a layer.
FIRST_WARN_DEPTH="$(printf '%s\n' "${NEW}" \
	| grep -ao 'is [0-9]* snapshot(s) deep' | head -1 | grep -o '[0-9]*')"
if [ -z "${FIRST_WARN_DEPTH}" ]; then
	bad "no chain-depth warning at all"
elif [ "${FIRST_WARN_DEPTH}" = "${SOFT}" ]; then
	ok "the first warning appeared at depth ${SOFT}"
else
	bad "the first warning appeared at depth ${FIRST_WARN_DEPTH}, want ${SOFT}"
fi
# The warning has to say what to do, otherwise operators know there is a
# problem and not how to fix it.
if printf '%s' "${NEW}" | grep -q 'Deleting an intermediate snapshot'; then
	ok "and it says what to do (delete an intermediate snapshot)"
else
	bad "the warning has no actionable advice"
fi

echo
info "[4] past the hard limit ${HARD}, the wording should be stronger"
if printf '%s' "${NEW}" | grep -q "past the limit of ${HARD}"; then
	ok "$(printf '%s' "${NEW}" | grep -ao "is [0-9]* snapshot(s) deep, past the limit of ${HARD}" | head -1)"
else
	bad "no stronger warning after crossing ${HARD}"
fi
if printf '%s' "${NEW}" | grep -q 'copy the whole volume'; then
	ok "and it names the consequence (a whole-volume copy)"
else
	bad "the stronger warning does not name the consequence"
fi

echo
info "[5] deleting an intermediate snapshot must actually shorten the chain"
# This is the action [3]/[4] recommend. If it does nothing, those warnings are
# sending the user on a wild goose chase.
BEFORE="$(depth_of v)"
# s2 is an intermediate layer: it has a parent (s1) and a child (s3), and
# clone_count is exactly 1, so it is deletable.
DEL="$(rpc rcow_delete_lvol "$(printf '{"lvol_name":"s2","lvs_name":"%s"}' "${LVS}")" 2>&1)"
info "delete s2: ${DEL}"
sleep 2
AFTER="$(depth_of v)"
if [ -n "${AFTER}" ] && [ "${AFTER}" -lt "${BEFORE}" ] 2>/dev/null; then
	ok "after deleting an intermediate snapshot, depth ${BEFORE} -> ${AFTER}"
else
	bad "depth did not drop (${BEFORE} -> ${AFTER:-<empty>}): the recommended action is a no-op"
fi

echo
info "[6] an export within ${HARD} layers should still be zero-copy"
# These changes only add observability; they must not change routing. Check
# with a shallow-chain snapshot.
rpc rcow_create_lvol '{"lvol_name":"w","size_gib":1}' >/dev/null 2>&1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS}")" >/dev/null 2>&1
rpc rcow_create_snapshot '{"lvol_name":"w","snapshot_name":"ws"}' >/dev/null 2>&1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS}")" >/dev/null 2>&1
sleep 2
EXP="$(rpc rcow_export_snapshot '{"snapshot_name":"ws"}' 2>&1 | tr -d '"[:space:]\n')"
if [ -n "${EXP}" ]; then
	UUIDS="${EXP}"
	for _ in $(seq 60); do
		ST="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP}")" \
			2>/dev/null | tr -d '"[:space:]\n')"
		case "${ST}" in *DONE*) break ;; esac
		sleep 1
	done
	LAYOUT="$(python3 "${GET_MANIFEST}" -e "${EP}" -b "${BK}" -r "${RG}" -u "${EXP}" \
		--field layout 2>/dev/null)"
	want "a shallow-chain snapshot still exports as ref" "${LAYOUT}" "ref"
else
	bad "export of ws failed"
fi

echo
echo "===== SUMMARY"
if [ "${FAILED}" = "0" ]; then
	echo "  chain depth is reported, the soft threshold warns, and the"
	echo "  recommended remedy actually shortens the chain."
else
	echo "  ${FAILED} failure(s) -- see [FAIL] above"
fi
exit "${FAILED}"
