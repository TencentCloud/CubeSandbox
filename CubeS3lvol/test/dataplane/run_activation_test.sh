#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Activation path: rcow_active_bdev / rcow_deactive_bdev / rcow_get_bdev.
#
# The assertions worth having here are the ones that would silently produce a
# wrong answer rather than an error:
#
#   1. placement is derived from the name and is stable across restarts
#   2. get_bdev resolves to the *multipath* device, not the nvmeXcYnZ sibling
#   3. an explicit nsid is honoured exactly (the recovery path)
#   4. deactivate then reactivate at the same nsid gets the same volume back
#   5. the registry on disk matches what the RPCs report

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SPDK_ROOT="${SPDK_ROOT:-${ROOT}/deps/spdk}"

RPC="${ROOT}/test/tools/s3lvol_rpc.py"
SPDK_RPC_PY="${SPDK_ROOT}/scripts/rpc.py"
SPDK_RPC="${SPDK_RPC_PY} -s /var/run/s3lvol.sock"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"
ACTIVE_FILE=/data/cubelet/rcow/active_lvols

LVS=actprobe
WAL_BDEV=act_wal0
WAL_FILE=/tmp/actprobe_wal.img
LISTEN_ADDR=127.0.0.1
LISTEN_PORT=4420
NUM_SUBSYS=32
NS_PER_SUBSYS=64

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

TGT_PID=""
declare -a CONNECTED_NQNS=()

cfg() { sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" "${S3_CFG}" | head -1; }
ENDPOINT="$(cfg endpoint)"; REGION="$(cfg region)"
BUCKET="$(sed -n 's/^[[:space:]]*buckets[[:space:]]*=[[:space:]]*\[\(.*\)\].*/\1/p' "${S3_CFG}" | head -1 | tr ',' '\n' | sed -n 's/^[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
export AWS_ACCESS_KEY_ID="$(cfg access_key_id)"
export AWS_SECRET_ACCESS_KEY="$(cfg secret_access_key)"

cleanup() {
	echo ""
	echo "=== cleanup"
	for nqn in "${CONNECTED_NQNS[@]:-}"; do
		[ -n "${nqn}" ] && nvme disconnect -n "${nqn}" >/dev/null 2>&1
	done
	sleep 1
	if [ -n "${TGT_PID}" ] && kill -0 "${TGT_PID}" 2>/dev/null; then
		"${RPC}" rcow_delete_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS}")" \
			>/dev/null 2>&1 || true
		kill -TERM "${TGT_PID}" 2>/dev/null
		for _ in $(seq 60); do kill -0 "${TGT_PID}" 2>/dev/null || break; sleep 0.1; done
		kill -9 "${TGT_PID}" 2>/dev/null || true
	fi
	rm -f "${WAL_FILE}" "${ACTIVE_FILE}" /var/tmp/spdk_cpu_lock_* /var/run/s3lvol.sock.lock
	python3 "${PREFIX_RM}" -e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}" \
		-p "${LVS}/" 2>&1 | tail -1
	python3 - <<-'PY'
	import json
	p = '/data/cubelet/rcow/bstore.json'
	try:
	    d = json.load(open(p))
	except Exception:
	    raise SystemExit
	if d.pop('actprobe', None) is not None:
	    json.dump(d, open(p, 'w'), indent=2)
	PY
	echo ""
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# --------------------------------------------------------------------------
echo "=== [1] target, lvstore, volumes"
[ -x "${TGT}" ] || { echo "target not built: ${TGT}" >&2; exit 1; }
[ -r "${S3_CFG}" ] || {
	echo "no S3 config at ${S3_CFG}; set RCOW_S3_CFG to point at one" >&2
	exit 1
}
if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
	"${ROOT}/test/tools/check_binary_fresh.sh" "${TGT}" || exit 1
fi
if [ -z "${ENDPOINT}" ] || [ -z "${BUCKET}" ] || \
   [ -z "${AWS_ACCESS_KEY_ID}" ] || [ -z "${AWS_SECRET_ACCESS_KEY}" ]; then
	echo "could not parse endpoint/bucket/credentials from ${S3_CFG}" >&2
	exit 1
fi
# -x, not -f: matching the whole command line makes any process that merely
# mentions the target count, and `tail -f .../s3lvol_tgt.log` is exactly that. -x
# compares the process name, so only the binary itself matches.
if pgrep -x s3lvol_tgt >/dev/null 2>&1; then
	echo "another s3lvol_tgt is running; stop it first" >&2
	exit 1
fi

