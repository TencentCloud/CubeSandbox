#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# check-procs.sh — Cube Sandbox process readiness checker
# Standalone script; no external dependencies.
#
# Checks whether all Cube Sandbox daemons are up and responding:
#   • network-agent  (127.0.0.1:19090, /healthz, /readyz, UNIX sockets)
#   • cubelet        (ports 9966/9998/9999, /data/cubelet/cubelet.sock, assets)
#   • cubemaster     (port 8089, /notify/health)
#   • cube-api       (port 3000, /health)
#   • containerd-shim-cube-rs
#
# Role-aware: set ONE_CLICK_DEPLOY_ROLE=compute to check a compute node
# (cube-api/cubemaster expected on control plane, not locally).
#
# Usage:
#   ./check-procs.sh              # check all
#   ./check-procs.sh --quiet      # suppress OK lines
#   ./check-procs.sh --json       # machine-readable JSON to stdout
#
# Exit: 0 = all pass, 1 = one or more failures

set -uo pipefail

# ── Help ──────────────────────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
Usage: check-procs.sh [OPTIONS]

Check whether all Cube Sandbox daemon processes are running and healthy.

Processes / components checked:
  network-agent        Port 19090, /healthz, /readyz, UNIX sockets
  cubelet              Ports 9966/9998/9999, /data/cubelet/cubelet.sock,
                       config files and runtime asset files
  cubemaster           Port 8089, /notify/health HTTP endpoint
  cube-api             Port 3000, /health HTTP endpoint, binary and config
  cubeshim             containerd-shim-cube-rs binary and running instance count
  cube-proxy           Docker containers for reverse proxy and routing:
                         cube-proxy        nginx/openresty, ports 80/443
                         cube-proxy-coredns internal DNS (no host port)
                         cube-webui        management UI, port 12088
  infrastructure       Docker containers for storage / metadata:
                         cube-sandbox-redis  session state, port 6379
                         cube-sandbox-mysql  metadata DB, port 3306
  cube-kernel          vmlinux / vmlinux-pvm kernel image files;
                       PVM variant checked against host environment

Options:
  --quiet      Suppress OK lines; only print WARN and FAIL entries
  --json       Print a machine-readable JSON summary to stdout after the run
  --help       Show this help message and exit

Environment variables:
  ONE_CLICK_DEPLOY_ROLE              control (default) or compute
  ONE_CLICK_TOOLBOX_ROOT             Installation root (default: /usr/local/services/cubetoolbox)
  ONE_CLICK_RUNTIME_DIR              PID file directory (default: /var/run/cube-sandbox-one-click)
  NETWORK_AGENT_HEALTH_ADDR          network-agent health address (default: 127.0.0.1:19090)
  CUBE_API_HEALTH_ADDR               cube-api health address (default: 127.0.0.1:3000)
  CUBEMASTER_ADDR                    cubemaster address (default: 127.0.0.1:8089)
  ONE_CLICK_CONTROL_PLANE_IP         Control plane IP for compute role
  ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR  Full cubemaster address for compute role

  When ONE_CLICK_DEPLOY_ROLE=compute, cubemaster and cube-api checks use the
  control-plane address instead of localhost.

Exit codes:
  0   All checks passed (warnings are allowed)
  1   One or more checks failed

Examples:
  ./check-procs.sh
  ./check-procs.sh --quiet
  ./check-procs.sh --json
  ONE_CLICK_DEPLOY_ROLE=compute ONE_CLICK_CONTROL_PLANE_IP=10.0.0.1 ./check-procs.sh
EOF
}

# ── Config (override via env) ──────────────────────────────────────────────────
TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
RUNTIME_DIR="${ONE_CLICK_RUNTIME_DIR:-/var/run/cube-sandbox-one-click}"
NA_HEALTH_ADDR="${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}"
CUBE_API_HEALTH_ADDR="${CUBE_API_HEALTH_ADDR:-127.0.0.1:3000}"
CUBEMASTER_ADDR="${CUBEMASTER_ADDR:-127.0.0.1:8089}"

# Load .one-click.env if present (for ROLE / CONTROL_PLANE_IP, etc.)
_env_file="${TOOLBOX_ROOT}/.one-click.env"
if [[ -f "${_env_file}" ]]; then
  set +u; set -a
  # shellcheck disable=SC1090
  source "${_env_file}"
  set +a; set -u
fi

