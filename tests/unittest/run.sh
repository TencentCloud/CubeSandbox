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
# gated component explicitly (e.g. `run.sh cubelet`) forces it to run.
#
# Usage:
#   tests/unittest/run.sh                 # run the default (self-contained) gate
#   tests/unittest/run.sh cubemaster agent   # run only the named components
#   tests/unittest/run.sh --list          # list components and exit
#   tests/unittest/run.sh --no-kvm        # skip components needing /dev/kvm
#   tests/unittest/run.sh -h | --help
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

REPO_ROOT="$(find_repo_root)" || {
	printf 'error: could not locate repo root (no Makefile defining builder-run found above %s)\n' \
		"$(dirname "${BASH_SOURCE[0]}")" >&2
	exit 2
}
cd "$REPO_ROOT"

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

# cubelet does NOT use `make cubelet-test`: that target runs `go test
# -coverprofile`, and the builder's Go toolchain lacks the `covdata` tool, so
# any coverage build fails. Bypass coverage with a direct `go test` over the
# same package set (`-short` skips the Redis/KVM-dependent cases).
WITH_TESTS=(
	"cubeops|Go|0|make cubeops-test"
	# -gcflags=all=-l disables inlining, which is REQUIRED by the gomonkey-based
	# tests in ./pkg/templatecenter: gomonkey rewrites a function's machine code
	# to redirect it, but an inlined call site bypasses that patch, so a stub
	# intermittently fails to take effect and the real function runs (e.g. the
	# flaky "template store is not initialized" failures). The repo already uses
	# this flag for the integration tests (CubeMaster/Makefile testlocal/testtt).
	"cubemaster|Go|0|make builder-run BUILDER_CMD='cd /workspace/CubeMaster && go mod download && make proto && if [ -f test/conf.yaml ]; then export CUBE_MASTER_CONFIG_PATH=/workspace/CubeMaster/test/conf.yaml; fi && CI=true go test -short -gcflags=all=-l -timeout=20m ./api/... ./pkg/...'"
	# cubelet-network: the standalone network-agent module was removed in #1285 and
	# folded into Cubelet/network/runtime (NetworkController). Its tests moved there
	# and need the generated CubeNet/cubevs code but no cubecow/CGO, so build cubevs
	# then run just the runtime package rather than the whole Cubelet suite.
	"cubelet-network|Go|0|make builder-run BUILDER_CMD='cd /workspace/CubeNet/cubevs && make gen && cd /workspace/Cubelet && go mod download && go test ./network/runtime/...'"
	# cubevs: the CubeNet/cubevs module's OWN unit tests (dataplane policy, DNS
	# learning, migration, dump, classify). cubelet-network builds cubevs's
	# generated code but runs Cubelet's tests, so these never ran. cubevs-test
	# regenerates the BPF objects (make gen) and runs the full module set in a
	# privileged root builder (eBPF load + bpffs mount need the privilege).
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
	"cubedb|Go|0|cd CubeDB && go mod download && go test ./..."
	# cubelet runs only ./pkg/... here; the cgroupfs/host-cap-dependent tests
	# live under ./plugins/... and ./services/... and are not in this set, so
	# the pkg tests are self-contained in the builder.
	"cubelet|Go|0|make builder-run BUILDER_CMD='cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk && cd /workspace/Cubelet && go mod download && make proto && go test -short ./pkg/...'"
	# Only unit tests (--lib --bins) run here; the tests/integration.rs target
	# needs a full VM (OS disk images, sudo/ip networking, VFIO, windows guest)
	# and is excluded so `run.sh hypervisor` exercises the self-contained tests.
	# This entry does NOT pass /dev/kvm into the builder, so the vmm/hypervisor
	# crate tests that open /dev/kvm at runtime are never reached here — see
	# `hypervisor-kvm` below for those.
	"hypervisor|Rust|1|make builder-run BUILDER_CMD='cd /workspace/hypervisor && cargo test --features kvm --lib --bins'"
)

GATED_TESTS=(
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
	"cubeegress|no unit tests present in CubeEgress/"
	"web|no unit test script in web/package.json"
	# CubeS3lvol ships integration tests only (test/integration/, driven by
	# `make check` / `make check-offline`). They need a full SPDK/DPDK/AWS
	# build plus root for the mount-based cases, so the builder sweep cannot
	# run them as unit tests; the offline subset is covered by the dedicated
	# CubeS3lvol CI workflow instead.
	"cube-s3lvol|no unit tests; integration tests in test/ run via make check / check-offline (need SPDK build + root, covered by CI workflow)"
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
	sed -n '5,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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

NO_KVM=0
SELECTED=()
for arg in "$@"; do
	case "$arg" in
	-h | --help)
		usage
		exit 0
		;;
	--list)
		list_components
		exit 0
		;;
	--no-kvm) NO_KVM=1 ;;
	-*)
		fail "unknown option: $arg"
		usage
		exit 2
		;;
	*) SELECTED+=("$arg") ;;
	esac
done

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

# --- run ---------------------------------------------------------------------

PASSED=()
FAILED=()
SKIPPED=()
START_TS=$(date +%s)

for entry in "${WITH_TESTS[@]}"; do
	IFS='|' read -r name lang kvm cmd <<<"$entry"
	requested "$name" || continue

	if [[ "$kvm" == "1" && "$NO_KVM" == "1" ]]; then
		skip "$name ($lang) — needs /dev/kvm, skipped by --no-kvm"
		SKIPPED+=("$name")
		continue
	fi

	info "$name ($lang): $cmd"
	comp_start=$(date +%s)
	if bash -c "$cmd"; then
		comp_dur=$(($(date +%s) - comp_start))
		pass "$name (${comp_dur}s)"
		PASSED+=("$name")
	else
		rc=$?
		comp_dur=$(($(date +%s) - comp_start))
		fail "$name (${comp_dur}s, exit $rc)"
		FAILED+=("$name")
	fi
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

	info "$name ($lang, gated): $cmd"
	comp_start=$(date +%s)
	if bash -c "$cmd"; then
		comp_dur=$(($(date +%s) - comp_start))
		pass "$name (${comp_dur}s)"
		PASSED+=("$name")
	else
		rc=$?
		comp_dur=$(($(date +%s) - comp_start))
		fail "$name (${comp_dur}s, exit $rc)"
		FAILED+=("$name")
	fi
done

# Components with no unit tests: always announced when in scope.
for entry in "${NO_TESTS[@]}"; do
	IFS='|' read -r name reason <<<"$entry"
	requested "$name" || continue
	skip "$name — $reason"
	SKIPPED+=("$name")
done

# --- summary -----------------------------------------------------------------

TOTAL_DUR=$(($(date +%s) - START_TS))
printf '\n%s===== unit test summary (%ss) =====%s\n' "$C_BOLD" "$TOTAL_DUR" "$C_RESET"
printf '  passed:  %d  %s\n' "${#PASSED[@]}" "${PASSED[*]:-}"
printf '  failed:  %d  %s\n' "${#FAILED[@]}" "${FAILED[*]:-}"
printf '  skipped: %d  %s\n' "${#SKIPPED[@]}" "${SKIPPED[*]:-}"

[[ ${#FAILED[@]} -eq 0 ]]
