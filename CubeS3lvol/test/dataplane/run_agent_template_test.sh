#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  The agent-template chain across two real s3lvol_tgt processes
#
#  === The scenario this records ===
#
#  An agent image is built once and cloned many times. The chain:
#
#    A (source node):
#      create lvol -> mkfs.ext4 -> write template data -> snapshot (the template)
#      -> delete the lvol (the snapshot now *is* the template)
#      -> clone from the template (an agent instance) -> mount, write agent data
#      -> snapshot the clone (the "agent + its state")
#      -> delete the clone (merged into its snapshot) -> export the snapshot
#    B (destination node):
#      a fresh s3lvol_tgt process -> import the export -> verify the data
#
#  What it proves beyond the single-process suites (run_export_test.sh runs
#  both sides inside one target):
#
#    - the module's state files (bstore, active registry) are per-process, so a
#      second target on the same node must not see or fight over the first's;
#    - two nvmf subsystems on one node coexist on different ports;
#    - deleting the clone merges its data into its snapshot, and the snapshot
#      then exports that merged state cleanly -- the agent instance is
#      transient, the image it produced is the asset;
#    - an import on an unrelated node reads the exact data (file-level md5,
#      not just a dd of one region) of the source-side snapshot.
#
#  === Why two real processes instead of two lvstores ===
#
#  run_export_test.sh and run_srcdel_test.sh run one target and two lvstore
#  prefixes; everything the module computes is shared in that process. The
#  cross-node deployment has two targets that only share the bucket: separate
#  state files, separate chunk maps, separate S3 clients, separate nvmf
#  transports. This suite is the deployment form, and it is the only one that
#  would notice, say, the second process silently using the first's bstore.
#
#  Requires: root, nvme-cli, e2fsprogs (mkfs.ext4), a bucket + credentials in
#  the environment. Leaves nothing behind on a clean run.
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS="${ROOT}/test/tools"
RPC_PY="${TOOLS}/s3lvol_rpc.py"
# bdev_aio_create is an SPDK rpc, not an s3lvol one.
SPDK_RPC_PY="${SPDK_RPC_PY:-${SPDK_ROOT:-${ROOT}/deps/spdk}/scripts/rpc.py}"

TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"

# --- Process A (source): the template builder ---
SOCK_A="/var/run/s3lvol_a.sock"
BSTORE_A="/var/tmp/bstore_agent_a.json"
ACTIVE_A="/var/tmp/active_agent_a.json"
WAL_A="/data/s3lvol_agent_a.img"
WAL_A_BDEV="agent_a_wal0"
LVS_A="agenta"
NQN_A="nqn.2026-08.io.spdk:agent-a"
PORT_A="4420"

# --- Process B (destination): the importer ---
SOCK_B="/var/run/s3lvol_b.sock"
BSTORE_B="/var/tmp/bstore_agent_b.json"
ACTIVE_B="/var/tmp/active_agent_b.json"
WAL_B="/data/s3lvol_agent_b.img"
WAL_B_BDEV="agent_b_wal0"
LVS_B="agentb"
NQN_B="nqn.2026-08.io.spdk:agent-b"
PORT_B="4421"

CAPACITY_GIB=4
LVOL_GIB=2
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

ENDPOINT=""
BUCKET=""
REGION="ap-nanjing"

PASS=0; FAIL=0
TGT_A_PID=""; TGT_B_PID=""
WORKDIR=""
CONNECTED_A=0; CONNECTED_B=0
LVS_A_CREATED=0; LVS_B_CREATED=0
LVS_A_WAS=0; LVS_B_WAS=0
MOUNTED_TMPL=0; MOUNTED_AGENT=0; MOUNTED_IMPORT=0
TEARDOWN_ANOMALY=0

TEMPLATE="tpl-001"
CLONE="agent-001"
AGENT_SNAP="agent-001-snap"
IMPORT_VOL="imp-001"

pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

usage()
{
	cat <<EOF
Usage: $0 -e <endpoint> -b <bucket> [-r <region>]

  -e   S3/COS endpoint host
  -b   bucket (a test bucket: a clean run deletes its own prefixes)
  -r   region (default: ${REGION})

Credentials from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
EOF
}

