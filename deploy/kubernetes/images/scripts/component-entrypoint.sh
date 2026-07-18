#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Big Pod / Installer per-component entrypoint (REV3.1).
# Env:
#   CUBE_COMPONENT  cubelet|network-agent|cube-shim|cube-kernel|cube-guest
#   CUBE_ROLE       install|run
#     install — artifact-only components on cube-node-installer Pod: stage + pause
#     run     — cubelet / network-agent on Big Pod: self-stage then start the process
#   IMAGE_ROOT      default /opt/cube-image
#   TOOLBOX_ROOT    default /usr/local/services/cubetoolbox
set -euo pipefail

IMAGE_ROOT="${IMAGE_ROOT:-/opt/cube-image}"
TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
CUBE_COMPONENT="${CUBE_COMPONENT:-}"
CUBE_ROLE="${CUBE_ROLE:-install}"
CUBE_PID_DIR="${CUBE_PID_DIR:-/run/cube-node}"
STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"

log() { printf '[cube-component:%s:%s] %s\n' "${CUBE_COMPONENT:-?}" "${CUBE_ROLE}" "$*"; }
fail() { printf '[cube-component:%s:%s] ERROR: %s\n' "${CUBE_COMPONENT:-?}" "${CUBE_ROLE}" "$*" >&2; exit 1; }

apply_effective_pvm_from_state() {
  local path="${STATE_DIR}/effective-pvm"
  local val
  [[ -f "${path}" ]] || return 0
  val="$(tr -d '[:space:]' < "${path}" 2>/dev/null || true)"
  case "${val}" in
    0|1)
      CUBE_PVM_ENABLE="${val}"
      export CUBE_PVM_ENABLE
      log "CUBE_PVM_ENABLE overridden from ${path}=${val}"
      ;;
  esac
}

component_relpath() {
  case "$1" in
    cubelet) echo "Cubelet" ;;
    network-agent) echo "network-agent" ;;
    cube-shim) echo "cube-shim" ;;
    cube-kernel) echo "cube-kernel-scf" ;;
    cube-guest) echo "cube-image" ;;
    *) fail "unknown CUBE_COMPONENT=$1" ;;
  esac
}

component_sentinel() {
  case "$1" in
    cubelet) echo "${TOOLBOX_ROOT}/.staged-cubelet" ;;
    network-agent) echo "${TOOLBOX_ROOT}/.staged-network-agent" ;;
    cube-shim) echo "${TOOLBOX_ROOT}/.staged-cube-shim" ;;
    cube-kernel) echo "${TOOLBOX_ROOT}/.staged-cube-kernel" ;;
    cube-guest) echo "${TOOLBOX_ROOT}/.staged-cube-guest" ;;
    *) fail "unknown CUBE_COMPONENT=$1" ;;
  esac
}

wait_sentinel() {
  local path="$1"
  local name="$2"
  local i
  for i in $(seq 1 300); do
    if [[ -f "${path}" ]]; then
      log "sentinel ready: ${name} (${path})"
      return 0
    fi
    sleep 1
  done
  fail "timeout waiting for sentinel ${name} at ${path}"
}

# Atomic directory replace: any concurrent reader sees either the full old
# tree or the full new tree (no rm -rf gap). Requires src/dst on same FS.
atomic_replace_dir() {
  local src="$1"
  local dst="$2"
  local parent new
  parent="$(dirname "${dst}")"
  mkdir -p "${parent}"
  new="${dst}.new.$$"
  rm -rf "${new}"
  cp -a "${src}" "${new}"
  # Promote staged tree into place. Prefer mv -T (GNU); fall back to rm+mv.
  if mv -T "${new}" "${dst}" 2>/dev/null; then
    return 0
  fi
  rm -rf "${dst}"
  mv "${new}" "${dst}"
}

