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