while getopts "e:b:r:h" opt; do
	case "${opt}" in
	e) ENDPOINT="${OPTARG}" ;;
	b) BUCKET="${OPTARG}" ;;
	r) REGION="${OPTARG}" ;;
	h) usage; exit 0 ;;
	*) usage; exit 1 ;;
	esac
done
[ -n "${ENDPOINT}" ] && [ -n "${BUCKET}" ] || { usage; exit 1; }
[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || {
	echo "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY must be set" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }

for tool in nvme dd mkfs.ext4 mountpoint md5sum python3; do
	command -v "${tool}" >/dev/null 2>&1 || {
		echo "${tool} is required" >&2; exit 1; }
done

WORKDIR="$(mktemp -d /tmp/s3lvol_agent.XXXXXX)"
MNT_TMPL="${WORKDIR}/mnt_tmpl"
MNT_AGENT="${WORKDIR}/mnt_agent"
MNT_IMPORT="${WORKDIR}/mnt_import"
mkdir -p "${MNT_TMPL}" "${MNT_AGENT}" "${MNT_IMPORT}"

rpc_a() { python3 "${RPC_PY}" --sock "${SOCK_A}" "$@"; }
rpc_b() { python3 "${RPC_PY}" --sock "${SOCK_B}" "$@"; }

# SPDK-native rpcs (nvmf_*, bdev_*) go through rpc.py, which takes -s.
spdk_a() { python3 "${SPDK_RPC_PY}" -s "${SOCK_A}" "$@"; }
spdk_b() { python3 "${SPDK_RPC_PY}" -s "${SOCK_B}" "$@"; }

# --- helpers --------------------------------------------------------------

jget() { python3 -c 'import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }

# Every file under a mount point with its md5, paths relative ("etc/tpl-1.conf"),
# in a stable order. Two different mount points therefore produce comparable
# manifests.
fs_manifest()
{
	local mnt="$1"
	( cd "${mnt}" && find . -type f -print0 | sort -z | xargs -0 md5sum ) \
		| sed 's#  \./#  #'
}

target_alive()
{
	local pid="$1"
	[ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null
}

check_target()
{
	local pid="$1" log="$2" where="$3"

	if ! target_alive "${pid}"; then
		fail "target died during ${where}"
		tail -25 "${log}" | sed 's/^/       /'
		return 1
	fi
	if grep -qE 'Assertion|SIGSEGV|panic:' "${log}" 2>/dev/null; then
		fail "target hit an assertion during ${where}"
		grep -nE 'Assertion|SIGSEGV|panic:' "${log}" | head -5 | sed 's/^/       /'
		return 1
	fi
	return 0
}

wait_sock()
{
	local sock="$1" log="$2"
	local _i
	for _i in $(seq 80); do
		[ -S "${sock}" ] && return 0
		sleep 0.25
	done
	tail -20 "${log}" | sed 's/^/       /'
	return 1
}

# Wait until a fresh /dev/nvmeXnY appears. Echoes the device, empty on timeout.
# The controller's admin queue only learns about a new namespace through AEN or
# a rescan; rescanning on every pass makes the wait robust on hosts where the
# AEN is slow to arrive.
wait_new_ns()
{
	local before="$1"
	local found=""
	local ctrl=""
	local _i

	ctrl="$(printf '%s\n' "${before}" | head -1 | sed 's/n[0-9]*$//')"
	for _i in $(seq 30); do
		[ -n "${ctrl}" ] && nvme ns-rescan "${ctrl}" >/dev/null 2>&1 || true
		found="$(comm -13 <(echo "${before}") \
				<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
		[ -n "${found}" ] && break
		sleep 0.5
	done
	printf '%s' "${found}"
}

# nsid of a bdev in a subsystem, via SPDK rpc on the given sock.
nsid_of_sock()
{
	local sock="$1" nqn="$2" bdev="$3"

	"${SPDK_RPC_PY}" -s "${sock}" nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
nqn, bdev = sys.argv[1], sys.argv[2]
for sub in json.load(sys.stdin):
    if sub.get("nqn") != nqn:
        continue
    for ns in sub.get("namespaces", []):
        if ns.get("bdev_name") == bdev:
            print(ns["nsid"])
            sys.exit(0)
' "${nqn}" "${bdev}"
}

nvme_settle()
{
	udevadm settle --timeout=5 >/dev/null 2>&1 || true
}

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

start_target()
{
	local which="$1"
	local sock bstore active wal wal_bdev pid

	case "${which}" in
	a) sock="${SOCK_A}"; bstore="${BSTORE_A}"; active="${ACTIVE_A}"
	   wal="${WAL_A}"; wal_bdev="${WAL_A_BDEV}" ;;
	b) sock="${SOCK_B}"; bstore="${BSTORE_B}"; active="${ACTIVE_B}"
	   wal="${WAL_B}"; wal_bdev="${WAL_B_BDEV}" ;;
	esac

	truncate -s "${WAL_FILE_MB}M" "${wal}"

	# Two targets cannot share cores: SPDK locks the assigned mask per process.
	# A gets cores 0-1, B gets 2-3. On a 2-core box B needs the same cores, so
	# prefer a disjoint pair but fall back to the same mask when the box is
	# small -- the two processes then share, which is legal, just not disjoint.
	local mask
	if [ "${which}" = "a" ]; then
		mask="${AGENT_CPUMASK_A:-0x3}"
	else
		mask="${AGENT_CPUMASK_B:-0xC}"
	fi

	S3LVOL_ACTIVE_FILE="${active}" S3LVOL_BSTORE_FILE="${bstore}" \
		"${TGT_BIN}" -m "${mask}" --no-huge -s 2048 -r "${sock}" \
		>"${WORKDIR}/target_${which}.log" 2>&1 &
	pid=$!

	if ! wait_sock "${sock}" "${WORKDIR}/target_${which}.log"; then
		fail "target ${which} did not come up"
		return 1
	fi
	sleep 1

	if [ "${which}" = "a" ]; then
		TGT_A_PID="${pid}"
	else
		TGT_B_PID="${pid}"
	fi

	"${SPDK_RPC_PY}" -s "${sock}" bdev_aio_create "${wal}" "${wal_bdev}" 4096 \
		>/dev/null 2>&1 || { fail "bdev_aio_create ${wal_bdev}"; return 1; }
	pass "target ${which} up (pid ${pid}); ${wal_bdev} attached"
	return 0
}

# --------------------------------------------------------------------------
cleanup()
{
	local rc=$?

	echo ""
	echo "=== cleanup"

	unmount_quietly "${MNT_IMPORT}"
	unmount_quietly "${MNT_AGENT}"
	unmount_quietly "${MNT_TMPL}"

	nvme disconnect -n "${NQN_A}" >/dev/null 2>&1
	nvme disconnect -n "${NQN_B}" >/dev/null 2>&1
	nvme_settle

	if target_alive "${TGT_A_PID}"; then
		if [ "${LVS_A_CREATED}" -eq 1 ]; then
			if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
				rpc_a rcow_delete_lvstore \
					"$(printf '{"lvs_name":"%s"}' "${LVS_A}")" >/dev/null 2>&1 \
					|| info "A delete_lvstore failed"
			else
				rpc_a rcow_unload_lvstore \
					"$(printf '{"lvs_name":"%s"}' "${LVS_A}")" >/dev/null 2>&1 || true
			fi
			LVS_A_CREATED=0
		fi
		kill -TERM "${TGT_A_PID}" 2>/dev/null
		sleep 1
	fi

	if target_alive "${TGT_B_PID}"; then
		if [ "${LVS_B_CREATED}" -eq 1 ]; then
			if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
				rpc_b rcow_delete_lvstore \
					"$(printf '{"lvs_name":"%s"}' "${LVS_B}")" >/dev/null 2>&1 \
					|| info "B delete_lvstore failed"
			else
				rpc_b rcow_unload_lvstore \
					"$(printf '{"lvs_name":"%s"}' "${LVS_B}")" >/dev/null 2>&1 || true
			fi
			LVS_B_CREATED=0
		fi
		kill -TERM "${TGT_B_PID}" 2>/dev/null
		sleep 1
	fi

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		for prefix in "${LVS_A}/" "${LVS_B}/" "exports/"; do
			python3 "${TOOLS}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
				-r "${REGION}" -p "${prefix}" >/dev/null 2>&1 || true
		done
		rm -f "${WAL_A}" "${WAL_B}" "${BSTORE_A}" "${BSTORE_B}" \
			"${ACTIVE_A}" "${ACTIVE_B}" "${SOCK_A}" "${SOCK_B}"
		rm -rf "${WORKDIR}"
	else
		info "state kept: logs in ${WORKDIR}, WAL ${WAL_A}/${WAL_B}"
	fi

	echo ""
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# --------------------------------------------------------------------------
echo "=== s3lvol agent-template cross-node test ==="
echo "    endpoint : ${ENDPOINT}"
echo "    bucket   : ${BUCKET}"
echo "    workdir  : ${WORKDIR}"
echo ""

# ==========================================================================
# [1] two targets
# ==========================================================================
echo "[1] starting two independent s3lvol_tgt processes"

if pgrep -f "${TGT_BIN}" >/dev/null 2>&1; then
	fail "an s3lvol_tgt is already running; stop it first"
	exit 1
fi

rm -f "${SOCK_A}" "${SOCK_B}" "${BSTORE_A}" "${BSTORE_B}" \
	"${ACTIVE_A}" "${ACTIVE_B}" "${WAL_A}" "${WAL_B}"

if ! start_target a; then exit 1; fi
if ! start_target b; then exit 1; fi
check_target "${TGT_A_PID}" "${WORKDIR}/target_a.log" "step 1 (A)" || exit 1
check_target "${TGT_B_PID}" "${WORKDIR}/target_b.log" "step 1 (B)" || exit 1

# ==========================================================================
# [2] A: lvstore + template volume + filesystem + template data
# ==========================================================================
echo ""
echo "[2] A: template volume with a filesystem"

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
rpc_a rcow_add_cos_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
	"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" >/dev/null 2>&1 \
	|| { fail "A add_cos_config"; exit 1; }
rpc_a rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
	"${LVS_A}" "${BUCKET}" "${CAPACITY_GIB}" "${WAL_A_BDEV}" \
	"${JOURNAL_MB}" "${WAL_MB}")" >/dev/null 2>&1 \
	|| { fail "A create_lvstore"; exit 1; }
LVS_A_CREATED=1; LVS_A_WAS=1
pass "A lvstore ${LVS_A} created"

rpc_a rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
	"${TEMPLATE}" "${LVOL_GIB}")" >/dev/null 2>&1 \
	|| { fail "A create_lvol ${TEMPLATE}"; exit 1; }
