#!/usr/bin/env bash
# Derived-export self-renewal probe: after publish, with nobody importing,
# is the upstream it names still protected?
#
# This is design §6, "E pins A while E is alive". Previously E did not pin --
# only E's *readers* renewed A's lease. That left a window:
#
#   B imports from A -> B publishes derived export E (naming A's prefix)
#                    -> no node imports E
#                    -> nobody renews A -> A's lease goes stale -> A deletes
#                       the snapshot
#                    -> E is a manifest that looks valid and resolves to
#                       nothing
#
# E now renews upstream itself from the moment it is published, until the
# user explicitly rcow_release_export.
#
# Checks:
#   [3] after B publishes E (with no import), the log says E is renewing
#       upstream, and the key names A
#   [4] A's lease object is actually being refreshed, and the writer is E
#       not vb -- that is the only thing A's side looks at
#   [5] it stops on unload and resumes on attach -- the duty is to another
#       node, so it must survive a restart
#   [6] renewals stop after release_export -- that is the only designed exit
#
# [4] is the one that matters: a log line saying "renewing" is not enough;
# updated_at on the S3 object has to actually move, or an implementation
# whose PUTs all fail would print the same log. importer_id must be checked
# too: vb also renews the same key (it is reading EXP_A), just on a much
# longer period, so "the timestamp moved" does not by itself say E moved it.
#
# [5] is stop-then-resume rather than just "still renewing after attach":
# the latter also passes if some poller was never stopped, which is a
# different bug.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
GET_MANIFEST="${ROOT}/test/tools/s3_get_manifest.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

A_LVS=psrc_a
B_LVS=psrc_b
A_WAL=/tmp/psrc_a_wal.img
B_WAL=/tmp/psrc_b_wal.img
RPC_SOCK=/tmp/psrc.sock
TGT_LOG=/tmp/psrc_target.log
NQN="nqn.2026-09.io.spdk:psrc"
PORT="4476"
WORKDIR="$(mktemp -d /tmp/psrc.XXXXXX)"
FILL_MB=8
HALF_MB=4

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

# One field out of a lease object, or empty if the object is not there.
#
# importer_id matters as much as updated_at here, and that is not obvious: the
# volume B imported and the export B published both renew the *same* key -- vb
# reads EXP_A, and E references EXP_A's prefix -- so "the timestamp advanced"
# alone does not say which of the two wrote it. The two identify themselves
# differently (a bare lvstore name from import_lease_renew(), "export:<uuid>"
# from export_src_lease_renew()), so the writer is checkable rather than
# inferred. Without that check this probe would pass on the strength of the
# importer's own 20-second renewals.
lease_field()
{
	python3 - "$1" "$2" <<'PY' 2>/dev/null
import json, os, sys
sys.path.insert(0, os.environ["TOOLS"])
from s3_prefix_rm import Client
c = Client(os.environ["EP"], os.environ["BK"], os.environ["RG"], False,
           os.environ["AWS_ACCESS_KEY_ID"], os.environ["AWS_SECRET_ACCESS_KEY"])
st, body = c.request("GET", c._base_path() + "/" + sys.argv[1])
if st == 200:
    print(json.loads(body).get(sys.argv[2], ""))
PY
}
lease_updated_at() { lease_field "$1" updated_at; }

cleanup()
{
	set +u
	[ "${CONNECTED}" = "1" ] && nvme disconnect -n "${NQN}" >/dev/null 2>&1
	[ -n "${TGT_PID}" ] && { kill "${TGT_PID}" 2>/dev/null; sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null; }
	rcow_load_credentials
	for p in "${A_LVS}" "${B_LVS}"; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "${p}/" >/dev/null 2>&1
	done
	for u in ${UUIDS:-}; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "exports/${u}" >/dev/null 2>&1
	done
	rm -f "${A_WAL}" "${B_WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo "===== log: ${TGT_LOG}"
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null
sleep 2
rm -f "${RPC_SOCK}" "${A_WAL}" "${B_WAL}"
truncate -s 320M "${A_WAL}"
truncate -s 320M "${B_WAL}"

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"; BK="$(rcow_s3_buckets|head -1)"; RG="$(rcow_cfg_get region)"
export EP BK RG
export TOOLS="${ROOT}/test/tools"
for p in "${A_LVS}" "${B_LVS}"; do
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
raw bdev_aio_create "$(printf '{"filename":"%s","name":"a_wal0","block_size":4096}' \
	"${A_WAL}")" >/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"b_wal0","block_size":4096}' \
	"${B_WAL}")" >/dev/null 2>&1
raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PSRC00000000001"}' \
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
await_export()
{
	local u="$1" st=""
	for _ in $(seq 120); do
		st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${u}")" \
			2>/dev/null | tr -d '"[:space:]\n')"
		case "${st}" in *DONE*) return 0 ;; esac
		sleep 1
	done
	return 1
}

echo
info "[1] node A: a snapshot, exported by reference"
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"a_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${A_LVS}" "${BK}")" >/dev/null || { echo "create a"; exit 1; }
rpc rcow_create_lvol '{"lvol_name":"va","size_gib":1}' >/dev/null || exit 1
NSID="$(expose "${A_LVS}/va")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 2
DEV="$(wait_dev "${NSID}")" || { echo "a device"; exit 1; }

dd if=/dev/urandom of="${WORKDIR}/pat.bin" bs=1M count="${FILL_MB}" status=none
dd if="${WORKDIR}/pat.bin" of="${DEV}" bs=1M count="${FILL_MB}" oflag=direct \
	conv=fsync status=none
sync
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${A_LVS}")" >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"va","snapshot_name":"sa"}' >/dev/null || exit 1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${A_LVS}")" >/dev/null
sleep 3
EXP_A="$(rpc rcow_export_snapshot '{"snapshot_name":"sa"}' 2>&1 | tr -d '"[:space:]\n')"
[ -n "${EXP_A}" ] || { echo "export a"; exit 1; }
UUIDS="${EXP_A}"
await_export "${EXP_A}" || { echo "export a never done"; exit 1; }
info "export A = ${EXP_A}"

unexpose "${NSID}"; sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${A_LVS}")" >/dev/null 2>&1
sleep 2

echo
info "[2] node B: import it, rewrite half, snapshot, export again"
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"b_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${B_LVS}" "${BK}")" >/dev/null || { echo "create b"; exit 1; }
rpc rcow_import_lvol "$(printf '{"lvol_name":"vb","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_A}" "${B_LVS}")" >/dev/null 2>&1 || { echo "import"; exit 1; }
NSID="$(expose "${B_LVS}/vb")"
sleep 2
DEV="$(wait_dev "${NSID}")" || { echo "b device"; exit 1; }
# Only half, so the derived export still inherits the rest from A -- otherwise B
# would own every chunk and there would be no upstream reference at all.
dd if=/dev/urandom of="${WORKDIR}/tail.bin" bs=1M count="${HALF_MB}" status=none
dd if="${WORKDIR}/tail.bin" of="${DEV}" bs=1M count="${HALF_MB}" seek="${HALF_MB}" \
	oflag=direct conv=fsync status=none
sync
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"vb","snapshot_name":"sb"}' >/dev/null || exit 1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" >/dev/null
sleep 3

MARK="$(wc -l <"${TGT_LOG}")"
EXP_B="$(rpc rcow_export_snapshot '{"snapshot_name":"sb"}' 2>&1 | tr -d '"[:space:]\n')"
[ -n "${EXP_B}" ] || { echo "export b"; exit 1; }
UUIDS="${UUIDS} ${EXP_B}"
await_export "${EXP_B}" || { echo "export b never done"; exit 1; }
want "B's export is derived (version 3)" "$(mfield "${EXP_B}" version)" "3"
info "export B = ${EXP_B}"

echo
info "[3] nobody has imported E, yet it renews upstream"
sleep 3
NEW="$(tail -n "+${MARK}" "${TGT_LOG}")"
if printf '%s' "${NEW}" | grep -q 'renews .* upstream lease'; then
	ok "$(printf '%s' "${NEW}" | grep -o 'renews .* upstream lease(s).*second(s)' | head -1)"
else
	bad "the derived export renews nothing upstream"
fi
UP_KEY="${A_LVS}/meta/exports/${EXP_A}.lease"
if printf '%s' "${NEW}" | grep -q "upstream lease \[0\]: ${UP_KEY}"; then
	ok "and the key is A's own export lease"
else
	bad "the upstream key is not ${UP_KEY}: $(printf '%s' "${NEW}" \
		| grep -o 'upstream lease \[0\].*' | head -1)"
fi

echo
info "[4] A's lease object is really being refreshed, by E and not by vb"
# The log alone would also be printed by an implementation whose PUTs all fail.
T1="$(lease_updated_at "${UP_KEY}")"
if [ -z "${T1}" ]; then
	bad "no lease object at ${UP_KEY}"
