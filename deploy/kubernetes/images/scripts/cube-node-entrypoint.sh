#!/usr/bin/env bash
set -euo pipefail

TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
NETWORK_AGENT_BIN="${TOOLBOX_ROOT}/network-agent/bin/network-agent"
CUBELET_BIN="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
CUBELET_CONFIG="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
CUBELET_DYNAMICCONF="${CUBELET_DYNAMICCONF:-${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml}"
CUBE_KERNEL_DIR="${TOOLBOX_ROOT}/cube-kernel-scf"
NETWORK_AGENT_STATE_DIR="${NETWORK_AGENT_STATE_DIR:-/data/cubelet/network-agent/state}"
NETWORK_AGENT_HEALTH_URL="${NETWORK_AGENT_HEALTH_URL:-http://127.0.0.1:19090/readyz}"
CUBE_MASTER_ENDPOINT="${CUBE_MASTER_ENDPOINT:-cube-master.cube-system.svc.cluster.local:8089}"
CUBE_PVM_ENABLE="${CUBE_PVM_ENABLE:-1}"
CUBE_SANDBOX_AUTO_DETECT_ETH="${CUBE_SANDBOX_AUTO_DETECT_ETH:-true}"

log() { printf '[cube-node-entrypoint] %s\n' "$*"; }
fail() { printf '[cube-node-entrypoint] ERROR: %s\n' "$*" >&2; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing command in cube-node image: $1"
}

select_guest_kernel() {
  local target="vmlinux-bm"
  case "${CUBE_PVM_ENABLE}" in
    1|true|TRUE|yes|YES) target="vmlinux-pvm" ;;
    0|false|FALSE|no|NO) target="vmlinux-bm" ;;
    *) fail "unsupported CUBE_PVM_ENABLE=${CUBE_PVM_ENABLE}; expected true/false" ;;
  esac
  [[ -f "${CUBE_KERNEL_DIR}/${target}" ]] || fail "missing guest kernel: ${CUBE_KERNEL_DIR}/${target}"
  ln -sfn "${target}" "${CUBE_KERNEL_DIR}/vmlinux"
  log "selected guest kernel: ${CUBE_KERNEL_DIR}/vmlinux -> ${target}"
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

validate_runtime_commands() {
  for cmd in mkfs.ext4 mount umount losetup cube-runtime containerd-shim-cube-rs cubecli cubevsmapdump; do
    require_cmd "${cmd}"
  done
}

patch_common_yaml_list() {
  local key="$1"
  local raw_values="$2"
  [[ -n "${raw_values//[[:space:],;]/}" ]] || return 0

  local tmp_file
  tmp_file="$(mktemp)"
  awk -v key="${key}" -v raw_values="${raw_values}" '
    BEGIN {
      gsub(/[,;]/, " ", raw_values)
      count = split(raw_values, raw, /[[:space:]]+/)
      for (i = 1; i <= count; i++) {
        if (raw[i] != "") {
          values[++value_count] = raw[i]
        }
      }
    }
    function emit(indent,    i, item) {
      print indent key ":"
      for (i = 1; i <= value_count; i++) {
        item = values[i]
        gsub(/"/, "\\\"", item)
        print indent "  - \"" item "\""
      }
      emitted = 1
    }
    /^common:[[:space:]]*$/ {
      in_common = 1
      print
      next
    }
    in_common && /^[^[:space:]][^:]*:/ {
      if (!emitted) {
        emit("  ")
      }
      in_common = 0
    }
    in_common && $0 ~ "^[[:space:]]*" key ":[[:space:]]*.*$" {
      indent = substr($0, 1, match($0, /[^[:space:]]/) - 1)
      emit(indent)
      skipping = 1
      next
    }
    skipping {
      if ($0 ~ /^[[:space:]]*-[[:space:]]/) {
        next
      }
      skipping = 0
    }
    {
      print
    }
    END {
      if (in_common && !emitted) {
        emit("  ")
      }
    }
  ' "${CUBELET_DYNAMICCONF}" > "${tmp_file}"
  cat "${tmp_file}" > "${CUBELET_DYNAMICCONF}"
  rm -f "${tmp_file}"
  log "patched ${key} in ${CUBELET_DYNAMICCONF}"
}

configure_sandbox_dns() {
  patch_common_yaml_list default_dns_servers "${CUBE_SANDBOX_DNS_SERVERS:-}"
}

[[ -x "${NETWORK_AGENT_BIN}" ]] || fail "missing executable: ${NETWORK_AGENT_BIN}"
[[ -x "${CUBELET_BIN}" ]] || fail "missing executable: ${CUBELET_BIN}"
[[ -f "${CUBELET_CONFIG}" ]] || fail "missing config: ${CUBELET_CONFIG}"
[[ -f "${CUBELET_DYNAMICCONF}" ]] || fail "missing dynamic config: ${CUBELET_DYNAMICCONF}"
[[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]] || fail "CUBE_SANDBOX_NODE_IP is required"

validate_runtime_commands
select_guest_kernel

sed -i -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${CUBE_MASTER_ENDPOINT}\"#" "${CUBELET_DYNAMICCONF}"
configure_sandbox_dns

