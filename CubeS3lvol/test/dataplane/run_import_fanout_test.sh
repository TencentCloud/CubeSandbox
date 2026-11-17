#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Concurrent imports of one export onto one hidden template.
#
#  Widths are 10 and 50 (override with FANOUT_WIDTHS="10 50"). Each width is
#  submitted twice, as one burst:
#
#    decouple:false   every import must return, all modes are esnap, and a
#                     sample of the clones reads the source pattern. Nothing
#                     is copied.
#    decouple:true    every import must return. The imports already waiting
#                     clone first; the template is copied once after that
#                     batch, not by the first clone to finish. An import that
#                     arrives while the copy is running waits for it. After
#                     the copy, one more import is a local clone and does not
#                     start a second copy. A sample still matches.
#
#  The payload is large enough that the copy is still running when the burst's
#  clones return, so a success is not explained by the copy having finished
#  before the later imports cloned.
#
#  Usage:
#    sudo -E ./test/dataplane/run_import_fanout_test.sh
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

SRC_LVS=ifsrc
DST_LVS=ifdst
SRC_WAL=/data/ifan_src.img
DST_WAL=/data/ifan_dst.img
RPC_SOCK=/tmp/ifan.sock
TGT_LOG=/tmp/ifan_target.log
NQN="nqn.2026-08.io.spdk:ifan"
PORT="4491"
FILL_MB=128
WIDTHS="${FANOUT_WIDTHS:-10 50}"

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
		for p in "${SRC_LVS}" "${DST_LVS}"; do
			rcow_discard_writeback_shm "${p}" || true
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "${p}/" >/dev/null 2>&1 || true
		done
		if [ -n "${EXP_UUID}" ]; then
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "exports/${EXP_UUID}" >/dev/null 2>&1 || true
		fi
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

wait_decouple_idle()
{
	local deadline=$((SECONDS + 600))
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
			fail "decouple still running after 600s"
			return 1
		fi
		sleep 0.5
	done
}

wait_decouple_active()
{
	local deadline=$((SECONDS + 30))
	local n

	while [ "${SECONDS}" -lt "${deadline}" ]; do
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
		[ "${n}" -gt 0 ] 2>/dev/null && return 0
		sleep 0.05
	done
	fail "template copy did not remain active long enough for a late import"
	return 1
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

# One namespace is exposed at a time. Linux reuses /dev/nvmeXnY, and the
# namespace directory is not always a sibling of the controller, so the
# device is whichever block node this subsystem currently has.
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
	# sysfs layout we do not recognise: the only node on this controller.
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
	echo "  ---- no block device for nsid token '${nsid}'" >&2
	echo "  ---- controller nodes: $(devs_of_ctrl | tr '\n' ' ')" >&2
	return 1
}

