#!/usr/bin/env bash
#
#  run_all.sh -- every test in this tree, in one command, with an honest summary.
#
#  What made this worth writing: the suites had drifted into needing different
#  things. Ten integration tests run anywhere; two more want real S3 credentials;
#  ten dataplane scripts want root, credentials, a writable /data, and exclusive
#  use of the machine's nvme stack. Their arguments differ too -- four take
#  -e/-b/-r, six read /data/cubelet/s3.cfg themselves. So "run the tests" meant
#  remembering twenty-two invocations, and in practice meant running the two or
#  three related to whatever had just changed.
#
#  === Skipped is not passed ===
#
#  The one thing this must not do is let an unrun test look like a green one.
#  Anything that cannot run is reported as SKIP with the reason, the summary
#  counts skips separately, and the exit status is 0 only if nothing failed *and*
#  the reason for any skip was a deliberate flag rather than a broken
#  environment.
#
#  === Why serial, and why this order ===
#
#  Serial because the dataplane scripts each expect to own the machine: they
#  start a target on a fixed RPC socket, connect nvme controllers, and write
#  /data/cubelet/rcow/bstore.json and /data/cubelet/rcow/active_lvols, whose
#  paths are compiled
#  into the module and therefore shared by every instance. Two at once do not
#  fail cleanly; they interleave.
#
#  The order goes cheapest and least invasive first -- offline integration, then
#  the S3 ones, then the dataplane scripts -- so that a plain mistake surfaces in
#  seconds rather than after ten minutes of nvme traffic. activation and control
#  come last of all because they are the two most sensitive to leftover global
#  state, which makes them the best final word on whether the run left the
#  machine tidy.
#
#  === A failure does not stop the run ===
#
#  A full pass is a quarter of an hour, and stopping at the first red discards
#  everything the remaining suites would have said -- which is usually how you
#  find out whether one thing broke or ten did. Failures are collected and
#  repeated at the end.
#
#  Usage:
#    test/run_all.sh                 everything the environment allows
#    test/run_all.sh --offline       only the suites needing no S3 and no root
#    test/run_all.sh --no-dataplane  integration only, including the S3 ones
#    test/run_all.sh --list          show what would run, run nothing
#
#  Environment:
#    S3LVOL_TEST_BUCKET   override the bucket taken from s3.cfg
#    AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
#                         used as-is when set; otherwise read from s3.cfg
#

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/.." && pwd)"
cd "${ROOT}" || exit 1

S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"

MODE=all
for arg in "$@"; do
	case "${arg}" in
	--offline)      MODE=offline ;;
	--no-dataplane) MODE=integration ;;
	--list)         MODE=list ;;
	-h|--help)      sed -n '2,50p' "${BASH_SOURCE[0]}"; exit 0 ;;
	*)echo "unknown option: ${arg} (try --help)" >&2; exit 1 ;;
	esac
done

# --------------------------------------------------------------------------
# Tally
#
# Assertion counts come from the suites' own summary lines, which are not in one
# format: the older tests print Chinese, s3_flush_test omits the "result:"
# prefix, and the scripts use the English form. All three are parsed rather than
# unified, because rewriting eight suites' output to tidy up a report is a poor
# trade -- and a suite whose line cannot be parsed is reported as such instead of
# being counted as zero.
# --------------------------------------------------------------------------
PASS_TOTAL=0
FAIL_TOTAL=0
XFAIL_TOTAL=0
XPASS_TOTAL=0
SUITES_OK=0
SUITES_BAD=0
SUITES_SKIPPED=0
SKIPPED_BY_CHOICE=1   # cleared by any skip that was not asked for
FAILED_NAMES=""
SKIPPED_NAMES=""
XPASS_NAMES=""

parse_counts()
{
	sed -n \
	    -e 's/.*result:[[:space:]]*\([0-9]\+\)[[:space:]]*passed,[[:space:]]*\([0-9]\+\)[[:space:]]*failed.*/\1 \2/p' \
	    -e 's/.*===[[:space:]]*\([0-9]\+\)[[:space:]]*passed,[[:space:]]*\([0-9]\+\)[[:space:]]*failed.*/\1 \2/p' \
	    | tail -1
}

