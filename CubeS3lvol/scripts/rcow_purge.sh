#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  rcow_purge.sh -- throw an lvstore away and leave a clean slate
#
#  This deletes data irreversibly. It exists because getting back to a clean
#  environment otherwise means four separate steps, each of which is easy to get
#  half-right: delete the objects under the lvstore's prefix in the bucket, drop the
#  entry from bstore.json, drop the host-side activation registry, and re-create the
#  WAL image. Miss the bstore.json entry and the next start tries to attach a prefix
#  that is no longer there; miss the WAL image and it attaches a journal describing
#  objects that have been deleted.
#
#  === Why an lvstore ever needs throwing away ===
#
#  Normally it does not: rcow_stop.sh keeps everything on purpose, because the next
#  attach needs it. Two cases where it does.
#
#  A test run that is finished with. The bucket accumulates one prefix per lvstore
#  ever created, and nothing removes them.
#
#  An lvstore that can no longer be attached. Blobstore metadata can be left
#  inconsistent by a process that died mid-operation, and SPDK's recovery path does
#  not defend against every shape of that -- bs_load_replay_extent_pages() will
#  segfault on a broken extent chain rather than report an error. The symptom is a
#  target that vanishes during rcow_attach_lvstore with the log stopping partway
#  through "Recover: blob 0x...". The objects are all still in the bucket, but
#  nothing can index them, and there is no repair tool. Recognising that state and
#  starting over is the only option, which is what this script is for.
#
#  === What it refuses to do ===
#
#  It will not touch an lvstore that a running target has loaded. That is not
#  politeness: deleting the objects underneath a live blobstore gives you a process
#  that keeps serving reads from cache and fails them at random as the cache turns
#  over, which is far harder to diagnose than a target that will not start. Stop the
#  target first.
#
#  It also refuses an empty prefix, because s3_prefix_rm.py accepts one and treats
#  it as the whole bucket. A typo that resolves to empty would take every lvstore in
#  the bucket with it.
#
#  === Usage ===
#
#    rcow_purge.sh                       # this node's lvstore, with a prompt
#    rcow_purge.sh --yes                 # no prompt, for scripts
#    rcow_purge.sh --lvs-name other      # a specific lvstore
#    rcow_purge.sh --keep-s3             # local state only, leave the bucket
#    rcow_purge.sh --keep-wal            # do not re-create the WAL image
#    rcow_purge.sh --dry-run             # list what would go, delete nothing
#
#  The lvstore name defaults to the same per-node derivation rcow_start.sh uses, so
#  running it with no arguments purges what a plain rcow_start.sh would have
#  created. RCOW_LVS_NAME in the environment works too.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=rcow_common.sh
. "${SELF_DIR}/rcow_common.sh"

ASSUME_YES=0
KEEP_S3=0
KEEP_WAL=0
DRY_RUN=0

while [ $# -gt 0 ]; do
	case "$1" in
	--yes|-y)      ASSUME_YES=1 ;;
	--keep-s3)     KEEP_S3=1 ;;
	--keep-wal)    KEEP_WAL=1 ;;
	--dry-run|-n)  DRY_RUN=1 ;;
	--lvs-name)
		[ $# -ge 2 ] || rcow_die "--lvs-name needs a value"
		RCOW_LVS_NAME="$2"
		shift
		;;
	-h|--help)
		sed -n '5,50p' "${BASH_SOURCE[0]}" | sed 's/^#\{0,2\} \{0,1\}//'
		exit 0
		;;
	*) rcow_die "unknown argument '$1' (see --help)" ;;
	esac
	shift
done

# s3_prefix_rm.py lives in test/tools/ in the repo and in scripts/ in a release
# package, so look in both rather than assuming a layout.
PREFIX_RM=""
for cand in "${SELF_DIR}/s3_prefix_rm.py" \
	    "${SELF_DIR}/../test/tools/s3_prefix_rm.py"; do
	if [ -f "${cand}" ]; then
		PREFIX_RM="${cand}"
		break
	fi
done

[ -n "${RCOW_LVS_NAME}" ] || rcow_die "the lvstore name is empty"

