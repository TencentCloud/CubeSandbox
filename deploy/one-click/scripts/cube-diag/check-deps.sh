#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# check-deps.sh — Cube Sandbox environment dependency checker
# Standalone script; no external dependencies.
#
# Checks:
#   • Kernel version floor (>= 5.4)
#   • Virtualization capability — three supported environments:
#       Bare-metal KVM:   no hypervisor flag, has vmx/svm → /dev/kvm required
#       PVM host VM:      kvm_pvm module loaded          → /dev/pvm required
#       Nested KVM:       hypervisor flag, no kvm_pvm    → /dev/kvm present, WARN
#   • cgroup v2 + subsystems actually required by Cubelet (cpu, memory, cpuset)
#   • /data/cubesandbox must be XFS; /data/cubelet must exist; disk >= 20 GB
#   • Required CLI tools (curl, ss, findmnt, awk, lsmod, modinfo)
#
# Exit: 0 = all pass/warn, 1 = one or more failures
# All checks run even on failure; exit code is deferred to the end.

set -uo pipefail

# ── Help ───────────────────────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
Usage: check-deps.sh [OPTIONS]

Verify host-level prerequisites for Cube Sandbox.

Supported host environments:
  Bare-metal KVM    No hypervisor CPU flag; has vmx/svm; /dev/kvm required.
  PVM host VM       Host runs inside a VM but has kvm_pvm module loaded;
                    /dev/kvm is backed by kvm_pvm (no separate /dev/pvm device).
  Nested KVM        Host is a VM (hypervisor flag), no kvm_pvm; /dev/kvm
                    present but nested virtualisation performance is degraded.

Checks performed:
  kernel       Kernel version >= 5.4
  virt         Detect host environment; verify appropriate /dev/kvm or /dev/pvm;
               warn explicitly on nested KVM (degraded performance)
  cgroup       cgroup v2 with subsystems required by Cubelet: cpu, memory, cpuset
  filesystem   /data/cubesandbox must be XFS; /data/cubelet must exist;
               disk free >= 20 GB
  commands     curl, ss, findmnt, awk, lsmod, modinfo, docker

Options:
  --quiet      Suppress OK lines; only print WARN and FAIL entries
  --json       Print a machine-readable JSON summary to stdout after the run
  --help       Show this help message and exit

Environment variables:
  CUBE_DATA_PATH   Override the XFS data path check (default: /data/cubesandbox)

Exit codes:
  0   All checks passed (warnings are allowed)
  1   One or more checks failed

Examples:
  ./check-deps.sh
  ./check-deps.sh --quiet
  ./check-deps.sh --json
  CUBE_DATA_PATH=/mnt/data ./check-deps.sh
EOF
}

# ── Config (override via env) ──────────────────────────────────────────────────
CUBE_DATA_PATH="${CUBE_DATA_PATH:-/data/cubesandbox}"

# ── CLI flags ──────────────────────────────────────────────────────────────────
QUIET=0
JSON_OUT=0
for _arg in "$@"; do
  case "${_arg}" in
    --quiet)   QUIET=1 ;;
    --json)    JSON_OUT=1 ;;
    --help|-h) usage; exit 0 ;;
  esac
done

# ── Result tracking ────────────────────────────────────────────────────────────
PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0
declare -a RESULTS=()

_record() {
  local level="$1" name="$2" detail="$3"
  RESULTS+=("${level}::${name}::${detail}")
  case "${level}" in
    PASS) PASS_COUNT=$((PASS_COUNT + 1)) ;;
    WARN) WARN_COUNT=$((WARN_COUNT + 1)) ;;
    FAIL) FAIL_COUNT=$((FAIL_COUNT + 1)) ;;
  esac
}

