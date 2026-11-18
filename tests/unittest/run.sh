#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# One-click runner for the per-component unit tests in this monorepo.
#
# The repo has no aggregate `make test`; each component is tested on its own,
# most inside the builder container via `make <component>-test` or a
# `builder-run` invocation. This script drives them all from one place, prints
# a clear "no unit tests" line for components that have none, and ends with a
# pass/fail/skip summary so a full sweep is a single command.
#
# By default it runs only the self-contained components (the green gate) and
# skips "gated" components that need a live database or a full VM. Naming a
# gated component explicitly (e.g. `run.sh hypervisor-kvm`) forces it to run.
#
# Components run concurrently (each in its own builder container or host
# process); every component's output goes to <log-dir>/<name>.log and a
# PASS/FAIL line (plus the log tail on failure) is printed as each finishes.
# `-j 1` restores the sequential mode that streams output live.
#
# Usage:
#   tests/unittest/run.sh                 # run the default (self-contained) gate
#   tests/unittest/run.sh cubemaster agent   # run only the named components
#   tests/unittest/run.sh -j 1            # one component at a time, live output
#   tests/unittest/run.sh --list          # list components and exit
#   tests/unittest/run.sh --no-kvm        # skip components needing /dev/kvm
#   tests/unittest/run.sh -h | --help
#
# Options:
#   -j N, --jobs N   max components running at once (default: $UNITTEST_JOBS,
#                    else nproc/4 clamped to 2..8)
#   --log-dir DIR    per-component logs for parallel runs
#                    (default: $UNITTEST_LOG_DIR, else _output/unittest)
#
# Component names are the keys shown by `--list` (lower-case, e.g. cubemaster,
# cubelet, agent, hypervisor). Exit status is non-zero if any run failed.

set -uo pipefail

