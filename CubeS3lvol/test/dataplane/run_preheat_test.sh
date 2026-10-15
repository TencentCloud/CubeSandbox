#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Import preheat on the real dataplane.
#
#  Five volumes are exported from one lvstore. Four are imported together, with
#  decouple left off, onto a second lvstore. That is the path that preheats
#  each hidden template into the destination's shared object cache. The
#  imports are one burst. At least two preheat walks must have started before
#  the first of them finishes, which is the walks running together rather than
#  one after another.
#
#  The fifth export is imported eight times in one burst. Those imports must
#  share one hidden template and start exactly one preheat walk.
#
#  The target is then stopped and started again, and the destination lvstore is
#  attached rather than created. The cache index does not survive the process,
#  so each hidden template must start one new preheat and populate its objects
#  again. A read after that must hit the new cache.
#
#  Only after every present object has been populated does the test read. Each
#  clone is read as eight direct streams at once, and all four clones are
#  started together. The bytes must match what was exported. The reads must
#  add cache hits and must not add cache misses or further populates: a miss
#  would have gone back to S3.
#
#  Usage:
#    sudo -E ./test/dataplane/run_preheat_test.sh
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

N=4
EXPORTS=5
SAME_N=8
SAME_IDX=4
FILL_MB=32
READS=8
SRC_LVS=phsrc
DST_LVS=phdst
SRC_WAL=/tmp/preheat_src.img
DST_WAL=/tmp/preheat_dst.img
RPC_SOCK=/tmp/preheat.sock
TGT_LOG=/tmp/preheat_target.log
NQN="nqn.2026-08.io.spdk:preheat"
PORT="4493"

PASS=0
FAIL=0
TGT_PID=""
CONNECTED=0
WORKDIR=""
BUCKET=""
EP=""
RG=""
UUIDS=()

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
		for p in "${SRC_LVS}" "${DST_LVS}"; do
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "${p}/" >/dev/null 2>&1 || true
		done
		for u in "${UUIDS[@]}"; do
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "exports/${u}" >/dev/null 2>&1 || true
		done
		rm -f "${SRC_WAL}" "${DST_WAL}"
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

lvs_stat()
{
	local lvs="$1" field="$2"

	rpc rcow_get_lvstores 2>/dev/null | python3 -c '
import json, sys
lvs, field = sys.argv[1], sys.argv[2]
try:
    rows = json.load(sys.stdin)
except Exception:
    print(-1)
    sys.exit(0)
for row in rows:
    if row.get("lvs_name") == lvs:
        print(row.get("write_path", {}).get(field, -1))
        sys.exit(0)
print(-1)
' "${lvs}" "${field}"
}

