#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# collect-logs.sh — Cube Sandbox log & diagnostic collector
# Standalone script; no external dependencies.
#
# Log sources collected:
#   /var/log/cube-sandbox-one-click/   runtime logs (cubemaster, cubelet, cube-api, …)
#   /data/log/CubeMaster/              CubeMaster request/stat logs
#   /data/log/Cubelet/                 Cubelet request/stat/audit logs
#   /data/log/CubeAPI/                 cube-api rotated daily logs
#   /data/log/CubeShim/                CubeShim request/stat logs
#   /data/log/CubeVmm/                 VMM logs
#   /data/log/network-agent/           network-agent request logs
#   cube-proxy (Docker)                error.log and access.log via docker exec
#   dmesg                              kernel ring buffer
#   process/env snapshot               ps, ports, mounts, cgroup, cpuinfo, …
#   config files                       with secrets redacted
#
# Usage:
#   ./collect-logs.sh [OPTIONS]
#
# Options:
#   --module <name>     Collect only the specified module(s); repeat for multiple.
#                       Module names: cubemaster cubelet cube-api cubeshim
#                       cubevmm network-agent cube-proxy runtime dmesg env configs
#                       Default: all modules
#   --lines <n>         Tail N lines per log file (default: 2000)
#   --all-lines         Collect entire log files (may be very large)
#   --dir <dir>         Output directory (default: cube-diag-<timestamp> under CWD)
#                       Must not already exist or be non-empty (exits with error if non-empty)
#
# Exit: 0 = collection complete (partial source failures are warned, not fatal)

set -uo pipefail

# ── Help ──────────────────────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
Usage: collect-logs.sh [OPTIONS]

Collect Cube Sandbox logs and diagnostic information into a directory.

Log sources:
  /var/log/cube-sandbox-one-click/   Runtime startup logs
  /data/log/CubeMaster/              CubeMaster request/stat logs
  /data/log/Cubelet/                 Cubelet request/stat/audit logs
  /data/log/CubeAPI/                 cube-api rotated daily logs
  /data/log/CubeShim/                CubeShim request/stat logs
  /data/log/CubeVmm/                 VMM logs
  /data/log/network-agent/           network-agent request logs
  cube-proxy (Docker)                error.log and access.log via docker exec
  dmesg                              Full kernel ring buffer + filtered views
  env                                Process list, ports, mounts, cgroup, cpuinfo, ...
  configs                            Config files with secrets redacted

Options:
  --module <name>   Collect only the specified module. Repeat to select multiple.
                    Available modules:
                      cubemaster  cubelet  cube-api  cubeshim  cubevmm
                      network-agent  cube-proxy  runtime  dmesg  env  configs
                    Default: all modules
  --lines <n>       Tail N lines per log file (default: 2000)
  --all-lines       Copy entire log files without truncation.
                    WARNING: some logs exceed 1 million lines; output may be very large.
  --dir <dir>       Output directory.
                    Default: cube-diag-<timestamp> under the current working directory.
                    Must not already exist or be non-empty (exits with error if non-empty).
  --help            Show this help message and exit

Environment variables:
  ONE_CLICK_LOG_DIR        Runtime log directory (default: /var/log/cube-sandbox-one-click)
  ONE_CLICK_RUNTIME_DIR    PID file directory (default: /var/run/cube-sandbox-one-click)
  CUBE_DATA_LOG_DIR        Structured log root (default: /data/log)

Exit codes:
  0   Collection finished (individual source failures are warned, not fatal)

Examples:
  # Collect everything (default)
  ./collect-logs.sh

  # Only cubelet and dmesg, last 500 lines each
  ./collect-logs.sh --module cubelet --module dmesg --lines 500

  # Full cubemaster log (may be very large)
  ./collect-logs.sh --module cubemaster --all-lines

  # Save to a specific directory without creating a tarball
  ./collect-logs.sh --dir ./my-diag

EOF
}