pass "A lvol ${TEMPLATE} created"

spdk_a nvmf_create_transport -t TCP >/dev/null 2>&1 || true
spdk_a nvmf_create_subsystem "${NQN_A}" -a \
	-s AAAA0000000000001 >/dev/null 2>&1 \
	|| { fail "A create_subsystem"; exit 1; }
spdk_a nvmf_subsystem_add_ns "${NQN_A}" \
	"${LVS_A}/${TEMPLATE}" >/dev/null 2>&1 \
	|| { fail "A add_ns ${TEMPLATE}"; exit 1; }
spdk_a nvmf_subsystem_add_listener "${NQN_A}" \
	-t tcp -a 127.0.0.1 -s "${PORT_A}" >/dev/null 2>&1 \
	|| { fail "A add_listener"; exit 1; }

if ! nvme connect -t tcp -a 127.0.0.1 -s "${PORT_A}" -n "${NQN_A}" \
		>"${WORKDIR}/connect_a.log" 2>&1; then
	fail "A nvme connect"
	sed 's/^/       /' "${WORKDIR}/connect_a.log"
	exit 1
fi
CONNECTED_A=1

DEV_A="$(wait_new_ns "$(ls /dev/nvme*n* 2>/dev/null | sort || true)")"
if [ -z "${DEV_A}" ]; then
	fail "A: no namespace appeared"; exit 1
