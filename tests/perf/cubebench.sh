#!/bin/bash
# cubebench.sh - all-in-one performance benchmark script for CubeSandbox
#
# Run it from any machine that can reach a deployed CubeSandbox, typically the
# single-host install itself or the control node of a multi-node cluster. It
# needs cubemastercli plus a reachable CubeAPI and CubeProxy; everything else it
# discovers.
#
# Where you run it matters for one section:
#   - Section 3.3 measures memory with free(1) on the local machine, so it only
#     runs on a single-node deployment whose Cubelet is this machine. It is
#     skipped, with a note in the report, everywhere else.
#
# WARNING:
#   The run subcommand destroys ALL existing sandboxes before every measurement,
#   on every node of the cluster. Do not point it at a deployment with sandboxes
#   you care about. Existing templates are left untouched.
#
# PREREQUISITE 1 - build cube-bench (needed by the 3.x sections):
#
#     make -C examples/cube-bench
#
#   The 4.x sections instead need python3-venv plus network access, so the
#   script can pip install the cubesandbox SDK into a throwaway venv.
#
# PREREQUISITE 2 - raise the host quota on every Cubelet node:
#   The benchmark creates hundreds of sandboxes per node. With the default
#   host.quota, CubeMaster rejects creations with "no more resource" long before
#   the real capacity limit is reached, so the numbers are useless. Edit the
#   Cubelet dynamic config on each node (see CUBELET_CONF below, default
#   /usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml):
#
#     host:
#       scheduler_label: "default-cluster"
#       quota:
#         mcpu_limit: 1000000
#         mem_limit: "4096Gi"
#         mvm_limit: 2000
#         creation_concurrent_num: 0   # 0 means unlimited
#
#   then restart Cubelet on that node so the new quota takes effect:
#
#     systemctl restart cube-sandbox-cubelet.service
#
#   The run subcommand checks this machine's config on startup and warns when the
#   values look too low, but it never changes them and never blocks the run. On a
#   cluster it can only see the node it runs on; check the others yourself.
#
#   WARNING: a raised quota lets the benchmark pack more sandboxes than the node's
#   RAM can actually back, so the density test (section 3.3) can drive the host
#   into a whole-machine OOM. Leave memory headroom and watch it while running.
#
# TROUBLESHOOTING - two failures show up often enough to name here:
#
#   "no more resource" (CubeMaster rejects the creation)
#     The node ran out of host.quota, not out of hardware. Raise the quota in
#     Cubelet/dynamicconf/conf.yaml as described in PREREQUISITE 2 and restart
#     Cubelet. Note that 0 or "" does not mean unlimited: Cubelet then derives
#     the quota from the node's real CPU and memory, which the benchmark
#     exhausts quickly.
#
#   HTTP 408 (the request times out)
#     Usually the host is stuck reclaiming memory. Check whether kswapd0 is
#     burning CPU, which typically comes from dirty page writeback:
#
#       top -b -n 2 -d 1 | grep kswapd
#       vmstat 1 5            # watch bo (writeback) and wa (io wait)
#
#     If kswapd0 is hot, leave the host more free memory (benchmark a lower
#     density) or tune writeback, e.g. vm.dirty_ratio / vm.dirty_background_ratio,
#     so reclaim is not waiting behind disk IO.
#
# Usage:
#   bash cubebench.sh [run]                 # run every section, emit a markdown report
#   bash cubebench.sh run 4.3               # run a single section
#   bash cubebench.sh 4.1 4.3               # same, "run" may be omitted
#   bash cubebench.sh run 4                 # run a whole chapter (all 4.x sections)
#   bash cubebench.sh run -y 4.3            # skip the destructive-run confirmation prompt
#   bash cubebench.sh sections              # list the selectable sections
#   bash cubebench.sh help
#
#   -y, --yes   skip the "destroys ALL sandboxes" confirmation prompt (same as
#               CUBE_BENCH_YES=1); may appear anywhere among the section ids
#
# Sections (selectable individually, always reported in this order):
#   3.2  Cold-Start Latency and Concurrency Scaling
#   3.3  Single-Host Deployment Density (Memory Overhead)
#   4.1  Snapshot Creation vs Concurrency
#   4.2  Snapshot Creation vs Dirty Page Size
#   4.3  Create Sandbox from Snapshot
#   4.4  Rollback
#   4.5  Clone
#   4.6  Pause / Resume
#
# Environment variables (run subcommand), all optional:
#   CUBE_TEMPLATE_IMAGE   test image, default cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
#                         (outside mainland China: cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest)
#   CUBE_API_URL          CubeAPI base URL, default http://127.0.0.1:3000
#   CUBE_API_KEY          CubeAPI key, default e2b_000000 (any non-empty value for a local deployment)
#   CUBE_MASTER_IP        cubemaster address, default 127.0.0.1
#                         (multi-node cluster: the control node)
#   CUBE_MASTER_PORT      cubemaster port, default 8089
#   CUBE_OPS_IP           CubeOps address used to list all registered nodes; defaults to CUBE_MASTER_IP
#   CUBE_OPS_PORT         CubeOps internal API port, default 3010
#   CUBE_PROXY_NODE_IP    CubeProxy address used to reach sandboxes, default 127.0.0.1
#   CUBE_PROXY_PORT_HTTP  CubeProxy HTTP port, default 80
#   OUTPUT_MD             report output path, default /tmp/cubebench_<timestamp>.md
#   PYTHON_BIN            python interpreter, default python3
#   PIP_INDEX_URL         read by pip itself; set it when the host cannot reach PyPI directly,
#                         e.g. https://mirrors.tencent.com/pypi/simple/
#   CUBELET_CONF          Cubelet dynamic config whose host.quota is checked on startup and
#                         shown in the report,
#                         default /usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml
#   CUBE_BENCH_YES        set to 1 to skip the "destroys ALL sandboxes" confirmation prompt
#                         (same as -y/--yes); REQUIRED for non-interactive runs — without a
#                         terminal and without this opt-in the run aborts rather than assuming consent
#   CUBE_BENCH_TPL_TIMEOUT  seconds to wait for the test template to reach READY, default 300;
#                         raise it for a cold image pull or a slow registry
#   CUBE_BENCH_RESET_TIMEOUT  base seconds to wait for a between-section reset to drain, default 60;
#                         the effective wait adds a per-sandbox budget on top of this floor
#   CUBE_BENCH_RESET_PER_SANDBOX_MS  extra drain budget per outstanding sandbox in ms, default 150;
#                         scales the reset wait with tier size so large tiers do not warn spuriously
#   CUBE_BENCH_RESET_POLL_INTERVAL  seconds between drain polls, default auto (2 + count/200, capped
#                         at 15); each poll is a full cluster list, so this scales with the drain size
#   CUBE_BENCH_MVM_MAX    hard cap on total sandboxes the memory-density sweep (3.3) will create,
#                         default 2000; stops the sweep before the host OOMs even if creation only
#                         trickles instead of cleanly stalling
#
# Example:
#   # full report on a single-host deployment, from the repository root
#   bash tests/perf/cubebench.sh
#
#   # cluster deployment: point at the control node, skip the memory density test
#   CUBE_MASTER_IP=10.0.0.5 CUBE_API_URL=http://10.0.0.5:3000 CUBE_PROXY_NODE_IP=10.0.0.6 \
#     bash tests/perf/cubebench.sh run 3.2 4
#
#   # only "4.3 Create Sandbox from Snapshot", report to a separate file
#   OUTPUT_MD=/tmp/cubebench-4.3.md bash tests/perf/cubebench.sh run 4.3

color_echo() {
	# Leveled, colorful log line with a uniform "[LEVEL] message" layout. The
	# level is inferred from the message prefix so callers keep writing plain
	# messages: ERROR -> red, WARN/WARNING -> yellow, HINT/SUGGESTION -> cyan,
	# everything else -> green INFO. Any level word the caller wrote is stripped
	# so it is not duplicated, e.g. "ERROR: boom" and "boom" both render as a
	# tagged line, "[ERROR] boom" / "[INFO] boom".
	local RED='\e[31m' YELLOW='\e[33m' GREEN='\e[32m' CYAN='\e[36m'
	local NC='\e[0m' # No Color
	local msg="$1" color="$GREEN" label="INFO"
	case "$msg" in
	ERROR:* | ERROR\ *)
		label="ERROR"
		color="$RED"
		;;
	WARNING:* | WARNING\ * | WARN:* | WARN\ *)
		label="WARNING"
		color="$YELLOW"
		;;
	HINT:* | HINT\ *)
		label="HINT"
		color="$CYAN"
		;;
	SUGGESTION:* | SUGGESTION\ *)
		label="SUGGESTION"
		color="$CYAN"
		;;
	esac
	# Strip the caller's leading level word ("ERROR:", "WARN ", ...) so the tag
	# is not printed twice.
	if [ "$label" != "INFO" ]; then
		msg="${msg#"$label"}" # exact label (ERROR, WARNING, ...)
		msg="${msg#WARN}"     # the short WARN alias maps to WARNING
		msg="${msg#:}"
		msg="${msg# }"
	fi
	# Pass the message through %s so a $1 containing % or \ is not read as a
	# format string. The "[LABEL]" field is left-padded to a fixed width so the
	# message columns line up like dmesg (widest tag is "[SUGGESTION]" = 12).
	# Indented continuation lines (e.g. list items) are left untagged so their
	# alignment survives.
	case "$1" in
	[[:space:]]*) printf "${color}%s${NC}\n" "$1" ;;
	*) printf "${color}%-12s %s${NC}\n" "[$label]" "$msg" ;;
	esac
}

welcome_echo() {
	# Banner shown once at startup
	local CYAN='\e[36m' NC='\e[0m'
	printf "${CYAN}%s${NC}\n" "This is CubeSandbox Core Operations Performance Benchmark"
	color_echo "NOTE: only the memory-density test (3.3) is single-node-only; all other sections work against a multi-node cluster"
}

usage() {
	# Print the comment block at the top of the file (skipping the shebang), stop at the first non-comment line
	awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
}