# ── Config ─────────────────────────────────────────────────────────────────────
TOOLBOX_ROOT="/usr/local/services/cubetoolbox"
RUNTIME_LOG_DIR="${ONE_CLICK_LOG_DIR:-/var/log/cube-sandbox-one-click}"
RUNTIME_PID_DIR="${ONE_CLICK_RUNTIME_DIR:-/var/run/cube-sandbox-one-click}"
DATA_LOG_DIR="${CUBE_DATA_LOG_DIR:-/data/log}"
TS="$(date +%Y%m%d_%H%M%S)"

# ── CLI flags ──────────────────────────────────────────────────────────────────
TAIL_LINES=2000
ALL_LINES=0
# Default: cube-diag-<ts> under CWD (not /tmp), no compression
_DEFAULT_OUT_NAME="cube-diag-${TS}"
OUT_DIR="${PWD}/${_DEFAULT_OUT_NAME}"
ALLOW_EXISTING=0
declare -a SELECTED_MODULES=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --module)    SELECTED_MODULES+=("$2"); shift 2 ;;
    --lines)     TAIL_LINES="$2"; shift 2 ;;
    --all-lines) ALL_LINES=1; shift ;;
    --dir)
      # Support both absolute and relative paths
      case "$2" in
        /*) OUT_DIR="$2" ;;
        *)  OUT_DIR="${PWD}/$2" ;;
      esac
      shift 2 ;;
    # Internal flag used by cube-check.sh to merge into an existing directory
    --allow-existing) ALLOW_EXISTING=1; shift ;;
    # Kept for backward compatibility but now a no-op (no tarball is created)
    --no-tar) shift ;;
    --help|-h)   usage; exit 0 ;;
    *)           echo "[collect] WARNING: unrecognized option: $1" >&2; shift ;;
  esac
done

# If no --module given, collect everything
_ALL_MODULES=(cubemaster cubelet cube-api cubeshim cubevmm network-agent cube-proxy runtime dmesg env configs)
if [[ "${#SELECTED_MODULES[@]}" -eq 0 ]]; then
  SELECTED_MODULES=("${_ALL_MODULES[@]}")
fi

_module_selected() {
  local m
  for m in "${SELECTED_MODULES[@]}"; do
    [[ "${m}" == "$1" ]] && return 0
  done
  return 1
}

# ── Setup ──────────────────────────────────────────────────────────────────────
if [[ "${ALLOW_EXISTING}" -eq 0 ]] && [[ -e "${OUT_DIR}" ]] && [[ -n "$(ls -A "${OUT_DIR}" 2>/dev/null)" ]]; then
  echo "[collect] ERROR: output directory already exists and is not empty: ${OUT_DIR}" >&2
  echo "[collect] Use a different path with --dir, or remove the existing directory first." >&2
  exit 1
fi
mkdir -p "${OUT_DIR}"
SUMMARY="${OUT_DIR}/SUMMARY.txt"

_info() { echo "[collect] INFO:  $*" | tee -a "${SUMMARY}" >&2; }
_warn() { echo "[collect] WARN:  $*" | tee -a "${SUMMARY}" >&2; }

_capture() {
  # _capture <label> <dest_file> <cmd...>
  local label="$1" dest="$2"; shift 2
  if "$@" >"${dest}" 2>&1; then
    _info "${label} → $(basename "${dest}")"
  else
    _warn "${label} failed (partial output may exist)"
  fi
}

_redact() {
  sed -E \
    -e 's/(password|pwd|passwd|secret|token|key)[[:space:]]*[:=][[:space:]]*\S*/\1: [REDACTED]/gi' \
    -e 's/"(Password|Pwd|Secret|Token|Key)"[[:space:]]*:[[:space:]]*"[^"]*/"\1": "[REDACTED]/g' \
    -e 's/^([[:space:]]*)(password|secret|token|key)[[:space:]]*:[[:space:]]*\S*/\1\2: [REDACTED]/gmi'
}