wait_export_done()
{
	local uuid="$1"
	local deadline=$((SECONDS + 180))
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

wait_populated()
{
	local want="$1"
	local deadline=$((SECONDS + ${2:-120}))
	local n failed dropped resident bytes

	while :; do
		n="$(lvs_stat "${DST_LVS}" cache_object_populates)"
		failed="$(lvs_stat "${DST_LVS}" cache_object_populates_failed)"
		if [ "${n}" -ge "${want}" ] 2>/dev/null && [ "${failed}" = "0" ]; then
			return 0
		fi
		if [ "${SECONDS}" -ge "${deadline}" ]; then
			dropped="$(lvs_stat "${DST_LVS}" cache_object_populates_dropped)"
			resident="$(lvs_stat "${DST_LVS}" cache_object_slots_resident)"
			bytes="$(lvs_stat "${DST_LVS}" cache_object_bytes_populated)"
			fail "preheat populated ${n} objects (${failed} failed, ${dropped} dropped, ${resident} resident, ${bytes} bytes), want ${want}"
			return 1
		fi
		sleep 0.2
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

ctrl_base()
{
	local ctrl

	ctrl="$(ctrl_of_nqn)" || return 1
	basename "${ctrl}"
}

nsid_number()
{
	local raw="$1"

	raw="${raw//\"/}"
	raw="${raw//[[:space:]]/}"
	printf '%d' "${raw}" 2>/dev/null
}

devs_of_ctrl()
{
	local base

	base="$(ctrl_base)" || return 1
	compgen -G "/dev/${base}n*" || true
}

dev_of_nsid()
{
	local want="$1" base ns id want_n dev

	base="$(ctrl_base)" || return 1
	want_n="$(nsid_number "${want}")" || want_n=""
	for ns in /sys/class/nvme/"${base}"/nvme*n* \
		/sys/class/nvme/"${base}"n* \
		/sys/block/"${base}"n*; do
		[ -e "${ns}" ] || continue
		dev="/dev/$(basename "${ns}")"
		[ -b "${dev}" ] || continue
		if [ -n "${want_n}" ] && [ -r "${ns}/nsid" ]; then
			id="$(nsid_number "$(cat "${ns}/nsid" 2>/dev/null)")" || id=""
			if [ "${id}" = "${want_n}" ]; then
				printf '%s' "${dev}"
				return 0
			fi
			continue
		fi
		printf '%s' "${dev}"
		return 0
	done
	dev="$(devs_of_ctrl | head -1)"
	if [ -n "${dev}" ] && [ -b "${dev}" ] && [ "$(devs_of_ctrl | wc -l)" = "1" ]; then
		printf '%s' "${dev}"
		return 0
	fi
	return 1
}

rescan_ctrl()
{
	local ctrl

	ctrl="$(ctrl_of_nqn)" || return 1
	nvme ns-rescan "${ctrl}" >/dev/null 2>&1 || true
	udevadm settle --timeout=2 >/dev/null 2>&1 || true
}

wait_dev_nsid()
{
	local nsid="$1"
	local deadline=$((SECONDS + 40))
	local before now dev

	before="$(devs_of_ctrl | sort)"
	while [ "${SECONDS}" -lt "${deadline}" ]; do
		rescan_ctrl || true
		if dev="$(dev_of_nsid "${nsid}")"; then
			printf '%s' "${dev}"
			return 0
		fi
		now="$(devs_of_ctrl | sort)"
		dev="$(comm -13 <(printf '%s\n' "${before}") <(printf '%s\n' "${now}") | head -1)"
		if [ -n "${dev}" ] && [ -b "${dev}" ]; then
			printf '%s' "${dev}"
			return 0
		fi
		sleep 0.2
	done
	return 1
}

wait_dev_gone()
{
	local dev="$1"
	local deadline=$((SECONDS + 20))

	while [ "${SECONDS}" -lt "${deadline}" ]; do
		rescan_ctrl || true
		if [ -n "${dev}" ] && [ -b "${dev}" ]; then
			sleep 0.2
			continue
		fi
		return 0
	done
	return 1
}

expose()
{
	local err="${WORKDIR}/expose.err"
	local out

	out="$(raw nvmf_subsystem_add_ns \
		"$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' "${NQN}" "$1")" \
		2>"${err}")" || {
		echo "  ---- expose $1: $(tr '\n' ' ' < "${err}" | head -c 240)" >&2
		return 1
	}
	printf '%s' "${out}" | tr -d '[:space:]"'
}

unexpose()
{
	local nsid="$1" dev="${2:-}"

	if [ -n "${nsid}" ]; then
		raw nvmf_subsystem_remove_ns \
			"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "${nsid}")" \
			>/dev/null 2>"${WORKDIR}/unexpose.err" || true
		wait_dev_gone "${dev}" || true
	fi
	return 0
}

stop_target()
{
	if [ "${CONNECTED}" = "1" ]; then
		nvme disconnect -n "${NQN}" >/dev/null 2>&1 || true
		CONNECTED=0
	fi
	if [ -n "${TGT_PID}" ]; then
		kill -TERM "${TGT_PID}" 2>/dev/null || true
		local _
		for _ in $(seq 50); do
			kill -0 "${TGT_PID}" 2>/dev/null || break
			sleep 0.1
		done
		if kill -0 "${TGT_PID}" 2>/dev/null; then
			kill -KILL "${TGT_PID}" 2>/dev/null || true
		fi
		wait "${TGT_PID}" 2>/dev/null || true
		TGT_PID=""
	fi
	rm -f "${RPC_SOCK}"
}

# The WAL files and the S3 objects stay. This only brings the process back.
start_target()
{
	local _
	local connected=0

	"${TGT_BIN}" -m "${S3LVOL_TGT_CPUMASK:-0x3}" --no-huge -s 2048 -r "${RPC_SOCK}" \
		>>"${TGT_LOG}" 2>&1 &
	TGT_PID=$!
	for _ in $(seq 80); do
		[ -S "${RPC_SOCK}" ] && break
		sleep 0.25
	done
	[ -S "${RPC_SOCK}" ] || die "target failed to start"
	sleep 1
	kill -0 "${TGT_PID}" 2>/dev/null || die "target exited during startup"

	rpc rcow_add_s3_config "${S3_JSON}" >/dev/null || die "rcow_add_s3_config failed"
	raw bdev_aio_create "$(printf '{"filename":"%s","name":"ph_src_wal0","block_size":4096}' \
		"${SRC_WAL}")" >/dev/null || die "source wal bdev"
	raw bdev_aio_create "$(printf '{"filename":"%s","name":"ph_dst_wal0","block_size":4096}' \
		"${DST_WAL}")" >/dev/null || die "dest wal bdev"
	raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1 || true
	raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PREHEAT00000001"}' \
		"${NQN}")" >/dev/null || die "nvmf subsystem"
	raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
		"${NQN}" "${PORT}")" >/dev/null || die "nvmf listener"
	for _ in $(seq 40); do
		if nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1; then
			connected=1
			break
		fi
		sleep 0.25
	done
	[ "${connected}" = "1" ] || die "nvme connect"
	CONNECTED=1
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

WORKDIR="$(mktemp -d /tmp/preheat.XXXXXX)"
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
truncate -s 512M "${SRC_WAL}"
truncate -s 512M "${DST_WAL}"

for p in "${SRC_LVS}" "${DST_LVS}"; do
	python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" -p "${p}/" \
		>/dev/null 2>&1 || true
done

info "starting target"
"${TGT_BIN}" -m "${S3LVOL_TGT_CPUMASK:-0x3}" --no-huge -s 2048 -r "${RPC_SOCK}" \
	>"${TGT_LOG}" 2>&1 &
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

raw bdev_aio_create "$(printf '{"filename":"%s","name":"ph_src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null || die "source wal bdev"
raw bdev_aio_create "$(printf '{"filename":"%s","name":"ph_dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null || die "dest wal bdev"

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":8,"wal_bdev":"ph_src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BUCKET}")" >/dev/null || die "create ${SRC_LVS}"

raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1 || true
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PREHEAT00000001"}' \
	"${NQN}")" >/dev/null || die "nvmf subsystem"
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null || die "nvmf listener"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1 \
	|| die "nvme connect"
CONNECTED=1
pass "target and source lvstore are up"

# --------------------------------------------------------------------------
echo ""
echo "=== [1] export ${EXPORTS} volumes of ${FILL_MB} MiB"

for i in $(seq 0 $((EXPORTS - 1))); do
	name="v${i}"
	rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":1}' "${name}")" \
		>/dev/null || die "create ${name}"
	nsid="$(expose "${SRC_LVS}/${name}")" || nsid=""
	[ -n "${nsid}" ] || die "expose ${name}"
	dev="$(wait_dev_nsid "${nsid}")" || die "no device for ${name}"
	dd if=/dev/urandom of="${WORKDIR}/pat.${i}" bs=1M count="${FILL_MB}" status=none
	dd if="${WORKDIR}/pat.${i}" of="${dev}" bs=1M oflag=direct conv=fsync status=none \
		|| die "write ${name}"
	unexpose "${nsid}" "${dev}"
done
pass "wrote ${EXPORTS} source patterns"

rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "flush source"
for i in $(seq 0 $((EXPORTS - 1))); do
	rpc rcow_create_snapshot "$(printf '{"lvol_name":"v%s","snapshot_name":"snap%s"}' \
		"${i}" "${i}")" >/dev/null || die "snapshot v${i}"
