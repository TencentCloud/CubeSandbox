#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# One-click runner for the client SDK unit tests under sdk/.
#
# This is the companion to run.sh (which drives the monorepo's server/component
# tests inside the builder container). The SDKs have heterogeneous host
# toolchains — Go module, Node/vitest, Python/pytest — that don't fit the
# builder-image matrix, so they run directly on the host toolchain instead.
# .github/workflows/sdk-test-check.yml sets up each toolchain, then defers to
# this script so CI and local runs share one code path.
#
# Each SDK's live/integration tests are gated (build tag / env flag / not
# pytest-collected), so the commands below run only the hermetic unit tests.
#
# Usage:
#   tests/unittest/run_sdk_test.sh            # run all SDKs
#   tests/unittest/run_sdk_test.sh go python  # run only the named SDKs
#   tests/unittest/run_sdk_test.sh --list     # list SDKs and exit
#   tests/unittest/run_sdk_test.sh -h | --help
#
# SDK names: go, node, python. Exit status is non-zero if any run failed.

set -uo pipefail

# Locate the repo root by walking up from the script's own directory until we
# find the Makefile that defines the `builder-run` target — same approach as
# run.sh, so the script works regardless of where it is invoked from.
find_repo_root() {
	local src="${BASH_SOURCE[0]}"
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

# --- SDK table ---------------------------------------------------------------
#
# SDKS: "name|language|command"
#   command runs from REPO_ROOT and includes dependency install so a single
#   invocation reproduces CI. Live/integration suites are excluded by each
#   tool's own gate:
#     go     — integration_test.go is `//go:build integration`, so the default
#              `go test ./...` skips it.
#     node   — the live suite is `describe.skipIf(CUBE_RUN_INTEGRATION!=1)`, so
#              `npm test` runs only unit tests.
#     python — pytest collects only tests/test_*.py; the standalone
#              integration_test_* drivers are never collected. The command
#              resolves python3 (falling back to python) since some hosts ship
#              only one of the two.

SDKS=(
	"go|Go|cd sdk/go && go test ./..."
	"node|Node|cd sdk/node && npm ci && npm test"
	"python|Python|cd sdk/python && py=\$(command -v python3 || command -v python) && \"\$py\" -m pip install -e '.[dev]' && \"\$py\" -m pytest"
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
	sed -n '5,23p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

list_sdks() {
	printf '%sClient SDK unit tests:%s\n' "$C_BOLD" "$C_RESET"
	local entry name lang
	for entry in "${SDKS[@]}"; do
		IFS='|' read -r name lang _ <<<"$entry"
		printf '  %-10s %s\n' "$name" "$lang"
	done
}

# --- arg parsing -------------------------------------------------------------

SELECTED=()
for arg in "$@"; do
	case "$arg" in
	-h | --help)
		usage
		exit 0
		;;
	--list)
		list_sdks
		exit 0
		;;
	-*)
		fail "unknown option: $arg"
		usage
		exit 2
		;;
	*) SELECTED+=("$arg") ;;
	esac
done

known_sdk() {
	local q="$1" entry name
	for entry in "${SDKS[@]}"; do
		IFS='|' read -r name _ <<<"$entry"
		[[ "$name" == "$q" ]] && return 0
	done
	return 1
}
for want in "${SELECTED[@]}"; do
	if ! known_sdk "$want"; then
		fail "unknown SDK: $want (use --list to see valid names)"
		exit 2
	fi
done

requested() {
	[[ ${#SELECTED[@]} -eq 0 ]] && return 0
	local q="$1" s
	for s in "${SELECTED[@]}"; do [[ "$s" == "$q" ]] && return 0; done
	return 1
}

# --- run ---------------------------------------------------------------------

PASSED=()
FAILED=()
START_TS=$(date +%s)

for entry in "${SDKS[@]}"; do
	IFS='|' read -r name lang cmd <<<"$entry"
	requested "$name" || continue

	info "$name ($lang): $cmd"
	sdk_start=$(date +%s)
	# `bash -c` runs in its own process, so each command's `cd` stays local and
	# does not leak into the next SDK.
	if bash -c "$cmd"; then
		sdk_dur=$(($(date +%s) - sdk_start))
		pass "$name (${sdk_dur}s)"
		PASSED+=("$name")
	else
		rc=$?
		sdk_dur=$(($(date +%s) - sdk_start))
		fail "$name (${sdk_dur}s, exit $rc)"
		FAILED+=("$name")
	fi
done

# --- summary -----------------------------------------------------------------

TOTAL_DUR=$(($(date +%s) - START_TS))
printf '\n%s===== SDK unit test summary (%ss) =====%s\n' "$C_BOLD" "$TOTAL_DUR" "$C_RESET"
printf '  passed:  %d  %s\n' "${#PASSED[@]}" "${PASSED[*]:-}"
printf '  failed:  %d  %s\n' "${#FAILED[@]}" "${FAILED[*]:-}"

[[ ${#FAILED[@]} -eq 0 ]]