# Collect one log file: tail N lines or full copy
_collect_log() {
  local src="$1" dest_dir="$2"
  [[ -f "${src}" ]] || return 0
  local base; base="$(basename "${src}")"
  local dest="${dest_dir}/${base}"

  if [[ "${ALL_LINES}" -eq 1 ]]; then
    cp "${src}" "${dest}" 2>/dev/null \
      && _info "full copy: ${src}" \
      || _warn "could not copy ${src}"
  else
    tail -n "${TAIL_LINES}" "${src}" >"${dest}" 2>/dev/null \
      && _info "tail -${TAIL_LINES}: ${src}" \
      || _warn "could not tail ${src}"
  fi
}

# Collect all logs under a /data/log/<Module>/ directory
_collect_data_log_dir() {
  local module_dir="$1" dest_dir="$2"
  [[ -d "${module_dir}" ]] || { _warn "${module_dir} not found — skipping"; return; }
  mkdir -p "${dest_dir}"
  while IFS= read -r -d '' f; do
    _collect_log "${f}" "${dest_dir}"
  done < <(find "${module_dir}" -maxdepth 2 -name "*.log" -print0 2>/dev/null)
}

# ── Module collectors ─────────────────────────────────────────────────────────

collect_cubemaster() {
  _info "── CubeMaster ──"
  local dest="${OUT_DIR}/CubeMaster"
  mkdir -p "${dest}"
  _collect_data_log_dir "${DATA_LOG_DIR}/CubeMaster" "${dest}"
  # Also grab the runtime startup log if present
  _collect_log "${RUNTIME_LOG_DIR}/cubemaster.log" "${dest}"
}

collect_cubelet() {
  _info "── Cubelet ──"
  local dest="${OUT_DIR}/Cubelet"
  mkdir -p "${dest}"
  _collect_data_log_dir "${DATA_LOG_DIR}/Cubelet" "${dest}"
  _collect_log "${RUNTIME_LOG_DIR}/cubelet.log"       "${dest}"
  _collect_log "${RUNTIME_LOG_DIR}/cubelet-debug.log" "${dest}"
}

collect_cube_api() {
  _info "── CubeAPI ──"
  local dest="${OUT_DIR}/CubeAPI"
  mkdir -p "${dest}"
  _collect_data_log_dir "${DATA_LOG_DIR}/CubeAPI" "${dest}"
  _collect_log "${RUNTIME_LOG_DIR}/cube-api.log" "${dest}"
}

collect_cubeshim() {
  _info "── CubeShim ──"
  local dest="${OUT_DIR}/CubeShim"
  mkdir -p "${dest}"
  _collect_data_log_dir "${DATA_LOG_DIR}/CubeShim" "${dest}"
}

collect_cubevmm() {
  _info "── CubeVmm ──"
  local dest="${OUT_DIR}/CubeVmm"
  mkdir -p "${dest}"
  _collect_data_log_dir "${DATA_LOG_DIR}/CubeVmm" "${dest}"
}

collect_network_agent() {
  _info "── network-agent ──"
  local dest="${OUT_DIR}/network-agent"
  mkdir -p "${dest}"
  _collect_data_log_dir "${DATA_LOG_DIR}/network-agent" "${dest}"
  _collect_log "${RUNTIME_LOG_DIR}/network-agent.log" "${dest}"
}