else
	ok "the lease exists, updated_at=${T1}"
	# Decisive, and cheap: vb renews this same key too, so a timestamp that
	# moved does not by itself say the export moved it.
	want "and it was written by the export, not the import" \
		"$(lease_field "${UP_KEY}" importer_id)" "export:${EXP_B}"
	# Derived exports and importers use the same fixed cadence now that export
	# lifetime is independent of a deadline.
	RS="$(lease_field "${UP_KEY}" renew_s)"
	if [ "${RS:-0}" = 20 ] 2>/dev/null; then
		ok "and reports the fixed lease cadence (renew_s=${RS})"
	else
		bad "renew_s=${RS:-<none>}, expected 20"
	fi
	# The renew interval is the floor, 20 s. Wait past one tick.
	info "waiting for the next renewal (interval is the 20 s floor)"
	sleep 25
	T2="$(lease_updated_at "${UP_KEY}")"
	if [ -n "${T2}" ] && [ "${T2}" -gt "${T1}" ] 2>/dev/null; then
		ok "and it moved on: ${T1} -> ${T2}"
	else
		bad "updated_at did not advance (${T1} -> ${T2:-<gone>}); the PUTs are "\
"not landing"
	fi
fi

echo
info "[5] a restart resumes it, from the registry rather than the manifest"
# The obligation is to another node, so it has to outlive this one's uptime: a
# restart that forgets it hands A permission to delete the snapshot behind chunks
# E still names. Asserted as stop-then-resume rather than just "still renewing",
# because the latter also passes if some poller was simply never stopped.
unexpose "${NSID}"; sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" >/dev/null 2>&1 \
	&& info "unloaded ${B_LVS}" || bad "could not unload ${B_LVS}"
sleep 2
T5="$(lease_updated_at "${UP_KEY}")"
info "waiting to confirm the renewals stopped with the lvstore"
sleep 25
T6="$(lease_updated_at "${UP_KEY}")"
if [ "${T6:-0}" = "${T5}" ]; then
	ok "unloaded: the renewals stopped (${T5} unchanged)"
else
	bad "still renewing after unload: ${T5} -> ${T6}"
fi

MARK3="$(wc -l <"${TGT_LOG}")"
rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"b_wal0"}' \
	"${B_LVS}" "${BK}")" >/dev/null 2>&1 \
	&& info "re-attached ${B_LVS}" || { bad "could not re-attach ${B_LVS}"; }
sleep 3
NEW3="$(tail -n "+${MARK3}" "${TGT_LOG}")"
if printf '%s' "${NEW3}" | grep -q "upstream lease \[0\]: ${UP_KEY}"; then
	ok "the attach read the key back out of the registry"
else
	bad "the attach did not resume any upstream lease; a derived export's "\
"obligation did not survive a restart"
fi
# arm() renews immediately, so this catches the resume without a further wait;
# the second window then proves the poller runs and not merely that one PUT went.
T7="$(lease_updated_at "${UP_KEY}")"
if [ -n "${T7}" ] && [ "${T7}" -gt "${T6:-0}" ] 2>/dev/null; then
	ok "and renewed at once on attach: ${T6} -> ${T7}"
else
	bad "no renewal after attach (${T6} -> ${T7:-<gone>})"
fi
want "still identifying the export as the writer" \
	"$(lease_field "${UP_KEY}" importer_id)" "export:${EXP_B}"
info "waiting to confirm the poller is running, not just the one PUT"
sleep 25
T8="$(lease_updated_at "${UP_KEY}")"
if [ -n "${T8}" ] && [ "${T8}" -gt "${T7:-0}" ] 2>/dev/null; then
	ok "the resumed poller keeps renewing: ${T7} -> ${T8}"
else
	bad "renewals did not continue after the attach (${T7} -> ${T8:-<gone>})"
fi

echo
info "[6] release_export is what stops it"
MARK2="$(wc -l <"${TGT_LOG}")"
REL="$(rpc --raw rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
	"${EXP_B}" "${B_LVS}")" 2>&1)"
info "release reply: ${REL}"
T3="$(lease_updated_at "${UP_KEY}")"
sleep 25
T4="$(lease_updated_at "${UP_KEY}")"
if [ -z "${T3}" ]; then
	bad "the lease vanished before the check"
elif [ "${T4:-0}" = "${T3}" ]; then
	ok "the upstream lease stopped being renewed (${T3} unchanged)"
else
	bad "still renewing after release: ${T3} -> ${T4}"
fi

echo
echo "===== SUMMARY"
if [ "${FAILED}" = "0" ]; then
	echo "  an unimported derived export still renews its upstream until"
	echo "  release_export."
else
	echo "  ${FAILED} failure(s) -- see [FAIL] above"
fi
exit "${FAILED}"