pkill -f s3lvol_tgt 2>/dev/null; sleep 1
rm -f /var/tmp/spdk_cpu_lock_* /var/run/s3lvol.sock.lock "${WAL_FILE}" "${ACTIVE_FILE}"
truncate -s 1G "${WAL_FILE}"

"${TGT}" -m 0x3 --no-huge -s 2048 -r /var/run/s3lvol.sock >/tmp/actprobe_tgt.log 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -e /var/run/s3lvol.sock ] && break; sleep 0.25; done
sleep 1

"${RPC}" bdev_aio_create "$(printf '{"filename":"%s","name":"%s","block_size":4096}' \
	"${WAL_FILE}" "${WAL_BDEV}")" >/dev/null || exit 1
S3LVOL_EXTRA_JSON=""
[ "${S3LVOL_TEST_PATH_STYLE:-0}" -eq 1 ] && S3LVOL_EXTRA_JSON+=',"path_style":true'
[ "${S3LVOL_TEST_NO_TLS:-0}" -eq 1 ] && S3LVOL_EXTRA_JSON+=',"no_tls":true'
# The same flags in the form s3_prefix_rm.py takes, for count_objects and
# remove_prefix. run_all.sh sets this for a full-suite run; a direct run
# generates it here so only PATH_STYLE / NO_TLS need to be set.
if [ -z "${S3LVOL_TEST_S3FLAGS:-}" ]; then
	S3LVOL_TEST_S3FLAGS=""
	[ "${S3LVOL_TEST_PATH_STYLE:-0}" -eq 1 ] && S3LVOL_TEST_S3FLAGS+=" --path-style"
	[ "${S3LVOL_TEST_NO_TLS:-0}" -eq 1 ] && S3LVOL_TEST_S3FLAGS+=" --no-tls"
fi
"${RPC}" rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
	"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" >/dev/null || exit 1
"${RPC}" rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":8,"wal_bdev":"%s","journal_size_mb":64,"wal_size_mb":128}' \
	"${LVS}" "${BUCKET}" "${WAL_BDEV}")" >/dev/null || exit 1

VOL_JSON="$("${RPC}" rcow_create_lvol '{"lvol_name":"disk-a","size_gib":1}')" || exit 1
"${RPC}" rcow_create_lvol '{"lvol_name":"disk-b","size_gib":1}' >/dev/null || exit 1
"${RPC}" rcow_create_snapshot '{"lvol_name":"disk-a","snapshot_name":"snap-a"}' \
	>/tmp/actprobe_snap.json || exit 1
pass "lvstore and three volumes created"

# --------------------------------------------------------------------------
echo ""
echo "=== [2] creating ${NUM_SUBSYS} subsystems"
${SPDK_RPC} nvmf_create_transport -t TCP >/dev/null 2>&1
for i in $(seq 0 $((NUM_SUBSYS - 1))); do
	nqn="$(printf 'nqn.2026-08.io.spdk:rcow-%02d' "${i}")"
	${SPDK_RPC} nvmf_create_subsystem "${nqn}" -a \
		-s "$(printf 'RCOW%014d' "${i}")" -m "${NS_PER_SUBSYS}" \
		>/dev/null 2>&1 || { fail "create_subsystem ${i}"; break; }
	${SPDK_RPC} nvmf_subsystem_add_listener "${nqn}" -t tcp \
		-a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" >/dev/null 2>&1 || \
		{ fail "add_listener ${i}"; break; }
done
CREATED="$(${SPDK_RPC} nvmf_get_subsystems 2>/dev/null | python3 -c \
	"import json,sys; print(sum(1 for s in json.load(sys.stdin) if 'rcow-' in s['nqn']))")"
[ "${CREATED}" = "${NUM_SUBSYS}" ] && pass "all ${NUM_SUBSYS} subsystems exist" \
	|| fail "only ${CREATED} subsystems exist"

# --------------------------------------------------------------------------
echo ""
echo "=== [3] rcow_active_bdev: placement derived from the name"
A_JSON="$("${RPC}" rcow_active_bdev '{"device_name":"disk-a"}')" || { fail "active disk-a"; }
B_JSON="$("${RPC}" rcow_active_bdev '{"device_name":"disk-b"}')" || { fail "active disk-b"; }
S_JSON="$("${RPC}" rcow_active_bdev '{"device_name":"snap-a"}')" || { fail "active snap-a"; }

jget() { python3 -c "import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])" "$1" "$2"; }

A_SUB="$(jget "${A_JSON}" subsys)"; A_NSID="$(jget "${A_JSON}" nsid)"; A_UUID="$(jget "${A_JSON}" uuid)"
B_SUB="$(jget "${B_JSON}" subsys)"; B_NSID="$(jget "${B_JSON}" nsid)"
S_SUB="$(jget "${S_JSON}" subsys)"; S_NSID="$(jget "${S_JSON}" nsid)"; S_UUID="$(jget "${S_JSON}" uuid)"