stage_component() {
  local rel="$1"
  local src="${IMAGE_ROOT}/${rel}"
  local dst="${TOOLBOX_ROOT}/${rel}"
  local sentinel
  sentinel="$(component_sentinel "${CUBE_COMPONENT}")"

  [[ -d "${src}" ]] || fail "image bypass missing: ${src}"
  mkdir -p "${TOOLBOX_ROOT}"
  # Clear previous sentinel so peers re-wait during refresh.
  rm -f "${sentinel}"
  log "staging ${src} -> ${dst} (atomic replace)"
  atomic_replace_dir "${src}" "${dst}"

  case "${CUBE_COMPONENT}" in
    cubelet)
      chmod +x "${dst}/bin/cubelet" "${dst}/bin/cubecli" 2>/dev/null || true
      [[ -x "${dst}/bin/cubelet" ]] || fail "missing cubelet after stage"
      ;;
    network-agent)
      chmod +x "${dst}/bin/network-agent" "${dst}/bin/cubevsmapdump" 2>/dev/null || true
      [[ -x "${dst}/bin/network-agent" ]] || fail "missing network-agent after stage"
      ;;
    cube-shim)
      chmod +x "${dst}/bin/cube-runtime" "${dst}/bin/containerd-shim-cube-rs" 2>/dev/null || true
      [[ -x "${dst}/bin/containerd-shim-cube-rs" ]] || fail "missing shim after stage"
      [[ -x "${dst}/bin/cube-runtime" ]] || fail "missing cube-runtime after stage"
      # containerd resolves io.containerd.cube.rs via PATH (same as one-click install.sh).
      mkdir -p /usr/local/bin
      ln -sf "${dst}/bin/containerd-shim-cube-rs" /usr/local/bin/containerd-shim-cube-rs
      ln -sf "${dst}/bin/cube-runtime" /usr/local/bin/cube-runtime
      ;;
    cube-kernel)
      # Prefer existing vmlinux symlink selection by cubelet run; ensure files exist.
      [[ -e "${dst}/vmlinux-bm" || -e "${dst}/vmlinux-pvm" || -e "${dst}/vmlinux" ]] \
        || fail "missing guest kernel files under ${dst}"
      ;;
    cube-guest)
      [[ -d "${dst}" ]] || fail "missing guest image dir ${dst}"
      ;;
  esac

  # Digest: informational change marker (completeness is write-order: cp then sentinel).
  {
    printf 'component=%s\n' "${CUBE_COMPONENT}"
    printf 'staged_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    find "${dst}" -type f -printf '%P %s %T@\n' 2>/dev/null | sort | head -n 50 || true
  } > "${sentinel}.tmp"
  mv -f "${sentinel}.tmp" "${sentinel}"
  log "wrote sentinel ${sentinel}"
}

run_install() {
  stage_component "$(component_relpath "${CUBE_COMPONENT}")"
  log "install complete; pausing"
  exec sleep infinity
}

sed_escape_replacement() {
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g' -e 's/[/]/\\\//g'
}

detect_primary_interface() {
  ip route get 1.1.1.1 2>/dev/null | awk '
    {
      for (i = 1; i <= NF; i++) {
        if ($i == "dev" && (i + 1) <= NF) {
          print $(i + 1)
          exit
        }
      }
    }'
}

select_guest_kernel() {
  local dir="${TOOLBOX_ROOT}/cube-kernel-scf"
  local target="vmlinux-bm"
  case "${CUBE_PVM_ENABLE:-1}" in
    1|true|TRUE|yes|YES) target="vmlinux-pvm" ;;
    0|false|FALSE|no|NO) target="vmlinux-bm" ;;
    *) fail "unsupported CUBE_PVM_ENABLE=${CUBE_PVM_ENABLE}" ;;
  esac
  [[ -f "${dir}/${target}" ]] || fail "missing guest kernel: ${dir}/${target}"
  ln -sfn "${target}" "${dir}/vmlinux"
  log "selected guest kernel: ${dir}/vmlinux -> ${target}"
}