# Role: control (default) or compute
ROLE="${ONE_CLICK_DEPLOY_ROLE:-control}"
case "${ROLE}" in control|compute) ;; *) ROLE=control ;; esac

# For compute nodes, resolve the control-plane cubemaster address
_resolve_master_addr() {
  if [[ "${ROLE}" != "compute" ]]; then
    printf '%s\n' "${CUBEMASTER_ADDR}"
    return
  fi
  local addr="${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR:-}"
  local ip="${ONE_CLICK_CONTROL_PLANE_IP:-}"
  local port="${CUBEMASTER_ADDR##*:}"
  if [[ -n "${addr}" ]]; then printf '%s\n' "${addr}"; return; fi
  if [[ -n "${ip}" ]];   then printf '%s:%s\n' "${ip}" "${port}"; return; fi
  printf '%s\n' "${CUBEMASTER_ADDR}"
}
MASTER_ADDR="$(_resolve_master_addr)"

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
warn() { _record WARN "$1" "$2"; printf '  [WARN]  %s: %s\n' "$1" "$2"; }
fail() { _record FAIL "$1" "$2"; printf '  [FAIL]  %s: %s\n' "$1" "$2"; }
section() { [[ "${QUIET}" -eq 1 ]] || echo; echo "── $* ──"; }

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
_pidfile_alive() {
  local pid_file="${RUNTIME_DIR}/$1.pid"
  [[ -f "${pid_file}" ]] || return 1
  local pid; pid="$(<"${pid_file}")"
  [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null
}

_pidfile_pid() {
  cat "${RUNTIME_DIR}/$1.pid" 2>/dev/null || true
}

_proc_running() {
  pgrep -f -- "$1" >/dev/null 2>&1
}

_http_ok() {
  curl -fsS --max-time 5 "$1" >/dev/null 2>&1
}

_port_listening() {
  ss -tlnp "sport = :$1" 2>/dev/null | grep -q ":$1"
}

# _check_container <name> <expected_state> <expected_health|empty> <host_ports|empty>
# Checks a Docker container: state, optional health, and optional host-port listeners.
_check_container() {
  local name="$1" exp_state="$2" exp_health="$3" ports="$4"
  local key; key="${name//-/_}"

  local state
  state="$(docker inspect --format '{{.State.Status}}' "${name}" 2>/dev/null || true)"
  if [[ -z "${state}" ]]; then
    fail "${key}_container" "container '${name}' not found"
    return
  fi

  if [[ "${state}" == "${exp_state}" ]]; then
    pass "${key}_container" "'${name}' is ${state}"
  else
    fail "${key}_container" "'${name}' state=${state} (expected ${exp_state})"
  fi

  # Health check (only when a HEALTHCHECK is configured and caller expects it)
  if [[ -n "${exp_health}" ]]; then
    local health
    health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \
      "${name}" 2>/dev/null || true)"
    if [[ "${health}" == "${exp_health}" ]]; then
      pass "${key}_health" "'${name}' health=${health}"
    elif [[ "${health}" == "none" ]]; then
      warn "${key}_health" "'${name}' has no HEALTHCHECK configured"
    else
      warn "${key}_health" "'${name}' health=${health} (expected ${exp_health})"
    fi
  fi

  # Host port listeners
  for port in ${ports}; do
    if _port_listening "${port}"; then
      pass "${key}_port_${port}" "0.0.0.0:${port} listening"
    else
      fail "${key}_port_${port}" "0.0.0.0:${port} not listening"
    fi
  done
}

# ── Check functions ────────────────────────────────────────────────────────────
check_network_agent() {
  section "network-agent"

  if _pidfile_alive "network-agent"; then
    pass "network_agent_pid" "pid=$(_pidfile_pid network-agent)"
  elif _proc_running "${TOOLBOX_ROOT}/network-agent/bin/network-agent"; then
    pass "network_agent_pid" "running (no pidfile)"
  else
    fail "network_agent_pid" "network-agent process not running"
  fi

  if _port_listening 19090; then
    pass "network_agent_port" "127.0.0.1:19090 listening"
  else
    fail "network_agent_port" "127.0.0.1:19090 not listening"
  fi

  if _http_ok "http://${NA_HEALTH_ADDR}/healthz"; then
    pass "network_agent_healthz" "/healthz → 200"
  else
    fail "network_agent_healthz" "http://${NA_HEALTH_ADDR}/healthz failed"
  fi

  if _http_ok "http://${NA_HEALTH_ADDR}/readyz"; then
    pass "network_agent_readyz" "/readyz → 200"
  else
    fail "network_agent_readyz" "http://${NA_HEALTH_ADDR}/readyz failed (still initialising?)"
  fi

  for sock in network-agent-grpc.sock network-agent.sock; do
    if [[ -S "/tmp/cube/${sock}" ]]; then
      pass "sock_${sock%.sock}" "/tmp/cube/${sock} present"
    else
      warn "sock_${sock%.sock}" "/tmp/cube/${sock} missing"
    fi
  done
}