wait_dev_gone()
{
	local dev="$1"
	local deadline=$((SECONDS + 20))
	local ctrl base

	ctrl="$(ctrl_of_nqn)" || ctrl=""
	base="$(basename "${ctrl}")"
	while [ "${SECONDS}" -lt "${deadline}" ]; do
		rescan_ctrl || true
		if [ -n "${dev}" ] && [ -b "${dev}" ]; then
			sleep 0.2
			continue
		fi
		if [ -z "${base}" ] || ! compgen -G "/dev/${base}n*" >/dev/null; then
			return 0
		fi
		sleep 0.2
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

md5_at()
{
	dd if="$1" bs=1M count="${FILL_MB}" iflag=direct status=none 2>/dev/null \
		| md5sum | cut -d' ' -f1
}

tgt_rss_kib()
{
	awk '/VmRSS:/ {print $2}' "/proc/${TGT_PID}/status" 2>/dev/null || echo 0
}

# One burst. Writes per-import status and milliseconds under $1.
fanout_burst()
{
	local dir="$1" n="$2" decouple="$3" submit_late="${4:-false}"
	local i name pid
	local -a pids=()

	rm -rf "${dir}"
	mkdir -p "${dir}"
	for i in $(seq 1 "${n}"); do
		name="$(printf 'fan-%02d' "${i}")"
		(
			local t0 t1 rc
			t0="$(date +%s%N)"
			if python3 "${RPC_PY}" --sock "${RPC_SOCK}" rcow_import_lvol \
				"$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":%s}' \
					"${name}" "${EXP_UUID}" "${DST_LVS}" "${decouple}")" \
				>"${dir}/${name}.out" 2>"${dir}/${name}.err"; then
				rc=0
			else
				rc=1
			fi
			t1="$(date +%s%N)"
			echo "${rc}" >"${dir}/${name}.rc"
			echo $(( (t1 - t0) / 1000000 )) >"${dir}/${name}.ms"
		) &
		pids+=("$!")
	done
	if [ "${submit_late}" = "true" ] && wait_decouple_active; then
		# Submit before waiting for the creating burst. Those RPCs return only
		# after the shared template copy has completed, which is too late to
		# exercise an arrival during COPY_IN_PROGRESS.
		(
			local rc
			if python3 "${RPC_PY}" --sock "${RPC_SOCK}" rcow_import_lvol \
				"$(printf '{"lvol_name":"fan-late","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
					"${EXP_UUID}" "${DST_LVS}")" \
				>"${dir}/fan-late.out" 2>"${dir}/fan-late.err"; then
				rc=0
			else
				rc=1
			fi
			echo "${rc}" >"${dir}/fan-late.rc"
		) &
		pids+=("$!")
		: >"${dir}/fan-late.submitted"
	fi
	for pid in "${pids[@]}"; do
		wait "${pid}" || true
	done
}

summarize_burst()
{
	local dir="$1" n="$2" label="$3"
	local ok=0 i name line
	local stats

	for i in $(seq 1 "${n}"); do
		name="$(printf 'fan-%02d' "${i}")"
		if [ "$(cat "${dir}/${name}.rc" 2>/dev/null || echo 1)" = "0" ]; then
			ok=$((ok + 1))
		fi
	done
	stats="$(
		for i in $(seq 1 "${n}"); do
			name="$(printf 'fan-%02d' "${i}")"
			cat "${dir}/${name}.ms" 2>/dev/null || echo 0
		done | python3 -c '
import sys
xs = sorted(int(x) for x in sys.stdin if x.strip())
def pct(p):
    if not xs:
        return 0
    i = min(len(xs) - 1, max(0, int(round((p / 100.0) * (len(xs) - 1)))))
    return xs[i]
print("%d %d %d" % (pct(50), pct(99), xs[-1] if xs else 0))
'
	)"
	# shellcheck disable=SC2086
	set -- ${stats}
	line="${label}: ${ok}/${n} ok, p50=${1}ms p99=${2}ms max=${3}ms rss=$(tgt_rss_kib)KiB"
	if [ "${ok}" = "${n}" ]; then
		pass "${line}"
	else
		fail "${line}"
		for i in $(seq 1 "${n}"); do
			name="$(printf 'fan-%02d' "${i}")"
			[ "$(cat "${dir}/${name}.rc" 2>/dev/null || echo 1)" = "0" ] && continue
			info "${name}: $(tr '\n' ' ' < "${dir}/${name}.err" 2>/dev/null | head -c 200)"
		done
	fi
	echo "${ok}" >"${dir}/ok"
}

mode_histogram()
{
	local dir="$1" n="$2"
	local i name

	for i in $(seq 1 "${n}"); do
		name="$(printf 'fan-%02d' "${i}")"
		[ "$(cat "${dir}/${name}.rc" 2>/dev/null || echo 1)" = "0" ] || continue
		import_mode "$(cat "${dir}/${name}.out")"
	done | sort | uniq -c
}