fi
pass "A template device ${DEV_A}"

mkfs.ext4 -F "${DEV_A}" >"${WORKDIR}/mkfs_tmpl.log" 2>&1 || {
	fail "mkfs.ext4 (template)"
	sed 's/^/       /' "${WORKDIR}/mkfs_tmpl.log"
	exit 1; }
pass "mkfs.ext4 on the template volume"

mount "${DEV_A}" "${MNT_TMPL}" || { fail "mount template"; exit 1; }
MOUNTED_TMPL=1

# Template data: a few files with known contents, at a level of detail that a
# dd of one region cannot express.
mkdir -p "${MNT_TMPL}/etc" "${MNT_TMPL}/opt/agent"
for i in 1 2 3; do
	printf 'template-file-%d %08d\n' "${i}" "${RANDOM}${RANDOM}" \
		> "${MNT_TMPL}/etc/tpl-${i}.conf"
done
dd if=/dev/urandom of="${MNT_TMPL}/opt/agent/blob.bin" bs=1M count=8 status=none
TMPL_MANIFEST="$(fs_manifest "${MNT_TMPL}")"
sync
umount "${MNT_TMPL}" || true
MOUNTED_TMPL=0
pass "template data written (manifest: $(echo "${TMPL_MANIFEST}" | wc -l) entries)"

