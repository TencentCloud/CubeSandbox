#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Several imports of one export on one lvstore share one hidden template.
#  The template blob is created first, as the esnap, and frozen read-only.
#  Every user lvol, including the first, is a clone of that template.
#
#  The template is named __rcow_tpl_<export uuid>. It is not a volume the caller
#  asked for, so listing, lookup, clone, export, activate and decouple-by-name
#  all answer as if it were not there. Every user lvol is a local clone of it,
#  whether or not the import asked for decouple.
#
#  What is asserted:
#
#    1. Importing the export back into the lvstore that produced it stays a
#       local clone. No template is created and nothing is written to the
#       import registry.
#    2. Two decouple:false imports on a different lvstore both report mode
#       esnap, both read the export, and a write to one is not visible to the
#       other. The listing grows by exactly those two names. The template name
#       is refused by get, delete, clone, export, activate and decouple.
#       The two volumes share one registry entry.
#    3. Deleting one leaves the registry and the survivor. Deleting the last
#       one drops the registry, and the next import is esnap again -- a leftover
#       template would have been cloned instead, and the mode would be
#       local_clone.
#    4. Decoupling one user lvol decouples the template. A later import on that
#       lvstore reports local_clone (it waited out the copy, or the copy had
#       already finished) and does not write the export back into the registry,
#       so the source lease stays down. A user lvol cloned from the template
#       then has nothing left to decouple. Deleting every such clone reaps the
#       local template; the next import is an esnap again.
#    5. This process can host one blobstore at a time, so the second lvstore is
#       created only after the first is unloaded. It imports the same export as
#       esnap: the hidden template did not travel with the export. After the
#       source objects and the manifest are deleted, that second lvstore cannot
#       read the pattern. The first lvstore's local copy survives unload/reload
#       after decouple, independently of the source.
#
#  Usage:
#    sudo -E ./test/dataplane/run_shared_chain_test.sh
#
#  Needs root, a readable /data/cubelet/s3.cfg, and nvme-cli.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"

# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=schsrc
DST_LVS=schdst
DST2_LVS=schdst2
SRC_WAL=/tmp/sch_src_wal.img
DST_WAL=/tmp/sch_dst_wal.img
DST2_WAL=/tmp/sch_dst2_wal.img
RPC_SOCK=/tmp/schain.sock
TGT_LOG=/tmp/schain_target.log
NQN="nqn.2026-08.io.spdk:schain"
PORT="4483"
FILL_MB=64
COW_OFF_MB=80
COW_MB=4

PASS=0
FAIL=0
TGT_PID=""
CONNECTED=0
WORKDIR=""
EXP_UUID=""
BUCKET=""
EP=""
RG=""