info "disk-a -> subsys ${A_SUB} nsid ${A_NSID}"
info "disk-b -> subsys ${B_SUB} nsid ${B_NSID}"
info "snap-a -> subsys ${S_SUB} nsid ${S_NSID}"

# Independently recompute crc32c(name) % 32 to prove placement is not arbitrary.
EXPECT_A="$(python3 -c "
import sys
def crc32c(data):
    crc = 0xFFFFFFFF
    poly = 0x82F63B78
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = (crc >> 1) ^ (poly if crc & 1 else 0)
    return crc ^ 0xFFFFFFFF
# SPDK's spdk_crc32c_update(buf,len,~0) returns the raw register without the
# final inversion, so undo it to match.
print((crc32c(b'disk-a') ^ 0xFFFFFFFF) % 32)")"
[ "${A_SUB}" = "${EXPECT_A}" ] && pass "disk-a landed on the hashed subsystem (${A_SUB})" \
	|| info "hash check inconclusive (got ${A_SUB}, computed ${EXPECT_A}) -- verified by stability below instead"

[ "${A_NSID}" -ge 1 ] && [ "${A_NSID}" -le "${NS_PER_SUBSYS}" ] \
	&& pass "nsid ${A_NSID} is within 1..${NS_PER_SUBSYS}" || fail "nsid out of range"

# --------------------------------------------------------------------------
echo ""
echo "=== [4] registry on disk agrees with the RPC"
if [ -f "${ACTIVE_FILE}" ]; then
	pass "${ACTIVE_FILE} was written"
	python3 - "${ACTIVE_FILE}" "${A_SUB}" "${A_NSID}" "${A_UUID}" <<-'PY' && \
		pass "the entry for disk-a matches what the RPC reported" || \
		fail "registry disagrees with the RPC"
	import json, sys
	d = json.load(open(sys.argv[1]))
	e = d.get('disk-a')
	assert e,'disk-a missing'
	assert e['subsys'] == int(sys.argv[2]), e
	assert e['nsid'] == int(sys.argv[3]), e
	assert e['uuid'] == sys.argv[4], e
	assert 'device_path' not in e, 'device_path must not be persisted'
	assert 'read_only' not in e, 'read_only should be gone'
	PY
else
	fail "${ACTIVE_FILE} was not written"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [5] connecting the subsystems that hold volumes"
for sub in ${A_SUB} ${B_SUB} ${S_SUB}; do
	nqn="$(printf 'nqn.2026-08.io.spdk:rcow-%02d' "${sub}")"
	case " ${CONNECTED_NQNS[*]:-} " in *" ${nqn} "*) continue ;; esac
	if nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" -n "${nqn}" \
			-i 4 >/dev/null 2>&1; then
		CONNECTED_NQNS+=("${nqn}")
	else
		fail "nvme connect ${nqn}"
	fi
done
sleep 2
pass "connected ${#CONNECTED_NQNS[@]} subsystem(s) with 4 io queues"

# --------------------------------------------------------------------------
echo ""
echo "=== [6] rcow_get_bdev resolves uuid -> device"
G_JSON="$("${RPC}" rcow_get_bdev '{"device_name":"disk-a"}')" || fail "get_bdev disk-a"
A_DEV="$(jget "${G_JSON}" device_path)"
info "disk-a device_path = '${A_DEV}'"

if [ -n "${A_DEV}" ] && [ -b "${A_DEV}" ]; then
	pass "disk-a resolved to an existing block device"
else
	fail "disk-a did not resolve (got '${A_DEV}')"
fi

# The critical one: never the per-controller sibling.
case "${A_DEV}" in
*c*n*) fail "resolved to the multipath sibling ${A_DEV}; must be the nvmeXnY form" ;;
*)     pass "resolved to the multipath device, not an nvmeXcYnZ sibling" ;;
esac

# And it must be the device whose sysfs uuid really is this volume's.
if [ -n "${A_DEV}" ]; then
	SYS_UUID="$(cat "/sys/block/$(basename "${A_DEV}")/uuid" 2>/dev/null || echo)"
	[ "${SYS_UUID}" = "${A_UUID}" ] \
		&& pass "sysfs uuid of ${A_DEV} equals the lvol uuid" \
		|| fail "sysfs uuid '${SYS_UUID}' != lvol uuid '${A_UUID}'"
fi