abs_path() {
	# $1: path, $2: base directory for relative paths. The path does not need to exist
	case "$1" in
	/*) printf '%s\n' "$1" ;;
	*) printf '%s\n' "${2%/}/$1" ;;
	esac
}

require_cmds() {
	local missing=() c
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 || missing+=("$c")
	done
	if [ "${#missing[@]}" -gt 0 ]; then
		color_echo "ERROR: missing required command(s): ${missing[*]}" >&2
		return 1
	fi
}

# ---------------------------------------------------------------------------
# Embedded bench_table.py: summarize cube-bench json reports into pipe-delimited rows
# ---------------------------------------------------------------------------
write_bench_table_py() {
	cat >"$1" <<'BENCH_TABLE_PY_EOF'
#!/usr/bin/env python3
"""bench_table - Turn cube-bench JSON reports into markdown table rows.

Usage: python3 bench_table.py report.json [report2.json ...]
"""

import sys
import json


def get_stat(doc):
    cfg = doc.get("config", {}) or {}
    summ = doc.get("summary", {}) or {}
    stat = doc.get("create") or doc.get("delete") or {}

    total = cfg.get("total") or stat.get("count")
    # Number of sandboxes actually started (SANDBOX_COUNT)
    successful = summ.get("successful")
    tts = summ.get("total_time_s")
    qps = summ.get("throughput_qps")

    # Amortize over the number of sandboxes actually created, not the requested
    # total. summary.total_time_s is cfg.elapsed in cube-bench: the wall-clock time
    # of the whole run, INCLUDING failed requests (cube-bench never retries, so a
    # slow HTTP 408 still burns up to the 120s client timeout). This column is
    # therefore the effective wall time per successful sandbox and is inflated by
    # failed-request overhead in the "no more resource"/408 cases; it is NOT the
    # per-request create latency reported by the avg/min/p95/max columns. Dividing
    # by the request count instead would understate it whenever creations fail.
    amortized_over = successful if successful is not None else stat.get("count")

    amortized = None
    if tts is not None and amortized_over:
        amortized = tts / amortized_over * 1000.0
    elif qps:
        amortized = 1000.0 / qps

    return {
        "concurrency": cfg.get("concurrency"),
        "requests":    total,
        "successful":  successful,
        "avg":         stat.get("avg"),
        "min":         stat.get("min"),
        "p95":         stat.get("p95"),
        "max":         stat.get("max"),
        "amortized":   amortized,
        "qps":         qps,
    }


def fmt_ms(x):
    return f"{x:.1f} ms" if x is not None else "-"


def fmt_qps(x):
    return f"{x:.1f} /s" if x is not None else "-"


def fmt_int(x):
    return str(x) if x is not None else "-"


def fmt_requests(r):
    # When the success count differs from the request count, append the actual
    # SANDBOX_COUNT, e.g. "500 (386)"
    total = r["requests"]
    successful = r.get("successful")
    if total is None:
        return "-"
    if successful is not None and successful != total:
        return f"{total} ({successful})"
    return str(total)


def make_row(r):
    """Build: | conc | reqs | avg | min | p95 | max | amortized | qps |"""
    return [
        fmt_int(r["concurrency"]),
        fmt_requests(r),
        fmt_ms(r["avg"]),
        fmt_ms(r["min"]),
        fmt_ms(r["p95"]),
        fmt_ms(r["max"]),
        fmt_ms(r["amortized"]),
        fmt_qps(r["qps"]),
    ]


def print_rows(rows):
    body = [make_row(r) for r in rows]
    # Column widths used for alignment
    widths = [max(len(cells[i]) for cells in body) for i in range(len(body[0]))]
    for cells in body:
        print("| " + " | ".join(c.ljust(widths[i]) for i, c in enumerate(cells)) + " |")


def main():
    rows = []
    for path in sys.argv[1:]:
        try:
            with open(path) as f:
                doc = json.load(f)
        except Exception as e:
            print(f"[!] failed {path}: {e}", file=sys.stderr)
            continue
        docs = doc if isinstance(doc, list) else [doc]
        for d in docs:
            rows.append(get_stat(d))

    if not rows:
        print("[!] no data", file=sys.stderr)
        sys.exit(1)

    rows.sort(key=lambda r: (r["concurrency"] or 0))
    print_rows(rows)


if __name__ == "__main__":
    main()
BENCH_TABLE_PY_EOF
}

# ---------------------------------------------------------------------------
# Embedded to_md.py: convert fixed-width bench_*.py output into markdown data rows
# (the header row is supplied by the caller in shell; this only emits data rows)
# ---------------------------------------------------------------------------
write_to_md_py() {
	cat >"$1" <<'TO_MD_PY_EOF'
#!/usr/bin/env python3
"""to_md - Convert whitespace-aligned benchmark output into markdown table rows.

With --no-header, bench_*.py prints one record per line with space-aligned fields
and no spaces inside a field, so splitting on whitespace recovers the cells.

Usage: python3 to_md.py data.txt [data2.txt ...]
"""

import sys


def is_numeric(cell):
    return cell.lstrip("-+").replace(".", "", 1).isdigit()


def clean(cell):
    # A negative number is a sentinel for "unavailable", not a real reading (e.g.
    # bench_snapshot_dirty.py emits -1.0 for the dirty-page column when it cannot
    # read VMM_LOG). No benchmark metric here is ever legitimately negative, so
    # render it as "-" rather than a real-looking value.
    if is_numeric(cell) and cell.lstrip("+").startswith("-"):
        return "-"
    return cell


def rows(stream):
    for raw in stream:
        cells = raw.split()
        if not cells:
            continue
        # Keep data rows only: the first column is numeric, which skips rule
        # lines and any header row that slipped in
        if not is_numeric(cells[0]):
            continue
        yield "| " + " | ".join(clean(c) for c in cells) + " |"


def main():
    printed = 0
    for path in sys.argv[1:]:
        try:
            with open(path) as f:
                lines = list(rows(f))
        except OSError as e:
            print(f"[!] failed {path}: {e}", file=sys.stderr)
            continue
        for line in lines:
            print(line)
            printed += 1

    if printed == 0:
        print("[!] no data", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
TO_MD_PY_EOF
}

PYTHON_BIN="${PYTHON_BIN:-python3}"
CUBELET_CONF="${CUBELET_CONF:-/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml}"

# This script ships inside the CubeSandbox repository, so cube-bench and the
# 4.x bench scripts are located relative to it instead of being configured
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." 2>/dev/null && pwd)"
EXAMPLES_DIR="${REPO_ROOT}/examples/snapshot-rollback-clone"

# Recommended host.quota for a benchmark host; see the PREREQUISITE block above
QUOTA_MIN_MCPU=1000000
QUOTA_MIN_MEM_GI=4096
QUOTA_MIN_MVM=2000

# ---------------------------------------------------------------------------
# Section registry: every benchmark section can be selected individually
# ---------------------------------------------------------------------------
ALL_SECTIONS="3.2 3.3 4.1 4.2 4.3 4.4 4.5 4.6"

section_title() {
	case "$1" in
	3.2) echo "3.2 Cold-Start Latency and Concurrency Scaling" ;;
	3.3) echo "3.3 Single-Host Deployment Density (Memory Overhead)" ;;
	4.1) echo "4.1 Snapshot Creation vs Concurrency" ;;
	4.2) echo "4.2 Snapshot Creation vs Dirty Page Size" ;;
	4.3) echo "4.3 Create Sandbox from Snapshot" ;;
	4.4) echo "4.4 Rollback" ;;
	4.5) echo "4.5 Clone" ;;
	4.6) echo "4.6 Pause / Resume" ;;
	esac
}

section_summary() {
	case "$1" in
	3.2) echo "creation latency, wall time per successful sandbox and throughput when creating sandboxes in batches at different concurrency levels." ;;
	3.3) echo "launch sandboxes in batches to measure single-host capacity and per-instance memory overhead." ;;
	4.1) echo "snapshot creation time as concurrency grows." ;;
	4.2) echo "snapshot creation time as the dirty page size grows." ;;
	4.3) echo "time to start sandboxes from a Snapshot." ;;
	4.4) echo "time to roll back to a Snapshot." ;;
	4.5) echo "time to clone a sandbox." ;;
	4.6) echo "time to pause and resume a sandbox." ;;
	esac
}

run_section() {
	# Tee this section's stderr into a per-section log while still showing it on
	# the terminal, then scan the log for the known failures. The 4.x sections
	# call the python SDK, whose "ApiError: HTTP 408" surfaces only on stderr and
	# would otherwise never reach hint_known_failures (only cube-bench logs do).
	local log="$WORK_DIR/section_${1//./_}.stderr"
	local rc=0 tee_fd="" tee_pid=""
	# Open the tee process substitution into a dedicated fd and capture ITS pid
	# right away. Grabbing $! immediately after creating the substitution (before
	# the section body runs) makes the reap robust: reading $! only after the block
	# would instead return the last background job the section itself started, so
	# any future section that backgrounds a worker would orphan this tee. Closing
	# the fd afterwards sends EOF to tee so it flushes the log before we scan it.
	# INVARIANT (bash 4.1+/5.x): nothing may start a background job between this
	# exec and the tee_pid=$! capture below — otherwise $! would point at that job,
	# wait would return early, and hint_known_failures could scan the log before
	# tee has flushed its final lines. Keep the two statements adjacent.
	#
	# If the exec fails (e.g. fd exhaustion) tee_fd stays empty; degrade loudly by
	# running the section with its stderr on the terminal only, so we never wait on
	# a stale/wrong PID or silently drop the section's error log. hint_known_failures
	# then has no per-section log to scan, but the 3.x path still scans $CUBE_BENCH_LOG.
	if exec {tee_fd}> >(tee "$log" >&2); then
		tee_pid=$!
	else
		color_echo "WARN: could not open section log fd; section stderr will not be scanned for known-failure hints" >&2
		tee_fd=""
	fi
	{
		case "$1" in
		3.2) section_3_2 ;;
		3.3) section_3_3 ;;
		4.1) section_4_1 ;;
		4.2) section_4_2 ;;
		4.3) section_4_3 ;;
		4.4) section_4_4 ;;
		4.5) section_4_5 ;;
		4.6) section_4_6 ;;
		esac
	} 2>&"${tee_fd:-2}" || rc=$?
	if [ -n "$tee_fd" ]; then
		exec {tee_fd}>&-
		# Closing our copy of the fd normally gives tee EOF so it exits promptly.
		# But a process backgrounded by a section body would INHERIT tee_fd and
		# hold it open, so tee would never see EOF and an unbounded `wait` would
		# block the whole harness forever. No section backgrounds anything today,
		# but bound the wait defensively: poll (fine-grained, so the normal path
		# costs ~0.1s) for tee to exit on its own, then force it closed so
		# hint_known_failures still runs. `wait` reaps the pid either way (only
		# this shell can reap its own child, so a `timeout`-wrapped wait cannot).
		local waited=0
		while [ "$waited" -lt 300 ] && kill -0 "$tee_pid" 2>/dev/null; do
			sleep 0.1
			waited=$((waited + 1))
		done
		if kill -0 "$tee_pid" 2>/dev/null; then
			color_echo "WARN: section log writer (tee pid ${tee_pid}) still open after $((waited / 10))s; forcing it closed" >&2
			kill "$tee_pid" 2>/dev/null || true
		fi
		wait "$tee_pid" 2>/dev/null || true
		hint_known_failures "$log"
	fi
	return "$rc"
}

parse_sections() {
	# Turn the caller's arguments into a canonical, deduplicated section list.
	# Accepts exact ids ("4.3"), comma separated lists ("3.2,4.1") and chapter
	# prefixes ("4" selects every 4.x section). Empty input selects everything.
	local raw="$*"
	raw="${raw//,/ }"
	if [ -z "${raw// /}" ]; then
		printf '%s\n' "$ALL_SECTIONS"
		return 0
	fi

	local tok s matched selected=""
	for tok in $raw; do
		matched=0
		for s in $ALL_SECTIONS; do
			if [ "$tok" = "$s" ] || [ "$tok" = "${s%%.*}" ]; then
				selected="$selected $s"
				matched=1
			fi
		done
		if [ "$matched" -eq 0 ]; then
			color_echo "ERROR: unknown section: $tok (valid: ${ALL_SECTIONS}, or chapter 3 / 4)" >&2
			return 1
		fi
	done

	# Emit in registry order, dropping duplicates
	local ordered=""
	for s in $ALL_SECTIONS; do
		case " $selected " in
		*" $s "*) ordered="$ordered $s" ;;
		esac
	done
	printf '%s\n' "${ordered# }"
}

# ---------------------------------------------------------------------------
# Subcommand: sections  (list what can be selected)
# ---------------------------------------------------------------------------
cmd_sections() {
	local s
	for s in $ALL_SECTIONS; do
		section_title "$s"
	done
}

# ---------------------------------------------------------------------------
# Subcommand: run  (drive the selected sections, write the markdown report)
# ---------------------------------------------------------------------------
cmd_run() {
	local orig_pwd sections section need_examples=0 need_cube_bench=0
	local assume_yes="${CUBE_BENCH_YES:-0}"
	orig_pwd="$(pwd)"

	welcome_echo

	# Pull the -y/--yes flag out of the arguments so the rest are section ids.
	# It has the same effect as CUBE_BENCH_YES=1: skip the destructive-run prompt.
	local -a rest=()
	for arg in "$@"; do
		case "$arg" in
		-y | --yes) assume_yes=1 ;;
		*) rest+=("$arg") ;;
		esac
	done
	set -- "${rest[@]}"

	sections="$(parse_sections "$@")" || exit 1
	case " $sections " in
	*" 4."*) need_examples=1 ;;
	esac
	case " $sections " in
	*" 3."*) need_cube_bench=1 ;;
	esac

	# Resolve the report path up front (the script cd's around later) so it can be
	# announced before the run starts. The default name carries a timestamp so
	# repeated runs do not clobber each other's reports.
	local default_md="/tmp/cubebench_$(date '+%Y%m%d_%H%M%S').md"
	OUTPUT_MD="$(abs_path "${OUTPUT_MD:-$default_md}" "$orig_pwd")"
	color_echo "report: $OUTPUT_MD"

	color_echo "selected sections:"
	for section in $sections; do
		color_echo "  - $(section_title "$section")"
	done

	# Resolve the master endpoint defaults before the warning below so the
	# destructive-run prompt names the cluster that is actually about to be wiped.
	# Deferring these to after the prompt (as an earlier revision did) printed
	# "on every node of :" for the default invocation, defeating the whole point
	# of showing the operator which deployment is targeted.
	export CUBE_MASTER_IP="${CUBE_MASTER_IP:-127.0.0.1}"
	export CUBE_MASTER_PORT="${CUBE_MASTER_PORT:-8089}"
	export CUBE_OPS_IP="${CUBE_OPS_IP:-$CUBE_MASTER_IP}"
	export CUBE_OPS_PORT="${CUBE_OPS_PORT:-3010}"

	# Prove the harness can actually operate BEFORE asking the operator to
	# authorize a destructive run. If a dependency is missing or cubemaster is
	# unreachable, the run would fail downstream anyway — but only after the
	# operator has already typed "yes" to wipe a cluster the harness never runs
	# against (e.g. the default CUBE_MASTER_IP=127.0.0.1 pointing nowhere on a
	# real cluster). So check deps and reachability first; both only need the
	# CUBE_MASTER_IP/PORT defaults resolved just above.
	require_cmds "${PYTHON_BIN}" awk jq cubemastercli cubeopscli || exit 1

	# Talking to CubeMaster is the first thing that can fail on a cluster, where
	# the default 127.0.0.1 is wrong; say so here instead of failing later on. A
	# "0" total (NODES_SCANNED 0/0: no healthy Cubelets, or a wrong IP that
	# answers with an empty cluster) is as unusable as an empty read — creating
	# the template would fail later with a more confusing error — so treat both
	# as unreachable and surface the real problem up front.
	local nodes
	nodes="$(get_node_count)"
	if [ -z "$nodes" ] || [ "$nodes" = "0" ]; then
		color_echo "ERROR: cubemaster at ${CUBE_MASTER_IP}:${CUBE_MASTER_PORT} reported no usable Cubelet nodes; set CUBE_MASTER_IP / CUBE_MASTER_PORT (and check that Cubelets are healthy)" >&2
		exit 1
	fi
	color_echo "cubemaster ${CUBE_MASTER_IP}:${CUBE_MASTER_PORT}, cubelet nodes: ${nodes}"

	# This run is destructive: it destroys ALL sandboxes on EVERY node of the
	# target cluster, and the default endpoints (CUBE_MASTER_IP=127.0.0.1,
	# CUBE_API_URL=http://127.0.0.1:3000) can plausibly reach an unintended but
	# live deployment. So require an explicit opt-in and never treat "no TTY" as
	# consent: automation must pass -y/--yes or CUBE_BENCH_YES=1 on purpose. With a
	# TTY and no opt-in, prompt interactively; without a TTY and no opt-in, abort.
	color_echo "WARNING: this run will destroy ALL existing sandboxes on every node of ${CUBE_MASTER_IP}:${CUBE_MASTER_PORT}" >&2
	if [ "$assume_yes" != "1" ]; then
		if [ ! -t 0 ]; then
			color_echo "ERROR: refusing to run destructively without a terminal; pass -y/--yes or set CUBE_BENCH_YES=1 to opt in" >&2
			exit 1
		fi
		local reply
		printf 'Type "yes" to continue: ' >&2
		read -r reply
		if [ "$reply" != "yes" ]; then
			color_echo "ERROR: aborted by user (expected \"yes\", got \"${reply}\")" >&2
			exit 1
		fi
	fi

	check_host_quota

	# CUBE_MASTER_IP / CUBE_MASTER_PORT were already defaulted above (before the
	# reachability probe), so only the CubeProxy/CubeAPI vars remain here.
	# The 4.x bench scripts reach sandboxes through CubeProxy on this host;
	# without these the SDK would need DNS for *.cube.app
	export CUBE_PROXY_NODE_IP="${CUBE_PROXY_NODE_IP:-127.0.0.1}"
	export CUBE_PROXY_PORT_HTTP="${CUBE_PROXY_PORT_HTTP:-80}"
	# The python SDK reads CUBE_API_*, cube-bench reads the E2B-compatible names
	export CUBE_API_URL="${CUBE_API_URL:-http://127.0.0.1:3000}"
	export CUBE_API_KEY="${CUBE_API_KEY:-e2b_000000}"
	export E2B_API_URL="$CUBE_API_URL"
	export E2B_API_KEY="$CUBE_API_KEY"

	# cubecli clears sandboxes much faster than deleting them one by one, but it
	# only clears the node it runs on, so it is safe only when this host is the
	# cluster's single node. "nodes" from `list --all` counts HEALTHY Cubelets only
	# (Total is GetHealthyNodesByInstanceType().Len()), so a 2-node cluster with one
	# peer down would also report 1 — and cubecli would leave the down peer's
	# sandboxes behind while list --all (also healthy-only) never sees them, so the
	# reset would look clean when it is not. Prove single-node with CubeOps'
	# authoritative `node list`, which returns ALL registered nodes regardless of
	# health; only take the fast path when the total node count is exactly 1 (and
	# this host actually runs Cubelet). Otherwise fall back to the cluster-wide
	# destroy, which at least covers every currently-healthy node.
	USE_CUBECLI=0
	if [ "$nodes" = "1" ] && [ -r "$CUBELET_CONF" ] && command -v cubecli >/dev/null 2>&1; then
		local total_nodes
		total_nodes="$(get_total_node_count)"
		if [ "$total_nodes" = "1" ]; then
			USE_CUBECLI=1
		else
			color_echo "WARN: cluster has ${total_nodes:-an unknown number of} total node(s) but only 1 healthy; using cluster-wide destroy instead of the local cubecli fast path" >&2
		fi
	fi

	# cube-bench is always the copy built from this checkout, so its version
	# matches the repo commit shown in the report
	CUBE_BENCH_BIN="${REPO_ROOT}/examples/cube-bench/bin/cube-bench"
	if [ "$need_cube_bench" -eq 1 ] && [ ! -x "$CUBE_BENCH_BIN" ]; then
		color_echo "cube-bench not built yet: $CUBE_BENCH_BIN"
		color_echo "building it with: make -C ${REPO_ROOT}/examples/cube-bench"
		if ! make -C "${REPO_ROOT}/examples/cube-bench"; then
			color_echo "ERROR: failed to build cube-bench, build it manually: make -C ${REPO_ROOT}/examples/cube-bench" >&2
			exit 1
		fi
		if [ ! -x "$CUBE_BENCH_BIN" ]; then
			color_echo "ERROR: cube-bench still not executable after build: $CUBE_BENCH_BIN" >&2
			exit 1
		fi
	fi

	# Only the 4.x sections need the bench scripts shipped with the repository
	if [ "$need_examples" -eq 1 ] && [ ! -d "$EXAMPLES_DIR" ]; then
		color_echo "ERROR: bench script directory not found: $EXAMPLES_DIR" >&2
		color_echo "run this script from its place in a CubeSandbox checkout (tests/perf/cubebench.sh)" >&2
		exit 1
	fi

	if ! mkdir -p "$(dirname "$OUTPUT_MD")" || ! : >"$OUTPUT_MD"; then
		color_echo "ERROR: cannot write report file: $OUTPUT_MD" >&2
		exit 1
	fi

	WORK_DIR="$(mktemp -d)" || exit 1
	# Clean up the temporary work directory (venv, json reports, ...) on exit
	trap 'cd /; [ -n "$WORK_DIR" ] && [ -d "$WORK_DIR" ] && rm -rf -- "$WORK_DIR"' EXIT
	cd "$WORK_DIR" || exit 1

	CUBE_BENCH_LOG="$WORK_DIR/cube-bench.log"
	DATA_TXT="$WORK_DIR/data.txt"

	BENCH_TABLE="$WORK_DIR/bench_table.py"
	write_bench_table_py "$BENCH_TABLE"
	TO_MD="$WORK_DIR/to_md.py"
	write_to_md_py "$TO_MD"

	# Reset before building the template, and honor the status like every
	# section-level env_reset does. Ignoring it here would let a stuck destroy or
	# an undrained straggler leave the cluster dirty while the template is built
	# and section 1 runs, so a drain failure that clears before section 1 would
	# still have measured against contaminated state — contrary to the script's
	# "refuse to measure against a dirty cluster" guarantee.
	if ! env_reset; then
		color_echo "ERROR: initial cluster reset failed: ${SECTION_FAIL_REASON:-unknown}" >&2
		exit 1
	fi

	CUBE_TEMPLATE_IMAGE="${CUBE_TEMPLATE_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest}"
	CUBE_TEMPLATE_ID="$(create_tpl "${CUBE_TEMPLATE_IMAGE}")"
	export CUBE_TEMPLATE_ID
	if [ -z "$CUBE_TEMPLATE_ID" ] || [ "$CUBE_TEMPLATE_ID" = "null" ]; then
		color_echo "ERROR: failed to create template from ${CUBE_TEMPLATE_IMAGE}" >&2
		exit 1
	fi

	wait_status_to_ready "${CUBE_TEMPLATE_ID}" || exit 1

	# Section titles and measured data are appended to $OUTPUT_MD (truncated at startup)

	# ---- ## 1 Overview ----
	{
		echo "# CubeSandbox Core Operations Performance Benchmark Report"
		echo ""
		echo "> Generated at: $(date '+%Y-%m-%d %H:%M:%S')"
		echo ""
		echo "## 1 Overview"
		echo ""
		echo "This report benchmarks CubeSandbox across the following dimensions:"
		echo ""
		for section in $sections; do
			echo "- **$(section_title "$section")**: $(section_summary "$section")"
		done
		echo ""
	} | tee -a "$OUTPUT_MD"

	# ---- ## 2 Test Environment ----
	print_test_env

	# When 4.x sections are selected, set up the SDK venv now so the report can
	# show the concrete installed SDK version. A failure here is not fatal: the
	# owning section reports it later and sdk_version falls back to the spec.
	if [ "$need_examples" -eq 1 ]; then
		prepare_snapshot_bench || true
	fi

	# ---- ## 3 Sandbox Spec and Test Image ----
	{
		echo "## 3 Sandbox Spec and Test Image"
		echo ""
		echo "| Item | Detail |"
		echo "|:--|:--|"
		echo "| Test Image | ${CUBE_TEMPLATE_IMAGE} |"
		echo "| Template ID | ${CUBE_TEMPLATE_ID} |"
		echo "| Writable Layer Size | 1G |"
		echo "| Expose Ports | 49999, 49983 |"
		echo "| Probe Port | 49999 |"
		echo "| Cube CA | false |"
		echo "| SDK Version | $(sdk_version) |"
		echo "| Repo Commit | $(repo_commit) |"
		echo ""
	} | tee -a "$OUTPUT_MD"

	for section in $sections; do
		# Use ### so the "3.2 / 4.1 ..." sections nest as subsections rather than
		# becoming siblings of the report's own "## N ..." chapters above
		emit_heading "### $(section_title "$section")"
		SECTION_FAIL_REASON=""
		if ! run_section "$section"; then
			{
				echo "> Section skipped: ${SECTION_FAIL_REASON:-setup failed, see the terminal output}."
				echo ""
			} | tee -a "$OUTPUT_MD"
		fi
	done

	env_reset

	color_echo "report: $OUTPUT_MD"
	cat "$OUTPUT_MD"

	# env_reset only clears sandboxes; the 4.x snapshot/clone runs leave templates
	# and snapshots behind that this script does not track, so tell the operator
	# to sweep them manually rather than silently accumulating them across runs
	color_echo "HINT: leftover templates/snapshots (tpl/snap) created by the 4.x runs may remain; review and clean them up manually"
}

# ---------------------------------------------------------------------------
# Section bodies. Each one leaves the report table in $OUTPUT_MD and is safe to
# run on its own, so it cd's to whatever directory its benchmark needs.
# ---------------------------------------------------------------------------
section_3_2() {
	cd "$WORK_DIR" || return 1

	# Remove stale reports in the current directory only; no -r since these are files
	rm -f ./*.json

	# No warmup (-w): cube-bench folds the warmup wall time into total_time_s /
	# throughput_qps, and in create-only mode the warmup sandboxes are never
	# deleted, so they would both distort the amortized/throughput columns and
	# push print_sandbox_count above the reported request count. This section
	# also measures cold-start latency, which warmed connections would mask.
	# Track tier failures like the 4.x sections do, but note cube-bench exits
	# non-zero whenever ANY single request errored (main.go: os.Exit(1) on
	# hasErrors), so a tier that created 19 of 20 sandboxes still counts as
	# "failed" here. The JSON report is written regardless and carries valid rows,
	# so a per-tier non-zero exit only flags the tier as incomplete, not empty.
	local failed=0 total=4
	env_reset || return 1
	run_cube_bench -c 1 -n 20 -m create-only -o report_c1.json || failed=$((failed + 1))
	print_sandbox_count

	env_reset || return 1
	run_cube_bench -c 10 -n 200 -m create-only -o report_c10.json || failed=$((failed + 1))
	print_sandbox_count

	env_reset || return 1
	run_cube_bench -c 20 -n 300 -m create-only -o report_c20.json || failed=$((failed + 1))
	print_sandbox_count

	env_reset || return 1
	run_cube_bench -c 50 -n 500 -m create-only -o report_c50.json || failed=$((failed + 1))
	print_sandbox_count

	shopt -s nullglob
	local reports=(./*.json)
	shopt -u nullglob

	# "All failed" must key on whether ANY sandbox was actually created, not on the
	# CLI exit status: when every tier has a single straggler (e.g. one HTTP 408
	# per 500 requests) each exits non-zero yet the rows are almost entirely good,
	# so a "failed >= total" check would wrongly discard a near-complete run. Sum
	# summary.successful across the reports and only skip the section when it is 0.
	local created=0
	if [ "${#reports[@]}" -gt 0 ]; then
		created="$("${PYTHON_BIN}" - "${reports[@]}" <<'PY' 2>/dev/null || echo 0
import json, sys
n = 0
for p in sys.argv[1:]:
    try:
        with open(p) as f:
            doc = json.load(f)
    except Exception:
        continue
    for d in (doc if isinstance(doc, list) else [doc]):
        n += (d.get("summary", {}) or {}).get("successful") or 0
print(n)
PY
)"
	fi
	# No report produced a single sandbox: emit no misleading table, mark skipped.
	if [ "${created:-0}" -eq 0 ]; then
		SECTION_FAIL_REASON="no sandbox was created in any tier, see the terminal output"
		return 1
	fi

	{
		echo "| Concurrency | Requests (successful) | avg | min | p95 | max | Wall/successful sandbox | Throughput |"
		echo "|:----:|:------:|----:|----:|----:|----:|----------:|-----:|"
		"${PYTHON_BIN}" "${BENCH_TABLE}" "${reports[@]}"
		echo ""
		if [ "$failed" -gt 0 ]; then
			echo "> Partial: ${failed} of ${total} tier(s) had at least one failed request; the successful column shows the sandbox count actually created per tier."
			echo ""
		fi
	} | tee -a "$OUTPUT_MD"
}

section_3_3() {
	# Memory is read from this machine's free(1), so the result only means
	# something when every sandbox is created here
	local nodes reason=""
	if [ ! -r "$CUBELET_CONF" ]; then
		reason="this machine does not run Cubelet (no ${CUBELET_CONF})"
	else
		# Count TOTAL nodes via CubeOps' authoritative `node list`, not
		# get_node_count's `list --all` which is HEALTHY-only. On a
		# 2-node cluster with one peer down, the healthy count is 1, so the density
		# test would run — but once the peer rejoins, get_sandbox_count (also
		# list --all) starts counting its sandboxes while free(1) on this host never
		# sees their memory, diluting the per-VM overhead. The reset path already
		# proves single-node with get_total_node_count for the same reason; use it
		# here too.
		nodes="$(get_total_node_count)"
		if [ -z "$nodes" ] || [ "$nodes" = "0" ]; then
			# Empty means a list error or a malformed response; "0" means node list
			# returned an empty data array (jq -e also exits non-zero). Neither is a
			# usable single-node proof — an unknown/zero node count could be a
			# multi-node cluster, and this section would then attribute cluster-wide
			# memory to this single host. Skip instead, matching the comment's
			# "refusing to guess this is a single-node deployment".
			reason="could not determine the Cubelet node count (cubeopscli node list error or no nodes); refusing to guess this is a single-node deployment"
		elif [ "$nodes" -gt 1 ]; then
			reason="the cluster has ${nodes} Cubelet nodes, so sandboxes are spread over hosts this script cannot measure"
		fi
	fi
	if [ -n "$reason" ]; then
		{
			echo "> Section skipped: ${reason}. Run it on a single-node deployment, or on one compute node with the others drained."
			echo ""
		} | tee -a "$OUTPUT_MD"
		return 0
	fi

	env_reset || return 1
	run_mvm_bench
}

section_4_1() {
	prepare_snapshot_bench || return 1
	env_reset || return 1
	local failed=0 total=3
	: >"$DATA_TXT"
	run_bench_tier python bench_snapshot_concurrency.py -c 1 -n 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_snapshot_concurrency.py -c 5 -n 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_snapshot_concurrency.py -c 10 -n 5 --no-header || failed=$((failed + 1))

	emit_section_table "$failed" "$total" "$DATA_TXT" \
		"| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-snapshot avg |" \
		"|:----:|:----:|--------:|--------:|--------:|--------:|----------------:|"
}

section_4_2() {
	prepare_snapshot_bench || return 1
	env_reset || return 1
	# The Dirty Page column comes from bench_snapshot_dirty.py grepping the local
	# VMM log; off the Cubelet host that log is absent and the column renders as
	# "-" (a -1 sentinel), which reads like a real zero. Warn up front so an
	# all-"-" column is understood as "not measured here" rather than "no dirty
	# pages". The latency columns are still valid, so run the tiers regardless.
	local vmm_log="${VMM_LOG:-/data/log/CubeVmm/vmm.log}"
	local vmm_readable=1
	if [ ! -r "$vmm_log" ]; then
		vmm_readable=0
		color_echo "WARN: VMM log ${vmm_log} not readable here; the Dirty Page column will show '-'. Run on the Cubelet host or set VMM_LOG to measure snapshot bytes." >&2
	fi
	local failed=0 total=8 d
	: >"$DATA_TXT"
	for d in 0 10 50 100 200 500 800 1024; do
		run_bench_tier python bench_snapshot_dirty.py -d "$d" -n 3 --no-header || failed=$((failed + 1))
	done

	emit_section_table "$failed" "$total" "$DATA_TXT" \
		"| Write Size | Dirty Page | snapshot avg | snapshot min | snapshot p95 | snapshot max | create sandbox avg | create sandbox min | create sandbox p95 | create sandbox max |" \
		"|:------:|----------:|------------:|------------:|------------:|------------:|------------------:|------------------:|------------------:|------------------:|"
	local rc=$?

	# The unreadable-VMM-log warning above only reaches stderr, so a reader of the
	# .md alone sees an unexplained all-"-" Dirty Page column and could read it as
	# "no dirty pages" rather than "not measured". Mirror the density test's
	# incomplete_note and persist the caveat into the report artifact itself.
	# Append it BEFORE honoring the table's exit status: when every tier fails the
	# section is skipped, and that is exactly when the reader most needs to know
	# the Dirty Page column would not have been measured here anyway.
	if [ "$vmm_readable" -eq 0 ]; then
		{
			echo "> Dirty Page not measured: the VMM log (${vmm_log}) was not readable on this host, so that column shows \"-\". Run on the Cubelet host or set VMM_LOG to measure snapshot bytes."
			echo ""
		} | tee -a "$OUTPUT_MD"
	fi
	return "$rc"
}

section_4_3() {
	prepare_snapshot_bench || return 1
	env_reset || return 1
	local failed=0 total=4
	: >"$DATA_TXT"
	run_bench_tier python bench_create_concurrency.py -c 1 -n 3 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_create_concurrency.py -c 10 -n 3 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_create_concurrency.py -c 20 -n 3 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_create_concurrency.py -c 50 -n 3 --no-header || failed=$((failed + 1))

	emit_section_table "$failed" "$total" "$DATA_TXT" \
		"| Concurrency | n/round | Rounds | wall avg | wall min | wall p95 | wall max | per-sandbox avg |" \
		"|:----:|:------:|:----:|--------:|--------:|--------:|--------:|----------------:|"
}

section_4_4() {
	prepare_snapshot_bench || return 1
	env_reset || return 1
	local failed=0 total=3
	: >"$DATA_TXT"
	run_bench_tier python bench_rollback_concurrency.py -c 1 -n 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_rollback_concurrency.py -c 5 -n 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_rollback_concurrency.py -c 10 -n 5 --no-header || failed=$((failed + 1))

	emit_section_table "$failed" "$total" "$DATA_TXT" \
		"| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-rollback avg |" \
		"|:----:|:----:|--------:|--------:|--------:|--------:|-----------------:|"
}

section_4_5() {
	prepare_snapshot_bench || return 1
	env_reset || return 1
	local failed=0 total=4
	: >"$DATA_TXT"
	run_bench_tier python bench_clone_concurrency.py -n 1 -c 1 --rounds 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_clone_concurrency.py -n 100 -c 10 --rounds 2 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_clone_concurrency.py -n 100 -c 20 --rounds 2 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_clone_concurrency.py -n 100 -c 50 --rounds 2 --no-header || failed=$((failed + 1))

	emit_section_table "$failed" "$total" "$DATA_TXT" \
		"| n | Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-clone avg |" \
		"|:----:|:---:|:----:|--------:|--------:|--------:|--------:|-------------:|"
}

section_4_6() {
	prepare_snapshot_bench || return 1
	env_reset || return 1
	local failed=0 total=3
	: >"$DATA_TXT"
	run_bench_tier python bench_pause_resume_concurrency.py -c 1 -n 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_pause_resume_concurrency.py -c 5 -n 5 --no-header || failed=$((failed + 1))
	run_bench_tier python bench_pause_resume_concurrency.py -c 10 -n 5 --no-header || failed=$((failed + 1))

	emit_section_table "$failed" "$total" "$DATA_TXT" \
		"| Concurrency | Rounds | wall avg (Pause) | wall min | wall p95 | wall max | per-pause avg | wall avg (Resume) | wall min | wall p95 | wall max | per-resume avg |" \
		"|:---:|:----:|--------:|--------:|--------:|--------:|-------------:|--------:|--------:|--------:|--------:|--------------:|"
}

# ---------------------------------------------------------------------------
# Helper functions used by the run subcommand
# ---------------------------------------------------------------------------
env_reset() {
	# Every section must start from an empty cluster, otherwise leftovers are
	# counted as part of the next measurement. Returns non-zero and sets
	# SECTION_FAIL_REASON when it cannot GUARANTEE a clean cluster (list error or
	# the cluster did not drain), so the caller skips the section instead of
	# silently recording numbers measured against contaminated state.
	if [ "${USE_CUBECLI:-0}" = "1" ]; then
		color_echo "cubecli unsafe rm --all"
		cubecli unsafe rm --all >/dev/null 2>&1
	elif ! destroy_all_sandboxes; then
		SECTION_FAIL_REASON="could not list sandboxes to reset the cluster (cubemaster error)"
		return 1
	fi
	if ! wait_sandboxes_gone; then
		SECTION_FAIL_REASON="cluster did not drain to empty before the measurement"
		return 1
	fi
	return 0
}

destroy_all_sandboxes() {
	# Distinguish a genuinely empty cluster from a failed list: cubemastercli
	# returns non-zero on a transport error, empty response, or non-200 ret, and
	# with 2>/dev/null both cases produce empty output. Treating a list failure as
	# "nothing to destroy" would skip the reset and contaminate the next section,
	# so gate on the command's exit status, not just on $ids being empty.
	local ids
	if ! ids="$(cubemaster list --all --size 1000 --quiet 2>/dev/null)"; then
		color_echo "WARN: could not list sandboxes to destroy (cubemaster error); not treating this as an empty cluster" >&2
		return 1
	fi
	[ -n "$ids" ] || return 0
	color_echo "destroying $(printf '%s\n' "$ids" | wc -l) sandboxes through cubemaster"
	# The per-destroy exit status is intentionally not the gate here: a straggler
	# that fails to delete is caught by wait_sandboxes_gone confirming the cluster
	# actually reached 0, which is the real guarantee the reset needs.
	#
	# "destroy" is a subcommand of "cubebox", not a top-level cubemastercli command
	# (only cubebox.Command.Subcommands registers it), so it must be invoked as
	# "cubemastercli cubebox destroy". Without the "cubebox" prefix the CLI exits
	# non-zero with "No help topic for 'destroy'", which >/dev/null hides — the
	# reset would then delete nothing and every section would fail to drain.
	xargs -r -n 50 -P 4 cubemastercli --address "${CUBE_MASTER_IP}" \
		--port "${CUBE_MASTER_PORT}" cubebox destroy >/dev/null 2>&1 <<<"$ids"
	return 0
}

wait_sandboxes_gone() {
	# Destroying is asynchronous and a failed destroy is easy to miss, so confirm
	# the cluster is really empty instead of trusting the delete. Scale the wait
	# with the number of sandboxes still present: destroys go through serialized
	# `xargs -P 4 cubemastercli destroy` calls (a few hundred ms each), so at the
	# 500-per-tier scale of section 3.2 a fixed timeout is routinely exceeded and
	# the next tier would measure against a still-draining cluster. The floor and
	# the per-sandbox budget are both overridable.
	local base="${CUBE_BENCH_RESET_TIMEOUT:-60}"
	local per_ms="${CUBE_BENCH_RESET_PER_SANDBOX_MS:-150}"
	local elapsed=0 count timeout poll
	# A failed read (non-zero from get_sandbox_count) means cubemaster is
	# unreachable, not that the cluster is empty; fail fast rather than poll a full
	# timeout against a CLI that will keep erroring.
	if ! count="$(get_sandbox_count)"; then
		color_echo "WARN: could not read SANDBOX_COUNT from cubemaster; refusing to assume the cluster is empty" >&2
		return 1
	fi
	if [ "${count:-}" = "0" ]; then
		color_echo "SANDBOX_COUNT: 0"
		return 0
	fi
	# get_sandbox_count yields digits or empty; treat empty as 0 for the estimate
	timeout=$(( base + ( ${count:-0} * per_ms ) / 1000 ))
	# Each poll runs a full `list --all` that fetches and parses the entire
	# sandbox table (SANDBOX_COUNT is len(rsp.Data); there is no count-only
	# endpoint), so on a large drain that scan is the dominant cost of the reset.
	# Scale the poll interval with the outstanding count so a 500-sandbox drain is
	# not a full cluster scan every 2s, while a small drain stays responsive.
	# Overridable; a fixed value pins it.
	poll="${CUBE_BENCH_RESET_POLL_INTERVAL:-0}"
	if [ "$poll" -le 0 ]; then
		poll=$(( 2 + ${count:-0} / 200 ))
		[ "$poll" -gt 15 ] && poll=15
	fi
	color_echo "waiting up to ${timeout}s for ${count:-?} sandbox(es) to drain (poll every ${poll}s)"
	while [ "$elapsed" -lt "$timeout" ]; do
		sleep "$poll"
		elapsed=$((elapsed + poll))
		if ! count="$(get_sandbox_count)"; then
			color_echo "WARN: could not read SANDBOX_COUNT from cubemaster; refusing to assume the cluster is empty" >&2
			return 1
		fi
		if [ "${count:-}" = "0" ]; then
			color_echo "SANDBOX_COUNT: 0"
			return 0
		fi
	done
	# Timed out with sandboxes still present (a swallowed destroy failure, a stuck
	# straggler, or an unreadable count). Report failure so the caller skips the
	# section rather than recording numbers measured against a dirty cluster.
	color_echo "WARN: ${count:-an unknown number of} sandbox(es) still present after ${timeout}s, refusing to measure against a dirty cluster" >&2
	return 1
}

print_sandbox_count() {
	color_echo "SANDBOX_COUNT: $(get_sandbox_count)"
}

run_cube_bench() {
	# Funnel cube-bench output into one log; on failure report it with the log
	# tail. Truncate first so the tail and hint scan always reflect this run: a
	# section issues several invocations, and a failure that emits nothing (e.g.
	# the binary never started) would otherwise show the previous run's output.
	: >"$CUBE_BENCH_LOG"
	if ! "${CUBE_BENCH_BIN}" "$@" >>"$CUBE_BENCH_LOG" 2>&1; then
		color_echo "WARN: cube-bench failed: $*" >&2
		tail -n 20 "$CUBE_BENCH_LOG" >&2
		hint_known_failures "$CUBE_BENCH_LOG"
		return 1
	fi
}

run_bench_tier() {
	# Run one 4.x benchmark tier, appending its data row(s) to $DATA_TXT. Returns
	# non-zero when the tier fails so the caller can count partial failures. A
	# failed tier writes no row, and a "{ tierA; tierB; ... } >file" group reports
	# only the LAST tier's exit status, so without this per-tier check a failed
	# intermediate tier would silently drop from the table and still look complete.
	# stderr is left on the terminal (captured by run_section) so the SDK's
	# "ApiError: HTTP 408" still reaches hint_known_failures.
	if "$@" >>"$DATA_TXT"; then
		return 0
	fi
	color_echo "WARN: bench tier failed: $*" >&2
	return 1
}

hint_known_failures() {
	# Two errors dominate benchmark runs and neither is obvious from the raw
	# message, so name the usual cause instead of leaving it to the reader. Each
	# hint fires at most once per run: it is scanned from several logs (cube-bench
	# for 3.x, the section stderr for 4.x SDK failures) and the 408 hint text
	# itself contains "408", so an un-guarded re-scan would print it twice.
	[ -r "$1" ] || return 0
	local recent
	recent="$(tail -n 200 "$1" 2>/dev/null)"
	if [ -z "${HINT_QUOTA_SHOWN:-}" ] && printf '%s' "$recent" | grep -qi "no more resource"; then
		HINT_QUOTA_SHOWN=1
		color_echo "HINT: the node is out of host.quota, not out of hardware: raise it in ${CUBELET_CONF} and restart Cubelet" >&2
	fi
	# Matched narrowly on purpose: a bare "408" also shows up in latency numbers.
	# Also matches the python SDK form: "cubesandbox._exceptions.ApiError: HTTP 408".
	if [ -z "${HINT_408_SHOWN:-}" ] && printf '%s' "$recent" | grep -qiE '(http[^ ]{0,8} ?408|status[^0-9]{0,12}408|408 request timeout)'; then
		HINT_408_SHOWN=1
		color_echo "HINT: HTTP 408 usually means the host is stalled reclaiming memory; check kswapd0 CPU with 'top -b -n 2 -d 1 | grep kswapd' (high usage points at dirty page writeback)" >&2
	fi
}

snapshot_bench_failed() {
	# $1: reason, repeated in the report for every 4.x section since the setup is
	# attempted only once. $2: optional log file to show on the terminal.
	SNAPSHOT_BENCH_REASON="$1"
	SECTION_FAIL_REASON="$1"
	color_echo "ERROR: $1" >&2
	if [ -n "${2:-}" ] && [ -s "$2" ]; then
		tail -n 20 "$2" >&2
	fi
}

prepare_snapshot_bench() {
	# Shared setup for the 4.x sections: a venv with the cubesandbox SDK plus the
	# bench script directory as cwd. Done on first use so a 3.x-only run needs
	# neither python3-venv nor network access.
	local log="$WORK_DIR/venv.log"
	case "${SNAPSHOT_BENCH_STATE:-}" in
	ready) ;;
	failed)
		SECTION_FAIL_REASON="$SNAPSHOT_BENCH_REASON"
		return 1
		;;
	*)
		SNAPSHOT_BENCH_STATE=failed
		if ! "${PYTHON_BIN}" -m venv "$WORK_DIR/.venv" >"$log" 2>&1; then
			snapshot_bench_failed "cannot create a python venv with ${PYTHON_BIN}, install python3-venv" "$log"
			return 1
		fi
		# shellcheck source=/dev/null
		if ! . "$WORK_DIR/.venv/bin/activate"; then
			snapshot_bench_failed "cannot activate the venv in ${WORK_DIR}/.venv"
			return 1
		fi
		# Reuse the bench scripts' own requirements so the SDK version stays in
		# one place
		if ! pip install -r "$EXAMPLES_DIR/requirements.txt" >"$log" 2>&1; then
			snapshot_bench_failed "pip install -r ${EXAMPLES_DIR}/requirements.txt failed, set PIP_INDEX_URL to a reachable index if this host cannot use PyPI directly" "$log"
			return 1
		fi
		# Record the concrete SDK version now that it is installed, so the report
		# reflects what actually ran rather than the version spec
		SDK_INSTALLED_VERSION="$(read_installed_sdk_version)"
		SNAPSHOT_BENCH_STATE=ready
		;;
	esac

	if ! cd "$EXAMPLES_DIR"; then
		SECTION_FAIL_REASON="cannot enter ${EXAMPLES_DIR}"
		return 1
	fi
}

emit_heading() {
	printf '%s\n\n' "$1" | tee -a "$OUTPUT_MD"
}

emit_table() {
	# $1: data file, $2: markdown header row, $3: markdown separator row.
	# Convert first: a header with no rows below it would look like a benchmark
	# that measured nothing rather than one that failed.
	local data="$1" header="$2" sep="$3"
	local rows="${data}.md"
	if ! "${PYTHON_BIN}" "${TO_MD}" "$data" >"$rows" 2>/dev/null; then
		color_echo "WARN: no valid data in: $data" >&2
		{
			echo "> No data: the benchmark produced no parsable rows, see the terminal output."
			echo ""
		} | tee -a "$OUTPUT_MD"
		return 0
	fi
	{
		echo "$header"
		echo "$sep"
		cat "$rows"
		echo ""
	} | tee -a "$OUTPUT_MD"
}

emit_section_table() {
	# Render a section whose tiers were run as a group: $1 failed-tier count, $2
	# total tiers, then the emit_table args ($3 data, $4 header, $5 separator).
	# A grouped "{ python; python; ... } >file" only reports the last tier's exit
	# status, so a failed intermediate tier would otherwise vanish into a table
	# that looks like a complete measurement. Distinguish three outcomes:
	#   - every tier failed  -> mark the whole section skipped (no misleading table)
	#   - some tiers failed   -> emit the partial table and flag it as incomplete
	#   - all tiers succeeded -> emit the table as-is
	local failed="$1" total="$2" data="$3" header="$4" sep="$5"
	if [ "$failed" -ge "$total" ]; then
		SECTION_FAIL_REASON="all ${total} tier(s) failed, see the terminal output"
		return 1
	fi
	emit_table "$data" "$header" "$sep"
	if [ "$failed" -gt 0 ]; then
		{
			echo "> Partial: ${failed} of ${total} tier(s) failed; the rows above are incomplete, see the terminal output."
			echo ""
		} | tee -a "$OUTPUT_MD"
	fi
	return 0
}

cubemaster() {
	# Every call needs an explicit address: cubemastercli defaults to 0.0.0.0,
	# which only resolves when it runs next to CubeMaster itself
	cubemastercli --address "${CUBE_MASTER_IP}" --port "${CUBE_MASTER_PORT}" "$@"
}

cubeops() {
	cubeopscli --address "${CUBE_OPS_IP}" --port "${CUBE_OPS_PORT}" "$@"
}

sdk_version() {
	# The cubesandbox SDK version the 4.x sections benchmark against. Prefer the
	# concrete version installed into the venv (populated by prepare_snapshot_bench
	# and cached in SDK_INSTALLED_VERSION); fall back to the pinned spec from the
	# bench scripts' requirements when the venv has not been set up (e.g. a
	# 3.x-only run).
	if [ -n "${SDK_INSTALLED_VERSION:-}" ]; then
		printf '%s\n' "$SDK_INSTALLED_VERSION"
		return 0
	fi
	local req="$EXAMPLES_DIR/requirements.txt" spec=""
	if [ -r "$req" ]; then
		spec="$(awk -F'#' '{print $1}' "$req" |
			awk 'tolower($0) ~ /cubesandbox/ {gsub(/[ \t]/, ""); print; exit}')"
	fi
	printf '%s\n' "${spec:-unknown}"
}

read_installed_sdk_version() {
	# Query the concrete cubesandbox version from the active venv. Called after
	# pip install so the report can show exactly what was benchmarked.
	pip show cubesandbox 2>/dev/null |
		awk -F': ' '/^Version:/ {print $2; exit}'
}

repo_commit() {
	# Identify exactly which checkout produced the report
	git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || printf 'unknown\n'
}

create_tpl() {
	local IMAGE="$1" out rc
	# Capture the CLI's stdout and exit status before parsing. Piping straight
	# into jq would report jq's status (0 on empty stdin), so a failed create
	# (quota/image error, "template already exists") — which exits non-zero and
	# prints nothing to stdout — would yield an empty template_id and the real
	# reason would only surface later as the caller's generic "failed to create
	# template". Leaving stderr un-redirected lets the CLI's error reach the
	# operator immediately, matching how get_sandbox_count / wait_status_to_ready
	# propagate the CLI status instead of masking it behind a pipe.
	out="$(cubemaster tpl create-from-image \
		--with-cube-ca=false \
		--image "${IMAGE}" \
		--writable-layer-size 1G \
		--expose-port 49999 \
		--expose-port 49983 \
		--probe 49999 \
		--json)"
	rc=$?
	if [ "$rc" -ne 0 ]; then
		color_echo "ERROR: template creation failed (cubemaster exit ${rc}); see the CLI error above" >&2
		return 1
	fi
	printf '%s' "$out" | jq -r '.job.template_id'
}

tpl_info() {
	# Dump template details for troubleshooting when the build fails or times out
	cubemaster tpl info --template-id "$1"
}

wait_status_to_ready() {
	# Poll tpl info until status is READY, otherwise time out and fail. The
	# default can be raised via CUBE_BENCH_TPL_TIMEOUT for a cold image pull or a
	# slow registry, where a legitimate build can exceed 300s.
	local template_id="$1"
	local timeout="${2:-${CUBE_BENCH_TPL_TIMEOUT:-300}}"
	local interval=5
	local elapsed=0
	local status
	local raw
	while [ "$elapsed" -lt "$timeout" ]; do
		# Capture the CLI output and its exit status before piping into jq: a
		# straight "cubemaster ... | jq" would report jq's status (0 even on empty
		# input), so a failed read (bad CUBE_MASTER_IP, template not yet visible, a
		# transient master hiccup) would yield an empty status that matches neither
		# case and silently burns the full timeout. Surface it as a distinct
		# "could not read" line and keep polling, since the failure is often
		# transient right after commit; the timeout below still bounds the wait.
		if ! raw="$(cubemaster tpl info --template-id "${template_id}" --json 2>&1)"; then
			color_echo "WARN: could not read template ${template_id} status (${elapsed}s/${timeout}s): ${raw}" >&2
			sleep "$interval"
			elapsed=$((elapsed + interval))
			continue
		fi
		status="$(printf '%s' "$raw" | jq -r '.status // empty')"
		if [ -z "$status" ]; then
			color_echo "WARN: template ${template_id} status unreadable (${elapsed}s/${timeout}s): ${raw}" >&2
			sleep "$interval"
			elapsed=$((elapsed + interval))
			continue
		fi
		color_echo "template ${template_id} status: ${status} (${elapsed}s/${timeout}s)"
		# Match READY exactly. A "*READY*" glob also swallows PARTIALLY_READY, a
		# distinct terminal status the master sets when only some node replicas
		# finished the build (templatecenter store StatusPartiallyReady); accepting
		# it as fully ready lets a benchmark run against an incompletely-replicated
		# template. Keep polling on PARTIALLY_READY so it either promotes to READY
		# or hits the timeout below.
		case "$status" in
		READY) return 0 ;;
		*FAILED*)
			color_echo "ERROR: template ${template_id} status: FAILED" >&2
			tpl_info "${template_id}"
			return 1
			;;
		esac
		sleep "$interval"
		elapsed=$((elapsed + interval))
	done
	color_echo "ERROR: template ${template_id} not READY after ${timeout}s (timeout)" >&2
	tpl_info "${template_id}"
	return 1
}

read_host_quota() {
	# Extract host.quota.<key> from a Cubelet dynamic config (YAML) without yq.
	# Emits four lines in fixed order: mcpu_limit, mem_limit, mvm_limit,
	# creation_concurrent_num; a missing key comes back as an empty line.
	local file="$1"
	[ -r "$file" ] || return 1
	awk '
		# Indent depth = leading whitespace of ANY kind. Counting only spaces
		# would make a tab-indented (or tab/space-mixed) config yield i=0 for every
		# line, so the reset branch below fires right after "host:" and drops the
		# host/quota context before any quota key is read — check_host_quota would
		# then report all keys "unset" and print_test_env show n/a.
		function ind(s){if(match(s,/^[ \t]+/))return RLENGTH; return 0}
		BEGIN{hi=-1; qi=-1}
		{
			sub(/\r$/,"")
			if ($0 ~ /^[[:space:]]*$/ || $0 ~ /^[[:space:]]*#/) next
			i=ind($0); ci=index($0,":"); if(ci==0) next
			key=substr($0,1,ci-1); val=substr($0,ci+1)
			gsub(/^[ \t]+|[ \t]+$/,"",key)
			sub(/[ \t]+#.*$/,"",val); gsub(/^[ \t]+|[ \t]+$/,"",val)
			if(hi>=0 && i<=hi && key!="host"){hi=-1; qi=-1}
			if(qi>=0 && i<=qi && key!="quota"){qi=-1}
			if(key=="host" && val==""){hi=i; qi=-1; next}
			if(hi>=0 && key=="quota" && i>hi){qi=i; next}
			if(qi>=0 && i>qi){
				if(key=="mcpu_limit") mcpu=val
				else if(key=="mem_limit") mem=val
				else if(key=="mvm_limit") mvm=val
				else if(key=="creation_concurrent_num") ccn=val
			}
		}
		END{print mcpu; print mem; print mvm; print ccn}
	' "$file"
}

unquote() {
	local v="$1"
	v="${v#[\"\']}"
	printf '%s\n' "${v%[\"\']}"
}

mem_to_gi() {
	# Normalize a quantity such as 4096Gi / 512G / 1048576Mi / 128GiB to whole
	# GiB, matching the k8s resource.ParseQuantity semantics Cubelet uses to
	# validate host.quota.mem_limit (Cubelet/pkg/config/config.go): binary
	# suffixes Ki/Mi/Gi/Ti are powers of 1024, decimal SI k/M/G/T are powers of
	# 1000 (lowercase k; uppercase K is not a valid suffix), m is milli, and a
	# bare number is bytes. Conflating decimal with binary (e.g. treating 4T as
	# 4*1024 instead of 4*10^12/2^30 = 3725 GiB) or rejecting valid lowercase
	# spellings would mislead check_host_quota right before a destructive run.
	# An optional trailing B is tolerated so GiB/MiB/GB spellings still parse.
	# Unparseable input prints nothing.
	awk -v v="$(unquote "$1")" 'BEGIN{
		if (match(v, /^[0-9]+(\.[0-9]+)?/) == 0) exit 0
		n = substr(v, 1, RLENGTH) + 0
		u = substr(v, RLENGTH + 1)
		gsub(/[ \t]/, "", u)
		sub(/B$/, "", u)
		gib = 1073741824                              # 2^30
		if      (u == "Ti") bytes = n * 1099511627776 # 2^40
		else if (u == "Gi") bytes = n * 1073741824    # 2^30
		else if (u == "Mi") bytes = n * 1048576       # 2^20
		else if (u == "Ki") bytes = n * 1024          # 2^10
		else if (u == "T")  bytes = n * 1000000000000 # 10^12
		else if (u == "G")  bytes = n * 1000000000    # 10^9
		else if (u == "M")  bytes = n * 1000000       # 10^6
		else if (u == "k")  bytes = n * 1000          # 10^3
		else if (u == "m")  bytes = n / 1000          # milli
		else if (u == "")   bytes = n                 # bare = bytes
		else exit 0                                   # unknown suffix
		printf "%d", bytes / gib
	}'
}

quota_below() {
	# $1: configured value, $2: recommended minimum. Empty or non-numeric
	# values count as below the minimum so they get reported too.
	awk -v v="$(unquote "$1")" -v min="$2" 'BEGIN{
		if (v !~ /^[0-9]+(\.[0-9]+)?$/) exit 0
		exit (v + 0 >= min + 0) ? 1 : 0
	}'
}

check_host_quota() {
	# The benchmark creates hundreds of sandboxes, so a default host.quota makes
	# CubeMaster fail with "no more resource". Only warn: the operator decides
	# whether the configured quota is intentional. Only this machine's config can
	# be inspected, so a cluster run has to be checked node by node.
	if [ ! -r "$CUBELET_CONF" ]; then
		color_echo "no Cubelet config at ${CUBELET_CONF}, host quota not verified from here"
		return 0
	fi

	local -a quota=()
	mapfile -t quota < <(read_host_quota "$CUBELET_CONF")
	local mcpu="${quota[0]:-}" mem="${quota[1]:-}" mvm="${quota[2]:-}" ccn="${quota[3]:-}"

	local -a low=()
	quota_below "$mcpu" "$QUOTA_MIN_MCPU" &&
		low+=("mcpu_limit=${mcpu:-unset} (recommended ${QUOTA_MIN_MCPU})")
	quota_below "$(mem_to_gi "$mem")" "$QUOTA_MIN_MEM_GI" &&
		low+=("mem_limit=${mem:-unset} (recommended ${QUOTA_MIN_MEM_GI}Gi)")
	quota_below "$mvm" "$QUOTA_MIN_MVM" &&
		low+=("mvm_limit=${mvm:-unset} (recommended ${QUOTA_MIN_MVM})")
	case "$(unquote "$ccn")" in
	0 | "") ;;
	*) low+=("creation_concurrent_num=${ccn} caps concurrent creation (0 means unlimited)") ;;
	esac

	if [ "${#low[@]}" -eq 0 ]; then
		return 0
	fi

	local item
	color_echo "SUGGESTION: host.quota in ${CUBELET_CONF} looks low for this benchmark; a low quota makes CubeMaster return 'no more resource' before the real capacity limit" >&2
	color_echo "SUGGESTION: (0 or unset is not unlimited: Cubelet then derives the quota from this node's real CPU and memory)" >&2
	for item in "${low[@]}"; do
		color_echo "  - ${item}" >&2
	done
	color_echo "SUGGESTION: after editing ${CUBELET_CONF}, apply it with: systemctl restart cube-sandbox-cubelet.service" >&2
	color_echo "WARNING: raising host.quota lets the benchmark pack more sandboxes than the node's RAM can back; the density test (section 3.3) then risks a whole-machine OOM. Leave headroom or watch memory while it runs." >&2
}

print_test_env() {
	local os kernel arch cpu_model cpu_cfg numa mem disk
	local nodes local_cubelet=0
	local -a quota=()
	if [ -r "$CUBELET_CONF" ]; then
		local_cubelet=1
		mapfile -t quota < <(read_host_quota "$CUBELET_CONF")
	fi
	local mcpu_limit mem_limit mvm_limit ccn
	mcpu_limit="$(unquote "${quota[0]:-}")"
	mem_limit="$(unquote "${quota[1]:-}")"
	mvm_limit="$(unquote "${quota[2]:-}")"
	ccn="$(unquote "${quota[3]:-}")"
	[ -n "$mcpu_limit" ] || mcpu_limit="n/a"
	[ -n "$mem_limit" ] || mem_limit="n/a"
	[ -n "$mvm_limit" ] || mvm_limit="n/a"
	[ -n "$ccn" ] || ccn="n/a"

	nodes="$(get_node_count)"
	[ -n "$nodes" ] || nodes="unknown"

	os="$(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-unknown}")"
	[ -n "$os" ] || os="unknown"
	kernel="$(uname -sr)"
	arch="$(uname -m)"

	cpu_model="$(lscpu 2>/dev/null | awk -F: '/^Model name/{gsub(/^[ \t]+/,"",$2); print $2; exit}')"
	[ -z "$cpu_model" ] && cpu_model="$(awk -F: '/model name/{gsub(/^[ \t]+/,"",$2); print $2; exit}' /proc/cpuinfo 2>/dev/null)"
	[ -z "$cpu_model" ] && cpu_model="unknown"

	cpu_cfg="$(lscpu 2>/dev/null | awk -F: '
		/^CPU\(s\)/            {gsub(/^[ \t]+/,"",$2); cpus=$2}
		/^Socket\(s\)/         {gsub(/^[ \t]+/,"",$2); sock=$2}
		/^Core\(s\) per socket/{gsub(/^[ \t]+/,"",$2); cps=$2}
		/^Thread\(s\) per core/{gsub(/^[ \t]+/,"",$2); tpc=$2}
		END{printf "%s vCPU (%s socket x %s core/socket x %s thread/core)", cpus, sock, cps, tpc}')"
	# lscpu absent -> the awk above still prints the template with empty fields, so
	# test for that (a bare "vCPU" with no leading digit) as well as a truly empty
	# string, and fall back to unknown to keep the table cell well-formed.
	case "$cpu_cfg" in
	'' | ' vCPU '*) cpu_cfg="unknown" ;;
	esac

	numa="$(lscpu 2>/dev/null | awk -F: '/^NUMA node\(s\)/{gsub(/^[ \t]+/,"",$2); print $2; exit}')"
	[ -z "$numa" ] && numa="unknown"

	# LC_ALL=C so free(1) prints the C-locale "Mem:" header the awk pattern below
	# expects; procps-ng localizes it (e.g. "Speicher:", "Mémoire:") and would
	# otherwise fall through to the /proc/meminfo fallback on a non-English host.
	mem="$(LC_ALL=C free -h 2>/dev/null | awk '/^Mem:/{print $2; exit}')"
	[ -z "$mem" ] && mem="$(awk '/MemTotal/{printf "%.1f GiB", $2/1024/1024; exit}' /proc/meminfo 2>/dev/null)"
	[ -n "$mem" ] || mem="unknown"

	disk="$(lsblk -d -n -o NAME,SIZE,TYPE,MODEL 2>/dev/null | awk '$3=="disk"{printf "%s(%s) ", $1, $2}')"
	[ -z "$disk" ] && disk="$(df -h . 2>/dev/null | awk 'NR==2{print $1" "$2}')"
	[ -n "$disk" ] || disk="unknown"

	{
		echo "## 2 Test Environment"
		echo ""
		echo "| Item | Detail |"
		echo "|:--|:--|"
		echo "| OS | ${os} |"
		echo "| Kernel | ${kernel} |"
		echo "| Arch | ${arch} |"
		echo "| CPU Model | ${cpu_model} |"
		echo "| CPU Config | ${cpu_cfg} |"
		echo "| NUMA Nodes | ${numa} |"
		echo "| Total Memory | ${mem} |"
		echo "| Data Disk | ${disk} |"
		echo "| Cubelet Nodes | ${nodes} |"
		if [ "$local_cubelet" = "1" ]; then
			echo "| Host Quota mcpu_limit | ${mcpu_limit} |"
			echo "| Host Quota mem_limit | ${mem_limit} |"
			echo "| Host Quota mvm_limit | ${mvm_limit} |"
			echo "| Host Quota creation_concurrent_num | ${ccn} |"
		fi
		echo ""
		if [ "$local_cubelet" != "1" ]; then
			echo "> Note: the hardware above is the machine running the benchmark, which is not a Cubelet node; host quota is configured per Cubelet node and is not shown."
			echo ""
		fi
	} | tee -a "$OUTPUT_MD"
}

get_used_mem() {
	# free's "used" column (total - free - buff/cache on procps-ng, the standard
	# free on the distros this script targets) already excludes the page cache,
	# so it reports anonymous/VM memory directly. That is exactly the per-VM
	# overhead this section wants; no drop_caches dance is needed.
	#
	# "used" is $3 on procps-ng; other free implementations (busybox, older
	# Debian) lay the columns out differently, so fall back to /proc/meminfo the
	# way print_test_env falls back for the other hardware fields. procps-ng
	# reports used as MemTotal - MemAvailable, so prefer that (present since Linux
	# 3.14); only if MemAvailable is missing, approximate it by subtracting the
	# reclaimable caches.
	local used
	# LC_ALL=C pins the "Mem:" header (procps-ng localizes it); without it a
	# non-English locale silently skips this path and leans on the fallback below.
	used="$(LC_ALL=C free -m 2>/dev/null | awk '/^Mem:/ {print $3; exit}')"
	if [ -n "$used" ]; then
		printf '%s\n' "$used"
		return 0
	fi
	awk '
		/^MemTotal:/     {t=$2}
		/^MemFree:/      {f=$2}
		/^MemAvailable:/ {a=$2; have_a=1}
		/^Buffers:/      {b=$2}
		/^Cached:/       {c=$2}
		/^SReclaimable:/ {s=$2}
		END {
			if (!t) exit
			if (have_a) printf "%d\n", (t - a) / 1024
			else        printf "%d\n", (t - f - b - c - s) / 1024
		}
	' /proc/meminfo 2>/dev/null
}

get_sandbox_count() {
	# "list" prints "SANDBOX_COUNT<TAB>N"; keep the number only. Without --all it
	# would scan a single node and undercount on a multi-node cluster. --size
	# raises the page size (CLI default is 1, i.e. one sandbox per HTTP request),
	# which matters here because this runs on every 2s poll in wait_sandboxes_gone.
	# Capture the list output first: piping straight into awk would report awk's
	# exit status (always 0), so a failed CLI call (unreachable cubemaster, non-200)
	# would look identical to an empty cluster. Return non-zero instead so callers
	# can tell "could not read" from "genuinely zero". awk also exits non-zero when
	# the list succeeds but carries no SANDBOX_COUNT line (an unparseable response),
	# so that too is surfaced as a read failure rather than an empty-but-success
	# result that wait_sandboxes_gone would burn a full timeout on.
	local out
	out="$(cubemaster list --all --size 1000 2>/dev/null)" || return 1
	printf '%s\n' "$out" |
		awk '/SANDBOX_COUNT/ {gsub(/[^0-9]/, "", $2); print $2; found=1; exit}
		     END {exit !found}'
}

get_node_count() {
	# "list --all" reports "NODES_SCANNED<TAB>scanned/total"; keep the total.
	# Capture first for the same reason as get_sandbox_count: preserve the CLI's
	# exit status instead of masking it behind the awk pipe. The END guard makes a
	# successful-but-malformed response (no NODES_SCANNED line) a read failure too,
	# so callers can tell "could not read" from a real count instead of treating an
	# empty parse as a valid answer.
	local out
	out="$(cubemaster list --all --size 1000 2>/dev/null)" || return 1
	printf '%s\n' "$out" |
		awk '/NODES_SCANNED/ {split($2, a, "/"); print a[2]; found=1; exit}
		     END {exit !found}'
}

get_total_node_count() {
	# Unlike CubeMaster's `list --all` (which counts HEALTHY Cubelets only),
	# CubeOps is the authoritative node registry and returns EVERY node regardless
	# of health. Emit the count only when the request succeeds, so unreachable
	# CubeOps yields empty (never a bogus "0") and the caller keeps the safe
	# fallback. cubeopscli returns the node array directly, without a data envelope.
	local json
	json="$(cubeops node list --json 2>/dev/null)" || return 1
	printf '%s' "$json" | jq -e 'if type == "array" then length else error("expected node array") end' 2>/dev/null
}

run_mvm_bench() {
	local BATCH=100       # sandboxes launched per batch
	local expected=0      # expected total number of sandboxes
	local max_batches=200 # guards against an endless loop
	# Absolute ceiling on how many sandboxes the sweep will create. Under real RAM
	# pressure (as opposed to a hard mvm quota) creation degrades into a trickle of
	# 1-2 per batch rather than a clean zero, which resets the stall counter and
	# lets the loop run toward max_batches * BATCH create requests — driving the
	# host toward the whole-machine OOM this section warns about. Cap the total.
	local max_total="${CUBE_BENCH_MVM_MAX:-2000}"
	# Below this per-batch gain a batch counts as a stall even though it added a
	# few sandboxes, so the plateau of a RAM-pressure trickle is treated like the
	# clean zero-growth of a hard quota and stops the sweep instead of grinding on.
	local min_growth=$((BATCH / 10))
	[ "$min_growth" -lt 1 ] && min_growth=1
	local mem_baseline
	local iter=0
	local actual mem_now gained
	local prev_actual=0   # live count after the previous batch, to measure growth
	local stall=0         # consecutive batches that grew by less than min_growth
	local stall_limit=2   # declare the density limit only after this many stalls
	local created_any=0   # set once any batch has actually produced a sandbox
	local incomplete_note="" # non-empty when the loop stopped before the real limit
	local -a EXPECTED_ARR MEM_ARR

	# ---- baseline ----
	mem_baseline=$(get_used_mem)
	if [ -z "$mem_baseline" ]; then
		color_echo "ERROR: could not read baseline memory from free(1), skipping the density test" >&2
		SECTION_FAIL_REASON="could not read baseline memory from free(1)"
		return 1
	fi
	EXPECTED_ARR+=("0 (baseline)")
	MEM_ARR+=("${mem_baseline}")
	color_echo "[baseline] expected=0  used_mem=${mem_baseline} MiB"

	while [ "$iter" -lt "$max_batches" ]; do
		# 1. Launch one batch of sandboxes. A failed batch is usually a transient
		# 408 / "no more resource" rather than a hard wall, so do not stop on it;
		# the growth check below decides whether the node is actually full.
		color_echo "------------------------------------------"
		color_echo "[batch $((iter + 1))] launching ${BATCH} sandboxes ..."
		run_cube_bench -c 50 -n "${BATCH}" -m create-only || true

		# Update the expected total
		expected=$((expected + BATCH))

		# Give the sandboxes a moment to become fully ready (tune as needed)
		sleep 2

		# 2. Read the actual SANDBOX_COUNT
		actual=$(get_sandbox_count)

		# An unreadable count is a CubeMaster problem, not a density limit, so do
		# not record it as the final capacity
		if [ -z "$actual" ]; then
			color_echo "WARN: could not read SANDBOX_COUNT from cubemaster, stopping the density test" >&2
			incomplete_note="stopped early: could not read SANDBOX_COUNT from cubemaster, so the last row is not the true density limit"
			break
		fi

		# Read memory only after the count is confirmed, so the reading and the
		# SANDBOX_COUNT it is amortized against refer to the same moment. An empty
		# reading (both free(1) and the /proc/meminfo fallback failed) must not be
		# recorded: $((mem_now - mem_baseline)) would treat "" as 0 and print a
		# nonsensical negative per-VM overhead, so stop like the count guard does.
		mem_now=$(get_used_mem)
		if [ -z "$mem_now" ]; then
			color_echo "WARN: could not read used memory, stopping the density test" >&2
			incomplete_note="stopped early: could not read used memory, so the last row is not the true density limit"
			break
		fi

		gained=$((actual - prev_actual))
		color_echo "[batch $((iter + 1))] expected=${expected}  actual=${actual}  (+${gained})  used_mem=${mem_now} MiB"

		# Record what actually exists after this batch. Label the row when the
		# live count has fallen behind the requested total so the report shows
		# the real capacity rather than the request.
		if [ "$actual" != "$expected" ]; then
			EXPECTED_ARR+=("${actual} (actual)")
		else
			EXPECTED_ARR+=("${actual}")
		fi
		MEM_ARR+=("${mem_now}")

		# A density reading is only meaningful once the host has actually created
		# at least one sandbox. If the very first batches fail for a non-capacity
		# reason (host.quota too low, a cubemaster/Cubelet hiccup, a slow image
		# pull), actual stays 0 and the stall logic below would otherwise record
		# "0" as the host's density — a real-looking number for a test that never
		# ran. Track whether anything was ever created and bail out distinctly if
		# the run stalls before that happens.
		if [ "$actual" -gt 0 ]; then
			created_any=1
		fi

		# 3. The density limit shows up as a lack of *meaningful* growth, not as a
		# single count mismatch: a transient partial failure still adds some
		# sandboxes, so only stop once consecutive batches add (almost) nothing.
		# "almost nothing" is min_growth, not a clean zero, because under RAM
		# pressure creation trickles (1-2 per batch) rather than stopping dead —
		# without this a single successful straggler would reset the counter and
		# keep the sweep running toward OOM. Requiring stall_limit consecutive
		# stalls keeps one flaky batch from being recorded as the host's capacity.
		if [ "$gained" -lt "$min_growth" ]; then
			stall=$((stall + 1))
			color_echo "growth below ${min_growth} this batch (+${gained}) (${stall}/${stall_limit})"
			if [ "$stall" -ge "$stall_limit" ]; then
				if [ "$created_any" -eq 0 ]; then
					# Never created a single sandbox: this is a setup/quota failure,
					# not a density measurement. Fail the section so it is reported
					# as skipped rather than emitting "0" as the host's capacity.
					color_echo "ERROR: ${stall_limit} batches created no sandboxes at all; this is a setup/quota failure, not a density reading (check host.quota and the terminal output)" >&2
					SECTION_FAIL_REASON="could not create any sandbox (setup/quota failure, not a density reading)"
					return 1
				fi
				color_echo "!! ${stall_limit} consecutive batches grew by less than ${min_growth}, density limit reached at ${actual}"
				break
			fi
		else
			stall=0
		fi

		# Absolute safety cap: even if creation keeps trickling just above
		# min_growth, never push the host past max_total sandboxes. Unlike the
		# growth plateau (a real density reading), hitting this cap means the true
		# limit was not observed, so flag the last row as a floor in the report.
		if [ "$actual" -ge "$max_total" ]; then
			color_echo "WARN: reached the ${max_total}-sandbox cap (CUBE_BENCH_MVM_MAX); stopping before the host is pushed further" >&2
			incomplete_note="stopped early: hit the ${max_total}-sandbox cap (CUBE_BENCH_MVM_MAX) before growth stalled, so the last row is a floor, not the true density limit"
			break
		fi

		prev_actual="$actual"
		iter=$((iter + 1))
	done

	if [ "$iter" -ge "$max_batches" ]; then
		color_echo "WARN: reached the max batch count ${max_batches}, stopping early" >&2
		incomplete_note="stopped early: hit the ${max_batches}-batch cap before growth stalled, so the last row is a floor, not the true density limit"
	fi

	{
		echo "| Live Sandboxes | Used Memory | Per-VM Amortized Overhead |"
		echo "|:-----------:|:----:|:--------:|"

		local i cnt mem cnt_num diff avg
		for i in "${!EXPECTED_ARR[@]}"; do
			cnt="${EXPECTED_ARR[$i]}"
			mem="${MEM_ARR[$i]}"

			if [[ $i -eq 0 ]]; then
				# baseline row
				printf "| %-14s | %-14s MiB | %s |\n" "${cnt}" "${mem}" "—"
				continue
			fi

			cnt_num=$(echo "${cnt}" | grep -oE '[0-9]+' | head -n1)
			diff=$((mem - mem_baseline))
			# A reading below the baseline (host reclaimed memory between reads, or
			# the baseline was captured with template-build remnants still counted)
			# makes diff negative; a "~ -X.XX MiB" per-VM figure reads like a real
			# measurement. Render "—" for a negative diff, consistent with the
			# zero-count case and the script's "distinguish zero from unreadable" posture.
			if [[ -n "${cnt_num}" && "${cnt_num}" -gt 0 && "${diff}" -ge 0 ]]; then
				avg=$(awk -v d="${diff}" -v n="${cnt_num}" 'BEGIN{printf "%.2f", d / n}')
			else
				avg="—"
			fi
			printf "| %-14s | %-14s MiB | ~ %s MiB |\n" "${cnt}" "${mem}" "${avg}"
		done
		echo ""
		# The stderr WARN above never reaches the .md, so a reader could mistake a
		# run cut short by a transient read failure (or the batch cap) for a
		# completed density measurement. Mirror the tiered sections' "> Partial:"
		# note so the caveat is visible in the report itself.
		if [ -n "$incomplete_note" ]; then
			echo "> ${incomplete_note}."
			echo ""
		fi
	} | tee -a "$OUTPUT_MD"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
main() {
	local sub="${1:-run}"
	case "$sub" in
	sections)
		cmd_sections
		;;
	run)
		shift
		cmd_run "$@"
		;;
	[0-9]* | -y | --yes)
		# Section ids may be given without the "run" subcommand, and a leading
		# -y/--yes is a run flag (cmd_run strips it from anywhere in its args)
		cmd_run "$@"
		;;
	-h | --help | help)
		usage
		;;
	*)
		# Do not silently fall back to run: the full benchmark is expensive,
		# so a typo must be an error
		color_echo "ERROR: unknown subcommand: $sub" >&2
		usage >&2
		exit 1
		;;
	esac
}

# When sourced, only load the functions so helpers can be called individually
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	main "$@"
fi
