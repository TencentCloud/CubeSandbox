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

tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

# Args: dir online present then one "cpu:package:core_cpus_list" per on-line CPU.
make_cpu_sysfs() {
	local dir="$1" online="$2" present="$3" spec cpu pkg core
	shift 3
	mkdir -p "$dir"
	printf '%s\n' "$online" >"${dir}/online"
	[ -z "$present" ] || printf '%s\n' "$present" >"${dir}/present"
	for spec in "$@"; do
		IFS=: read -r cpu pkg core <<<"$spec"
		mkdir -p "${dir}/cpu${cpu}/topology"
		printf '%s\n' "$pkg" >"${dir}/cpu${cpu}/topology/physical_package_id"
		printf '%s\n' "$core" >"${dir}/cpu${cpu}/topology/core_cpus_list"
	done
}

assert_sysfs_cpu_config() {
	local name="$1" expected="$2" dir="$3" actual
	actual="$(sysfs_cpu_config "$dir")" || actual="<failed>"
	if [ "$actual" != "$expected" ]; then
		printf 'FAIL: %s\nexpected: %s\nactual:   %s\n' "$name" "$expected" "$actual" >&2
		return 1
	fi
	printf 'PASS: %s\n' "$name"
}

assert_cpu_config "complete topology" \
	"192 logical CPUs (2 sockets, 48 cores/socket, 2 threads/core); On-line CPU(s) list: 0-191" \
	$'CPU(s):                 192\n  On-line CPU(s) list:  0-191\n    Thread(s) per core: 2\n    Core(s) per socket: 48\n    Socket(s):          2'

assert_cpu_config "singular topology" \
	"1 logical CPU (1 socket, 1 core/socket, 1 thread/core); On-line CPU(s) list: 0" \
	$'CPU(s): 1\nOn-line CPU(s) list: 0\nSocket(s): 1\nCore(s) per socket: 1\nThread(s) per core: 1'

assert_cpu_config "partially on-line" \
	"8 logical CPUs, 6 on-line (1 socket, 4 cores/socket, 2 threads/core); On-line CPU(s) list: 0-3,6,7" \
	$'CPU(s): 8\nOn-line CPU(s) list: 0-3,6,7\nSocket(s): 1\nCore(s) per socket: 4\nThread(s) per core: 2'

assert_cpu_config "missing field" "unknown" \
	$'CPU(s): 8\nOn-line CPU(s) list: 0-7\nSocket(s): 1\nCore(s) per socket: 4'

assert_cpu_config "legacy output without on-line list" \
	"8 logical CPUs (1 socket, 4 cores/socket, 2 threads/core)" \
	$'CPU(s): 8\nSocket(s): 1\nCore(s) per socket: 4\nThread(s) per core: 2'

assert_cpu_config "malformed on-line list" \
	"8 logical CPUs (1 socket, 4 cores/socket, 2 threads/core)" \
	$'CPU(s): 8\nOn-line CPU(s) list: broken\nSocket(s): 1\nCore(s) per socket: 4\nThread(s) per core: 2'

assert_cpu_config "malformed field" "unknown" \
	$'CPU(s): 8\nOn-line CPU(s) list: 0-7\nSocket(s): two\nCore(s) per socket: 4\nThread(s) per core: 2'

assert_cpu_config "zero field" "unknown" \
	$'CPU(s): 8\nOn-line CPU(s) list: 0-7\nSocket(s): 1\nCore(s) per socket: 4\nThread(s) per core: 0'

assert_cpu_config "no lscpu output" "unknown" ""

make_cpu_sysfs "${tmp_root}/smt" "0-7" "0-7" \
	0:0:0,4 1:0:1,5 2:0:2,6 3:0:3,7 4:0:0,4 5:0:1,5 6:0:2,6 7:0:3,7
assert_sysfs_cpu_config "sysfs uniform SMT" \
	"8 logical CPUs; on-line topology (1 socket, 4 cores/socket, 2 threads/core); On-line CPU(s) list: 0-7" \
	"${tmp_root}/smt"

make_cpu_sysfs "${tmp_root}/multi" "0-3" "0-3" \
	0:0:0 1:0:1 2:1:2 3:1:3
assert_sysfs_cpu_config "sysfs two sockets" \
	"4 logical CPUs; on-line topology (2 sockets, 2 cores/socket, 1 thread/core); On-line CPU(s) list: 0-3" \
	"${tmp_root}/multi"

make_cpu_sysfs "${tmp_root}/offline" "0-2,4-6" "0-7" \
	0:0:0,4 1:0:1,5 2:0:2,6 4:0:0,4 5:0:1,5 6:0:2,6
