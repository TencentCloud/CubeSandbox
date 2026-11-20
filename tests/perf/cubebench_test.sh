#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=cubebench.sh
source "${CUBEBENCH_SCRIPT:-${SCRIPT_DIR}/cubebench.sh}"

assert_cpu_config() {
	local name="$1" expected="$2" input="$3" actual
	actual="$(parse_cpu_config <<<"$input")"
	if [ "$actual" != "$expected" ]; then
		printf 'FAIL: %s\nexpected: %s\nactual:   %s\n' "$name" "$expected" "$actual" >&2
		return 1
	fi
	printf 'PASS: %s\n' "$name"
}

assert_report_line() {
	local expected="$1" report="$2"
	if ! grep -Fxq "$expected" "$report"; then
		printf 'FAIL: report missing line: %s\nreport:\n' "$expected" >&2
		cat "$report" >&2
		return 1
	fi
}

assert_cpu_config "complete topology" \
	"192 logical CPUs (2 sockets, 48 cores/socket, 2 threads/core)" \
	$'CPU(s):                 192\n  On-line CPU(s) list:  0-191\n    Thread(s) per core: 2\n    Core(s) per socket: 48\n    Socket(s):          2'

assert_cpu_config "singular topology" \
	"1 logical CPU (1 socket, 1 core/socket, 1 thread/core)" \
	$'CPU(s): 1\nSocket(s): 1\nCore(s) per socket: 1\nThread(s) per core: 1'

assert_cpu_config "missing field" "unknown" \
	$'CPU(s): 8\nSocket(s): 1\nCore(s) per socket: 4'

assert_cpu_config "malformed field" "unknown" \
	$'CPU(s): 8\nSocket(s): two\nCore(s) per socket: 4\nThread(s) per core: 2'

assert_cpu_config "zero field" "unknown" \
	$'CPU(s): 8\nSocket(s): 1\nCore(s) per socket: 4\nThread(s) per core: 0'

assert_cpu_config "no lscpu output" "unknown" ""

lscpu() {
	[ "${LC_ALL:-}" = C ] || return 1
	printf '%s\n' \
		'CPU(s): 8' \
		'    Socket(s): 1' \
		'    Core(s) per socket: 4' \
		'    Thread(s) per core: 2' \
		'CPU:' \
		'  Model name: Test CPU' \
		'NUMA:' \
		'  NUMA node(s): 1'
}

get_node_count() {
	printf '1\n'
}

report="$(mktemp)"
trap 'rm -f "$report"' EXIT
# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
CUBELET_CONF="${report}.missing"
# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
OUTPUT_MD="$report"
(
	set +e +u +o pipefail
	print_test_env
) >/dev/null

assert_report_line '| CPU Model | Test CPU |' "$report"
assert_report_line '| CPU Config | 8 logical CPUs (1 socket, 4 cores/socket, 2 threads/core) |' "$report"
assert_report_line '| NUMA Nodes | 1 |' "$report"
printf 'PASS: C locale environment report\n'

# image_info is optional in the CLI's template JSON.
(
	cubemaster() {
		[[ "$*" == "tpl info --template-id tpl-test --json" ]] || return 91
		printf '%s\n' "$reply"
		return "${cli_status:-0}"
	}
	reply='{"template_id":"tpl-test","image_info":"registry.example/os:v1"}'
	[[ "$(slo_template_image tpl-test)" == "registry.example/os:v1" ]]
	for reply in '{"template_id":"tpl-test"}' '{"template_id":"tpl-test","image_info":null}' '{"template_id":"tpl-test","image_info":""}'; do
		[[ "$(slo_template_image tpl-test)" == unknown ]]
	done
	for reply in '{"template_id":"tpl-other"}' '{"template_id":"tpl-test","image_info":42}' \
		'invalid' '' '{"template_id":"tpl-test"} {"template_id":"tpl-test"}'; do
		if slo_template_image tpl-test >/dev/null 2>"$report"; then exit 1; fi
		grep -q 'tpl-test' "$report"
	done
	# A failing CLI can still print parseable output; its exit status must win.
	set +o pipefail
	cli_status=1
	reply='{"template_id":"tpl-test","image_info":"registry.example/os:v1"}'
	if slo_template_image tpl-test >/dev/null 2>"$report"; then exit 1; fi
	grep -q 'tpl-test' "$report"
)
printf 'PASS: SLO template image lookup\n'

# Check search-round validation and dispatch without a cluster.
(
	tmp=$(mktemp -d)
	trap 'rm -rf -- "$tmp"' EXIT
	export CUBE_TEMPLATE_ID=tpl-test CUBE_SNAPSHOT_ID=snap-test
	export CUBE_BENCH_SLO_OUTPUT="$tmp/samples.json" CUBE_BENCH_SLO_CONCURRENCY=16
	OUTPUT_MD="$tmp/report.md"
	for section in 3.2 4.3 4.6; do validate_slo_round "$section"; done
	for c in 0 501 foo 01 ""; do
		if CUBE_BENCH_SLO_CONCURRENCY="$c" validate_slo_round 3.2; then exit 1; fi
	done
	if validate_slo_round 3.3 || CUBE_SNAPSHOT_ID="" validate_slo_round 4.3; then exit 1; fi
	(
		cd "$tmp"
		WORK_DIR="$PWD/work"
		EXAMPLES_DIR="$PWD/examples"
		CUBE_BENCH_SDK_VENV="external SDK"
		mkdir -p "$WORK_DIR" "$EXAMPLES_DIR" "$CUBE_BENCH_SDK_VENV/bin"
		printf '%s\n' 'echo activated >> "$WORK_DIR/activations"' > "$CUBE_BENCH_SDK_VENV/bin/activate"
		python() { echo test-sdk-version; }
		validate_slo_round 4.3
		cd "$WORK_DIR"
		prepare_snapshot_bench
		prepare_snapshot_bench
		[[ "$PWD" == "$EXAMPLES_DIR" && "$SDK_INSTALLED_VERSION" == test-sdk-version ]]
		[[ "$(cat "$WORK_DIR/activations")" == activated ]]
	)
	touch "$CUBE_BENCH_SLO_OUTPUT"
	if validate_slo_round 3.2; then exit 1; fi
	prepare_snapshot_bench() { :; }
	run_cube_bench() { printf '%s\n' "$*" >"$tmp/args"; }
	python() { printf '%s\n' "$*" >"$tmp/args"; }
	run_slo_round 3.2
	grep -Fx -- "-c 16 -n 500 -w 0 -m create-only --no-tui -o $CUBE_BENCH_SLO_OUTPUT" "$tmp/args"
	run_slo_round 4.3
	grep -Fx -- "$REPO_ROOT/tests/perf/cubebench_slo_workload.py 4.3 -c 16 -n 1 --requests 500 --output $CUBE_BENCH_SLO_OUTPUT" "$tmp/args"
	run_slo_round 4.6
	grep -Fx -- "$REPO_ROOT/tests/perf/cubebench_slo_workload.py 4.6 -c 16 -n 1 --requests 500 --output $CUBE_BENCH_SLO_OUTPUT" "$tmp/args"
)

python3 -m unittest discover -s "$SCRIPT_DIR" -p 'test_cubebench_slo*.py'