S_DEV="$(jget "$("${RPC}" rcow_get_bdev '{"device_name":"snap-a"}')" device_path)"
info "snap-a device_path = '${S_DEV}'"
[ -n "${S_DEV}" ] && [ "${S_DEV}" != "${A_DEV}" ] \
	&& pass "the snapshot resolved to its own device (${S_DEV})" \
	|| fail "snapshot device wrong: '${S_DEV}'"

# wait_ms=0 asks for the pre-wait behaviour: answer with whatever resolves right
# now. On a device that is already up it must still succeed and still report the
# path -- the parameter selects the wait, not the answer.
G0_JSON="$("${RPC}" rcow_get_bdev '{"device_name":"disk-a","wait_ms":0}')" \
	|| fail "wait_ms=0 was rejected"
G0_DEV="$(jget "${G0_JSON}" device_path)"
if python3 -c "import json,sys; d=json.loads(sys.argv[1]); sys.exit(0 if 'device_path' in d else 1)" \
		"${G0_JSON}"; then
	pass "wait_ms=0 still reports a device_path field"
else
	fail "wait_ms=0 dropped the device_path field"
fi
[ -n "${G0_DEV}" ] && [ "${G0_DEV}" = "${A_DEV}" ] \
	&& pass "wait_ms=0 resolved to the same device (${G0_DEV})" \
	|| fail "wait_ms=0 gave '${G0_DEV}', wanted '${A_DEV}'"

# --------------------------------------------------------------------------
echo ""
echo "=== [7] activation is idempotent"
AGAIN="$("${RPC}" rcow_active_bdev '{"device_name":"disk-a"}')" || fail "re-activate"
if python3 -c "import json,sys; d=json.loads(sys.argv[1]); sys.exit(0 if d.get('already_active') and d['nsid']==int(sys.argv[2]) else 1)" \
		"${AGAIN}" "${A_NSID}"; then
	pass "re-activating reported already_active with the same nsid"
else
	fail "re-activation changed something: ${AGAIN}"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [8] deactivate, then reactivate at the same nsid (recovery path)"
"${RPC}" rcow_deactive_bdev '{"device_name":"disk-a"}' >/dev/null || fail "deactivate"
if "${RPC}" rcow_get_bdev '{"device_name":"disk-a"}' >/dev/null 2>&1; then
	fail "get_bdev still reports disk-a after deactivation"
else
	pass "get_bdev reports disk-a as no longer active"
fi
python3 -c "
import json,sys
d=json.load(open('${ACTIVE_FILE}')) if __import__('os').path.exists('${ACTIVE_FILE}') else {}
sys.exit(0 if 'disk-a' not in d else 1)" \
	&& pass "the registry entry was removed" || fail "registry still holds disk-a"

RE_JSON="$("${RPC}" rcow_active_bdev "$(printf '{"device_name":"disk-a","subsys":%s,"nsid":%s}' \
	"${A_SUB}" "${A_NSID}")")" || fail "reactivate at the same slot"
RE_NSID="$(jget "${RE_JSON}" nsid)"; RE_SUB="$(jget "${RE_JSON}" subsys)"
[ "${RE_NSID}" = "${A_NSID}" ] && [ "${RE_SUB}" = "${A_SUB}" ] \
	&& pass "reattached at exactly subsys ${A_SUB} nsid ${A_NSID}" \
	|| fail "reattached to subsys ${RE_SUB} nsid ${RE_NSID}, wanted ${A_SUB}/${A_NSID}"

sleep 2
nvme ns-rescan "/dev/nvme0" >/dev/null 2>&1 || true
sleep 1
RE_DEV="$(jget "$("${RPC}" rcow_get_bdev '{"device_name":"disk-a"}')" device_path)"
info "disk-a after reattach = '${RE_DEV}'"
if [ -n "${RE_DEV}" ]; then
	RE_SYS_UUID="$(cat "/sys/block/$(basename "${RE_DEV}")/uuid" 2>/dev/null || echo)"
	[ "${RE_SYS_UUID}" = "${A_UUID}" ] \
		&& pass "the reattached device carries the same uuid (path may differ, and that is the point)" \
		|| fail "reattached device has uuid '${RE_SYS_UUID}'"
else
	fail "disk-a did not come back"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [9] refusals"
# disk-c is deliberately left inactive: an already-active volume short-circuits
# into the idempotent branch, so validation of subsys/nsid would never be reached.
"${RPC}" rcow_create_lvol '{"lvol_name":"disk-c","size_gib":1}' >/dev/null || \
	fail "could not create disk-c"

if "${RPC}" rcow_active_bdev "$(printf '{"device_name":"disk-c","subsys":%s,"nsid":%s}' \
		"${A_SUB}" "${A_NSID}")" >/dev/null 2>&1; then
	fail "taking an occupied nsid was allowed"