check_cubelet() {
  section "Cubelet"

  if _pidfile_alive "cubelet"; then
    pass "cubelet_pid" "pid=$(_pidfile_pid cubelet)"
  elif _proc_running "${TOOLBOX_ROOT}/Cubelet/bin/cubelet"; then
    pass "cubelet_pid" "running (no pidfile)"
  else
    fail "cubelet_pid" "cubelet not running"
  fi

  for port in 9966 9998 9999; do
    if _port_listening "${port}"; then
      pass "cubelet_port_${port}" "0.0.0.0:${port} listening"
    else
      fail "cubelet_port_${port}" "0.0.0.0:${port} not listening"
    fi
  done

  if [[ -S /data/cubelet/cubelet.sock ]]; then
    pass "cubelet_sock" "/data/cubelet/cubelet.sock present"
  else
    fail "cubelet_sock" "/data/cubelet/cubelet.sock missing"
  fi

  for f in \
    "${TOOLBOX_ROOT}/Cubelet/config/config.toml" \
    "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml" \
    "${TOOLBOX_ROOT}/cube-shim/conf/config-cube.toml" \
    "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux" \
    "${TOOLBOX_ROOT}/cube-image/cube-guest-image-cpu.img"; do
    local label; label="$(basename "${f}")"
    if [[ -f "${f}" ]]; then
      pass "asset_${label}" "${f}"
    else
      warn "asset_${label}" "${f} not found"
    fi
  done
}

check_cubemaster() {
  section "CubeMaster"

  if [[ "${ROLE}" == "control" ]]; then
    if _pidfile_alive "cubemaster"; then
      pass "cubemaster_pid" "pid=$(_pidfile_pid cubemaster)"
    elif _proc_running "${TOOLBOX_ROOT}/CubeMaster/bin/cubemaster"; then
      pass "cubemaster_pid" "running (no pidfile)"
    else
      fail "cubemaster_pid" "cubemaster not running"
    fi

    if _port_listening 8089; then
      pass "cubemaster_port" "0.0.0.0:8089 listening"
    else
      fail "cubemaster_port" "0.0.0.0:8089 not listening"
    fi
  else
    pass "cubemaster_pid" "compute node — cubemaster on control plane (skipped)"
  fi

  if _http_ok "http://${MASTER_ADDR}/notify/health"; then
    pass "cubemaster_health" "http://${MASTER_ADDR}/notify/health → 200"
  else
    fail "cubemaster_health" "http://${MASTER_ADDR}/notify/health failed"
  fi
}

check_cube_api() {
  section "cube-api"

  if [[ "${ROLE}" == "compute" ]]; then
    pass "cube_api_skip" "compute node — cube-api on control plane (skipped)"
    return
  fi

  if _pidfile_alive "cube-api"; then
    pass "cube_api_pid" "pid=$(_pidfile_pid cube-api)"
  elif _proc_running "${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"; then
    pass "cube_api_pid" "running (no pidfile)"
  else
    fail "cube_api_pid" "cube-api not running"
  fi

  if _port_listening 3000; then
    pass "cube_api_port" "0.0.0.0:3000 listening"
  else
    fail "cube_api_port" "0.0.0.0:3000 not listening"
  fi

  if _http_ok "http://${CUBE_API_HEALTH_ADDR}/health"; then
    pass "cube_api_health" "http://${CUBE_API_HEALTH_ADDR}/health → 200"
  else
    fail "cube_api_health" "http://${CUBE_API_HEALTH_ADDR}/health failed"
  fi

  for f in \
    "${TOOLBOX_ROOT}/CubeAPI/bin/cube-api" \
    "${TOOLBOX_ROOT}/CubeMaster/conf.yaml"; do
    local label; label="$(basename "${f}")"
    if [[ -e "${f}" ]]; then
      pass "file_${label}" "${f}"
    else
      warn "file_${label}" "${f} not found"
    fi
  done
}