pass() { PASS=$((PASS + 1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }

cleanup()
{
	local rc=$?

	set +u
	echo ""
	echo "=== cleanup"
	if [ "${CONNECTED}" = "1" ]; then
		nvme disconnect -n "${NQN}" >/dev/null 2>&1 || true
	fi
	if [ -n "${TGT_PID}" ]; then
		kill "${TGT_PID}" 2>/dev/null || true
		sleep 1
		kill -9 "${TGT_PID}" 2>/dev/null || true
		wait "${TGT_PID}" 2>/dev/null || true
	fi
	rm -f "${RPC_SOCK}"

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ] && [ -n "${BUCKET}" ]; then
		for p in "${SRC_LVS}" "${DST_LVS}" "${DST2_LVS}"; do
			rcow_discard_writeback_shm "${p}" || true
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "${p}/" >/dev/null 2>&1 || true
		done
		if [ -n "${EXP_UUID}" ]; then
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "exports/${EXP_UUID}" >/dev/null 2>&1 || true
		fi
		rm -f "${SRC_WAL}" "${DST_WAL}" "${DST2_WAL}"
		rm -rf "${WORKDIR}"
	else
		info "state kept: ${TGT_LOG} ${WORKDIR:-}"
	fi

	echo ""
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	if [ "${FAIL}" -ne 0 ] || [ "${rc}" -ne 0 ]; then
		exit 1
	fi
}
trap cleanup EXIT

die()
{
	fail "$*"
	exit 1
}

import_mode()
{
	python3 - "$1" <<'PY'
import json, sys
try:
    print(json.loads(sys.argv[1]).get("mode", "<absent>"))
except Exception:
    print("<unparseable>")
PY
}

lvol_names()
{
	local lvs="$1"

	rpc rcow_get_lvstores 2>/dev/null | python3 -c '
import json, sys
lvs = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    sys.exit(2)
row = next((r for r in rows if r.get("lvs_name") == lvs), None)
if row is None:
    sys.exit(3)
for name in sorted(v.get("name", "") for v in (row.get("lvols") or [])):
    print(name)
' "${lvs}"
}

import_count()
{
	local lvs="$1" uuid="$2"

	rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${lvs}")" 2>/dev/null \
		| python3 -c '
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print("parse-error")
    sys.exit(0)
print(sum(1 for r in rows if r.get("export_uuid") == uuid))
' "${uuid}"
}

# A hidden name must fail, and the failure must be "not there" rather than a
# permissions error that would admit the name exists.
hidden_refused()
{
	local what="$1"
	shift
	local err

	if err="$("$@" 2>&1)"; then
		fail "${what} succeeded"
		return 1
	fi
	if printf '%s' "${err}" | grep -Eq 'not found|no such lvol'; then
		pass "${what} is hidden"
		return 0
	fi
	fail "${what} failed another way: ${err}"
	return 1
}

wait_export_done()
{
	local uuid="$1"
	local deadline=$((SECONDS + 120))
	local out st

	while :; do
		if ! out="$(rpc rcow_get_snapshot_status \
				"$(printf '{"export_uuid":"%s"}' "${uuid}")" 2>/dev/null)"; then
			fail "export ${uuid} finished without a manifest"
			return 1
		fi
		st="$(printf '%s' "${out}" | python3 -c \
			'import json,sys; print(json.load(sys.stdin).get("export_status",""))' \
			2>/dev/null)"
		if [ "${st}" = "DONE" ]; then
			return 0
		fi
		if [ "${SECONDS}" -ge "${deadline}" ]; then
			fail "export ${uuid} never reached DONE"
			return 1
		fi
		sleep 0.2
	done
}

wait_lvol_gone()
{
	local name="$1"
	local deadline=$((SECONDS + 30))

	while :; do
		if ! rpc rcow_get_lvol "$(printf '{"lvol_name":"%s"}' "${name}")" \
				>/dev/null 2>&1; then
			return 0
		fi
		if [ "${SECONDS}" -ge "${deadline}" ]; then
			return 1
		fi
		sleep 0.2
	done
}

wait_import_count()
{
	local lvs="$1" uuid="$2" want="$3"
	local deadline=$((SECONDS + 20))
	local got

	while :; do
		got="$(import_count "${lvs}" "${uuid}")"
		if [ "${got}" = "${want}" ]; then
			return 0
		fi
		if [ "${SECONDS}" -ge "${deadline}" ]; then
			fail "lvstore ${lvs} has ${got} registry entries for the export, want ${want}"
			return 1
		fi
		sleep 0.2
	done
}

wait_decouple_idle()
{
	local deadline=$((SECONDS + 300))
	local n

	while :; do
		n="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin)
except Exception:
    rows = None
if isinstance(rows, dict):
    rows = rows.get("queue", [])
print(-1 if not isinstance(rows, list) else len(rows))
' 2>/dev/null || echo -1)"
		if [ "${n}" = "0" ]; then
			return 0
		fi
		if [ "${SECONDS}" -ge "${deadline}" ]; then
			fail "decouple still running after 300s"
			return 1
		fi
		sleep 0.5
	done
}

ctrl_of_nqn()
{
	local c

	for c in /sys/class/nvme/nvme*; do
		if [ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ]; then
			printf '/dev/%s' "$(basename "${c}")"
			return 0
		fi
	done
	return 1
}

# The new namespace is whichever block device appeared. Linux does not name
# /dev/nvmeXnY from the nsid.
wait_new_dev()
{
	local before="$1"
	local deadline=$((SECONDS + 25))
	local ctrl now dev

	while [ "${SECONDS}" -lt "${deadline}" ]; do
		ctrl="$(ctrl_of_nqn)" || { sleep 0.2; continue; }
		nvme ns-rescan "${ctrl}" >/dev/null 2>&1 || true
		udevadm settle --timeout=2 >/dev/null 2>&1 || true
		now="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
		dev="$(comm -13 <(printf '%s\n' "${before}") <(printf '%s\n' "${now}") | head -1)"
		if [ -n "${dev}" ] && [ -b "${dev}" ]; then
			printf '%s' "${dev}"
			return 0
		fi
		sleep 0.2
	done
	return 1
}

expose()
{
	raw nvmf_subsystem_add_ns \
		"$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' "${NQN}" "$1")" \
		2>/dev/null | tr -d '[:space:]"'
}

unexpose()
{
	local nsid="$1"

	[ -n "${nsid}" ] && raw nvmf_subsystem_remove_ns \
		"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "${nsid}")" >/dev/null 2>&1
	return 0
}

md5_at()
{
	local dev="$1" off="$2" len="$3"

	dd if="${dev}" bs=1M count="${len}" skip="${off}" iflag=direct status=none \
		2>/dev/null | md5sum | cut -d' ' -f1
}

# --------------------------------------------------------------------------
echo "=== [0] preconditions"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -x "${TGT_BIN}" ] || { echo "target not built" >&2; exit 1; }
"${ROOT}/test/tools/check_binary_fresh.sh" "${TGT_BIN}" || exit 1
[ -r "${RCOW_S3_CFG:-/data/cubelet/s3.cfg}" ] || { echo "no S3 config" >&2; exit 1; }
command -v nvme >/dev/null || { echo "nvme-cli is required" >&2; exit 1; }
[ -n "$(rcow_target_instances)" ] && { echo "a target is already running" >&2; exit 1; }

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"
RG="$(rcow_cfg_get region)"
BUCKET="$(rcow_s3_buckets | head -1)"
[ -n "${EP}" ] && [ -n "${BUCKET}" ] || die "S3 config has no endpoint or bucket"