sample_matches()
{
	local label="$1"
	shift
	local name nsid dev got bad=0

	for name in "$@"; do
		nsid="$(expose "${DST_LVS}/${name}")" || nsid=""
		if [ -z "${nsid}" ]; then
			fail "${label}: could not expose ${name}"
			bad=1
			continue
		fi
		dev="$(wait_dev_nsid "${nsid}")" || dev=""
		if [ -z "${dev}" ]; then
			fail "${label}: no device for ${name} (nsid ${nsid})"
			unexpose "${nsid}"
			bad=1
			continue
		fi
		got="$(md5_at "${dev}")"
		unexpose "${nsid}" "${dev}"
		if [ "${got}" = "${PAT_MD5}" ]; then
			pass "${label}: ${name} matches the source pattern"
		else
			fail "${label}: ${name} mismatch"
			bad=1
		fi
	done
	return "${bad}"
}

delete_fan()
{
	local n="$1" extra
	local i name mark deadline
	shift

	mark="$(wc -l < "${TGT_LOG}")"
	: >"${WORKDIR}/delete.err"
	for i in $(seq 1 "${n}"); do
		name="$(printf 'fan-%02d' "${i}")"
		rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${name}")" \
			>/dev/null 2>>"${WORKDIR}/delete.err" || \
			info "delete ${name}: $(tail -n 1 "${WORKDIR}/delete.err" | head -c 200)"
	done
	for extra in "$@"; do
		rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${extra}")" \
			>/dev/null 2>>"${WORKDIR}/delete.err" || \
			info "delete ${extra}: $(tail -n 1 "${WORKDIR}/delete.err" | head -c 200)"
	done
	deadline=$((SECONDS + 60))
	while :; do
		if tail -n +"${mark}" "${TGT_LOG}" \
			| grep -q "Removed unused import template for export ${EXP_UUID}"; then
			return 0
		fi
		if [ "${SECONDS}" -ge "${deadline}" ]; then
			fail "template for ${EXP_UUID} was not reaped"
			return 1
		fi
		sleep 0.2
	done
}

decouple_lines()
{
	local mark="$1"

	tail -n +"${mark}" "${TGT_LOG}" \
		| grep -c "Decoupling lvol '__rcow_tpl_${EXP_UUID}'" || true
}