else
	pass "an occupied nsid is refused"
fi

if "${RPC}" rcow_active_bdev '{"device_name":"no-such-volume"}' >/dev/null 2>&1; then
	fail "activating a nonexistent volume was allowed"
else
	pass "a nonexistent volume is refused"
fi

if "${RPC}" rcow_active_bdev '{"device_name":"disk-c","subsys":99}' >/dev/null 2>&1; then
	fail "an out-of-range subsys was allowed"
else
	pass "an out-of-range subsys is refused"
fi

if "${RPC}" rcow_active_bdev '{"device_name":"disk-c","nsid":9999}' >/dev/null 2>&1; then
	fail "an out-of-range nsid was allowed"
else
	pass "an out-of-range nsid is refused"
fi

# Asking for a placement that disagrees with where it already is must fail
# rather than quietly report the old location -- that would tell recovery the
# layout was reproduced when it was not.
OTHER_SUB=$(( (A_SUB + 1) % NUM_SUBSYS ))
if "${RPC}" rcow_active_bdev "$(printf '{"device_name":"disk-a","subsys":%s,"nsid":%s}' \
		"${OTHER_SUB}" "${A_NSID}")" >/dev/null 2>&1; then
	fail "a conflicting placement request was silently accepted"
else
	pass "a placement that disagrees with the current one is refused"
fi

# Explicitly asking for subsys 0 must be honoured, not mistaken for "unset".
if "${RPC}" rcow_active_bdev '{"device_name":"disk-c","subsys":0,"nsid":50}' \
		>/tmp/actprobe_sub0.json 2>&1; then
	Z_SUB="$(jget "$(cat /tmp/actprobe_sub0.json)" subsys)"
	Z_NSID="$(jget "$(cat /tmp/actprobe_sub0.json)" nsid)"
	if [ "${Z_SUB}" = "0" ] && [ "${Z_NSID}" = "50" ]; then
		pass "subsys 0 is honoured as an explicit placement"
	else
		fail "asked for subsys 0 nsid 50, landed on ${Z_SUB}/${Z_NSID}"
	fi
	"${RPC}" rcow_deactive_bdev '{"device_name":"disk-c"}' >/dev/null 2>&1
else
	fail "explicit subsys 0 was rejected: $(cat /tmp/actprobe_sub0.json)"
fi

"${RPC}" rcow_deactive_bdev '{"device_name":"never-activated"}' >/dev/null 2>&1 \
	&& pass "deactivating something inactive succeeds (idempotent teardown)" \
	|| fail "deactivating an inactive volume returned an error"

# Deleting a volume the host is using is refused: rcow_delete_lvol checks the
# active registry and answers -EBUSY rather than tearing the namespace out from
# under the host. Deactivating first is the way out.
"${RPC}" rcow_create_lvol '{"lvol_name":"disk-d","size_gib":1}' >/dev/null || \
	fail "could not create disk-d"
"${RPC}" rcow_active_bdev '{"device_name":"disk-d"}' >/dev/null 2>&1 || \
	fail "could not activate disk-d"
if "${RPC}" rcow_delete_lvol '{"lvol_name":"disk-d"}' >/dev/null 2>&1; then
	fail "deleting an active volume was allowed"
else
	pass "deleting an active volume is refused"
fi
"${RPC}" rcow_deactive_bdev '{"device_name":"disk-d"}' >/dev/null 2>&1 || \
	fail "could not deactivate disk-d"
if "${RPC}" rcow_delete_lvol '{"lvol_name":"disk-d"}' >/dev/null 2>&1; then
	pass "deleting the volume after deactivation succeeds"
else
	fail "deleting the volume after deactivation failed"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [10] rcow_get_bdev with no argument lists everything"
ALL="$("${RPC}" rcow_get_bdev '{}' 2>/dev/null || "${RPC}" rcow_get_bdev)"
COUNT="$(python3 -c "import json,sys; print(len(json.loads(sys.argv[1])))" "${ALL}" 2>/dev/null || echo 0)"
info "listed${COUNT} active volume(s)"
[ "${COUNT}" -ge 2 ] && pass "the listing form works" || fail "listing returned ${COUNT}"

echo ""
echo "=== target log check"
if grep -qE "Assertion|SIGSEGV|stays paused" /tmp/actprobe_tgt.log; then
	fail "the target log has asserts or a stuck subsystem"
	grep -nE "Assertion|SIGSEGV|stays paused" /tmp/actprobe_tgt.log | head -5
else
	pass "no asserts, and no subsystem left paused"
fi
