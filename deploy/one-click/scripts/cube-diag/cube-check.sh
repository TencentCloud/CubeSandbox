#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-check.sh - Cube Sandbox one-click health check (top-level entry point)
# Standalone script; no external dependencies.
#
# Runs check modules in order and optionally collects diagnostic logs.
# All output is written to stdout AND saved to an output directory when
# --collect is given.
#
# Output directory layout (only created with --collect):
#   <out>/check.log          combined stdout of all checks
#   <out>/CubeMaster/        collected logs (per module sub-dirs)
#   <out>/Cubelet/
#   <out>/...
#
# Usage:
#   ./cube-check.sh                  # deps + procs only; no log collection
#   ./cube-check.sh --collect        # deps + procs + collect logs into <out>
#   ./cube-check.sh --deps           # only dependency check
#   ./cube-check.sh --procs          # only process check
#   ./cube-check.sh --quiet          # suppress OK lines in sub-checks
#   ./cube-check.sh --json           # JSON output from sub-checks
#   ./cube-check.sh --collect        # deps + procs + collect logs, dir name = cube-diag-<ts>
#   ./cube-check.sh --collect --dir <name>   # use custom directory name
#   ./cube-check.sh --lines <n>      # lines per log file (default: 2000)
#   ./cube-check.sh --since "<expr>" # journald since filter
#
# Exit: 0 = all selected checks passed, 1 = one or more failures

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Help ───────────────────────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
Usage: cube-check.sh [OPTIONS]

One-click Cube Sandbox health check.

This script orchestrates the sub-scripts in the same directory:
  check-deps.sh    Host environment dependency checker
  check-procs.sh   Process readiness checker
  collect-logs.sh  Log and diagnostic collector (only with --collect)

Each sub-script can also be run independently. Use --help on any of them
for its own detailed usage.

Options:
  --collect        Run collect-logs.sh and save everything (check output +
                   collected logs) into a single output directory.
                   Default: checks only, no log collection.
  --deps           Run only check-deps.sh (no log collection)
  --procs          Run only check-procs.sh (no log collection)
  --quiet          Suppress OK lines in sub-checks; only print WARN/FAIL
  --json           Pass --json to sub-checks for machine-readable output
  --dir <dirname>  Output directory for --collect.
                   Relative paths are resolved under the current working directory;
                   absolute paths are used as-is.
                   Must not already exist or be non-empty (exits with error if non-empty).
                   Default: cube-diag-<timestamp>
  --lines <n>      Lines per log file passed to collect-logs.sh (default: 2000)
  --all-lines      Pass --all-lines to collect-logs.sh (collect full files)
  --since <expr>   journald --since filter passed to collect-logs.sh
  --help           Show this help message and exit

Default behaviour (no flags):
  Runs check-deps.sh then check-procs.sh.
  Check output goes to stdout only. No files are written.

With --collect:
  Runs checks + collect-logs.sh.
  All stdout is ALSO written to <out>/check.log.
  Collected logs are written into subdirectories under <out>.
  At the end, the directory path is printed so you can tar it yourself:
    tar czf cube-diag-<ts>.tar.gz cube-diag-<ts>

Exit codes:
  0   All selected checks passed (warnings are allowed)
  1   One or more checks failed

Examples:
  # Quick health check (stdout only)
  ./cube-check.sh

  # Full check + collect logs, default directory name (./cube-diag-<ts>/)
  ./cube-check.sh --collect

  # Full check + collect logs, custom directory name (./my-server-diag/)
  ./cube-check.sh --collect --dir my-server-diag

  # Only deps, suppress OK lines
  ./cube-check.sh --deps --quiet

  # CI mode: JSON output, no collection
  ./cube-check.sh --json
EOF
}

# ── CLI flags ──────────────────────────────────────────────────────────────────
RUN_DEPS=1
RUN_PROCS=1
DO_COLLECT=0
OUT_NAME=""
declare -a SUB_FLAGS=()
declare -a COLLECT_FLAGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --collect)    DO_COLLECT=1; shift ;;
    --deps)       RUN_DEPS=1; RUN_PROCS=0; DO_COLLECT=0; shift ;;
    --procs)      RUN_DEPS=0; RUN_PROCS=1; DO_COLLECT=0; shift ;;
    --quiet)      SUB_FLAGS+=(--quiet); shift ;;
    --json)       SUB_FLAGS+=(--json);  shift ;;
    --dir)        OUT_NAME="$2"; shift 2 ;;
    --lines)      COLLECT_FLAGS+=(--lines "$2"); shift 2 ;;
    --all-lines)  COLLECT_FLAGS+=(--all-lines); shift ;;
    --since)      COLLECT_FLAGS+=(--since "$2"); shift 2 ;;
    --help|-h)    usage; exit 0 ;;
    *)            echo "[cube-check] WARNING: unrecognized option: $1" >&2; shift ;;
  esac
done