# The template snapshot; the source volume goes away right after.
rpc_a rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${TEMPLATE}" "tpl-snap-${TEMPLATE}")" >/dev/null 2>&1 \
	|| { fail "A create_snapshot (template)"; exit 1; }
pass "template snapshot created"

spdk_a nvmf_subsystem_remove_ns "${NQN_A}" \
	"$(nsid_of_sock "${SOCK_A}" "${NQN_A}" "${LVS_A}/${TEMPLATE}")" >/dev/null 2>&1 || true

if ! rpc_a rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${TEMPLATE}")" \
		>/dev/null 2>&1; then
	fail "A delete template lvol (the snapshot should take over)"
	exit 1
fi
pass "template lvol deleted; snapshot 'tpl-snap-${TEMPLATE}' is the template"

# ==========================================================================
# [3] A: clone the template, write agent data, snapshot, export, delete clone
# ==========================================================================
echo ""
echo "[3] A: clone, agent data, snapshot, export"

if ! rpc_a rcow_create_clone "$(printf '{"snapshot_name":"%s","clone_name":"%s"}' \
	"tpl-snap-${TEMPLATE}" "${CLONE}")" >/dev/null 2>&1; then
	fail "A create_clone ${CLONE} from template"
	exit 1
fi
pass "A clone ${CLONE} created from the template"

# The template volume was deleted while connected, which took its namespace
# away underneath the host. Rather than rely on the AEN for the new namespace
# arriving on a controller whose state is stale, disconnect and reconnect: the
# fresh connect gets the subsystem exactly as it is now, clone included.
nvme disconnect -n "${NQN_A}" >/dev/null 2>&1
CONNECTED_A=0
nvme_settle

# The old namespace's device has to be gone before the reconnect, or the clone's
# device below would be ambiguous against a stale leftover.
for _i in $(seq 20); do
	[ -z "$(ls /dev/nvme*n* 2>/dev/null)" ] && break
	sleep 0.5
done

# The baseline for the clone lookup below, taken before anything new appears.
CLONE_BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"

spdk_a nvmf_subsystem_add_ns "${NQN_A}" \
	"${LVS_A}/${CLONE}" >/dev/null 2>&1 || { fail "A add_ns ${CLONE}"; exit 1; }

if ! nvme connect -t tcp -a 127.0.0.1 -s "${PORT_A}" -n "${NQN_A}" \
		>"${WORKDIR}/connect_a2.log" 2>&1; then
	fail "A reconnect (clone)"
	sed 's/^/       /' "${WORKDIR}/connect_a2.log"
	exit 1
fi
CONNECTED_A=1