collect_cube_proxy() {
  _info "── cube-proxy ──"
  local dest="${OUT_DIR}/cube-proxy"
  mkdir -p "${dest}"

  # cube-proxy runs inside a Docker container; its logs are not on the host
  # filesystem. Find the running container by name (or image path containing
  # /cube-proxy:) and extract via docker exec.
  if ! command -v docker >/dev/null 2>&1; then
    _warn "docker not found — cannot collect cube-proxy container logs"
    return
  fi

  # Locate the cube-proxy container by exact name 'cube-proxy'.
  # docker --filter name= does prefix/substring matching, so we must verify
  # the exact name from the output to avoid matching 'cube-proxy-coredns'.
  local cid
  cid="$(docker ps --filter 'name=cube-proxy' --filter 'status=running' \
           --format '{{.Names}}\t{{.ID}}' 2>/dev/null \
         | awk '$1=="cube-proxy" {print $2}' | head -1)"
  if [[ -z "${cid}" ]]; then
    # Fall back: match by image name (local cube-proxy:* or registry .../cube-proxy:*)
    cid="$(docker ps --filter 'status=running' \
             --format '{{.Image}}\t{{.ID}}' 2>/dev/null \
           | awk '$1~/(^|\/)cube-proxy:/ {print $2}' | head -1)"
  fi
  if [[ -z "${cid}" ]]; then
    _warn "cube-proxy container not running — cannot collect its logs"
    return
  fi
  _info "cube-proxy container: ${cid}"

  local -a log_paths=(
    /data/log/cube-proxy/error.log
    /data/log/cube-proxy/access.log
  )

  for log_path in "${log_paths[@]}"; do
    local base; base="$(basename "${log_path}")"
    local out_file="${dest}/${base}"
    # Use '[ -f ... ]' via sh -c to avoid relying on the 'test' binary
    # which may not be present in the container's PATH.
    if docker exec "${cid}" sh -c "[ -f '${log_path}' ]" 2>/dev/null; then
      if [[ "${ALL_LINES}" -eq 1 ]]; then
        docker exec "${cid}" cat "${log_path}" > "${out_file}" 2>/dev/null \
          && _info "docker exec ${cid} cat ${log_path}" \
          || _warn "could not read ${log_path} from container ${cid}"
      else
        docker exec "${cid}" sh -c "tail -n ${TAIL_LINES} ${log_path}" \
          > "${out_file}" 2>/dev/null \
          && _info "docker exec ${cid} tail -${TAIL_LINES} ${log_path}" \
          || _warn "could not tail ${log_path} from container ${cid}"
      fi
    else
      _warn "${log_path} not found inside container ${cid}"
    fi
  done
}