WORKDIR="$(mktemp -d /tmp/schain.XXXXXX)"
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}" "${DST2_WAL}"
truncate -s 512M "${SRC_WAL}"
truncate -s 512M "${DST_WAL}"
truncate -s 512M "${DST2_WAL}"

for p in "${SRC_LVS}" "${DST_LVS}" "${DST2_LVS}"; do
	python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" -p "${p}/" \
		>/dev/null 2>&1 || true
done

info "starting target"
"${TGT_BIN}" -m "${S3LVOL_TGT_CPUMASK:-0x3}" --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do
	[ -S "${RPC_SOCK}" ] && break
	sleep 0.25
done
[ -S "${RPC_SOCK}" ] || die "target failed to start"
sleep 1
kill -0 "${TGT_PID}" 2>/dev/null || die "target exited during startup"

S3_JSON="$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"' \
	"${BUCKET}" "${EP}" "${BUCKET}" "${RG}")"
[ "$(rcow_cfg_get path_style)" = "true" ] && S3_JSON+=', "path_style": true'
[ "$(rcow_cfg_get no_tls)" = "true" ] && S3_JSON+=', "no_tls": true'
S3_JSON+="}"
rpc rcow_add_s3_config "${S3_JSON}" >/dev/null || die "rcow_add_s3_config failed"

raw bdev_aio_create "$(printf '{"filename":"%s","name":"sch_src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null || die "source wal bdev"
raw bdev_aio_create "$(printf '{"filename":"%s","name":"sch_dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null || die "dest wal bdev"
raw bdev_aio_create "$(printf '{"filename":"%s","name":"sch_dst2_wal0","block_size":4096}' \
	"${DST2_WAL}")" >/dev/null || die "second dest wal bdev"

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"sch_src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BUCKET}")" >/dev/null || die "create ${SRC_LVS}"

raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1 || true
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"SCHAIN000000001"}' \
	"${NQN}")" >/dev/null || die "nvmf subsystem"
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null || die "nvmf listener"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1 \
	|| die "nvme connect"
CONNECTED=1
pass "target and source lvstore are up"

# --------------------------------------------------------------------------
echo ""
echo "=== [1] source volume, snapshot, export"