done
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "flush snapshots"
for i in $(seq 0 $((EXPORTS - 1))); do
	uuid="$(rpc rcow_export_snapshot "$(printf '{"snapshot_name":"snap%s"}' "${i}")" \
		| tr -d '"[:space:]')"
	[ -n "${uuid}" ] || die "export snap${i} failed"
	UUIDS+=("${uuid}")
	echo "${uuid}" >"${WORKDIR}/uuid.${i}"
	wait_export_done "${uuid}" || exit 1
done
pass "exported ${EXPORTS} snapshots"

rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "unload source"
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":8,"wal_bdev":"ph_dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BUCKET}")" >/dev/null || die "create ${DST_LVS}"
pass "destination lvstore is up"

# --------------------------------------------------------------------------
echo ""
echo "=== [2] ${N} imports preheat together"

mark="$(wc -l < "${TGT_LOG}")"
dir="${WORKDIR}/burst"
mkdir -p "${dir}"
pids=()
for i in $(seq 0 $((N - 1))); do
	uuid="$(cat "${WORKDIR}/uuid.${i}")"
	(
		if python3 "${RPC_PY}" --sock "${RPC_SOCK}" rcow_import_lvol \
			"$(printf '{"lvol_name":"c%s","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
				"${i}" "${uuid}" "${DST_LVS}")" \
			>"${dir}/c${i}.out" 2>"${dir}/c${i}.err"; then
			echo 0 >"${dir}/c${i}.rc"
		else
			echo 1 >"${dir}/c${i}.rc"
		fi
	) &
	pids+=("$!")
done
for pid in "${pids[@]}"; do
	wait "${pid}" || true
done

ok=0
for i in $(seq 0 $((N - 1))); do
	if [ "$(cat "${dir}/c${i}.rc")" = "0" ]; then
		ok=$((ok + 1))
		mode="$(import_mode "$(cat "${dir}/c${i}.out")")"
		if [ "${mode}" = "esnap" ]; then
			pass "import c${i} is esnap"
		else
			fail "import c${i} is ${mode}, want esnap"
		fi
	else
		fail "import c${i} failed: $(tr '\n' ' ' < "${dir}/c${i}.err" | head -c 200)"
	fi
done
[ "${ok}" = "${N}" ] || die "not every import returned"

# Starts that landed before the first walk finished. Two or more means those
# templates were preheating at the same time.
read -r started finished early <<<"$(tail -n +"$((mark + 1))" "${TGT_LOG}" | awk '
	/template preheat started/ { started++; if (finished == 0) early++ }
	/template preheat walk finished/ { finished++ }
	END { print started+0, finished+0, early+0 }
')"
info "preheat walks: ${started} started, ${finished} finished, ${early} started before the first finish"
if [ "${started}" = "${N}" ]; then
	pass "each import started one template preheat"
else
	fail "preheat start count is ${started}, want ${N}"
fi
if [ "${early}" -ge 2 ]; then
	pass "at least two preheats were in progress together"
else
	fail "preheats did not overlap (only ${early} started before the first finished)"
fi

wait_populated $((N * FILL_MB)) || exit 1
pass "all ${N} templates are populated in the object cache"

# --------------------------------------------------------------------------
echo ""
echo "=== [3] concurrent cold reads after preheat"

devs=()
nsids=()
for i in $(seq 0 $((N - 1))); do
	nsid="$(expose "${DST_LVS}/c${i}")" || nsid=""
	[ -n "${nsid}" ] || die "expose c${i}"
	dev="$(wait_dev_nsid "${nsid}")" || die "no device for c${i}"
	devs+=("${dev}")
	nsids+=("${nsid}")