patch_common_yaml_list() {
  local key="$1"
  local raw_values="$2"
  local conf="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
  [[ -f "${conf}" ]] || return 0
  [[ -n "${raw_values//[[:space:],;]/}" ]] || return 0
  local tmp_file
  tmp_file="$(mktemp)"
  awk -v key="${key}" -v raw_values="${raw_values}" '
    BEGIN {
      gsub(/[,;]/, " ", raw_values)
      count = split(raw_values, raw, /[[:space:]]+/)
      for (i = 1; i <= count; i++) {
        if (raw[i] != "") values[++value_count] = raw[i]
      }
    }
    function emit(indent,    i) {
      print indent key ":"
      for (i = 1; i <= value_count; i++) print indent "  - " values[i]
    }
    {
      if ($0 ~ ("^[[:space:]]*" key ":")) {
        match($0, /^[[:space:]]*/)
        emit(substr($0, 1, RLENGTH))
        in_block = 1
        next
      }
      if (in_block) {
        if ($0 ~ /^[[:space:]]+- /) next
        in_block = 0
      }
      print
    }
  ' "${conf}" > "${tmp_file}"
  mv -f "${tmp_file}" "${conf}"
}

configure_sandbox_dns() {
  if [[ "${CUBE_SANDBOX_DNS_FOLLOW_NODE:-false}" == "true" && -z "${CUBE_SANDBOX_DNS_SERVERS:-}" ]]; then
    CUBE_SANDBOX_DNS_SERVERS="$(
      awk '
        $1 == "nameserver" {
          ip = $2
          if (ip ~ /^127\./) next
          if (ip ~ /^169\.254\./) next
          if (ip == "::1") next
          if (seen[ip]++) next
          if (n++) printf ","
          printf "%s", ip
        }
      ' /etc/resolv.conf
    )"
    log "sandbox DNS follow-node nameservers: ${CUBE_SANDBOX_DNS_SERVERS:-<empty>}"
  fi
  patch_common_yaml_list default_dns_servers "${CUBE_SANDBOX_DNS_SERVERS:-}"
}

write_pidfile() {
  local name="$1"
  local pid="$2"
  mkdir -p "${CUBE_PID_DIR}"
  printf '%s\n' "${pid}" > "${CUBE_PID_DIR}/${name}.pid"
}

kill_pidfile() {
  local name="$1"
  local file="${CUBE_PID_DIR}/${name}.pid"
  local pid
  [[ -f "${file}" ]] || return 0
  pid="$(cat "${file}" 2>/dev/null || true)"
  [[ -n "${pid}" ]] || return 0
  if kill -0 "${pid}" 2>/dev/null; then
    log "stopping ${name} pid=${pid}"
    kill -TERM "${pid}" 2>/dev/null || true
  fi
  rm -f "${file}"
}