collect_runtime() {
  _info "── Runtime logs ──"
  local dest="${OUT_DIR}/runtime"
  mkdir -p "${dest}"

  # Remaining runtime logs not already pulled by per-module collectors
  if [[ -d "${RUNTIME_LOG_DIR}" ]]; then
    while IFS= read -r -d '' f; do
      _collect_log "${f}" "${dest}"
    done < <(find "${RUNTIME_LOG_DIR}" -maxdepth 1 -name "*.log" -print0 2>/dev/null)
  fi

  # Process snapshot
  _capture "ps_cube" "${dest}/ps-cube.txt" \
    bash -c 'ps auxww | grep -E "cube|cubelet|cubemaster|network-agent|containerd-shim" | grep -v grep || true'
  _capture "ports"   "${dest}/ports.txt"   ss -tlnp

  {
    ls -la "${RUNTIME_PID_DIR}/" 2>/dev/null || echo "(${RUNTIME_PID_DIR} not found)"
    for f in "${RUNTIME_PID_DIR}"/*.pid; do
      [[ -f "${f}" ]] && printf '%s -> pid=%s\n' "$(basename "${f}")" "$(<"${f}")" || true
    done
  } > "${dest}/pidfiles.txt" 2>/dev/null
  _info "pidfiles → runtime/pidfiles.txt"
}

collect_dmesg() {
  _info "── dmesg ──"
  local dest="${OUT_DIR}/dmesg"
  mkdir -p "${dest}"

  # Full dmesg
  _capture "dmesg_full" "${dest}/dmesg.txt" \
    bash -c 'dmesg --notime 2>/dev/null || dmesg 2>/dev/null || true'

  # KVM/PVM/cube filtered
  _capture "dmesg_kvm" "${dest}/dmesg-kvm-pvm.txt" \
    bash -c 'dmesg --notime 2>/dev/null | grep -iE "kvm|pvm|cube|vmx|svm|hv_" | tail -200 || true'

  # OOM / hardware errors
  _capture "dmesg_errors" "${dest}/dmesg-errors.txt" \
    bash -c 'dmesg --notime 2>/dev/null | grep -iE "oom|kill|error|fail|warn|segfault" | tail -200 || true'
}

collect_env() {
  _info "── Environment snapshot ──"
  local dest="${OUT_DIR}/env"
  mkdir -p "${dest}"

  _capture "uname"     "${dest}/uname.txt"    uname -a
  _capture "cpuinfo"   "${dest}/cpuinfo.txt"  cat /proc/cpuinfo
  _capture "meminfo"   "${dest}/meminfo.txt"  cat /proc/meminfo
  _capture "lsmod"     "${dest}/lsmod.txt"    lsmod
  _capture "df"        "${dest}/df.txt"       df -Th
  _capture "mounts"    "${dest}/mounts.txt"   findmnt --notruncate
  _capture "cgroup"    "${dest}/cgroup.txt" \
    bash -c 'echo "=controllers="; cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null
             echo; echo "=subtree_control="; cat /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true'
  _capture "dev_virt"  "${dest}/dev-virt.txt" \
    bash -c 'ls -la /dev/kvm /dev/pvm 2>/dev/null || echo "no /dev/kvm or /dev/pvm"'
  _capture "ps_all"    "${dest}/ps-all.txt"   ps auxww
}

collect_configs() {
  _info "── Config files (secrets redacted) ──"
  local dest="${OUT_DIR}/configs"
  mkdir -p "${dest}"

  local -a files=(
    "${TOOLBOX_ROOT}/Cubelet/config/config.toml"
    "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
    "${TOOLBOX_ROOT}/cube-shim/conf/config-cube.toml"
    "${TOOLBOX_ROOT}/CubeMaster/conf.yaml"
    "${TOOLBOX_ROOT}/.one-click.env"
  )
  for f in "${files[@]}"; do
    if [[ -f "${f}" ]]; then
      local base; base="$(basename "${f}")"
      _redact < "${f}" > "${dest}/${base}" 2>/dev/null \
        && _info "${f} (secrets redacted)" \
        || _warn "could not read ${f}"
    fi
  done
}

# ── Tarball ────────────────────────────────────────────────────────────────────
print_done() {
  echo
  echo "╔══════════════════════════════════════════════════════╗"
  echo "║  Diagnostics collection complete                    ║"
  echo "╚══════════════════════════════════════════════════════╝"
  printf "  Directory : %s\n" "${OUT_DIR}"
  printf "  Size      : %s\n" "$(du -sh "${OUT_DIR}" 2>/dev/null | cut -f1)"
  echo
  echo "  To package and share:"
  printf "    tar czf %s.tar.gz %s\n" \
    "$(basename "${OUT_DIR}")" "$(basename "${OUT_DIR}")"
  echo
  echo "  Issues: https://github.com/TencentCloud/CubeSandbox/issues"
  echo "  Discord: https://discord.gg/kkapzDXShb"
}

# ── Main ───────────────────────────────────────────────────────────────────────
main() {
  echo "╔══════════════════════════════════════════════════════╗"
  echo "║    Cube Sandbox — Log & Diagnostic Collector        ║"
  echo "╚══════════════════════════════════════════════════════╝"
  echo "  Output   : ${OUT_DIR}"
  if [[ "${ALL_LINES}" -eq 1 ]]; then
    echo "  Lines    : ALL (--all-lines)"
  else
    echo "  Lines    : ${TAIL_LINES} per file"
  fi
  echo "  Modules  : ${SELECTED_MODULES[*]}"
  echo

  _module_selected "cubemaster"    && collect_cubemaster
  _module_selected "cubelet"       && collect_cubelet
  _module_selected "cube-api"      && collect_cube_api
  _module_selected "cubeshim"      && collect_cubeshim
  _module_selected "cubevmm"       && collect_cubevmm
  _module_selected "network-agent" && collect_network_agent
  _module_selected "cube-proxy"    && collect_cube_proxy
  _module_selected "runtime"       && collect_runtime
  _module_selected "dmesg"         && collect_dmesg
  _module_selected "env"           && collect_env
  _module_selected "configs"       && collect_configs

  _info "Collection complete"
  print_done
}

main "$@"