done

hits0="$(lvs_stat "${DST_LVS}" cache_object_hits)"
miss0="$(lvs_stat "${DST_LVS}" cache_object_misses)"
pop0="$(lvs_stat "${DST_LVS}" cache_object_populates)"
read_pids=()
for i in $(seq 0 $((N - 1))); do
	for r in $(seq 0 $((READS - 1))); do
		dd if="${devs[$i]}" of="${WORKDIR}/got.${i}.${r}" bs=1M count=4 \
			skip="$((r * 4))" iflag=direct status=none \
			>"${WORKDIR}/dd.${i}.${r}.err" 2>&1 &
		read_pids+=("$!")
	done
done
read_bad=0
for pid in "${read_pids[@]}"; do
	wait "${pid}" || read_bad=1
done
if [ "${read_bad}" = "0" ]; then
	pass "concurrent cold reads completed"
else
	fail "a cold read failed"
fi

for i in $(seq 0 $((N - 1))); do
	for r in $(seq 0 $((READS - 1))); do
		want="$(dd if="${WORKDIR}/pat.${i}" bs=1M count=4 skip="$((r * 4))" status=none \
			| md5sum | cut -d' ' -f1)"
		got="$(md5sum "${WORKDIR}/got.${i}.${r}" 2>/dev/null | cut -d' ' -f1)"
		if [ "${got}" = "${want}" ]; then
			pass "c${i} slice ${r} matches the export"
		else
			fail "c${i} slice ${r} mismatch"
		fi
	done
done

hits1="$(lvs_stat "${DST_LVS}" cache_object_hits)"
miss1="$(lvs_stat "${DST_LVS}" cache_object_misses)"
pop1="$(lvs_stat "${DST_LVS}" cache_object_populates)"
if [ "${hits1}" -gt "${hits0}" ] 2>/dev/null; then
	pass "cold reads hit the object cache (${hits0} -> ${hits1})"
else
	fail "cold reads did not hit the object cache (${hits0} -> ${hits1})"
fi
if [ "${miss1}" = "${miss0}" ]; then
	pass "cold reads added no object-cache misses"
else
	fail "cold reads added object-cache misses (${miss0} -> ${miss1})"
fi
if [ "${pop1}" = "${pop0}" ]; then
	pass "cold reads did not download anything new"
else
	fail "cold reads populated more objects (${pop0} -> ${pop1})"
fi

for i in $(seq 0 $((N - 1))); do
	unexpose "${nsids[$i]}" "${devs[$i]}"
done

# The walk-finished lines can lag the last disk fill by a poll. They must all
# be present once the reads, which waited for those fills, have returned.
read -r finished_now <<<"$(awk '/template preheat walk finished/ { n++ } END { print n+0 }' \
	"${TGT_LOG}")"
if [ "${finished_now}" -ge "${N}" ]; then
	pass "every template preheat walk finished"
else
	fail "only ${finished_now} preheat walks finished, want ${N}"
fi

# --------------------------------------------------------------------------
echo ""
echo "=== [4] import one export ${SAME_N} times"

mark="$(wc -l < "${TGT_LOG}")"
pop_base="$(lvs_stat "${DST_LVS}" cache_object_populates)"
same_uuid="$(cat "${WORKDIR}/uuid.${SAME_IDX}")"
same_dir="${WORKDIR}/same"
mkdir -p "${same_dir}"
pids=()
for i in $(seq 0 $((SAME_N - 1))); do
	(
		if python3 "${RPC_PY}" --sock "${RPC_SOCK}" rcow_import_lvol \
			"$(printf '{"lvol_name":"same%s","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
				"${i}" "${same_uuid}" "${DST_LVS}")" \
			>"${same_dir}/same${i}.out" 2>"${same_dir}/same${i}.err"; then
			echo 0 >"${same_dir}/same${i}.rc"
		else
			echo 1 >"${same_dir}/same${i}.rc"
		fi
	) &
	pids+=("$!")