rpc rcow_create_lvol '{"lvol_name":"src","size_gib":1}' >/dev/null || die "create src"
BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
NS_SRC="$(expose "${SRC_LVS}/src")"
[ -n "${NS_SRC}" ] || die "expose src"
SRC_DEV="$(wait_new_dev "${BEFORE}")" || die "source device never appeared"

dd if=/dev/urandom of="${WORKDIR}/pattern" bs=1M count="${FILL_MB}" status=none
PAT_MD5="$(md5sum "${WORKDIR}/pattern" | cut -d' ' -f1)"
dd if="${WORKDIR}/pattern" of="${SRC_DEV}" bs=1M oflag=direct conv=fsync status=none \
	|| die "write source"
sync
[ "$(md5_at "${SRC_DEV}" 0 "${FILL_MB}")" = "${PAT_MD5}" ] \
	&& pass "source pattern written" || die "source readback mismatch"

rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "flush source"
rpc rcow_create_snapshot '{"lvol_name":"src","snapshot_name":"snap0"}' >/dev/null \
	|| die "snapshot"
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "flush snapshot"
EXP_UUID="$(rpc rcow_export_snapshot '{"snapshot_name":"snap0"}' | tr -d '"[:space:]')"
[ -n "${EXP_UUID}" ] || die "export failed"
wait_export_done "${EXP_UUID}" || exit 1
pass "exported snap0"
TPL="__rcow_tpl_${EXP_UUID}"

# --------------------------------------------------------------------------
echo ""
echo "=== [2] the producing lvstore does not grow a template"

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"home","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${SRC_LVS}")" 2>&1)" || die "self-import failed: ${OUT}"
MODE="$(import_mode "${OUT}")"
if [ "${MODE}" = "local_clone" ]; then
	pass "same-lvstore import is mode=local_clone"
else
	fail "same-lvstore import mode='${MODE}', expected local_clone"
fi
NAMES="$(lvol_names "${SRC_LVS}")" || die "cannot list source lvols"
if printf '%s\n' "${NAMES}" | grep -q '^__rcow_tpl_'; then
	fail "same-lvstore import created a template"
else
	pass "same-lvstore import created no template"
fi
if [ "$(import_count "${SRC_LVS}" "${EXP_UUID}")" = "0" ]; then
	pass "same-lvstore import wrote no registry entry"
else
	fail "same-lvstore import wrote a registry entry"
fi
rpc rcow_delete_lvol '{"lvol_name":"home"}' >/dev/null || die "delete home"
wait_lvol_gone home || die "home was not deleted"

unexpose "${NS_SRC}"
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "unload source"

# --------------------------------------------------------------------------
echo ""
echo "=== [3] two decouple:false imports share one hidden template"

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"sch_dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BUCKET}")" >/dev/null || die "create ${DST_LVS}"

if rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
		"${TPL}" "${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1; then
	fail "an lvol named ${TPL} was accepted"
else
	pass "the template prefix is refused as an lvol name"
fi

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"a","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "import a failed: ${OUT}"
[ "$(import_mode "${OUT}")" = "esnap" ] && pass "import a (decouple:false) is mode=esnap" \
	|| fail "import a mode=$(import_mode "${OUT}"), expected esnap"

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"b","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "import b failed: ${OUT}"
[ "$(import_mode "${OUT}")" = "esnap" ] && pass "import b (decouple:false) is mode=esnap" \
	|| fail "import b mode=$(import_mode "${OUT}"), expected esnap"

NAMES="$(lvol_names "${DST_LVS}")" || die "cannot list dest lvols"
if [ "${NAMES}" = $'a\nb' ]; then
	pass "listing shows exactly a and b"
else
	fail "listing is not exactly a and b"
	printf '%s\n' "${NAMES}" | sed 's/^/       /'
fi

hidden_refused "get of the template" \
	rpc rcow_get_lvol "$(printf '{"lvol_name":"%s"}' "${TPL}")"
hidden_refused "delete of the template" \
	rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${TPL}")"
hidden_refused "clone of the template" \
	rpc rcow_create_clone "$(printf '{"snapshot_name":"%s","clone_name":"nope"}' "${TPL}")"