pass() { _record PASS "$1" "${2:-OK}"; [[ "${QUIET}" -eq 1 ]] || printf '  [ OK ]  %s%s\n' "$1" "${2:+: $2}"; }
warn() { _record WARN "$1" "$2";       printf '  [WARN]  %s: %s\n' "$1" "$2"; }
fail() { _record FAIL "$1" "$2";       printf '  [FAIL]  %s: %s\n' "$1" "$2"; }
section() { [[ "${QUIET}" -eq 1 ]] || printf '\n'; printf '── %s ──\n' "$*"; }

emit_json() {
  printf '{\n  "pass": %d,\n  "warn": %d,\n  "fail": %d,\n  "checks": [\n' \
    "${PASS_COUNT}" "${WARN_COUNT}" "${FAIL_COUNT}"
  local sep=""
  for r in "${RESULTS[@]}"; do
    local level name detail
    level="${r%%::*}"; r="${r#*::}"
    name="${r%%::*}"; detail="${r#*::}"
    detail="${detail//\\/\\\\}"; detail="${detail//\"/\\\"}"
    # strip control characters (newlines, tabs) to keep JSON on one line
    detail="$(printf '%s' "${detail}" | tr -d '\000-\037')"
    printf '%s    {"level":"%s","name":"%s","detail":"%s"}' \
      "${sep}" "${level}" "${name}" "${detail}"
    sep=$',\n'
  done
  printf '\n  ]\n}\n'
}

# ── Helpers ────────────────────────────────────────────────────────────────────
kernel_version_ok() {
  local req_major="$1" req_minor="$2"
  local kver major minor
  kver="$(uname -r)"
  major="$(echo "${kver}" | cut -d. -f1)"
  minor="$(echo "${kver}" | cut -d. -f2)"
  if [[ "${major}" -gt "${req_major}" ]]; then return 0; fi
  if [[ "${major}" -eq "${req_major}" && "${minor}" -ge "${req_minor}" ]]; then return 0; fi
  return 1
}

# Detect host environment.
# Sets globals: HOST_ENV (bare-metal | pvm-host | nested-kvm | unknown)
detect_host_env() {
  local has_hypervisor_flag=0
  local has_vmx_or_svm=0
  local has_kvm_pvm_mod=0

  grep -q 'hypervisor' /proc/cpuinfo 2>/dev/null && has_hypervisor_flag=1
  grep -qE '\bvmx\b|\bsvm\b' /proc/cpuinfo 2>/dev/null && has_vmx_or_svm=1
  lsmod 2>/dev/null | grep -qE '^kvm_pvm[[:space:]]' && has_kvm_pvm_mod=1

  if [[ "${has_kvm_pvm_mod}" -eq 1 ]]; then
    # kvm_pvm loaded — this is a PVM host (may itself be inside a VM)
    HOST_ENV="pvm-host"
  elif [[ "${has_hypervisor_flag}" -eq 0 && "${has_vmx_or_svm}" -eq 1 ]]; then
    # No hypervisor flag + native virt extensions → bare-metal
    HOST_ENV="bare-metal"
  elif [[ "${has_hypervisor_flag}" -eq 1 ]]; then
    # Running inside a hypervisor but no kvm_pvm → nested KVM
    HOST_ENV="nested-kvm"
  else
    HOST_ENV="unknown"
  fi
}

# ── Checks ─────────────────────────────────────────────────────────────────────
check_kernel() {
  section "Kernel"
  local kver; kver="$(uname -r)"
  if kernel_version_ok 5 4; then
    pass "kernel_version" "${kver}"
  else
    fail "kernel_version" "kernel ${kver} is below minimum 5.4"
  fi
}

