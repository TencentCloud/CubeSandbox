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
if [[ -z "${CUBE_MASTER_ENDPOINT:-}" ]]; then
  # Do not fall back to a hardcoded namespace like cube-system: when this env
  # var is missing it means the DaemonSet template did not inject the endpoint
  # for the current release/namespace, and silently defaulting would target
  # the wrong control plane. Fail fast so the operator notices.
  printf '[cube-node-entrypoint] FATAL: CUBE_MASTER_ENDPOINT is empty. The chart must inject it from cube.masterEndpoint helper.\n' >&2
  exit 1
fi
CUBE_PVM_ENABLE="${CUBE_PVM_ENABLE:-1}"
CUBE_SANDBOX_AUTO_DETECT_ETH="${CUBE_SANDBOX_AUTO_DETECT_ETH:-true}"
STATE_DIR="${STATE_DIR:-/var/lib/cube-node-bootstrap}"

log() { printf '[cube-node-entrypoint] %s\n' "$*"; }
fail() { printf '[cube-node-entrypoint] ERROR: %s\n' "$*" >&2; exit 1; }

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
  if [[ -z "${CUBE_SANDBOX_DNS_SERVERS:-}" && "${CUBE_SANDBOX_DNS_FOLLOW_NODE:-false}" == "true" ]]; then
    CUBE_SANDBOX_DNS_SERVERS="$(
      awk '
        /^nameserver[[:space:]]+/ {
          ip=$2
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

[[ -x "${NETWORK_AGENT_BIN}" ]] || fail "missing executable: ${NETWORK_AGENT_BIN}"
[[ -x "${CUBELET_BIN}" ]] || fail "missing executable: ${CUBELET_BIN}"
[[ -f "${CUBELET_CONFIG}" ]] || fail "missing config: ${CUBELET_CONFIG}"
[[ -f "${CUBELET_DYNAMICCONF}" ]] || fail "missing dynamic config: ${CUBELET_DYNAMICCONF}"
[[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]] || fail "CUBE_SANDBOX_NODE_IP is required"

validate_runtime_commands
apply_effective_pvm_from_state
select_guest_kernel

# Escape backslashes, ampersands, and forward slashes so they cannot terminate
# or reinterpret the sed replacement expression when the input comes from
# operator-supplied env vars (endpoint URLs, interface names, CIDRs, etc.).
sed_escape_replacement() {
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g' -e 's/[/]/\\\//g'
}

CUBE_MASTER_ENDPOINT_ESC="$(sed_escape_replacement "${CUBE_MASTER_ENDPOINT}")"
sed -i -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${CUBE_MASTER_ENDPOINT_ESC}\"#" "${CUBELET_DYNAMICCONF}"
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
  ETH_ESC="$(sed_escape_replacement "${CUBE_SANDBOX_ETH_NAME}")"
  sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${ETH_ESC}\"/" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
  CIDR_ESC="$(sed_escape_replacement "${CUBE_SANDBOX_NETWORK_CIDR}")"
  sed -i "s|cidr = \"[^\"]*\"|cidr = \"${CIDR_ESC}\"|" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_TAP_INIT_NUM:-}" ]]; then
  # Reject anything that is not an integer so operators cannot inject
  # replacement content via numeric-looking env variables.
  [[ "${CUBE_TAP_INIT_NUM}" =~ ^[0-9]+$ ]] || fail "CUBE_TAP_INIT_NUM must be a non-negative integer"
  sed -i "s/tap_init_num = [0-9]\+/tap_init_num = ${CUBE_TAP_INIT_NUM}/" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_CGROUP_POOL_SIZE:-}" ]]; then
  [[ "${CUBE_CGROUP_POOL_SIZE}" =~ ^[0-9]+$ ]] || fail "CUBE_CGROUP_POOL_SIZE must be a non-negative integer"
  sed -i "s/pool_size = [0-9]\+/pool_size = ${CUBE_CGROUP_POOL_SIZE}/" "${CUBELET_CONFIG}"
fi
if [[ -n "${CUBE_WORKFLOW_CONCURRENT:-}" ]]; then
  [[ "${CUBE_WORKFLOW_CONCURRENT}" =~ ^[0-9]+$ ]] || fail "CUBE_WORKFLOW_CONCURRENT must be a non-negative integer"
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
  /data/snapshot_pack/disks \
  /data/cubelet/state

# Keep shim bundle metadata on the dataCubelet hostPath across Pod rebuilds.
# cubelet mountTmpfsDir() skips when state is already mounted; without this,
# a 1Gi tmpfs in cubelet's private mount NS holds bootstrap.json/address and
# is discarded when the Pod (and that mount NS) goes away, breaking
# LoadExistingShims even if shim processes and /run/containerd sockets survive.
if ! findmnt --mountpoint /data/cubelet/state >/dev/null 2>&1; then
  mount --bind /data/cubelet/state /data/cubelet/state
  log "bound /data/cubelet/state to hostPath (skip state tmpfs)"
fi

rm -f \
  /tmp/cube/network-agent.sock \
  /tmp/cube/network-agent-grpc.sock \
  /tmp/cube/network-agent-tap.sock \
  || true

# stop_stale_processes intentionally matches ONLY cubelet / network-agent.
# Never broaden these patterns to containerd-shim-cube-rs, cube-runtime, or
# VMM processes: those must survive Pod rebuilds so existing sandboxes keep
# running across image upgrades (one-click KillMode=process equivalent).
# Also never call InitHost / cubecli unsafe init from this entrypoint — that
# path destroys every sandbox on the node.
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
  # TERM/INT/HUP/EXIT: stop only the control processes we started. Do not
  # pkill shim/runtime — Pod deletion must leave microVMs alive for recover.
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