# The guard that matters most. s3_prefix_rm.py takes an empty prefix to mean the
# whole bucket, so a name that came out empty or as bare "/" would delete every
# lvstore here. Checked even in --dry-run, so the refusal shows up while it is
# still cheap.
case "${RCOW_LVS_NAME}" in
*/*|.|..)
	rcow_die "lvstore name '${RCOW_LVS_NAME}' contains a slash or is a dot; \
refusing to build a prefix from it"
	;;
esac

S3_PREFIX="${RCOW_LVS_NAME}/"

rcow_step "what would be purged"
rcow_log "lvstore:        ${RCOW_LVS_NAME}"
rcow_log "S3 prefix:      ${S3_PREFIX}"
rcow_log "bstore entry:   ${RCOW_BSTORE_FILE}"
rcow_log "activation reg: ${RCOW_ACTIVE_FILE}"
rcow_log "WAL image:      ${RCOW_WAL_IMG}"

# --------------------------------------------------------------------------
# Refuse while a target has it loaded.
#
# Two questions, and both have to be asked. A target may be running without this
# lvstore loaded (perfectly fine to purge a different one), and the RPC socket may
# be stale from a process that died (in which case asking gets nothing and the purge
# should go ahead).
rcow_step "checking that nothing has it loaded"

TARGET_PIDS="$(rcow_target_instances)"
if [ -n "${TARGET_PIDS}" ]; then
	rcow_log "a target is running (pid ${TARGET_PIDS//$'\n'/, }); asking which \
lvstores it holds"

	LOADED=""
	if [ -S "${RCOW_RPC_SOCK}" ]; then
		# rcow_rpc, not rcow_srpc: the latter is SPDK's own rpc.py, which has a
		# fixed list of methods and rejects rcow_* as an invalid choice. Asking
		# it here looked exactly like a target that would not answer, and the
		# refusal below then fired with the wrong reason.
		LOADED="$(rcow_rpc rcow_get_lvstores 2>/dev/null | \
			python3 -c '
import json, sys
try:
    for e in json.load(sys.stdin):
        print(e.get("lvs_name", ""))
except Exception:
    pass' 2>/dev/null || true)"
	fi

	if [ -z "${LOADED}" ]; then
		# Either the socket is gone or the target did not answer. Not treated as
		# "nothing is loaded": a target that is alive but wedged is exactly the
		# case where deleting its objects does the most damage.
		rcow_die "a target is running but did not say what it has loaded. Stop it \
first (rcow_stop.sh, or rcow_stop.sh --force if it is wedged)"
	fi

	if printf '%s\n' "${LOADED}" | grep -qxF "${RCOW_LVS_NAME}"; then
		rcow_die "the running target has '${RCOW_LVS_NAME}' loaded. Deleting its \
objects now would leave that process serving reads it can no longer satisfy; run \
rcow_stop.sh first"
	fi

	rcow_log "it holds: $(printf '%s' "${LOADED}" | tr '\n' ' ')-- not this one"

	# The WAL image is per node, not per lvstore: RCOW_WAL_IMG does not contain
	# the lvstore name, so the image about to be re-created below is the one the
	# *loaded* lvstore is journalling into. Re-creating it would destroy an
	# lvstore this run was never asked to touch, and the failure would surface
	# later as an attach replaying a journal full of zeroes.
	if [ "${KEEP_WAL}" -eq 0 ]; then
		rcow_warn "a different lvstore is loaded and shares the WAL image \
${RCOW_WAL_IMG}; not re-creating it"
		rcow_warn "pass --keep-wal explicitly to silence this, or stop the target \
if the image really should be reset"
		KEEP_WAL=1
	fi
else
	rcow_log "no target running"
fi

# --------------------------------------------------------------------------
rcow_step "listing what is there"

S3_COUNT="?"
if [ "${KEEP_S3}" -eq 1 ]; then
	rcow_log "S3: skipped (--keep-s3)"
elif [ -z "${PREFIX_RM}" ]; then
	rcow_warn "s3_prefix_rm.py not found next to this script or in \
../test/tools; the bucket cannot be cleaned"
	KEEP_S3=1
else
	rcow_need_cmd python3
	if ! rcow_load_credentials; then
		rcow_die "no S3 credentials, so the bucket cannot be cleaned. Set \
AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, or pass --keep-s3 to purge local state \
only"
	fi
	S3_ENDPOINT="$(rcow_cfg_get endpoint)"
	S3_REGION="$(rcow_cfg_get region)"
	S3_BUCKET="$(rcow_s3_buckets | head -1)"
	[ -n "${S3_BUCKET}" ] || rcow_die "no bucket in ${RCOW_S3_CFG}"
	S3_ADDR_FLAGS=()
	rcow_s3_addr_flags S3_ADDR_FLAGS

	S3_COUNT="$(python3 "${PREFIX_RM}" -e "${S3_ENDPOINT}" -b "${S3_BUCKET}" \
		-r "${S3_REGION}" -p "${S3_PREFIX}" --list \
		"${S3_ADDR_FLAGS[@]+"${S3_ADDR_FLAGS[@]}"}" \
		2>/dev/null | grep -c . || true)"
	rcow_log "S3: ${S3_COUNT} object(s) under ${S3_BUCKET}/${S3_PREFIX}"
fi

HAS_BSTORE=no
if rcow_bstore_entry "${RCOW_LVS_NAME}" >/dev/null 2>&1; then
	HAS_BSTORE=yes
fi
rcow_log "bstore.json:    entry for '${RCOW_LVS_NAME}' present: ${HAS_BSTORE}"

ACTIVE_N=0
if [ -s "${RCOW_ACTIVE_FILE}" ]; then
	ACTIVE_N="$(python3 -c '
import json, sys
try:
    print(len(json.load(open(sys.argv[1]))))
except Exception:
    print(0)' "${RCOW_ACTIVE_FILE}" 2>/dev/null || echo 0)"
fi
rcow_log "activation reg: ${ACTIVE_N} volume(s) recorded"

WAL_SIZE=""
if [ -f "${RCOW_WAL_IMG}" ]; then
	WAL_SIZE="$(stat -c %s "${RCOW_WAL_IMG}" 2>/dev/null || echo 0)"
	rcow_log "WAL image:      $((WAL_SIZE / 1073741824)) GiB apparent, \
$(du -h "${RCOW_WAL_IMG}" 2>/dev/null | cut -f1) used"
else
	rcow_log "WAL image:      absent"
fi

if [ "${DRY_RUN}" -eq 1 ]; then
	rcow_step "dry run: nothing was deleted"
	exit 0
fi

# --------------------------------------------------------------------------
if [ "${ASSUME_YES}" -eq 0 ]; then
	printf '\n'
	printf 'This deletes the lvstore permanently. There is no undo, and the\n'
	printf 'objects are not recoverable from anywhere else.\n\n'
	printf "Type the lvstore name ('%s') to confirm: " "${RCOW_LVS_NAME}"
	read -r REPLY_NAME
	[ "${REPLY_NAME}" = "${RCOW_LVS_NAME}" ] || rcow_die "not confirmed; nothing done"
fi

# --------------------------------------------------------------------------
rcow_step "purging"
FAILED=0

if [ "${KEEP_S3}" -eq 0 ]; then
	rcow_log "deleting ${S3_COUNT} object(s) under ${S3_PREFIX}"
	if python3 "${PREFIX_RM}" -e "${S3_ENDPOINT}" -b "${S3_BUCKET}" \
			-r "${S3_REGION}" -p "${S3_PREFIX}" \
			"${S3_ADDR_FLAGS[@]+"${S3_ADDR_FLAGS[@]}"}" 2>&1 | tail -2; then
		# Verified rather than trusted: a partial delete leaves an owner marker
		# or a checkpoint behind, and the next create then refuses with -EEXIST
		# for a prefix that looks empty in every other respect.
		LEFT="$(python3 "${PREFIX_RM}" -e "${S3_ENDPOINT}" -b "${S3_BUCKET}" \
			-r "${S3_REGION}" -p "${S3_PREFIX}" --list \
			"${S3_ADDR_FLAGS[@]+"${S3_ADDR_FLAGS[@]}"}" \
			2>/dev/null | grep -c . || true)"
		if [ "${LEFT}" -eq 0 ]; then
			rcow_log "S3 prefix is empty"
		else
			rcow_err "${LEFT} object(s) still under ${S3_PREFIX}"
			FAILED=1
		fi
	else
		rcow_err "the delete did not complete"
		FAILED=1
	fi
fi

if [ "${HAS_BSTORE}" = yes ]; then
	if python3 - "${RCOW_BSTORE_FILE}" "${RCOW_LVS_NAME}" <<'PY'
import json, sys

path, name = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        d = json.load(f)
except FileNotFoundError:
    raise SystemExit(0)
except Exception as e:
    print("cannot parse %s: %s" % (path, e), file=sys.stderr)
    raise SystemExit(1)

if d.pop(name, None) is None:
    raise SystemExit(0)

# Rewritten whole, which is safe here because this file only ever holds one entry
# per lvstore and the others are preserved by the dict round-trip.
with open(path, "w") as f:
    json.dump(d, f, indent=2)
    f.write("\n")
PY
	then
		rcow_log "removed the bstore.json entry"
	else
		rcow_err "could not update ${RCOW_BSTORE_FILE}"
		FAILED=1
	fi
fi

# The activation registry is per-node, not per-lvstore: there is no lvstore name in
# it, only volume names. Removing it wholesale is right when purging the lvstore
# those volumes belonged to, and wrong if another lvstore's volumes are in there.
# One blobstore per node makes that impossible today, but the guard above has
# already established whether a *different* lvstore is loaded, so honour it here
# too rather than relying on that rule holding forever.
if [ -n "${TARGET_PIDS}" ] && [ "${ACTIVE_N}" -gt 0 ]; then
	rcow_warn "another lvstore is loaded and the activation registry is shared; \
leaving ${RCOW_ACTIVE_FILE} alone (${ACTIVE_N} entr\
$([ "${ACTIVE_N}" = 1 ] && echo y || echo ies))"
elif [ -e "${RCOW_ACTIVE_FILE}" ] || [ -e "${RCOW_ACTIVE_FILE}.replay" ]; then
	rm -f "${RCOW_ACTIVE_FILE}" "${RCOW_ACTIVE_FILE}.replay" \
		&& rcow_log "removed the activation registry (${ACTIVE_N} entr\
$([ "${ACTIVE_N}" = 1 ] && echo y || echo ies))" \
		|| { rcow_err "could not remove ${RCOW_ACTIVE_FILE}"; FAILED=1; }
fi

# Re-created rather than deleted: rcow_start.sh does not create it (it dies if the
# file is missing), so deleting it would trade one broken start for another. Same
# size as before when there was one, since that is the size the operator chose.
if [ "${KEEP_WAL}" -eq 1 ]; then
	if [ -n "${TARGET_PIDS}" ]; then
		# Set by the guard above, not by the caller: the image belongs to the
		# lvstore that is still loaded, so leaving it is the whole point and
		# there is nothing to warn about.
		rcow_log "WAL image: left alone, it belongs to the loaded lvstore"
	else
		rcow_log "WAL image: left alone (--keep-wal)"
		rcow_warn "it still holds a journal for the lvstore that was just \
deleted; the next create formats it, but an attach would try to replay it"
	fi
elif [ -n "${WAL_SIZE}" ] && [ "${WAL_SIZE}" -gt 0 ]; then
	if rm -f "${RCOW_WAL_IMG}" && truncate -s "${WAL_SIZE}" "${RCOW_WAL_IMG}"; then
		rcow_log "re-created ${RCOW_WAL_IMG} at $((WAL_SIZE / 1073741824)) GiB, \
sparse and unformatted"
	else
		rcow_err "could not re-create ${RCOW_WAL_IMG}"
		FAILED=1
	fi
else
	rcow_log "WAL image: nothing to re-create"
fi

# --------------------------------------------------------------------------
rcow_step "result"
if [ "${FAILED}" -eq 0 ]; then
	rcow_log "'${RCOW_LVS_NAME}' is gone; the next rcow_start.sh will create it"
	exit 0
fi

rcow_err "the purge did not finish cleanly -- see above"
rcow_err "re-running is safe: every step here is idempotent"
exit 1