assert_sysfs_cpu_config "sysfs with off-line CPUs" \
	"8 logical CPUs, 6 on-line; on-line topology (1 socket, 3 cores/socket, 2 threads/core); On-line CPU(s) list: 0-2,4-6" \
	"${tmp_root}/offline"

make_cpu_sysfs "${tmp_root}/partial-smt" "0,2" "0-3" 0:0:0-1 2:0:2-3
assert_sysfs_cpu_config "sysfs with off-line SMT siblings" \
	"4 logical CPUs, 2 on-line; on-line topology (1 socket, 2 cores/socket, 1 thread/core); On-line CPU(s) list: 0,2" \
	"${tmp_root}/partial-smt"

make_cpu_sysfs "${tmp_root}/hybrid" "0-5" "0-5" \
	0:0:0-1 1:0:0-1 2:0:2-3 3:0:2-3 4:0:4 5:0:5
assert_sysfs_cpu_config "sysfs hybrid cores" \
	"6 logical CPUs; on-line topology (1 socket, 4 cores/socket, 1-2 threads/core); On-line CPU(s) list: 0-5" \
	"${tmp_root}/hybrid"

make_cpu_sysfs "${tmp_root}/no-present" "0-1" "" 0:0:0 1:0:1
assert_sysfs_cpu_config "sysfs without present file" \
	"2 logical CPUs; on-line topology (1 socket, 2 cores/socket, 1 thread/core); On-line CPU(s) list: 0-1" \
	"${tmp_root}/no-present"

make_cpu_sysfs "${tmp_root}/thread-siblings" "0-1" "0-1" 0:0:0-1 1:0:0-1
for cpu in 0 1; do
	mv "${tmp_root}/thread-siblings/cpu${cpu}/topology/core_cpus_list" \
		"${tmp_root}/thread-siblings/cpu${cpu}/topology/thread_siblings_list"
done
assert_sysfs_cpu_config "sysfs thread siblings fallback" \
	"2 logical CPUs; on-line topology (1 socket, 1 core/socket, 2 threads/core); On-line CPU(s) list: 0-1" \
	"${tmp_root}/thread-siblings"

make_cpu_sysfs "${tmp_root}/invalid-package" "0" "0" 0:-1:0
assert_sysfs_cpu_config "sysfs invalid package id" "<failed>" "${tmp_root}/invalid-package"

make_cpu_sysfs "${tmp_root}/invalid-core-list" "0" "0" 0:0:broken
assert_sysfs_cpu_config "sysfs invalid core list" "<failed>" "${tmp_root}/invalid-core-list"

make_cpu_sysfs "${tmp_root}/wrong-core-list" "0" "0" 0:0:1
assert_sysfs_cpu_config "sysfs core list excludes CPU" "<failed>" "${tmp_root}/wrong-core-list"

make_cpu_sysfs "${tmp_root}/no-topology" "0-1" "0-1" 0:0:0
assert_sysfs_cpu_config "sysfs missing topology" "<failed>" "${tmp_root}/no-topology"

assert_sysfs_cpu_config "sysfs missing tree" "<failed>" "${tmp_root}/does-not-exist"

lscpu() {
	[ "${LC_ALL:-}" = C ] || return 1
	printf '%s\n' \
		'CPU(s): 8' \
		'  On-line CPU(s) list: 0-7' \
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

run_print_test_env() {
	(
		set +e +u +o pipefail
		print_test_env
	) >/dev/null
}

report="${tmp_root}/report.md"
# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
CUBELET_CONF="${report}.missing"
# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
OUTPUT_MD="$report"

# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
CPU_SYSFS_DIR="${tmp_root}/multi"
run_print_test_env
assert_report_line '| CPU Model | Test CPU |' "$report"
assert_report_line '| CPU Config | 4 logical CPUs; on-line topology (2 sockets, 2 cores/socket, 1 thread/core); On-line CPU(s) list: 0-3 |' "$report"
assert_report_line '| NUMA Nodes | 1 |' "$report"
printf 'PASS: sysfs environment report\n'

: >"$report"
# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
CPU_SYSFS_DIR="${tmp_root}/invalid-package"
run_print_test_env
assert_report_line '| CPU Config | 8 logical CPUs (1 socket, 4 cores/socket, 2 threads/core); On-line CPU(s) list: 0-7 |' "$report"
printf 'PASS: invalid sysfs lscpu fallback report\n'

: >"$report"
# shellcheck disable=SC2034 # Read by print_test_env from the sourced script.
CPU_SYSFS_DIR="${tmp_root}/does-not-exist"
run_print_test_env
assert_report_line '| CPU Config | 8 logical CPUs (1 socket, 4 cores/socket, 2 threads/core); On-line CPU(s) list: 0-7 |' "$report"
printf 'PASS: missing sysfs lscpu fallback report\n'