run_width()
{
	local n="$1"
	local dir ok modes copies mark probe out mode

	echo ""
	echo "=== [fanout ${n}] decouple:false"
	mark="$(wc -l < "${TGT_LOG}")"
	dir="${WORKDIR}/false-${n}"
	fanout_burst "${dir}" "${n}" false
	summarize_burst "${dir}" "${n}" "decouple:false n=${n}"
	ok="$(cat "${dir}/ok")"
	modes="$(mode_histogram "${dir}" "${n}" | tr '\n' ' ')"
	info "modes: ${modes}"
	if [ "${ok}" = "${n}" ] && printf '%s' "${modes}" | grep -Eq "(^|[[:space:]])${n} esnap([[:space:]]|$)"; then
		pass "all ${n} decouple:false imports are esnap"
	else
		fail "decouple:false modes were not all esnap (${modes})"
	fi
	copies="$(decouple_lines "${mark}")"
	if [ "${copies}" = "0" ]; then
		pass "decouple:false started no template copy"
	else
		fail "decouple:false started ${copies} template cop(ies)"
	fi
	if [ "${ok}" = "${n}" ]; then
		sample_matches "decouple:false n=${n}" fan-01 "$(printf 'fan-%02d' "$((n / 2))")" \
			"$(printf 'fan-%02d' "${n}")" || true
	fi
	delete_fan "${n}" || return 1
	if [ "$(import_count "${DST_LVS}" "${EXP_UUID}")" = "0" ]; then
		pass "registry dropped after the ${n} decouple:false clones"
	else
		fail "registry still names the export after the decouple:false clones were deleted"
	fi

	echo ""
	echo "=== [fanout ${n}] decouple:true"
	mark="$(wc -l < "${TGT_LOG}")"
	dir="${WORKDIR}/true-${n}"
	fanout_burst "${dir}" "${n}" true true
	if [ -f "${dir}/fan-late.submitted" ]; then
		if [ "$(cat "${dir}/fan-late.rc" 2>/dev/null || echo 1)" != "0" ]; then
			fail "import submitted during the active template copy failed"
			sed 's/^/       /' "${dir}/fan-late.err"
			return 1
		fi
		out="$(cat "${dir}/fan-late.out")"
		mode="$(import_mode "${out}")"
		if [ "${mode}" = "local_clone" ]; then
			pass "an import arriving during the copy waited and became a local clone"
		else
			fail "the import arriving during the copy returned mode ${mode}, want local_clone"
		fi
	fi
	summarize_burst "${dir}" "${n}" "decouple:true n=${n}"
	ok="$(cat "${dir}/ok")"
	copies="$(decouple_lines "${mark}")"
	if [ "${copies}" = "1" ]; then
		pass "the ${n} decouple:true imports started one template copy"
	else
		fail "decouple:true copy count is ${copies}, want 1"
	fi
	if ! wait_decouple_idle; then
		return 1
	fi
	pass "template copy for n=${n} finished"
	out="$(rpc rcow_import_lvol "$(printf '{"lvol_name":"fan-probe","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
		"${EXP_UUID}" "${DST_LVS}")" 2>"${WORKDIR}/probe-${n}.err")" || {
		fail "probe import after the n=${n} copy failed"
		sed 's/^/       /' "${WORKDIR}/probe-${n}.err"
		return 1
	}
	mode="$(import_mode "${out}")"
	if [ "${mode}" = "local_clone" ]; then
		pass "import after the n=${n} copy is local_clone"
	else
		fail "import after the n=${n} copy is ${mode}, want local_clone"
	fi
	if [ "$(decouple_lines "${mark}")" = "1" ]; then
		pass "the probe did not start a second copy"
	else
		fail "a second template copy started"
	fi
	if [ "${ok}" = "${n}" ]; then
		sample_matches "decouple:true n=${n}" fan-01 "$(printf 'fan-%02d' "${n}")" \
			|| true
	fi
	delete_fan "${n}" fan-late fan-probe || return 1
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

WORKDIR="$(mktemp -d /tmp/ifan.XXXXXX)"
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

raw bdev_aio_create "$(printf '{"filename":"%s","name":"ifan_src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null || die "source wal bdev"
raw bdev_aio_create "$(printf '{"filename":"%s","name":"ifan_dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null || die "dest wal bdev"

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":8,"wal_bdev":"ifan_src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BUCKET}")" >/dev/null || die "create ${SRC_LVS}"

raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1 || true
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"IFAN00000000001"}' \
	"${NQN}")" >/dev/null || die "nvmf subsystem"
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null || die "nvmf listener"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1 \
	|| die "nvme connect"
CONNECTED=1
pass "target and source lvstore are up"

# --------------------------------------------------------------------------
echo ""
echo "=== [1] source volume, snapshot, export (${FILL_MB} MiB)"

rpc rcow_create_lvol '{"lvol_name":"src","size_gib":1}' >/dev/null || die "create src"
NS_SRC="$(expose "${SRC_LVS}/src")" || NS_SRC=""
[ -n "${NS_SRC}" ] || die "expose src"
SRC_DEV="$(wait_dev_nsid "${NS_SRC}")" || die "source device never appeared"

dd if=/dev/urandom of="${WORKDIR}/pattern" bs=1M count="${FILL_MB}" status=none
PAT_MD5="$(md5sum "${WORKDIR}/pattern" | cut -d' ' -f1)"
dd if="${WORKDIR}/pattern" of="${SRC_DEV}" bs=1M oflag=direct conv=fsync status=none \
	|| die "write source"
[ "$(md5_at "${SRC_DEV}")" = "${PAT_MD5}" ] \
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
pass "exported snap0 (${FILL_MB} MiB)"

unexpose "${NS_SRC}" "${SRC_DEV}"
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null \
	|| die "unload source"

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":16,"wal_bdev":"ifan_dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BUCKET}")" >/dev/null || die "create ${DST_LVS}"
pass "destination lvstore is up"

# --------------------------------------------------------------------------
for n in ${WIDTHS}; do
	run_width "${n}" || die "fanout ${n} aborted"
done

pass "fanout widths ${WIDTHS} completed"