check_shim() {
  section "containerd-shim-cube-rs (CubeShim)"

  local shim="${TOOLBOX_ROOT}/cube-shim/bin/containerd-shim-cube-rs"
  if [[ -x "${shim}" ]]; then
    pass "cubeshim_binary" "${shim}"
  else
    warn "cubeshim_binary" "${shim} not found (expected after first sandbox launch)"
  fi

  local cnt; cnt="$(pgrep -c -f 'containerd-shim-cube-rs' 2>/dev/null || echo 0)"
  pass "cubeshim_instances" "${cnt} shim instance(s) running"
}

check_cube_proxy() {
  section "cube-proxy (Docker containers)"

  if ! command -v docker >/dev/null 2>&1; then
    warn "cube_proxy_docker" "docker not found — cannot check cube-proxy containers"
    return
  fi

  # ── cube-proxy ── nginx/openresty reverse proxy, ports 80/443
  _check_container "cube-proxy" "running" "" "80 443"

  # ── cube-proxy-coredns ── internal DNS for sandbox routing (no host port)
  _check_container "cube-proxy-coredns" "running" "" ""

  # ── cube-webui ── management Web UI, port 12088
  _check_container "cube-webui" "running" "healthy" "12088"
}

check_cube_infra() {
  section "Infrastructure (Docker containers)"

  if ! command -v docker >/dev/null 2>&1; then
    warn "cube_infra_docker" "docker not found — cannot check infrastructure containers"
    return
  fi

  # ── cube-sandbox-redis ── session/routing state store, port 6379
  _check_container "cube-sandbox-redis" "running" "healthy" "6379"

  # ── cube-sandbox-mysql ── persistent metadata store, port 3306
  _check_container "cube-sandbox-mysql" "running" "healthy" "3306"
}

check_cube_kernel() {
  section "cube-kernel"

  local kernel_dir="${TOOLBOX_ROOT}/cube-kernel-scf"

  if [[ ! -d "${kernel_dir}" ]]; then
    fail "cube_kernel_dir" "${kernel_dir} directory not found"
    return
  fi

  # Determine which kernel variant is expected based on host environment.
  # PVM hosts boot guests with vmlinux-pvm; KVM/bare-metal use vmlinux.
  local pvm_host=0
  lsmod 2>/dev/null | grep -qE '^kvm_pvm[[:space:]]' && pvm_host=1

  # Always check the generic vmlinux (required for KVM/nested)
  if [[ -f "${kernel_dir}/vmlinux" ]]; then
    local sz; sz="$(du -sh "${kernel_dir}/vmlinux" 2>/dev/null | cut -f1)"
    pass "cube_kernel_vmlinux" "${kernel_dir}/vmlinux (${sz})"
  else
    fail "cube_kernel_vmlinux" "${kernel_dir}/vmlinux not found"
  fi

  # Check PVM variant
  if [[ -f "${kernel_dir}/vmlinux-pvm" ]]; then
    local sz; sz="$(du -sh "${kernel_dir}/vmlinux-pvm" 2>/dev/null | cut -f1)"
    if [[ "${pvm_host}" -eq 1 ]]; then
      pass "cube_kernel_vmlinux_pvm" "${kernel_dir}/vmlinux-pvm (${sz}) — PVM host, this variant will be used"
    else
      pass "cube_kernel_vmlinux_pvm" "${kernel_dir}/vmlinux-pvm (${sz})"
    fi
  else
    if [[ "${pvm_host}" -eq 1 ]]; then
      fail "cube_kernel_vmlinux_pvm" \
        "${kernel_dir}/vmlinux-pvm not found — required on PVM host for guest VM boot"
    else
      warn "cube_kernel_vmlinux_pvm" "${kernel_dir}/vmlinux-pvm not found (only needed on PVM hosts)"
    fi
  fi
}

# ── Main ───────────────────────────────────────────────────────────────────────
main() {
  echo "╔══════════════════════════════════════════════════════╗"
  echo "║    Cube Sandbox — Process Readiness Check           ║"
  echo "╚══════════════════════════════════════════════════════╝"
  echo "  Role   : ${ROLE}"
  echo "  Toolbox: ${TOOLBOX_ROOT}"

  check_network_agent
  check_cubelet
  check_cubemaster
  check_cube_api
  check_shim
  check_cube_proxy
  check_cube_infra
  check_cube_kernel

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

main "$@"