done
for pid in "${pids[@]}"; do
	wait "${pid}" || true
done

ok=0
for i in $(seq 0 $((SAME_N - 1))); do
	if [ "$(cat "${same_dir}/same${i}.rc")" = "0" ]; then
		ok=$((ok + 1))
		mode="$(import_mode "$(cat "${same_dir}/same${i}.out")")"
		if [ "${mode}" = "esnap" ]; then
			pass "repeated import same${i} is esnap"
		else
			fail "repeated import same${i} is ${mode}, want esnap"
		fi
	else
		fail "repeated import same${i} failed: $(tr '\n' ' ' \
			< "${same_dir}/same${i}.err" | head -c 200)"
	fi
done
[ "${ok}" = "${SAME_N}" ] || die "not every repeated import returned"

same_starts="$(tail -n +"$((mark + 1))" "${TGT_LOG}" \
	| grep -c "Export ${same_uuid} template preheat started" || true)"
if [ "${same_starts}" = "1" ]; then
	pass "${SAME_N} imports started one shared-template preheat"
else
	fail "${SAME_N} imports started ${same_starts} preheats, want 1"
fi

wait_populated $((pop_base + FILL_MB)) || exit 1
pass "the shared template is populated once"

same_devs=()
same_nsids=()
for i in $(seq 0 $((SAME_N - 1))); do
	nsid="$(expose "${DST_LVS}/same${i}")" || nsid=""
	[ -n "${nsid}" ] || die "expose same${i}"
	dev="$(wait_dev_nsid "${nsid}")" || die "no device for same${i}"
	same_devs+=("${dev}")
	same_nsids+=("${nsid}")
done

hits0="$(lvs_stat "${DST_LVS}" cache_object_hits)"
miss0="$(lvs_stat "${DST_LVS}" cache_object_misses)"
pop0="$(lvs_stat "${DST_LVS}" cache_object_populates)"
read_pids=()
for i in $(seq 0 $((SAME_N - 1))); do
	dd if="${same_devs[$i]}" of="${WORKDIR}/same.got.${i}" bs=1M count=4 \
		skip="$((i * 4))" iflag=direct status=none \
		>"${WORKDIR}/same.dd.${i}.err" 2>&1 &
	read_pids+=("$!")
done
read_bad=0
for pid in "${read_pids[@]}"; do
	wait "${pid}" || read_bad=1
done
if [ "${read_bad}" = "0" ]; then
	pass "all repeated clones read concurrently"
else
	fail "a repeated clone read failed"
fi

for i in $(seq 0 $((SAME_N - 1))); do
	want="$(dd if="${WORKDIR}/pat.${SAME_IDX}" bs=1M count=4 skip="$((i * 4))" \
		status=none | md5sum | cut -d' ' -f1)"
	got="$(md5sum "${WORKDIR}/same.got.${i}" 2>/dev/null | cut -d' ' -f1)"
	if [ "${got}" = "${want}" ]; then
		pass "same${i} reads the shared export bytes"
	else
		fail "same${i} data mismatch"
	fi
done

hits1="$(lvs_stat "${DST_LVS}" cache_object_hits)"
miss1="$(lvs_stat "${DST_LVS}" cache_object_misses)"
pop1="$(lvs_stat "${DST_LVS}" cache_object_populates)"
if [ "${hits1}" -gt "${hits0}" ] 2>/dev/null; then
	pass "repeated clones hit the shared object cache (${hits0} -> ${hits1})"
else
	fail "repeated clones did not hit the shared object cache"
fi
if [ "${miss1}" = "${miss0}" ] && [ "${pop1}" = "${pop0}" ]; then
	pass "repeated-clone reads added no miss or download"
else
	fail "repeated-clone reads changed miss/populate (${miss0}/${pop0} -> ${miss1}/${pop1})"
fi

for i in $(seq 0 $((SAME_N - 1))); do
	unexpose "${same_nsids[$i]}" "${same_devs[$i]}"