# ── Output directory (only used with --collect) ────────────────────────────────
TS="$(date +%Y%m%d_%H%M%S)"
# Resolve output directory: absolute path used as-is; relative path resolved under CWD
_dirname="${OUT_NAME:-cube-diag-${TS}}"
case "${_dirname}" in
  /*) OUT_DIR="${_dirname}" ;;
  *)  OUT_DIR="${PWD}/${_dirname}" ;;
esac
CHECK_LOG=""   # set after OUT_DIR is created (only with --collect)

# ── Tee helpers ────────────────────────────────────────────────────────────────
# _println: print a line to stdout; also append to check.log when collecting
_println() { echo "$@"; [[ -n "${CHECK_LOG}" ]] && echo "$@" >> "${CHECK_LOG}"; }

# ── Helpers ────────────────────────────────────────────────────────────────────
OVERALL_RC=0

_run_module() {
  local name="$1"; shift
  local script="${SCRIPT_DIR}/${name}"

  if [[ ! -x "${script}" ]]; then
    _println "[cube-check] ERROR: ${script} not found or not executable"
    OVERALL_RC=1
    return 1
  fi

  _println
  _println "┌──────────────────────────────────────────────────────┐"
  _println "│  Running: $(printf '%-42s' "${name}") │"
  _println "└──────────────────────────────────────────────────────┘"

  local rc=0
  if [[ -n "${CHECK_LOG}" ]]; then
    # tee sub-script stdout to both terminal and check.log
    "${script}" "$@" 2>&1 | tee -a "${CHECK_LOG}" || rc=${PIPESTATUS[0]}
  else
    "${script}" "$@" || rc=$?
  fi

  if [[ "${rc}" -eq 0 ]]; then
    _println "[cube-check] ${name}: PASS"
  else
    _println "[cube-check] ${name}: FAIL"
    OVERALL_RC=1
  fi
  return "${rc}"
}

# ── Main ───────────────────────────────────────────────────────────────────────
main() {
  # Create output directory and start check.log only when collecting
  if [[ "${DO_COLLECT}" -eq 1 ]]; then
    if [[ -e "${OUT_DIR}" ]]; then
      echo "[cube-check] ERROR: output directory already exists: ${OUT_DIR}" >&2
      echo "[cube-check] Use a different name with --dir, or remove the existing directory first." >&2
      exit 1
    fi
    mkdir -p "${OUT_DIR}"
    CHECK_LOG="${OUT_DIR}/check.log"
    : > "${CHECK_LOG}"   # truncate / create
  fi

  _println
  _println "╔══════════════════════════════════════════════════════╗"
  _println "║        Cube Sandbox — One-Click Health Check        ║"
  _println "╚══════════════════════════════════════════════════════╝"
  _println "  $(date '+%Y-%m-%d %H:%M:%S %Z')"
  _println "  Host: $(hostname)"
  _println "  Role: ${ONE_CLICK_DEPLOY_ROLE:-control}"
  if [[ "${DO_COLLECT}" -eq 1 ]]; then
    _println "  Output dir: ${OUT_DIR}"
  fi

  if [[ "${RUN_DEPS}"  -eq 1 ]]; then
    _run_module "check-deps.sh"  "${SUB_FLAGS[@]+"${SUB_FLAGS[@]}"}"
  fi
  if [[ "${RUN_PROCS}" -eq 1 ]]; then
    _run_module "check-procs.sh" "${SUB_FLAGS[@]+"${SUB_FLAGS[@]}"}"
  fi

  # Collect logs into the same output directory (only with --collect)
  if [[ "${DO_COLLECT}" -eq 1 ]]; then
    _println
    _println "[cube-check] Collecting logs into ${OUT_DIR} ..."
    if [[ -n "${CHECK_LOG}" ]]; then
      "${SCRIPT_DIR}/collect-logs.sh" \
        --dir "${OUT_DIR}" \
        --no-tar \
        --allow-existing \
        "${COLLECT_FLAGS[@]+"${COLLECT_FLAGS[@]}"}" 2>&1 | tee -a "${CHECK_LOG}"
    else
      "${SCRIPT_DIR}/collect-logs.sh" \
        --dir "${OUT_DIR}" \
        --no-tar \
        --allow-existing \
        "${COLLECT_FLAGS[@]+"${COLLECT_FLAGS[@]}"}"
    fi
    _println "[cube-check] collect-logs.sh: done"
  fi

  # ── Summary ────────────────────────────────────────────────────────────────
  _println
  _println "══════════════════════════════════════════════════════"
  if [[ "${OVERALL_RC}" -eq 0 ]]; then
    _println "  OVERALL RESULT: PASS ✓"
  else
    _println "  OVERALL RESULT: FAIL ✗"
  fi

  if [[ "${DO_COLLECT}" -eq 1 ]]; then
    _println
    _println "  Diagnostic bundle directory:"
    _println "    ${OUT_DIR}/"
    _println
    _println "  To package and share:"
    _println "    tar czf $(basename "${OUT_DIR}").tar.gz $(basename "${OUT_DIR}")"
  else
    _println
    if [[ "${OVERALL_RC}" -ne 0 ]]; then
      _println "  To collect diagnostic logs, re-run with --collect:"
      _println "    $(basename "$0") --collect"
    else
      _println "  To collect a diagnostic bundle: $(basename "$0") --collect"
    fi
  fi

  _println
  _println "  Issues: https://github.com/TencentCloud/CubeSandbox/issues"
  _println "  Discord: https://discord.gg/kkapzDXShb"
  _println "══════════════════════════════════════════════════════"

  exit "${OVERALL_RC}"
}

main "$@"