# Clone device is whichever namespace the fresh connect produced; there is
# exactly one (the template's is gone), and it is the *new* device -- matched by
# difference against the pre-connect baseline, never by "first nvme device",
# which on a host with other nvme drives would be the wrong one.
DEV_A2="$(wait_new_ns "${CLONE_BEFORE}")"
if [ -z "${DEV_A2}" ]; then
	fail "A: no namespace for the clone"
	info "host devices: $(ls /dev/nvme*n* 2>/dev/null | tr '\n' ' ')"
	info "target A subsystems:"
	spdk_a nvmf_get_subsystems 2>/dev/null | python3 -m json.tool | head -30
	exit 1
fi
pass "A clone device ${DEV_A2}"

mount "${DEV_A2}" "${MNT_AGENT}" || { fail "mount clone"; exit 1; }
MOUNTED_AGENT=1

# Agent data: extends the template rather than replacing it.
mkdir -p "${MNT_AGENT}/var"
printf 'agent-%s config line\n' "${CLONE}" > "${MNT_AGENT}/etc/agent.conf"
dd if=/dev/urandom of="${MNT_AGENT}/var/agent-state.bin" bs=1M count=4 status=none
AGENT_MANIFEST="$(fs_manifest "${MNT_AGENT}")"
sync
umount "${MNT_AGENT}" || true
MOUNTED_AGENT=0
pass "agent data written on the clone"

rpc_a rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${CLONE}" "${AGENT_SNAP}")" >/dev/null 2>&1 \
	|| { fail "A create_snapshot (agent)"; exit 1; }
pass "agent snapshot ${AGENT_SNAP} created"

# Delete the clone first -- the template lifecycle in this test is "the agent
# instance is transient, the snapshot it produced is the asset". Deleting the
# clone merges it into its one clone (agent-001-snap), which then owns the
# data; exporting after the merge references exactly those objects. Order A
# (export then delete) would equally work -- export pins the snapshot, not the
# clone -- but B is the sequence an agent platform actually uses: tear down
# the instance, keep the image. The export afterwards is of the final state.
nvme disconnect -n "${NQN_A}" >/dev/null 2>&1
CONNECTED_A=0
nvme_settle

if ! rpc_a rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${CLONE}")" \
		>/dev/null 2>&1; then
	fail "A delete the clone"
	exit 1
fi
pass "clone deleted (merged into ${AGENT_SNAP})"
check_target "${TGT_A_PID}" "${WORKDIR}/target_a.log" "step 3, after the clone delete" \
	|| exit 1

# Export the snapshot that now owns the merged data.
EXP_UUID="$(rpc_a rcow_export_snapshot "$(printf '{"snapshot_name":"%s"}' "${AGENT_SNAP}")" \
	2>/dev/null | tr -d ' \t\r\n')"
if [ -z "${EXP_UUID}" ]; then fail "A export_snapshot"; exit 1; fi
pass "export started: ${EXP_UUID}"

st=""
for _i in $(seq 60); do
	st="$(rpc_a rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP_UUID}")" \
		2>/dev/null || true)"
	[ "$(jget "${st}" export_status 2>/dev/null)" = "DONE" ] && break
	sleep 0.5
done
[ "$(jget "${st}" export_status 2>/dev/null)" = "DONE" ] \
	|| { fail "export did not reach DONE"; exit 1; }
pass "export DONE"
check_target "${TGT_A_PID}" "${WORKDIR}/target_a.log" "step 3, after the export" \
	|| exit 1

# The template manifest plus the agent manifest, with paths made relative to
# one root, is what B must reproduce. Union, sorted, is the comparable form.
EXPECTED="$( { echo "${TMPL_MANIFEST}"; echo "${AGENT_MANIFEST}"; } | sort -u )"

# ==========================================================================
# [4] B: import the export, mount, compare files
# ==========================================================================
echo ""
echo "[4] B: import and verify the data"

rpc_b rcow_add_cos_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
	"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" >/dev/null 2>&1 \
	|| { fail "B add_cos_config"; exit 1; }