# Expected failures, for suites that have any. An XPASS is surfaced rather than
# summed away: it means a backend gained a capability the code was written not to
# rely on, and that is a decision to revisit, not a number to bump.
parse_xcounts()
{
	sed -n \
	    -e 's/^\[xfail\][[:space:]]*\([0-9]\+\)[[:space:]]*expected failure(s),[[:space:]]*\([0-9]\+\)[[:space:]]*unexpected pass.*/\1 \2/p' \
	    | tail -1
}

report_skip()
{
	local name="$1" why="$2" asked="${3:-0}"

	printf '  SKIP  %-26s %s\n' "${name}" "${why}"
	SUITES_SKIPPED=$((SUITES_SKIPPED + 1))
	SKIPPED_NAMES="${SKIPPED_NAMES}  ${name}: ${why}"$'\n'
	[ "${asked}" -eq 1 ] || SKIPPED_BY_CHOICE=0
}

# Run one suite, keep its output, report the one line that matters.
run_suite()
{
	local name="$1"; shift
	local log rc counts p f started elapsed

	log="${LOG_DIR}/${name}.log"
	started="${SECONDS}"

	"$@" >"${log}" 2>&1
	rc=$?
	elapsed=$((SECONDS - started))

	counts="$(parse_counts <"${log}")"
	p="${counts%% *}"
	f="${counts##* }"

	if [ -z "${counts}" ]; then
		# No summary line at all. Nearly always a suite that died on its way
		# up (a missing binary, a target already running), and counting it
		# as 0/0 would hide that behind an unchanged total.
		printf '  FAIL  %-26s no summary line (rc=%d, %ds) -- %s\n' \
		       "${name}" "${rc}" "${elapsed}" "${log}"
		SUITES_BAD=$((SUITES_BAD + 1))
		FAILED_NAMES="${FAILED_NAMES}  ${name}: produced no summary (see ${log})"$'\n'
		return 1
	fi

	PASS_TOTAL=$((PASS_TOTAL + p))
	FAIL_TOTAL=$((FAIL_TOTAL + f))

	local xc xf xp note=""
	xc="$(parse_xcounts <"${log}")"
	if [ -n "${xc}" ]; then
		xf="${xc%% *}"
		xp="${xc##* }"
		XFAIL_TOTAL=$((XFAIL_TOTAL + xf))
		XPASS_TOTAL=$((XPASS_TOTAL + xp))
		[ "${xf}" -gt 0 ] && note=" (${xf} xfail)"
		if [ "${xp}" -gt 0 ]; then
			note="${note} (${xp} XPASS -- a backend capability changed)"
			XPASS_NAMES="${XPASS_NAMES}  ${name}: ${xp} assertion(s) now pass that were expected to fail (see ${log})"$'\n'
		fi
	fi

	if [ "${rc}" -eq 0 ] && [ "${f}" -eq 0 ]; then
		printf '  ok    %-26s %s/%s in %ds%s\n' "${name}" "${p}" "$((p + f))" \
		       "${elapsed}" "${note}"
		SUITES_OK=$((SUITES_OK + 1))
		return 0
	fi

	printf '  FAIL  %-26s %s failed of %s (rc=%d, %ds) -- %s\n' \
	       "${name}" "${f}" "$((p + f))" "${rc}" "${elapsed}" "${log}"
	SUITES_BAD=$((SUITES_BAD + 1))
	FAILED_NAMES="${FAILED_NAMES}  ${name}: ${f} assertion(s) failed (see ${log})"$'\n'
	return 1
}

# --------------------------------------------------------------------------
# What the environment allows
# --------------------------------------------------------------------------
HAVE_ROOT=0
[ "$(id -u)" -eq 0 ] && HAVE_ROOT=1