hidden_refused "export of the template" \
	rpc rcow_export_snapshot "$(printf '{"snapshot_name":"%s"}' "${TPL}")"
hidden_refused "activate of the template" \
	rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${TPL}")"
hidden_refused "decouple of the template by name" \
	rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' "${TPL}")"

wait_import_count "${DST_LVS}" "${EXP_UUID}" 1 && pass "a and b share one registry entry"

BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
NS_A="$(expose "${DST_LVS}/a")"
A_DEV="$(wait_new_dev "${BEFORE}")" || die "device for a never appeared"
BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
NS_B="$(expose "${DST_LVS}/b")"
B_DEV="$(wait_new_dev "${BEFORE}")" || die "device for b never appeared"

A_MD5="$(md5_at "${A_DEV}" 0 "${FILL_MB}")"
B_MD5="$(md5_at "${B_DEV}" 0 "${FILL_MB}")"
if [ "${A_MD5}" = "${PAT_MD5}" ] && [ "${B_MD5}" = "${PAT_MD5}" ]; then
	pass "both volumes read the export"
else
	fail "export data mismatch on a or b"
fi

dd if=/dev/urandom of="${WORKDIR}/cow" bs=1M count="${COW_MB}" status=none
COW_MD5="$(md5sum "${WORKDIR}/cow" | cut -d' ' -f1)"
ZERO_MD5="$(dd if=/dev/zero bs=1M count="${COW_MB}" status=none | md5sum | cut -d' ' -f1)"
dd if="${WORKDIR}/cow" of="${A_DEV}" bs=1M seek="${COW_OFF_MB}" oflag=direct conv=fsync \
	status=none || die "cow write"
if [ "$(md5_at "${A_DEV}" "${COW_OFF_MB}" "${COW_MB}")" = "${COW_MD5}" ] \
		&& [ "$(md5_at "${B_DEV}" "${COW_OFF_MB}" "${COW_MB}")" = "${ZERO_MD5}" ] \
		&& [ "$(md5_at "${B_DEV}" 0 "${FILL_MB}")" = "${PAT_MD5}" ]; then
	pass "a write on one clone is not visible to the other"
else
	fail "the two clones do not have independent copy-on-write"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [4] the template goes away with its last clone"

unexpose "${NS_A}"
rpc rcow_delete_lvol '{"lvol_name":"a"}' >/dev/null || die "delete a"
wait_lvol_gone a || die "a was not deleted"
wait_import_count "${DST_LVS}" "${EXP_UUID}" 1 \
	&& pass "deleting one clone keeps the registry"
[ "$(md5_at "${B_DEV}" 0 "${FILL_MB}")" = "${PAT_MD5}" ] \
	&& pass "the survivor still reads the export" \
	|| fail "the survivor lost the export"

unexpose "${NS_B}"
rpc rcow_delete_lvol '{"lvol_name":"b"}' >/dev/null || die "delete b"
wait_lvol_gone b || die "b was not deleted"
wait_import_count "${DST_LVS}" "${EXP_UUID}" 0 \
	&& pass "deleting the last clone drops the registry"

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"c","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "reimport failed: ${OUT}"
if [ "$(import_mode "${OUT}")" = "esnap" ]; then
	pass "a new import after the reap is esnap again"
else
	fail "reimport mode=$(import_mode "${OUT}"), expected esnap (template was not reaped)"
fi
rpc rcow_delete_lvol '{"lvol_name":"c"}' >/dev/null || die "delete c"
wait_lvol_gone c || die "c was not deleted"
wait_import_count "${DST_LVS}" "${EXP_UUID}" 0 || exit 1

# --------------------------------------------------------------------------
echo ""
echo "=== [5] one decouple covers the lvstore, and not the next one"

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"d","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "import d failed: ${OUT}"
[ "$(import_mode "${OUT}")" = "esnap" ] || die "import d was not esnap"

rpc rcow_decouple_lvol '{"lvol_name":"d"}' >/dev/null || die "decouple d failed to start"
if grep -a "decoupling the template" "${TGT_LOG}" >/dev/null 2>&1; then
	pass "decouple of a user lvol targets the hidden template"