if [[ -z "${CUBE_SANDBOX_ETH_NAME:-}" && "${CUBE_SANDBOX_AUTO_DETECT_ETH}" == "true" ]]; then
  CUBE_SANDBOX_ETH_NAME="$(detect_primary_interface || true)"
  if [[ -n "${CUBE_SANDBOX_ETH_NAME}" ]]; then
    log "auto detected primary interface: ${CUBE_SANDBOX_ETH_NAME}"
  else
    log "primary interface auto detection failed; keeping packaged Cubelet eth_name"
  fi
fi
if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
  sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${CUBE_SANDBOX_ETH_NAME}\"/" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
  sed -i "s|cidr = \"[^\"]*\"|cidr = \"${CUBE_SANDBOX_NETWORK_CIDR}\"|" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_TAP_INIT_NUM:-}" ]]; then
  sed -i "s/tap_init_num = [0-9]\+/tap_init_num = ${CUBE_TAP_INIT_NUM}/" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_CGROUP_POOL_SIZE:-}" ]]; then
  sed -i "s/pool_size = [0-9]\+/pool_size = ${CUBE_CGROUP_POOL_SIZE}/" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_WORKFLOW_CONCURRENT:-}" ]]; then
  sed -i "s/concurrent = [0-9]\+/concurrent = ${CUBE_WORKFLOW_CONCURRENT}/g" "${CUBELET_CONFIG}"
fi

mkdir -p \
  "${NETWORK_AGENT_STATE_DIR}" \
  "${TOOLBOX_ROOT}/cube-vs/network" \
  "${TOOLBOX_ROOT}/cube-snapshot" \
  /tmp/cube \
  /data/log/Cubelet \
  /data/log/CubeShim \
  /data/log/CubeVmm \
  /data/cube-shim/disks \
  /data/snapshot_pack/disks

rm -f \
  /tmp/cube/network-agent.sock \
  /tmp/cube/network-agent-grpc.sock \
  /tmp/cube/network-agent-tap.sock \
  || true

stop_stale_processes() {
  local name="$1"
  local pattern="$2"
  local pids

  pids="$(pgrep -f "${pattern}" || true)"
  [[ -n "${pids}" ]] || return 0

  log "stopping stale ${name} process(es): ${pids//$'\n'/ }"
  kill ${pids} 2>/dev/null || true
  for _ in $(seq 1 10); do
    if ! pgrep -f "${pattern}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  pgrep -f "${pattern}" | xargs -r kill -9 2>/dev/null || true
}

stop_stale_processes network-agent "${NETWORK_AGENT_BIN}"
stop_stale_processes cubelet "${CUBELET_BIN}"

cleanup() {
  if [[ -n "${NETWORK_AGENT_PID:-}" ]]; then
    kill "${NETWORK_AGENT_PID}" 2>/dev/null || true
  fi
  if [[ -n "${CUBELET_PID:-}" ]]; then
    kill "${CUBELET_PID}" 2>/dev/null || true
  fi
}
trap cleanup TERM INT HUP EXIT

log "starting network-agent"
"${NETWORK_AGENT_BIN}" --cubelet-config "${CUBELET_CONFIG}" --state-dir "${NETWORK_AGENT_STATE_DIR}" &
NETWORK_AGENT_PID=$!

for i in $(seq 1 120); do
  if curl -fsS "${NETWORK_AGENT_HEALTH_URL}" >/dev/null 2>&1; then
    log "network-agent ready"
    break
  fi
  if ! kill -0 "${NETWORK_AGENT_PID}" >/dev/null 2>&1; then
    fail "network-agent exited before ready"
  fi
  [[ "${i}" -lt 120 ]] || fail "network-agent did not become ready"
  sleep 1
done

log "starting cubelet for node ${CUBE_SANDBOX_NODE_IP}"
"${CUBELET_BIN}" --config "${CUBELET_CONFIG}" --dynamic-conf-path "${CUBELET_DYNAMICCONF}" &
CUBELET_LAUNCH_PID=$!

for i in $(seq 1 60); do
  real_pid="$(pidof cubelet 2>/dev/null | awk '{print $1}' || true)"
  if [[ -n "${real_pid}" ]] && kill -0 "${real_pid}" >/dev/null 2>&1 && ss -lntp 2>/dev/null | grep -q ':9999'; then
    CUBELET_PID="${real_pid}"
    log "cubelet ready, pid=${CUBELET_PID}"
    break
  fi
  if ! kill -0 "${CUBELET_LAUNCH_PID}" >/dev/null 2>&1 && [[ -z "${real_pid}" ]]; then
    fail "cubelet exited before listening on 9999"
  fi
  [[ "${i}" -lt 60 ]] || fail "cubelet did not become ready"
  sleep 1
done

while true; do
  if ! kill -0 "${NETWORK_AGENT_PID}" >/dev/null 2>&1; then
    fail "network-agent exited"
  fi
  if ! kill -0 "${CUBELET_PID}" >/dev/null 2>&1; then
    fail "cubelet exited"
  fi
  sleep 10
done