check_virtualization() {
  section "Virtualization"

  detect_host_env

  case "${HOST_ENV}" in

    bare-metal)
      pass "host_env" "bare-metal (native vmx/svm, no hypervisor flag)"
      # Must have /dev/kvm
      if [[ -e /dev/kvm ]]; then
        pass "dev_kvm" "/dev/kvm present"
      else
        fail "dev_kvm" "/dev/kvm not found — load kvm module or enable VT-x/AMD-V in BIOS"
      fi
      # kvm or kvm_intel/kvm_amd module
      if lsmod 2>/dev/null | grep -qE '^kvm[[:space:]]'; then
        local kmod; kmod="$(lsmod 2>/dev/null | grep -oE '^kvm[_a-z]+' | head -1)"
        pass "kmod_kvm" "${kmod} module loaded"
      else
        fail "kmod_kvm" "kvm module not loaded — run: modprobe kvm_intel (or kvm_amd)"
      fi
      ;;

    pvm-host)
      pass "host_env" "PVM host (kvm_pvm module loaded — /dev/kvm provides PVM-accelerated virtualisation)"
      # On a PVM host, kvm_pvm module replaces the standard KVM backend.
      # /dev/kvm is the unified device node — there is no separate /dev/pvm.
      # The presence and health of kvm_pvm is confirmed by the module check below.
      pass "kmod_kvm_pvm" "kvm_pvm module loaded"
      if [[ -e /dev/kvm ]]; then
        pass "dev_kvm" "/dev/kvm present (backed by kvm_pvm)"
      else
        fail "dev_kvm" "/dev/kvm not found — kvm_pvm loaded but device missing; check dmesg"
      fi
      ;;

    nested-kvm)
      warn "host_env" \
        "nested KVM detected (running inside a VM without kvm_pvm) — performance will be degraded; PVM host is strongly recommended"
      if [[ -e /dev/kvm ]]; then
        pass "dev_kvm" "/dev/kvm present (nested KVM, performance degraded)"
      else
        fail "dev_kvm" "/dev/kvm not found — nested virtualisation not enabled or kvm module not loaded"
      fi
      if lsmod 2>/dev/null | grep -qE '^kvm[[:space:]]'; then
        local kmod; kmod="$(lsmod 2>/dev/null | grep -oE '^kvm[_a-z]+' | head -1)"
        pass "kmod_kvm" "${kmod} module loaded"
      else
        fail "kmod_kvm" "kvm module not loaded"
      fi
      ;;

    *)
      warn "host_env" \
        "could not determine host environment (no vmx/svm, no hypervisor flag, no kvm_pvm)"
      if [[ -e /dev/kvm ]]; then
        pass "dev_kvm" "/dev/kvm present"
      else
        fail "dev_kvm" "/dev/kvm not found and host environment unknown"
      fi
      ;;
  esac
}

check_cgroup() {
  section "cgroup v2"
  local cgroot="/sys/fs/cgroup"

  local fstype; fstype="$(stat -f -c '%T' "${cgroot}" 2>/dev/null || echo unknown)"
  if [[ "${fstype}" == "cgroup2fs" ]]; then
    pass "cgroup_v2" "cgroup v2 at ${cgroot}"
  else
    fail "cgroup_v2" "${cgroot} type=${fstype}; expected cgroup2fs — boot with systemd.unified_cgroup_hierarchy=1"
    # Cannot check subsystems without cgroup v2; return early
    return
  fi

  # Subsystems actually required by Cubelet (from cgroup/handle/v2/manager.go):
  #   cpu     — CPU quota (cpu.max)
  #   memory  — memory limit (memory.max)
  #   cpuset  — CPU pinning (cpuset.cpus) when cpuset is requested
  # pids / io / rdma / hugetlb are used when the corresponding resource field
  # is set in the sandbox spec, but cpu+memory+cpuset are the mandatory minimum.
  local controllers; controllers="$(cat "${cgroot}/cgroup.controllers" 2>/dev/null || true)"
  for sub in cpu memory cpuset; do
    if echo "${controllers}" | grep -qw "${sub}"; then
      pass "cgroup_subsys_${sub}" "${sub} available in cgroup.controllers"
    else
      fail "cgroup_subsys_${sub}" "${sub} missing from cgroup.controllers (got: ${controllers:-<empty>})"
    fi
  done

  # subtree_control must propagate cpu+memory+cpuset so child cgroups can be managed
  local subtree; subtree="$(cat "${cgroot}/cgroup.subtree_control" 2>/dev/null || true)"
  for sub in cpu memory cpuset; do
    if echo "${subtree}" | grep -qw "${sub}"; then
      pass "cgroup_subtree_${sub}" "${sub} in cgroup.subtree_control"
    else
      warn "cgroup_subtree_${sub}" \
        "${sub} not in cgroup.subtree_control — Cubelet may fail to create child cgroups; " \
        "fix: echo '+${sub}' > /sys/fs/cgroup/cgroup.subtree_control"
    fi
  done
}