# Credentials: whatever is already exported wins, so a run can use a different
# account than s3.cfg names without editing it.
HAVE_CREDS=0
if [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
	HAVE_CREDS=1
elif [ -r "${S3_CFG}" ]; then
	# shellcheck source=../scripts/rcow_common.sh
	. "${ROOT}/scripts/rcow_common.sh"
	if rcow_load_credentials 2>/dev/null; then
		HAVE_CREDS=1
	fi
fi

ENDPOINT=""; BUCKET=""; REGION=""
if [ -r "${S3_CFG}" ]; then
	ENDPOINT="$(sed -n 's/^[[:space:]]*endpoint[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "${S3_CFG}" | head -1)"
	REGION="$(sed -n 's/^[[:space:]]*region[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "${S3_CFG}" | head -1)"
fi

# Kept out of a ${VAR:-$(...)} default: nesting a command substitution that
# itself contains both quote kinds inside a parameter expansion is where bash's
# quote parsing stops agreeing with the obvious reading, and it fails as a syntax
# error at the end of the file rather than here.
BUCKET="${S3LVOL_TEST_BUCKET:-}"
if [ -z "${BUCKET}" ] && [ -r "${S3_CFG}" ]; then
	BUCKET="$(sed -n 's/^[[:space:]]*buckets[[:space:]]*=[[:space:]]*\[\(.*\)\].*/\1/p' "${S3_CFG}" |
		head -1 | tr ',' '\n' |
		sed -n 's/^[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
fi
[ -n "${REGION}" ] || REGION=us-east-1

# A path-style, plain-HTTP backend (MinIO in CI) has to be addressed
# differently from the default virtual-hosted, HTTPS S3. The S3 config may
# carry both flags; whatever run_all.sh reads is exported so the dataplane
# scripts fold it into their rcow_add_s3_config calls too.
S3LVOL_TEST_PATH_STYLE=0
S3LVOL_TEST_NO_TLS=0
if [ -r "${S3_CFG}" ]; then
	[ "$(sed -n 's/^[[:space:]]*path_style[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
		"${S3_CFG}" | head -1)" = "true" ] && S3LVOL_TEST_PATH_STYLE=1
	[ "$(sed -n 's/^[[:space:]]*no_tls[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
		"${S3_CFG}" | head -1)" = "true" ] && S3LVOL_TEST_NO_TLS=1
fi
# The same two flags in the string form s3_prefix_rm.py takes, so count_objects
# and remove_prefix in the dataplane scripts can address the backend identically.
S3LVOL_TEST_S3FLAGS=""
[ "${S3LVOL_TEST_PATH_STYLE}" -eq 1 ] && S3LVOL_TEST_S3FLAGS+=" --path-style"
[ "${S3LVOL_TEST_NO_TLS}" -eq 1 ] && S3LVOL_TEST_S3FLAGS+=" --no-tls"
export S3LVOL_TEST_PATH_STYLE S3LVOL_TEST_NO_TLS S3LVOL_TEST_S3FLAGS

HAVE_S3=0
[ "${HAVE_CREDS}" -eq 1 ] && [ -n "${ENDPOINT}" ] && [ -n "${BUCKET}" ] && HAVE_S3=1

S3_ARGS=(-e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}")

if [ "${MODE}" = list ]; then
	echo "offline integration: spawner thread_bounce journal wal cache flush export"
	echo "                     statefile local_dev checkpoint"
	echo "with S3:             s3_client_test s3_bs_dev_test"
	echo "dataplane:           dataplane recovery snapshot export selfimport decouple_queue snapdelete fs guards activation control"
	echo ""
	echo "root:        $([ "${HAVE_ROOT}" -eq 1 ] && echo yes || echo no)"
	echo "credentials: $([ "${HAVE_CREDS}" -eq 1 ] && echo yes || echo no)"
	echo "endpoint:    ${ENDPOINT:-(none)}"
	echo "bucket:      ${BUCKET:-(none)}"
	echo "region:      ${REGION}"
	exit 0
fi

LOG_DIR="$(mktemp -d /tmp/s3lvol_testrun.XXXXXX)"
STARTED="${SECONDS}"

echo "=== s3lvol test run, logs in ${LOG_DIR}"
echo ""

# --------------------------------------------------------------------------
# Build first
#
# Not a courtesy: the dataplane scripts refuse to run against a binary older
# than its sources (check_binary_fresh.sh), so a stale tree turns the whole
# second half into skips. Building here makes that impossible rather than
# reported.
# --------------------------------------------------------------------------
echo "--- building"
if ! make --no-print-directory >"${LOG_DIR}/build.log" 2>&1; then
	echo "  FAIL  build -- see ${LOG_DIR}/build.log" >&2
	tail -20 "${LOG_DIR}/build.log" | sed 's/^/        /' >&2
	exit 1
fi
if ! make --no-print-directory -C test/integration >"${LOG_DIR}/build_tests.log" 2>&1; then
	echo "  FAIL  building the tests -- see ${LOG_DIR}/build_tests.log" >&2
	tail -20 "${LOG_DIR}/build_tests.log" | sed 's/^/        /' >&2
	exit 1
fi
echo "  ok    libraries, target and test binaries are current"
echo ""

# --------------------------------------------------------------------------
# Layering rules. Cheap, and a violation invalidates the premise the
# integration tests are built on (that lib/ links on its own).
# --------------------------------------------------------------------------
echo "--- layering rules"
if ./test/tools/check_layering.sh >"${LOG_DIR}/layering.log" 2>&1; then
	echo "  ok    all three hold"
	SUITES_OK=$((SUITES_OK + 1))
else
	rc=$?
	if [ "${rc}" -eq 2 ]; then
		report_skip "layering" "the check could not run (no compiler)"
	else
		echo "  FAIL  see ${LOG_DIR}/layering.log"
		grep -A4 '\[FAIL\]' "${LOG_DIR}/layering.log" | sed 's/^/        /'
		SUITES_BAD=$((SUITES_BAD + 1))
		FAILED_NAMES="${FAILED_NAMES}  layering: a rule is broken (see ${LOG_DIR}/layering.log)"$'\n'
	fi
fi
echo ""

# --------------------------------------------------------------------------
# Integration, no S3
# --------------------------------------------------------------------------
echo "--- offline tools"
run_suite s3_bucket_selftest python3 ./test/tools/s3_bucket.py --self-test
run_suite isa_baseline ./test/tools/test_isa_baseline.sh
run_suite rpc_py38_compat ./test/tools/test_rpc_py38_compat.sh
echo ""

echo "--- integration (no S3, no root)"
for t in s3_spawner_test s3_thread_bounce_test s3_journal_test s3_wal_test \
	 s3_cache_test s3_flush_test s3_export_test s3_statefile_test \
	 s3_local_dev_test s3_checkpoint_test; do
	run_suite "${t}" "./test/integration/${t}"
done
echo ""

# --------------------------------------------------------------------------
# Integration, real S3
# --------------------------------------------------------------------------
if [ "${MODE}" = offline ]; then
	echo "--- integration (real S3): skipped, --offline"
	report_skip "s3_client_test"  "--offline" 1
	report_skip "s3_bs_dev_test"  "--offline" 1
elif [ "${HAVE_S3}" -eq 0 ]; then
	echo "--- integration (real S3)"
	report_skip "s3_client_test" "no credentials or no endpoint/bucket"
	report_skip "s3_bs_dev_test" "no credentials or no endpoint/bucket"
else
	echo "--- integration (real S3: ${BUCKET} @ ${ENDPOINT})"
	# One assertion here is a known COS behaviour difference, not a
	# regression: COS ignores If-None-Match: *, so the probe the test makes
	# for it fails by design (HANDOFF 5.5). It is left failing rather than
	# skipped so that the day COS starts honouring it does not go unnoticed.
	# A MinIO backend honours it and produces an XPASS, which is a capability
	# gain and accepted by the suite's result accounting.
	S3_ADDR_ARGS=()
	[ "${S3LVOL_TEST_PATH_STYLE}" -eq 1 ] && S3_ADDR_ARGS+=(--path-style)
	[ "${S3LVOL_TEST_NO_TLS}" -eq 1 ] && S3_ADDR_ARGS+=(--no-tls)
	run_suite s3_client_test ./test/integration/s3_client_test \
		--endpoint "${ENDPOINT}" --bucket "${BUCKET}" --region "${REGION}" \
		"${S3_ADDR_ARGS[@]}"
	run_suite s3_bs_dev_test ./test/integration/s3_bs_dev_test \
		--endpoint "${ENDPOINT}" --bucket "${BUCKET}" --region "${REGION}" \
		"${S3_ADDR_ARGS[@]}"
fi
echo ""

# --------------------------------------------------------------------------
# Dataplane
# --------------------------------------------------------------------------
dataplane_blocker()
{
	local left

	[ "${HAVE_ROOT}" -eq 1 ] || { echo "not root"; return 0; }
	[ "${HAVE_S3}" -eq 1 ] || { echo "no credentials or no endpoint/bucket"; return 0; }

	# A target from an earlier run holds the RPC socket and the lvstore, so
	# every one of these would fail on start-up in a way that looks like a
	# code problem.
	if declare -F rcow_target_instances >/dev/null 2>&1; then
		left="$(rcow_target_instances)"
		if [ -n "${left}" ]; then
			echo "an s3lvol_tgt is already running (pid $(printf '%s ' ${left}))"
			return 0
		fi
	elif pgrep -f s3lvol_tgt >/dev/null 2>&1; then
		echo "something matching s3lvol_tgt is already running"
		return 0
	fi

	# Volumes are recorded as active, which means either a live deployment or
	# a previous run that did not clean up. The control test refuses in this
	# state for the same reason.
	#
	# The content is parsed rather than tested with -s. The registry is JSON, so
	# an empty one is "{}" -- two bytes, which -s calls non-empty. That state is
	# entirely normal: rcow_stop.sh leaves the file behind with its entries
	# removed. Judged by size, one clean shutdown would skip six suites on every
	# subsequent run, and the report would say volumes are active when none are.
	# Measured, after deploying a package by hand and stopping it properly.
	local active_file="${RCOW_ACTIVE_FILE:-/data/cubelet/rcow/active_lvols}"
	if [ -s "${active_file}" ]; then
		local n
		n="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    # Unparseable is not the same as empty, and is worth stopping for: a
    # corrupt registry is exactly what the recorded volumes would be restored
    # from.
    print(-1)
else:
    print(len(d) if hasattr(d, "__len__") else -1)
' "${active_file}" 2>/dev/null || echo -1)"

		if [ "${n}" = "-1" ]; then
			echo "cannot parse ${active_file}; refusing to guess"
			return 0
		fi
		if [ "${n}" -gt 0 ]; then
			echo "${n} volume(s) recorded as active in ${active_file}"
			return 0
		fi
	fi

	echo ""
}

if [ "${MODE}" != all ]; then
	echo "--- dataplane: skipped, $([ "${MODE}" = offline ] && echo --offline || echo --no-dataplane)"
	for t in dataplane recovery snapshot export selfimport decouple_queue snapdelete fs guards activation control; do
		report_skip "run_${t}_test.sh" "not requested" 1
	done
else
	BLOCKER="$(dataplane_blocker)"
	if [ -n "${BLOCKER}" ]; then
		echo "--- dataplane"
		for t in dataplane recovery snapshot export selfimport decouple_queue snapdelete fs guards activation control; do
			report_skip "run_${t}_test.sh" "${BLOCKER}"
		done
	else
		echo "--- dataplane (root, real S3, exclusive use of this machine)"
		# activation and control last: they are the most sensitive to global
		# state, so they are also the most useful check that the rest tidied
		# up after themselves. They read s3.cfg on their own and take no
		# arguments.
		run_suite run_dataplane_test.sh \
			./test/dataplane/run_dataplane_test.sh "${S3_ARGS[@]}"
		run_suite run_recovery_test.sh \
			./test/dataplane/run_recovery_test.sh "${S3_ARGS[@]}"
		run_suite run_snapshot_test.sh \
			./test/dataplane/run_snapshot_test.sh "${S3_ARGS[@]}"
		run_suite run_export_test.sh \
			./test/dataplane/run_export_test.sh "${S3_ARGS[@]}"
		# Deleting the source volume of an export/import chain; consumes the
		# same manifests as export, so it stays next to it.
		run_suite run_srcdel_test.sh \
			./test/dataplane/run_srcdel_test.sh "${S3_ARGS[@]}"
		# Next to export, whose manifests it consumes.
		run_suite run_selfimport_test.sh \
			./test/dataplane/run_selfimport_test.sh
		# A queued decouple must not be snapshotable: snapshotting an esnap
		# clone moves its external snapshot onto the new snapshot, and the
		# queued decouple then fails its detach. Regression for the 64-node
		# failure; consumes export manifests like export does.
		run_suite run_decouple_queue_test.sh \
			./test/dataplane/run_decouple_queue_test.sh "${S3_ARGS[@]}"
		# The agent-template chain across two real s3lvol_tgt processes (only
		# the bucket is shared); consumes export manifests like export does.
		run_suite run_agent_template_test.sh \
			./test/dataplane/run_agent_template_test.sh "${S3_ARGS[@]}"
		# Snapshot deletion semantics; next to the suites that build clone chains.
		run_suite run_snapdelete_test.sh \
			./test/dataplane/run_snapdelete_test.sh
		# Pending-delete marks: refused -> marked -> skipped while blocked ->
		# retried once clear. Right after snapdelete: same delete path, but the
		# question is what a *refused* delete leaves behind.
		run_suite run_pending_delete_test.sh \
			./test/dataplane/run_pending_delete_test.sh
		# Mounts a real filesystem, so it goes after the block-level suites:
		# when both fail, the one that speaks in dd is the easier read.
		run_suite run_fs_test.sh \
			./test/dataplane/run_fs_test.sh
		# Its whole point is that it does not disturb host state, so it is
		# safe anywhere in the order; kept next to fs because both are recent.
		run_suite run_guards_test.sh \
			./test/dataplane/run_guards_test.sh
		run_suite run_activation_test.sh \
			./test/dataplane/run_activation_test.sh
		run_suite run_control_test.sh \
			./test/dataplane/run_control_test.sh
	fi
fi

# --------------------------------------------------------------------------
# Summary
# --------------------------------------------------------------------------
ELAPSED=$((SECONDS - STARTED))

echo ""
echo "=========================================================="
printf 'suites:     %d ok, %d failed, %d skipped\n' \
       "${SUITES_OK}" "${SUITES_BAD}" "${SUITES_SKIPPED}"
printf 'assertions: %d passed, %d failed' "${PASS_TOTAL}" "${FAIL_TOTAL}"
if [ "${XFAIL_TOTAL}" -gt 0 ] || [ "${XPASS_TOTAL}" -gt 0 ]; then
	printf ', %d expected-fail, %d unexpected-pass' \
	       "${XFAIL_TOTAL}" "${XPASS_TOTAL}"
fi
printf '\n'
printf 'time:       %dm%02ds\n' "$((ELAPSED / 60))" "$((ELAPSED % 60))"
echo "logs:       ${LOG_DIR}"

if [ -n "${XPASS_NAMES}" ]; then
	echo ""
	echo "backend capabilities changed (not a failure, but do not ignore):"
	printf '%s' "${XPASS_NAMES}"
fi

if [ -n "${SKIPPED_NAMES}" ]; then
	echo ""
	echo "skipped:"
	printf '%s' "${SKIPPED_NAMES}"
fi
if [ -n "${FAILED_NAMES}" ]; then
	echo ""
	echo "failed:"
	printf '%s' "${FAILED_NAMES}"

	# Worth saying here rather than leaving to be rediscovered. A dataplane
	# script that fails deliberately keeps its S3 prefix and its bstore.json
	# entry, so that the failure can be looked into -- but that also means the
	# next run of the same script finds its lvstore already recorded, takes the
	# attach path instead of create, and fails in ways that have nothing to do
	# with the original problem ("it did not take the create path", "could not
	# create ctl-a"). Measured, on the second run of this very script.
	case "${FAILED_NAMES}" in
	*run_*_test.sh*)
		echo ""
		echo "note: a failed dataplane script keeps its S3 prefix and its"
		echo "      /data/cubelet/rcow/bstore.json entry on purpose, for diagnosis."
		echo "      Clear them before re-running, or the next run fails on the"
		echo "      attach path for unrelated-looking reasons:"
		echo "        test/tools/s3_prefix_rm.py -e <ep> -b <bucket> -r <region> -p <lvs>/"
		echo "      and remove <lvs> from /data/cubelet/rcow/bstore.json"
		;;
	esac
fi
echo "=========================================================="

# Exit status. A skip that was asked for is fine; a skip forced by the
# environment is not, because "it did not run" and "it passed" must not share an
# exit code -- that is exactly how a CI job goes green without testing anything.
if [ "${SUITES_BAD}" -gt 0 ] || [ "${FAIL_TOTAL}" -gt 0 ]; then
	echo "FAILED" >&2
	exit 1
fi
if [ "${SUITES_SKIPPED}" -gt 0 ] && [ "${SKIPPED_BY_CHOICE}" -eq 0 ]; then
	echo "PASSED, but some suites could not run -- see 'skipped' above" >&2
	exit 2
fi
echo "PASSED"
exit 0