else
	fail "decouple of d did not redirect to the template"
fi

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"f","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "import during decouple failed: ${OUT}"
if [ "$(import_mode "${OUT}")" = "local_clone" ]; then
	pass "an import that arrives during the copy joins the chain as a local clone"
else
	fail "import during decouple mode=$(import_mode "${OUT}"), expected local_clone"
fi

wait_decouple_idle || exit 1
NAMES="$(lvol_names "${DST_LVS}")" || die "cannot list dest after decouple"
if [ "${NAMES}" = $'d\nf' ]; then
	pass "the template stays hidden after it is decoupled"
else
	fail "listing after decouple is not exactly d and f"
	printf '%s\n' "${NAMES}" | sed 's/^/       /'
fi
if rpc rcow_decouple_lvol '{"lvol_name":"f"}' >/dev/null 2>&1; then
	fail "f still had an export to decouple"
else
	pass "f's chain no longer reads the export"
fi
wait_import_count "${DST_LVS}" "${EXP_UUID}" 0 \
	&& pass "the decoupled lvstore no longer records the export"

BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
NS_D="$(expose "${DST_LVS}/d")"
D_DEV="$(wait_new_dev "${BEFORE}")" || die "device for d never appeared"
BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
NS_F="$(expose "${DST_LVS}/f")"
F_DEV="$(wait_new_dev "${BEFORE}")" || die "device for f never appeared"
if [ "$(md5_at "${D_DEV}" 0 "${FILL_MB}")" = "${PAT_MD5}" ] \
		&& [ "$(md5_at "${F_DEV}" 0 "${FILL_MB}")" = "${PAT_MD5}" ]; then
	pass "both clones read the pattern after the shared decouple"
else
	fail "a clone did not read the pattern after the shared decouple"
fi

# The registry is already empty. Cloning the local template must not put the
# export back: that entry is what starts the source lease.
LEASE_MARK="$(wc -l < "${TGT_LOG}")"
OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"g","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "import g failed: ${OUT}"
if [ "$(import_mode "${OUT}")" = "local_clone" ]; then
	pass "an import after decouple is a local clone"
else
	fail "import g mode=$(import_mode "${OUT}"), expected local_clone"
fi
NAMES="$(lvol_names "${DST_LVS}")" || die "cannot list dest after import g"
if [ "${NAMES}" = $'d\nf\ng' ]; then
	pass "listing after the post-decouple import is d, f and g"
else
	fail "listing after import g is not exactly d, f and g"
	printf '%s\n' "${NAMES}" | sed 's/^/       /'
fi
if tail -n +"${LEASE_MARK}" "${TGT_LOG}" \
		| grep -aqF "already holds a local template for export ${EXP_UUID}"; then
	pass "the post-decouple import cloned the local template"
else
	fail "the post-decouple import did not take the local-template path"
fi
if tail -n +"${LEASE_MARK}" "${TGT_LOG}" | grep -aq "renewing"; then
	fail "the post-decouple import renewed a source lease"
else
	pass "the post-decouple import did not renew a source lease"
fi
wait_import_count "${DST_LVS}" "${EXP_UUID}" 0 \
	&& pass "the post-decouple import left the registry empty"

unexpose "${NS_D}"
unexpose "${NS_F}"
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null \
	|| die "unload ${DST_LVS} after decouple"
rpc rcow_attach_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"sch_dst_wal0"}' \
		"${DST_LVS}" "${BUCKET}")" >/dev/null \
	|| die "re-attach ${DST_LVS} after decouple"
BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
NS_D="$(expose "${DST_LVS}/d")"
D_DEV="$(wait_new_dev "${BEFORE}")" || die "device for reloaded d never appeared"
if [ "$(md5_at "${D_DEV}" 0 "${FILL_MB}")" = "${PAT_MD5}" ]; then
	pass "the decoupled template survives unload/reload"
else
	fail "the decoupled template came back empty after unload/reload"