check_filesystem() {
  section "Filesystem"

  if [[ -d "${CUBE_DATA_PATH}" ]]; then
    local fstype
    fstype="$(findmnt -n -o FSTYPE "${CUBE_DATA_PATH}" 2>/dev/null \
              || df -T "${CUBE_DATA_PATH}" 2>/dev/null | awk 'NR==2{print $2}' \
              || echo unknown)"
    if [[ "${fstype}" == "xfs" ]]; then
      pass "xfs_cubesandbox" "${CUBE_DATA_PATH} is XFS"
    else
      fail "xfs_cubesandbox" \
        "${CUBE_DATA_PATH} is ${fstype}; Cube requires XFS for CoW/reflink (pool_type=copy_reflink)
  Troubleshooting: https://github.com/TencentCloud/CubeSandbox/issues/311"
    fi

    local free_gb
    free_gb="$(df -BG "${CUBE_DATA_PATH}" 2>/dev/null | awk 'NR==2{gsub(/G/,"",$4); print $4}' || echo 0)"
    if [[ "${free_gb}" -ge 20 ]]; then
      pass "disk_free" "${free_gb} GB free on ${CUBE_DATA_PATH}"
    else
      warn "disk_free" "only ${free_gb} GB free on ${CUBE_DATA_PATH}; >= 20 GB recommended"
    fi
  else
    fail "xfs_cubesandbox" "${CUBE_DATA_PATH} does not exist"
  fi

  if [[ -d /data/cubelet ]]; then
    pass "dir_data_cubelet" "/data/cubelet exists"
  else
    fail "dir_data_cubelet" "/data/cubelet missing; run installer first"
  fi
}

check_commands() {
  section "Required commands"
  for cmd in curl ss findmnt awk lsmod modinfo; do
    if command -v "${cmd}" >/dev/null 2>&1; then
      pass "cmd_${cmd}" "$(command -v "${cmd}")"
    else
      fail "cmd_${cmd}" "'${cmd}' not found"
    fi
  done

  # docker is required for cube-proxy / infra containers
  if command -v docker >/dev/null 2>&1; then
    local docker_ver
    docker_ver="$(docker --version 2>/dev/null | head -1 || true)"
    pass "cmd_docker" "${docker_ver}"
  else
    fail "cmd_docker" "docker not found — required for cube-proxy and infrastructure containers"
  fi
}

# ── Main ───────────────────────────────────────────────────────────────────────
main() {
  echo "╔══════════════════════════════════════════════════════╗"
  echo "║    Cube Sandbox — Dependency Check                  ║"
  echo "╚══════════════════════════════════════════════════════╝"

  check_kernel
  check_virtualization
  check_cgroup
  check_filesystem
  check_commands

  echo
  echo "──────────────────────────────────────────────────────"
  printf "Summary: %d passed, %d warned, %d failed\n" \
    "${PASS_COUNT}" "${WARN_COUNT}" "${FAIL_COUNT}"
  echo "──────────────────────────────────────────────────────"

  if [[ "${JSON_OUT}" -eq 1 ]]; then emit_json; fi

  if [[ "${FAIL_COUNT}" -gt 0 ]]; then
    echo "RESULT: FAIL"
    exit 1
  fi
  echo "RESULT: PASS"
}

HOST_ENV="unknown"
main "$@"