run_network_agent() {
  local bin="${TOOLBOX_ROOT}/network-agent/bin/network-agent"
  local cfg="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
  local state_dir="${NETWORK_AGENT_STATE_DIR:-/data/cubelet/network-agent/state}"
  local health="${NETWORK_AGENT_HEALTH_URL:-http://127.0.0.1:19090/readyz}"
  local pid

  # Self-stage (no separate network-agent-install container).
  stage_component "$(component_relpath network-agent)"
  # Config lives under Cubelet tree — wait until cubelet run has staged it.
  wait_sentinel "$(component_sentinel cubelet)" "cubelet-config"
  [[ -x "${bin}" ]] || fail "missing ${bin}"
  [[ -f "${cfg}" ]] || fail "missing ${cfg}"

  mkdir -p "${state_dir}" "${TOOLBOX_ROOT}/cube-vs/network" /tmp/cube /data/log
  rm -f /tmp/cube/network-agent.sock /tmp/cube/network-agent-grpc.sock /tmp/cube/network-agent-tap.sock || true

  kill_pidfile network-agent

  if [[ -z "${CUBE_SANDBOX_ETH_NAME:-}" && "${CUBE_SANDBOX_AUTO_DETECT_ETH:-true}" == "true" ]]; then
    CUBE_SANDBOX_ETH_NAME="$(detect_primary_interface || true)"
    [[ -n "${CUBE_SANDBOX_ETH_NAME}" ]] && log "auto detected primary interface: ${CUBE_SANDBOX_ETH_NAME}"
  fi
  if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
    local eth_esc
    eth_esc="$(sed_escape_replacement "${CUBE_SANDBOX_ETH_NAME}")"
    sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${eth_esc}\"/" "${cfg}"
  fi
  if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
    local cidr_esc
    cidr_esc="$(sed_escape_replacement "${CUBE_SANDBOX_NETWORK_CIDR}")"
    sed -i "s|cidr = \"[^\"]*\"|cidr = \"${cidr_esc}\"|" "${cfg}"
  fi

  log "starting network-agent"
  "${bin}" --cubelet-config "${cfg}" --state-dir "${state_dir}" &
  pid=$!
  write_pidfile network-agent "${pid}"

  cleanup() { kill_pidfile network-agent; }
  trap cleanup TERM INT HUP EXIT

  local i
  for i in $(seq 1 120); do
    if curl -fsS "${health}" >/dev/null 2>&1; then
      log "network-agent ready"
      break
    fi
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      fail "network-agent exited before ready"
    fi
    [[ "${i}" -lt 120 ]] || fail "network-agent did not become ready"
    sleep 1
  done

  while kill -0 "${pid}" >/dev/null 2>&1; do
    sleep 10
  done
  fail "network-agent exited"
}