fi
unexpose "${NS_D}"
REAP_MARK="$(wc -l < "${TGT_LOG}")"
rpc rcow_delete_lvol '{"lvol_name":"d"}' >/dev/null || die "delete d"
wait_lvol_gone d || die "d was not deleted"
rpc rcow_delete_lvol '{"lvol_name":"f"}' >/dev/null || die "delete f"
wait_lvol_gone f || die "f was not deleted"
if tail -n +"${REAP_MARK}" "${TGT_LOG}" \
		| grep -aqF "Removed unused import template for export ${EXP_UUID}"; then
	fail "the local template was reaped while a clone remained"
else
	pass "the local template stays while a clone remains"
fi
rpc rcow_delete_lvol '{"lvol_name":"g"}' >/dev/null || die "delete g"
wait_lvol_gone g || die "g was not deleted"
REAPED=0
for _ in $(seq 50); do
	if tail -n +"${REAP_MARK}" "${TGT_LOG}" \
			| grep -aqF "Removed unused import template for export ${EXP_UUID}"; then
		REAPED=1
		break
	fi
	sleep 0.1
done
if [ "${REAPED}" = "1" ]; then
	pass "deleting the last clone reaped the local template"
else
	fail "the local template was not reaped after its last clone was deleted"
fi
wait_import_count "${DST_LVS}" "${EXP_UUID}" 0 \
	&& pass "the registry stays empty after the local template is reaped"

OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"h","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)" || die "import h failed: ${OUT}"
if [ "$(import_mode "${OUT}")" = "esnap" ]; then
	pass "a new import after the local template was reaped is esnap again"
else
	fail "import h mode=$(import_mode "${OUT}"), expected esnap (local template was not reaped)"
fi
rpc rcow_delete_lvol '{"lvol_name":"h"}' >/dev/null || die "delete h"
wait_lvol_gone h || die "h was not deleted"
wait_import_count "${DST_LVS}" "${EXP_UUID}" 0 || exit 1
nvme disconnect -n "${NQN}" >/dev/null 2>&1 || true
CONNECTED=0
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null \
	|| die "unload ${DST_LVS}"

# One blobstore per process. The second lvstore is created only once the first
# has been unloaded, which is also what makes the assertion sharp: nothing of
# the first chain is still open to be cloned by accident.
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"sch_dst2_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST2_LVS}" "${BUCKET}")" >/dev/null || die "create ${DST2_LVS}"
OUT="$(raw rcow_import_lvol "$(printf '{"lvol_name":"e","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST2_LVS}")" 2>&1)" || die "import e failed: ${OUT}"
if [ "$(import_mode "${OUT}")" = "esnap" ]; then
	pass "another lvstore imports the same export as its own esnap"
else
	fail "other-lvstore import mode=$(import_mode "${OUT}"), expected esnap"
fi
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST2_LVS}")" >/dev/null \
	|| die "unload ${DST2_LVS}"

# --------------------------------------------------------------------------
echo ""
echo "=== [6] another lvstore still depends on the source"

python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" -p "${SRC_LVS}/" \
	>/dev/null || die "could not delete the source prefix"
python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" -p "exports/${EXP_UUID}" \
	>/dev/null || die "could not delete the export manifest"

if ! ATTACH_ERR="$(rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"sch_dst2_wal0"}' \
		"${DST2_LVS}" "${BUCKET}")" 2>&1)"; then
	pass "the other lvstore cannot attach once the export is gone"
else
	nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1 \
		|| die "nvme reconnect for the other lvstore"
	CONNECTED=1
	BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	NS_E="$(expose "${DST2_LVS}/e")"
	E_DEV="$(wait_new_dev "${BEFORE}")" || {
		pass "the other lvstore's import did not come back as a device"
		exit 0
	}
	if ! dd if="${E_DEV}" of="${WORKDIR}/e.bin" bs=1M count="${FILL_MB}" \
			iflag=direct status=none 2>"${WORKDIR}/e.err"; then
		pass "reading the other lvstore fails once the export is gone"
	else
		E_MD5="$(md5sum "${WORKDIR}/e.bin" | cut -d' ' -f1)"
		if [ "${E_MD5}" = "${PAT_MD5}" ]; then
			fail "the other lvstore still served the export after its objects were deleted"
		else
			pass "the other lvstore does not serve the export after its objects are deleted"
		fi
	fi
fi