# Locate the repo root by walking up from the script's own directory until we
# find the Makefile that defines the `builder-run` target. This keeps the
# script working no matter how deep it lives in the tree or which directory it
# is invoked from — unlike a hardcoded relative path such as ../.., which
# breaks whenever the script is moved.
find_repo_root() {
	local src="${BASH_SOURCE[0]}"
	# Resolve symlinks so a symlinked script still finds its real location.
	while [[ -L "$src" ]]; do
		local target
		target="$(readlink "$src")"
		[[ "$target" == /* ]] && src="$target" || src="$(dirname "$src")/$target"
	done
	local dir
	dir="$(cd "$(dirname "$src")" && pwd)"
	while [[ "$dir" != "/" ]]; do
		if [[ -f "$dir/Makefile" ]] && grep -qE '^builder-run:' "$dir/Makefile"; then
			printf '%s' "$dir"
			return 0
		fi
		dir="$(dirname "$dir")"
	done
	return 1
}

INVOKE_DIR="$PWD"
REPO_ROOT="$(find_repo_root)" || {
	printf 'error: could not locate repo root (no Makefile defining builder-run found above %s)\n' \
		"$(dirname "${BASH_SOURCE[0]}")" >&2
	exit 2
}
cd "$REPO_ROOT" || exit 2

# --- component tables --------------------------------------------------------
#
# WITH_TESTS: "name|language|needs_kvm|command"
#   Self-contained unit tests that pass in the builder with no external deps.
#   These form the default green gate. command runs from REPO_ROOT; needs_kvm=1
#   means some tests require /dev/kvm and the whole component is skipped under
#   --no-kvm.
#
# GATED_TESTS: "name|language|needs_kvm|reason|command"
#   Components that DO have tests but need an environment the builder lacks
#   (a live MySQL/PostgreSQL/Redis, or a full VM for hypervisor integration
#   tests). Skipped by default so the sweep stays green, but run when named
#   explicitly (e.g. `run.sh cubelet`). This keeps them visible and runnable
#   without wedging the default gate on environment-only failures.
#
# NO_TESTS: "name|reason" — printed as an explicit skip so the absence of unit
#   tests is visible rather than silently missing from the sweep.

WITH_TESTS=(
	"cubeops|Go|0|make cubeops-test"
	"ops-agent|Go|0|make ops-agent-test"
	# CubeMaster's Makefile runs packages one at a time with the no-inline flag
	# required by its remaining gomonkey tests. Keeping that flag scoped to each
	# test binary also avoids cross-package runtime corruption on arm64.
	"cubemaster|Go|0|make cubemaster-test"
	# CubeTemplateCenter shares CubeMaster's pkg/templatecenter image code
	# (Linux-only syscall constants), so its unit tests go through the builder
	# like every other Go component. Docker-backed cases are excluded here and
	# covered by the gated cubetemplatecenter-docker lane below.
	"cubetemplatecenter|Go|0|make cubetemplatecenter-test"
	# cubevs: the CubeNet/cubevs module's OWN unit tests (dataplane policy, DNS
	# learning, migration, dump, classify). cubevs-test regenerates the BPF
	# objects (make gen) and runs the full module set in a privileged root
	# builder (eBPF load + bpffs mount need the privilege).
	"cubevs|Go|0|make cubevs-test"
	"cubecow|Go+CGO|0|make cubecow-test-native"
	"cube-lifecycle-manager|Go|0|make builder-run BUILDER_CMD='cd /workspace/cube-lifecycle-manager && go mod download && go test ./...'"
	"cube-api|Rust|0|make cube-api-test"
	"shim|Rust|0|make shim-test"
	"agent|Rust|1|make builder-run BUILDER_CMD='cd /workspace/agent && make test'"
	"cube-proxy|Lua|0|make cube-proxy-test"
	# cubelog/cubedb run on the host, not via builder-run: both are pure Go with
	# no CGO (no `import "C"`) and no builder-only build deps, so the host
	# toolchain is sufficient and skipping the container is faster.
	"cubelog|Go|0|cd pkgs/CubeLog && go test -short ./..."
	"cubedb|Go|0|cd pkgs/cubedb && go mod download && go test ./..."
	"blobstore|Go|0|cd pkgs/blobstore && go mod download && go test ./..."
	# sandboxrestart is stdlib-only, so no module download is needed.
	"sandboxrestart|Go|0|cd pkgs/sandboxrestart && go test ./..."
	"redisx|Go|0|make redisx-test"
	"cubebench|Shell|0|make cubebench-test"
	# Same prerequisites and package set as CI's `make cubelet-test`, but via
	# Cubelet's `make test-fast`: one `go test` over every package instead of a
	# per-package loop with -coverprofile, which serialises ~166 compile+link
	# steps. Use `make cubelet-test` directly for per-package coverage files.
	"cubelet|Go|0|make cubelet-test-fast"
	# Only unit tests (--lib --bins) run here; the tests/integration.rs target
	# needs a full VM (OS disk images, sudo/ip networking, VFIO, windows guest).
	# The root package command does not execute workspace member tests, so run
	# block_util explicitly. Runtime-KVM tests run in `hypervisor-kvm` below.
	"hypervisor|Rust|1|make builder-run BUILDER_CMD='cd /workspace/hypervisor && cargo test --features kvm --lib --bins && cargo test -p block_util --lib'"
	# Shell-only test; no builder or KVM needed.
	"cubeegress|Shell|0|bash CubeEgress/tests/cube-egress-iptables-init_test.sh"
)

GATED_TESTS=(
	# The race profile is intentionally separate from the fast default gate: it
	# recompiles every CubeMaster unit package with the race detector and runs
	# substantially longer than the regular -short suite.
	"cubemaster-race|Go+race|0|race-enabled CubeMaster sweep is an extended check|make builder-run BUILDER_CMD='cd /workspace/CubeMaster && go mod download && make test-race'"
	# Docker-backed CubeMaster tests run on the host so dockertest can reach the
	# daemon and its published 127.0.0.1 ports without nested-container socket or
	# network plumbing. The component target fails instead of skipping when the
	# daemon is unavailable.
	"cubemaster-docker|Go+Docker|0|database tests need a reachable Docker daemon|make -C CubeMaster test-docker"
	# Same split for CubeTemplateCenter: its reconcile MySQL test runs on the
	# host so dockertest can reach the daemon without nested-container plumbing.
	"cubetemplatecenter-docker|Go+Docker|0|reconcile Docker-backed MySQL test needs a reachable Docker daemon|make cubetemplatecenter-docker-test"
	# The ordinary cubelet entry stays unprivileged. Run this explicitly to cover
	# tests that format and mount images with CAP_SYS_ADMIN, matching CI's lane.
	"cubelet-mount|Go|0|mount tests need a privileged root builder|make cubelet-mount-test"
	# hypervisor-kvm exercises the tests that need a real /dev/kvm at runtime:
	# the `vmm` and `hypervisor` crate unit tests call hypervisor::new() /
	# create_vm(), which fail without KVM (verified: 4/4 vmm cpu:: tests fail with
	# no device, pass with it). The device is passed into the builder container via
	# BUILDER_RUN_EXTRA_MOUNTS='--device /dev/kvm'. Scoped to these two crates
	# rather than the whole workspace because a full --workspace test build trips a
	# pre-existing `#![deny(missing_docs)]` error in the rate_limiter crate that is
	# unrelated to KVM. needs_kvm=1 so `--no-kvm` skips it.
	"hypervisor-kvm|Rust|1|runtime KVM tests need /dev/kvm passed into the builder (vmm+hypervisor crates)|make builder-run BUILDER_RUN_EXTRA_MOUNTS='--device /dev/kvm' BUILDER_CMD='cd /workspace/hypervisor && cargo test --features kvm -p vmm -p hypervisor --lib --bins'"
)

NO_TESTS=(
	"web|no unit test script in web/package.json"
	# CubeS3lvol ships integration tests only (test/integration/, driven by
	# `make check` / `make check-offline`). They need a full SPDK/DPDK/AWS
	# build plus root for the mount-based cases, so the builder sweep cannot
	# run them as unit tests; the offline subset is covered by the dedicated
	# CubeS3lvol CI workflow instead.
	"cube-s3lvol|no unit tests; integration tests in test/ run via make check / check-offline (need SPDK build + root, covered by CI workflow)"
)

# SERIAL_GROUPS: "name|group[,group...]" — two components sharing a group never
#   run at the same time, because one rewrites in-tree files the other compiles:
#   cubevs-gen   `make gen` (bpf2go) rewrites CubeNet/cubevs/*_bpfel.{go,o},
#                which Cubelet compiles through its replace directive.
#   cubecow-sdk  `make cubecow-sdk` reinstalls Cubelet/third_party/cubecow
#                (libcubecow.a + header) that Cubelet's CGO packages link. It
#                also keeps cubelet, whose `make proto` rewrites
#                Cubelet/api/*.pb.go, apart from cubelet-mount.
SERIAL_GROUPS=(
	"cubevs|cubevs-gen"
	"cubecow|cubecow-sdk"
	"cubelet|cubevs-gen,cubecow-sdk"
	"cubelet-mount|cubevs-gen,cubecow-sdk"
)

# --- output helpers ----------------------------------------------------------

if [[ -t 1 ]]; then
	C_RESET=$'\033[0m'
	C_RED=$'\033[31m'
	C_GREEN=$'\033[32m'
	C_YELLOW=$'\033[33m'
	C_BLUE=$'\033[34m'
	C_BOLD=$'\033[1m'
else
	C_RESET=""
	C_RED=""
	C_GREEN=""
	C_YELLOW=""
	C_BLUE=""
	C_BOLD=""
fi

info() { printf '%s==>%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
pass() { printf '%sPASS%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
fail() { printf '%sFAIL%s %s\n' "$C_RED" "$C_RESET" "$*"; }
skip() { printf '%sSKIP%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }

usage() {
	awk 'NR < 5 { next } !/^#/ { exit } { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
}

list_components() {
	printf '%sDefault gate (self-contained unit tests):%s\n' "$C_BOLD" "$C_RESET"
	local entry name lang kvm reason
	for entry in "${WITH_TESTS[@]}"; do
		IFS='|' read -r name lang kvm _ <<<"$entry"
		if [[ "$kvm" == "1" ]]; then
			printf '  %-24s %-8s (some tests need /dev/kvm)\n' "$name" "$lang"
		else
			printf '  %-24s %s\n' "$name" "$lang"
		fi
	done
	printf '\n%sGated (have tests, skipped by default — run by name):%s\n' "$C_BOLD" "$C_RESET"
	for entry in "${GATED_TESTS[@]}"; do
		IFS='|' read -r name lang kvm reason _ <<<"$entry"
		printf '  %-24s %-8s %s\n' "$name" "$lang" "$reason"
	done
	printf '\n%sComponents without unit tests:%s\n' "$C_BOLD" "$C_RESET"
	for entry in "${NO_TESTS[@]}"; do
		IFS='|' read -r name reason <<<"$entry"
		printf '  %-24s %s\n' "$name" "$reason"
	done
}

# --- arg parsing -------------------------------------------------------------

default_jobs() {
	local n
	n="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 8)"
	n=$((n / 4))
	((n < 2)) && n=2
	((n > 8)) && n=8
	printf '%s' "$n"
}

need_value() {
	if [[ $2 -lt 2 || -z "$3" || "$3" == -* ]]; then
		fail "$1 needs a value"
		exit 2
	fi
}

NO_KVM=0
JOBS="${UNITTEST_JOBS:-}"
LOG_DIR="${UNITTEST_LOG_DIR:-$REPO_ROOT/_output/unittest}"
SELECTED=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--list)
		list_components
		exit 0
		;;
	--no-kvm) NO_KVM=1 ;;
	-j | --jobs)
		need_value "$1" $# "${2:-}"
		JOBS="$2"
		shift
		;;
	-j*) JOBS="${1#-j}" ;;
	--jobs=*) JOBS="${1#--jobs=}" ;;
	--log-dir)
		need_value "$1" $# "${2:-}"
		LOG_DIR="$2"
		shift
		;;
	--log-dir=*) LOG_DIR="${1#--log-dir=}" ;;
	-*)
		fail "unknown option: $1"
		usage
		exit 2
		;;
	*) SELECTED+=("$1") ;;
	esac
	shift
done

[[ -n "$JOBS" ]] || JOBS="$(default_jobs)"
if ! [[ "$JOBS" =~ ^[1-9][0-9]*$ ]]; then
	fail "invalid job count: $JOBS (want a positive integer)"
	exit 2
fi
if [[ -z "$LOG_DIR" ]]; then
	fail "log dir must not be empty"
	exit 2
fi
# Relative --log-dir / UNITTEST_LOG_DIR are relative to where the script was
# invoked, not to REPO_ROOT (the script has already cd'ed there).
[[ "$LOG_DIR" == /* ]] || LOG_DIR="$INVOKE_DIR/$LOG_DIR"

# Validate any explicitly requested names against all tables.
known_component() {
	local q="$1" entry name
	for entry in "${WITH_TESTS[@]}" "${GATED_TESTS[@]}" "${NO_TESTS[@]}"; do
		IFS='|' read -r name _ <<<"$entry"
		[[ "$name" == "$q" ]] && return 0
	done
	return 1
}
for want in "${SELECTED[@]}"; do
	if ! known_component "$want"; then
		fail "unknown component: $want (use --list to see valid names)"
		exit 2
	fi
done

# Whether a component was requested (empty selection == all).
requested() {
	[[ ${#SELECTED[@]} -eq 0 ]] && return 0
	local q="$1" s
	for s in "${SELECTED[@]}"; do [[ "$s" == "$q" ]] && return 0; done
	return 1
}

serial_groups_of() {
	local q="$1" entry name groups
	for entry in "${SERIAL_GROUPS[@]}"; do
		IFS='|' read -r name groups <<<"$entry"
		if [[ "$name" == "$q" ]]; then
			printf '%s' "${groups//,/ }"
			return 0
		fi
	done
}

# --- plan --------------------------------------------------------------------

PASSED=()
FAILED=()
SKIPPED=()
# Queue of components to run, as parallel arrays indexed together.
Q_NAME=()
Q_LANG=()
Q_CMD=()
Q_GROUPS=()

enqueue() {
	Q_NAME+=("$1")
	Q_LANG+=("$2")
	Q_CMD+=("$3")
	Q_GROUPS+=("$(serial_groups_of "$1")")
}

for entry in "${WITH_TESTS[@]}"; do
	IFS='|' read -r name lang kvm cmd <<<"$entry"
	requested "$name" || continue

	if [[ "$kvm" == "1" && "$NO_KVM" == "1" ]]; then
		skip "$name ($lang) — needs /dev/kvm, skipped by --no-kvm"
		SKIPPED+=("$name")
		continue
	fi
	enqueue "$name" "$lang" "$cmd"
done

# Gated components: run only when named explicitly; otherwise announced as
# skipped so a default sweep stays green without hiding that they exist.
for entry in "${GATED_TESTS[@]}"; do
	IFS='|' read -r name lang kvm reason cmd <<<"$entry"

	# Not part of the default gate: skip unless the user asked for it by name.
	if [[ ${#SELECTED[@]} -eq 0 ]]; then
		skip "$name ($lang) — $reason (run \`$(basename "$0") $name\` to include)"
		SKIPPED+=("$name")
		continue
	fi
	requested "$name" || continue

	if [[ "$kvm" == "1" && "$NO_KVM" == "1" ]]; then
		skip "$name ($lang) — needs /dev/kvm, skipped by --no-kvm"
		SKIPPED+=("$name")
		continue
	fi
	enqueue "$name" "$lang, gated" "$cmd"
done

# Components with no unit tests: always announced when in scope.
for entry in "${NO_TESTS[@]}"; do
	IFS='|' read -r name reason <<<"$entry"
	requested "$name" || continue
	skip "$name — $reason"
	SKIPPED+=("$name")
done

# --- run ---------------------------------------------------------------------

FAIL_TAIL_LINES=40
# "name seconds" from earlier passing runs; parallel runs start the slowest
# components first so the longest one does not end up queued behind short ones.
HISTORY_FILE="$LOG_DIR/.durations"
DURATIONS=()

last_duration() {
	[[ -f "$HISTORY_FILE" ]] || return 0
	awk -v n="$1" '$1 == n { d = $2 } END { if (d != "") print d }' "$HISTORY_FILE"
}

save_durations() {
	[[ ${#DURATIONS[@]} -gt 0 ]] || return 0
	mkdir -p "$LOG_DIR" 2>/dev/null || return 0
	{
		[[ -f "$HISTORY_FILE" ]] && cat "$HISTORY_FILE"
		printf '%s\n' "${DURATIONS[@]}"
	} | awk 'NF == 2 { d[$1] = $2 } END { for (n in d) print n, d[n] }' | sort >"$HISTORY_FILE.tmp" &&
		mv -f "$HISTORY_FILE.tmp" "$HISTORY_FILE"
}

# finish <queue-index> <exit-code> <seconds> [log]
finish() {
	local i="$1" rc="$2" dur="$3" log="${4:-}"
	local name="${Q_NAME[i]}" where=""
	[[ -n "$log" ]] && where="  log: ${log#"$REPO_ROOT"/}"
	if [[ "$rc" -eq 0 ]]; then
		pass "$name (${dur}s)$where"
		PASSED+=("$name")
		DURATIONS+=("$name $dur")
	else
		fail "$name (${dur}s, exit $rc)$where"
		FAILED+=("$name")
		if [[ -n "$log" ]]; then
			tail -n "$FAIL_TAIL_LINES" "$log" | sed 's/^/    | /'
		fi
	fi
}

run_foreground() {
	local i="$1" start rc
	info "${Q_NAME[i]} (${Q_LANG[i]}): ${Q_CMD[i]}"
	start=$(date +%s)
	bash -c "${Q_CMD[i]}"
	rc=$?
	finish "$i" "$rc" "$(($(date +%s) - start))"
}

# Whether a component's command uses the builder container: a direct
# `make builder-run`, or a root `make <target>` whose rule needs builder-image.
needs_builder() {
	local cmd="$1"
	[[ "$cmd" == *builder-run* ]] && return 0
	[[ "$cmd" =~ ^make\ ([A-Za-z0-9_.-]+)$ ]] || return 1
	grep -qE "^${BASH_REMATCH[1]}:.*builder-image" Makefile
}

# Every builder target depends on `make builder-image`, which builds the image
# when it is missing or stale. Resolve that once up front so parallel
# components do not each start the same `docker build`.
prepare_builder_image() {
	local i log="$LOG_DIR/builder-image.log"
	command -v docker >/dev/null 2>&1 || return 0
	for i in "${!Q_NAME[@]}"; do
		needs_builder "${Q_CMD[i]}" || continue
		info "builder-image: make builder-image"
		if ! make --no-print-directory builder-image >"$log" 2>&1; then
			fail "make builder-image failed (log: ${log#"$REPO_ROOT"/})"
			tail -n "$FAIL_TAIL_LINES" "$log" | sed 's/^/    | /'
			return 1
		fi
		return 0
	done
}

ORDER=()
plan_order() {
	local i d
	ORDER=()
	while IFS=$'\t' read -r _ i; do
		ORDER+=("$i")
	done < <(
		for i in "${!Q_NAME[@]}"; do
			d="$(last_duration "${Q_NAME[i]}")"
			# Never-timed components go first: they may well be the slow ones.
			printf '%s\t%s\n' "${d:-999999}" "$i"
		done | sort -t $'\t' -k1,1nr -k2,2n
	)
}

TMP_ROOT=""
RUN_ID=""
BUSY_GROUPS=" "
# Running components, as parallel arrays indexed together.
RUN_PIDS=()
RUN_IDX=()
RUN_START=()

groups_free() {
	local g
	for g in $1; do
		[[ "$BUSY_GROUPS" == *" $g "* ]] && return 1
	done
	return 0
}

launch() {
	local i="$1" g
	local name="${Q_NAME[i]}"
	local log="$LOG_DIR/$name.log" rcfile="$TMP_ROOT/$name.rc"
	for g in ${Q_GROUPS[i]}; do BUSY_GROUPS+="$g "; done
	info "start $name (${Q_LANG[i]})"
	# A per-component TMP_GIT_CREDENTIALS: builder-run deletes and re-creates
	# that file before bind-mounting it, which races between parallel runs.
	(
		printf '==> %s (%s): %s\n' "$name" "${Q_LANG[i]}" "${Q_CMD[i]}"
		TMP_GIT_CREDENTIALS="$TMP_ROOT/git-credentials.$name" \
			BUILDER_RUN_LABELS="--label cube-unittest-run=$RUN_ID" bash -c "${Q_CMD[i]}"
		rc=$?
		echo "$rc" >"$rcfile"
		exit "$rc"
	) >"$log" 2>&1 </dev/null &
	RUN_PIDS+=("$!")
	RUN_IDX+=("$i")
	RUN_START+=("$(date +%s)")
}

reap() {
	local k i g pid rc
	local -a pids=() idx=() starts=()
	for k in "${!RUN_PIDS[@]}"; do
		pid="${RUN_PIDS[k]}"
		i="${RUN_IDX[k]}"
		if [[ ! -f "$TMP_ROOT/${Q_NAME[i]}.rc" ]] && kill -0 "$pid" 2>/dev/null; then
			pids+=("$pid")
			idx+=("$i")
			starts+=("${RUN_START[k]}")
			continue
		fi
		wait "$pid"
		rc=$?
		for g in ${Q_GROUPS[i]}; do BUSY_GROUPS="${BUSY_GROUPS/ $g / }"; done
		finish "$i" "$rc" "$(($(date +%s) - RUN_START[k]))" "$LOG_DIR/${Q_NAME[i]}.log"
	done
	RUN_PIDS=("${pids[@]}")
	RUN_IDX=("${idx[@]}")
	RUN_START=("${starts[@]}")
}

kill_tree() {
	local pid="$1" sig="$2" child
	for child in $(pgrep -P "$pid" 2>/dev/null); do
		kill_tree "$child" "$sig"
	done
	kill "-$sig" "$pid" 2>/dev/null
}

# Signalling `docker run` does not stop the container: its PID 1 (bash) ignores
# SIGTERM/SIGINT, so it would keep running tests against the workspace. Builder
# containers launched above carry this run's unique label, allowing cleanup to
# avoid touching containers from another concurrent invocation.
stop_run_containers() {
	local id
	command -v docker >/dev/null 2>&1 || return 0
	for id in $(docker ps -q --filter "label=cube-unittest-run=$RUN_ID" 2>/dev/null); do
		docker kill "$id" >/dev/null 2>&1 &
	done
	wait
}

on_interrupt() {
	trap - INT TERM
	local pid
	printf '\n'
	fail "interrupted; stopping ${#RUN_PIDS[@]} running component(s)"
	for pid in "${RUN_PIDS[@]}"; do kill_tree "$pid" TERM; done
	stop_run_containers
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		pgrep -P $$ >/dev/null 2>&1 || break
		sleep 0.5
	done
	for pid in "${RUN_PIDS[@]}"; do kill_tree "$pid" KILL; done
	exit 130
}

run_parallel() {
	local i
	local -a pending waiting
	mkdir -p "$LOG_DIR" || {
		fail "cannot create log dir: $LOG_DIR"
		exit 2
	}
	TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/cube-unittest.XXXXXX")" || exit 2
	RUN_ID="${TMP_ROOT##*/}"
	trap 'rm -rf "$TMP_ROOT"' EXIT
	trap on_interrupt INT TERM

	info "running ${#Q_NAME[@]} components, up to $JOBS at a time; logs in ${LOG_DIR#"$REPO_ROOT"/}/"
	if ! prepare_builder_image; then
		FAILED+=("builder-image")
		return 0
	fi
	plan_order
	pending=("${ORDER[@]}")
	while [[ ${#pending[@]} -gt 0 || ${#RUN_PIDS[@]} -gt 0 ]]; do
		waiting=()
		for i in "${pending[@]}"; do
			if [[ ${#RUN_PIDS[@]} -lt $JOBS ]] && groups_free "${Q_GROUPS[i]}"; then
				launch "$i"
			else
				waiting+=("$i")
			fi
		done
		pending=("${waiting[@]}")
		sleep 0.5
		reap
	done
}

START_TS=$(date +%s)
PARALLEL=0
if [[ ${#Q_NAME[@]} -gt 1 && "$JOBS" -gt 1 ]]; then
	PARALLEL=1
	run_parallel
else
	for i in "${!Q_NAME[@]}"; do run_foreground "$i"; done
fi
save_durations

# --- summary -----------------------------------------------------------------

TOTAL_DUR=$(($(date +%s) - START_TS))
printf '\n%s===== unit test summary (%ss) =====%s\n' "$C_BOLD" "$TOTAL_DUR" "$C_RESET"
printf '  passed:  %d  %s\n' "${#PASSED[@]}" "${PASSED[*]:-}"
printf '  failed:  %d  %s\n' "${#FAILED[@]}" "${FAILED[*]:-}"
printf '  skipped: %d  %s\n' "${#SKIPPED[@]}" "${SKIPPED[*]:-}"
if [[ "$PARALLEL" == "1" ]]; then
	printf '  logs:    %s/\n' "${LOG_DIR#"$REPO_ROOT"/}"
fi

[[ ${#FAILED[@]} -eq 0 ]]