done

# --------------------------------------------------------------------------
echo ""
echo "=== [5] a restarted target preheats the attached templates again"

# An activated lvol cannot be closed, so unload would fail and the next start
# would have to recover a live WAL. Drop every bdev first.
for n in $(rpc rcow_get_bdev '{}' 2>/dev/null | python3 -c '
import json, sys
try:
    for e in json.load(sys.stdin):
        print(e["device_name"])
except Exception:
    pass'); do
	rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${n}")" >/dev/null \
		|| die "deactive ${n}"
done
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null \
	|| die "unload ${DST_LVS} before restart"
pass "destination lvstore unloaded"

stop_target
start_target
pass "target is up again"

mark="$(wc -l < "${TGT_LOG}")"
if rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"ph_dst_wal0"}' \
	"${DST_LVS}" "${BUCKET}")" >"${WORKDIR}/restart.attach.out" \
	2>"${WORKDIR}/restart.attach.err"; then
	pass "destination lvstore attached without formatting the WAL"
else
	die "attach ${DST_LVS} after restart: $(tr '\n' ' ' \
		< "${WORKDIR}/restart.attach.err" | head -c 300)"
fi

restart_deadline=$((SECONDS + 30))
restart_started=0
while :; do
	restart_started="$(tail -n +"$((mark + 1))" "${TGT_LOG}" \
		| grep -c "template preheat started" || true)"
	if [ "${restart_started}" = "${EXPORTS}" ]; then
		break
	fi
	if [ "${SECONDS}" -ge "${restart_deadline}" ]; then
		break
	fi
	sleep 0.2
done
if [ "${restart_started}" = "${EXPORTS}" ]; then
	pass "restart started one preheat for each of the ${EXPORTS} templates"
else
	fail "restart started ${restart_started} preheats, want ${EXPORTS}"
fi

wait_populated $((EXPORTS * FILL_MB)) 180 || exit 1
pass "restart populated every template into the fresh object cache"

nsid="$(expose "${DST_LVS}/c0")" || nsid=""
[ -n "${nsid}" ] || die "expose c0 after restart"
dev="$(wait_dev_nsid "${nsid}")" || die "no device for c0 after restart"
hits0="$(lvs_stat "${DST_LVS}" cache_object_hits)"
miss0="$(lvs_stat "${DST_LVS}" cache_object_misses)"
pop0="$(lvs_stat "${DST_LVS}" cache_object_populates)"
if dd if="${dev}" of="${WORKDIR}/restart.got" bs=1M count=4 iflag=direct status=none \
	>"${WORKDIR}/restart.dd.err" 2>&1; then
	pass "a clone reads after the restart"
else
	fail "a clone read failed after the restart"
fi
want="$(dd if="${WORKDIR}/pat.0" bs=1M count=4 status=none | md5sum | cut -d' ' -f1)"
got="$(md5sum "${WORKDIR}/restart.got" 2>/dev/null | cut -d' ' -f1)"
if [ "${got}" = "${want}" ]; then
	pass "the clone still returns the exported bytes"
else
	fail "the clone bytes changed across the restart"
fi
hits1="$(lvs_stat "${DST_LVS}" cache_object_hits)"
miss1="$(lvs_stat "${DST_LVS}" cache_object_misses)"
pop1="$(lvs_stat "${DST_LVS}" cache_object_populates)"
if [ "${hits1}" -gt "${hits0}" ] 2>/dev/null; then
	pass "the post-restart read hit the new object cache (${hits0} -> ${hits1})"
else
	fail "the post-restart read missed the new object cache (${hits0} -> ${hits1})"
fi
if [ "${miss1}" = "${miss0}" ] && [ "${pop1}" = "${pop0}" ]; then
	pass "the post-restart read did not download again"
else
	fail "the post-restart read changed miss/populate (${miss0}/${pop0} -> ${miss1}/${pop1})"
fi
unexpose "${nsid}" "${dev}"