run_cubelet() {
  local bin="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
  local cfg="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
  local dyn="${CUBELET_DYNAMICCONF:-${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml}"
  local pid launch

  # Self-stage (no separate cubelet-install container).
  stage_component "$(component_relpath cubelet)"
  wait_sentinel "$(component_sentinel cube-shim)" "cube-shim"
  wait_sentinel "$(component_sentinel cube-kernel)" "cube-kernel"
  wait_sentinel "$(component_sentinel cube-guest)" "cube-guest"
  wait_sentinel "$(component_sentinel network-agent)" "network-agent"

  [[ -x "${bin}" ]] || fail "missing ${bin}"
  [[ -f "${cfg}" ]] || fail "missing ${cfg}"
  [[ -f "${dyn}" ]] || fail "missing ${dyn}"
  [[ -n "${CUBE_MASTER_ENDPOINT:-}" ]] || fail "CUBE_MASTER_ENDPOINT is required"
  [[ -n "${CUBE_SANDBOX_NODE_ID:-}${CUBE_SANDBOX_NODE_IP:-}" ]] || fail "CUBE_SANDBOX_NODE_ID or CUBE_SANDBOX_NODE_IP is required"
  [[ -n "${CUBE_SANDBOX_ENDPOINT_IP:-}" ]] || fail "CUBE_SANDBOX_ENDPOINT_IP is required"

  apply_effective_pvm_from_state
  select_guest_kernel

  local ep_esc
  ep_esc="$(sed_escape_replacement "${CUBE_MASTER_ENDPOINT}")"
  sed -i -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${ep_esc}\"#" "${dyn}"
  configure_sandbox_dns

  if [[ -z "${CUBE_SANDBOX_ETH_NAME:-}" && "${CUBE_SANDBOX_AUTO_DETECT_ETH:-true}" == "true" ]]; then
    CUBE_SANDBOX_ETH_NAME="$(detect_primary_interface || true)"
  fi
  if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
    local eth_esc
    eth_esc="$(sed_escape_replacement "${CUBE_SANDBOX_ETH_NAME}")"
    sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${eth_esc}\"/" "${cfg}"
  fi
  if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
    local cidr_esc
    cidr_esc="$(sed_escape_replacement "${CUBE_SANDBOX_NETWORK_CIDR}")"
    sed -i "s|cidr = \"[^\"]*\"|cidr = \"${cidr_esc}\"|" "${cfg}"
  fi
  if [[ -n "${CUBE_TAP_INIT_NUM:-}" ]]; then
    [[ "${CUBE_TAP_INIT_NUM}" =~ ^[0-9]+$ ]] || fail "CUBE_TAP_INIT_NUM must be a non-negative integer"
    sed -i "s/tap_init_num = [0-9]\+/tap_init_num = ${CUBE_TAP_INIT_NUM}/" "${cfg}"
  fi
  if [[ -n "${CUBE_CGROUP_POOL_SIZE:-}" ]]; then
    [[ "${CUBE_CGROUP_POOL_SIZE}" =~ ^[0-9]+$ ]] || fail "CUBE_CGROUP_POOL_SIZE must be a non-negative integer"
    sed -i "s/pool_size = [0-9]\+/pool_size = ${CUBE_CGROUP_POOL_SIZE}/" "${cfg}"
  fi
  if [[ -n "${CUBE_WORKFLOW_CONCURRENT:-}" ]]; then
    [[ "${CUBE_WORKFLOW_CONCURRENT}" =~ ^[0-9]+$ ]] || fail "CUBE_WORKFLOW_CONCURRENT must be a non-negative integer"
    sed -i "s/concurrent = [0-9]\+/concurrent = ${CUBE_WORKFLOW_CONCURRENT}/g" "${cfg}"
  fi

  mkdir -p \
    /tmp/cube \
    /data/log/Cubelet \
    /data/log/CubeShim \
    /data/log/CubeVmm \
    /data/cube-shim/disks \
    /data/snapshot_pack/disks \
    /data/cubelet/state \
    "${TOOLBOX_ROOT}/cube-snapshot" \
    "${TOOLBOX_ROOT}/cube-vs/network"

  if ! findmnt --mountpoint /data/cubelet/state >/dev/null 2>&1; then
    mount --bind /data/cubelet/state /data/cubelet/state
    log "bound /data/cubelet/state to hostPath (skip state tmpfs)"
  fi

  # Wait for network-agent health before cubelet init (NA creates cube-dev).
  local i
  for i in $(seq 1 120); do
    if curl -fsS "${NETWORK_AGENT_HEALTH_URL:-http://127.0.0.1:19090/readyz}" >/dev/null 2>&1; then
      break
    fi
    [[ "${i}" -lt 120 ]] || fail "network-agent not healthy before cubelet start"
    sleep 1
  done

  kill_pidfile cubelet

  log "starting cubelet node_id=${CUBE_SANDBOX_NODE_ID:-} endpoint=${CUBE_SANDBOX_ENDPOINT_IP}"
  "${bin}" --config "${cfg}" --dynamic-conf-path "${dyn}" &
  launch=$!

  for i in $(seq 1 60); do
    pid="$(pidof cubelet 2>/dev/null | awk '{print $1}' || true)"
    if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1 && ss -lntp 2>/dev/null | grep -q ':9999'; then
      write_pidfile cubelet "${pid}"
      log "cubelet ready pid=${pid}"
      break
    fi
    if ! kill -0 "${launch}" >/dev/null 2>&1 && [[ -z "${pid}" ]]; then
      fail "cubelet exited before listening on 9999"
    fi
    [[ "${i}" -lt 60 ]] || fail "cubelet did not become ready"
    sleep 1
  done

  cleanup() { kill_pidfile cubelet; }
  trap cleanup TERM INT HUP EXIT

  while kill -0 "$(cat "${CUBE_PID_DIR}/cubelet.pid" 2>/dev/null || echo 0)" >/dev/null 2>&1; do
    sleep 10
  done
  fail "cubelet exited"
}

main() {
  [[ -n "${CUBE_COMPONENT}" ]] || fail "CUBE_COMPONENT is required"
  case "${CUBE_ROLE}" in
    install) run_install ;;
    run)
      case "${CUBE_COMPONENT}" in
        network-agent) run_network_agent ;;
        cubelet) run_cubelet ;;
        *) fail "CUBE_ROLE=run not supported for ${CUBE_COMPONENT}" ;;
      esac
      ;;
    *) fail "unknown CUBE_ROLE=${CUBE_ROLE}" ;;
  esac
}

main "$@"