rpc_b rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
	"${LVS_B}" "${BUCKET}" "${CAPACITY_GIB}" "${WAL_B_BDEV}" \
	"${JOURNAL_MB}" "${WAL_MB}")" >/dev/null 2>&1 \
	|| { fail "B create_lvstore"; exit 1; }
LVS_B_CREATED=1; LVS_B_WAS=1
pass "B lvstore ${LVS_B} created"

if ! rpc_b rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","decouple":true}' \
		"${IMPORT_VOL}" "${EXP_UUID}")" >/dev/null 2>&1; then
	fail "B import_lvol"
	exit 1
fi
pass "B imported ${IMPORT_VOL}"

# Wait out the decouple so the mount reads local data, not read-through.
# The empty-array answer is the completion signal; anything else -- a non-empty
# list, or an RPC that keeps failing -- must be a failure, not a silent pass.
DECOUPLE_DONE=0
for _i in $(seq 120); do
	n="$(rpc_b rcow_get_decouple '{}' 2>/dev/null || echo '[]')"
	if [ "$(printf '%s' "${n}" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))' 2>/dev/null)" = "0" ]; then
		DECOUPLE_DONE=1
		break
	fi
	sleep 1
done
if [ "${DECOUPLE_DONE}" -eq 1 ]; then
	pass "B decouple finished"
else
	fail "B decouple did not finish in 120s"
	exit 1
fi

spdk_b nvmf_create_transport -t TCP >/dev/null 2>&1 || true
spdk_b nvmf_create_subsystem "${NQN_B}" -a \
	-s BBBB0000000000002 >/dev/null 2>&1 \
	|| { fail "B create_subsystem"; exit 1; }
spdk_b nvmf_subsystem_add_ns "${NQN_B}" \
	"${LVS_B}/${IMPORT_VOL}" >/dev/null 2>&1 \
	|| { fail "B add_ns import"; exit 1; }
spdk_b nvmf_subsystem_add_listener "${NQN_B}" \
	-t tcp -a 127.0.0.1 -s "${PORT_B}" >/dev/null 2>&1 \
	|| { fail "B add_listener"; exit 1; }

if ! nvme connect -t tcp -a 127.0.0.1 -s "${PORT_B}" -n "${NQN_B}" \
		>"${WORKDIR}/connect_b.log" 2>&1; then
	fail "B nvme connect"
	sed 's/^/       /' "${WORKDIR}/connect_b.log"
	exit 1
fi
CONNECTED_B=1

DEV_B="$(wait_new_ns "$(ls /dev/nvme*n* 2>/dev/null | sort || true)")"
if [ -z "${DEV_B}" ]; then fail "B: no namespace appeared"; exit 1; fi
pass "B import device ${DEV_B}"

mount "${DEV_B}" "${MNT_IMPORT}" || { fail "mount import"; exit 1; }
MOUNTED_IMPORT=1

IMPORTED_MANIFEST="$(fs_manifest "${MNT_IMPORT}")"
if [ "$(echo "${IMPORTED_MANIFEST}" | sort -u)" = "${EXPECTED}" ]; then
	pass "the imported data matches the source snapshot file-for-file ($(echo "${IMPORTED_MANIFEST}" | wc -l) entries)"
else
	fail "imported data differs from the source snapshot"
	info "--- expected ---"; echo "${EXPECTED}" | sed 's/^/       /'
	info "--- imported ---"; echo "${IMPORTED_MANIFEST}" | sort -u | sed 's/^/       /'
fi
check_target "${TGT_B_PID}" "${WORKDIR}/target_b.log" "step 4" || exit 1

# ==========================================================================
# [5] the snapshot exported after the clone was merged into it
# ==========================================================================
echo ""
echo "[5] the snapshot exported cleanly after the clone's data was merged"

st="$(rpc_a rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP_UUID}")" \
	2>/dev/null || true)"
if [ "$(jget "${st}" export_status 2>/dev/null)" = "DONE" ]; then
	pass "the agent snapshot exported DONE with the clone already gone"
else
	fail "the agent snapshot did not export after the clone was deleted"
fi
