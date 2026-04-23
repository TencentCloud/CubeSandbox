#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

for arg in "$@"; do
  case "${arg}" in
    --node-ip=*)
      export CUBE_SANDBOX_NODE_IP="${arg#--node-ip=}"
      ;;
  esac
done

ENV_FILE="${ONE_CLICK_ENV_FILE:-${SCRIPT_DIR}/.env}"
if [[ -f "${ENV_FILE}" ]]; then
  load_env_file "${ENV_FILE}"
fi

DEPLOY_ROLE="$(one_click_deploy_role)"
TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
INSTALL_PREFIX="${ONE_CLICK_INSTALL_PREFIX:-${TOOLBOX_ROOT}}"

print_path_hint() {
  {
    echo
    echo "[one-click] Installed public commands in /usr/local/bin:"
    echo "[one-click]   cube-runtime"
    echo "[one-click]   containerd-shim-cube-rs"
    echo "[one-click]   cubecli"
    if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
      echo "[one-click]   cubemastercli"
    fi
    echo
  } >&2
}

detect_installed_role() {
  if [[ ! -f "${INSTALL_PREFIX}/.one-click.env" ]]; then
    return 0
  fi

  local installed_role_line
  installed_role_line="$(rg '^ONE_CLICK_DEPLOY_ROLE=' "${INSTALL_PREFIX}/.one-click.env" || true)"
  if [[ -n "${installed_role_line}" ]]; then
    printf '%s\n' "${installed_role_line#ONE_CLICK_DEPLOY_ROLE=}"
  fi
}

needs_docker_for_install() {
  if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
    return 0
  fi

  local installed_role
  installed_role="$(detect_installed_role)"
  [[ -n "${installed_role}" && "${installed_role}" != "compute" ]]
}

require_any_cmd() {
  local cmd
  for cmd in "$@"; do
    if command -v "${cmd}" >/dev/null 2>&1; then
      return 0
    fi
  done
  die "requires one of commands: $*"
}

check_dns_preflight() {
  # up-dns/down-dns parse resolv.conf via awk.
  require_cmd awk

  if command -v resolvectl >/dev/null 2>&1; then
    return 0
  fi

  require_cmd systemctl
  local nm_load_state
  nm_load_state="$(systemctl show -p LoadState --value NetworkManager 2>/dev/null || true)"
  [[ "${nm_load_state}" == "loaded" ]] || die "DNS setup requires resolvectl or NetworkManager"

  if ! command -v dnsmasq >/dev/null 2>&1; then
    require_any_cmd dnf yum apt-get
  fi
}

check_proxy_cert_preflight() {
  # mkcert is bundled inside the release package (support/bin/mkcert).
  # up-cube-proxy will copy it to /usr/local/bin/mkcert when not already present.
  :
}

check_hardware_preflight() {
  if [[ ! -e /dev/kvm ]]; then
    die "KVM is not supported or not enabled (/dev/kvm not found)."
  fi

  local mem_total_kb
  mem_total_kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  if [[ "${mem_total_kb}" -lt 7500000 ]]; then
    die "System memory must be at least 8GB."
  fi
}

check_cubelet_fs_preflight() {
  local cubelet_dir="/data/cubelet"

  # Walk up to find the nearest existing ancestor so we can query its filesystem.
  # This covers the case where /data/cubelet (or even /data) does not yet exist.
  local check_path="${cubelet_dir}"
  while [[ ! -e "${check_path}" ]]; do
    local parent
    parent="$(dirname "${check_path}")"
    [[ "${parent}" != "${check_path}" ]] || break
    check_path="${parent}"
  done

  local fs_type
  fs_type="$(df -T "${check_path}" 2>/dev/null | awk 'NR==2 {print $2}')"

  if [[ "${fs_type}" == "xfs" ]]; then
    return 0
  fi

  if [[ -d "${cubelet_dir}" ]] && mountpoint -q "${cubelet_dir}" 2>/dev/null; then
    die "/data/cubelet is a mount point but its filesystem type is '${fs_type}' (requires xfs).
  Please format the underlying partition as XFS and remount it at /data/cubelet:
    mkfs.xfs /dev/<your-partition>
    mount /dev/<your-partition> /data/cubelet"
  else
    die "The filesystem that will host /data/cubelet is on '${check_path}' (type: ${fs_type:-unknown}), which is not XFS.
  Cube Sandbox requires the /data/cubelet directory to reside on an XFS filesystem.
  Options:
    1. Mount a dedicated XFS-formatted partition at /data/cubelet:
         mkfs.xfs /dev/<your-partition>
         mount /dev/<your-partition> /data/cubelet
    2. Ensure the parent path (${check_path}) itself is on XFS."
  fi
}

check_install_preflight() {
  # install.sh itself.
  require_cmd tar
  require_cmd rg
  require_cmd ss

  # runtime common helpers used by up/down scripts.
  require_cmd bash
  require_cmd curl
  require_cmd sed
  require_cmd pgrep
  require_cmd date

  if needs_docker_for_install; then
    require_cmd docker
  fi

  # tencent mirror path may mutate /etc/docker/daemon.json via python3.
  if needs_docker_for_install && [[ "${ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR:-0}" == "1" && -f /etc/docker/daemon.json ]]; then
    require_cmd python3
  fi

  if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
    # control role executes up-with-deps -> up-cube-proxy/up-dns.
    require_cmd ip
    check_proxy_cert_preflight
    check_dns_preflight
  fi
}

configure_tencent_docker_mirror() {
  local enable_mirror="${ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR:-0}"
  local mirror_url="${ONE_CLICK_TENCENT_DOCKER_MIRROR_URL:-https://mirror.ccs.tencentyun.com}"
  local daemon_json="/etc/docker/daemon.json"

  if [[ "${enable_mirror}" != "1" ]]; then
    return 0
  fi

  mkdir -p /etc/docker
  if [[ ! -f "${daemon_json}" ]]; then
    cat >"${daemon_json}" <<EOF
{
  "registry-mirrors": [
    "${mirror_url}"
  ]
}
EOF
  else
    require_cmd python3
    python3 - "${daemon_json}" "${mirror_url}" <<'PY'
import json
import sys
from pathlib import Path

daemon_path = Path(sys.argv[1])
mirror = sys.argv[2]
raw = daemon_path.read_text(encoding="utf-8").strip()
data = json.loads(raw) if raw else {}
mirrors = data.get("registry-mirrors", [])
if isinstance(mirrors, str):
    mirrors = [mirrors]
elif not isinstance(mirrors, list):
    mirrors = []
if mirror not in mirrors:
    mirrors.append(mirror)
data["registry-mirrors"] = mirrors
daemon_path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
PY
  fi

  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl restart docker || die "failed to restart docker"
  else
    service docker restart || die "failed to restart docker"
  fi
}

require_root

CUBE_SANDBOX_NODE_IP="$(detect_node_ip)"
export CUBE_SANDBOX_NODE_IP
log "using node IP: ${CUBE_SANDBOX_NODE_IP}"
CUBE_SANDBOX_ETH_NAME="${CUBE_SANDBOX_ETH_NAME:-$(detect_primary_interface || true)}"
if [[ -n "${CUBE_SANDBOX_ETH_NAME}" ]]; then
  export CUBE_SANDBOX_ETH_NAME
  log "using primary network interface: ${CUBE_SANDBOX_ETH_NAME}"
else
  log "primary network interface not detected; keeping packaged Cubelet eth_name"
fi

install_dependencies
check_hardware_preflight
check_cubelet_fs_preflight
check_install_preflight
if needs_docker_for_install; then
  configure_tencent_docker_mirror
fi

PACKAGE_TAR="${ONE_CLICK_PACKAGE_TAR:-${SCRIPT_DIR}/assets/package/sandbox-package.tar.gz}"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

ensure_file "${PACKAGE_TAR}"

log "extracting package ${PACKAGE_TAR}"
tar -xzf "${PACKAGE_TAR}" -C "${WORK_DIR}"
PKG_ROOT="${WORK_DIR}/sandbox-package"
ensure_dir "${PKG_ROOT}"

installed_role="${DEPLOY_ROLE}"
detected_installed_role="$(detect_installed_role)"
if [[ -n "${detected_installed_role}" ]]; then
  installed_role="${detected_installed_role}"
fi

if [[ "${installed_role}" == "compute" && -x "${INSTALL_PREFIX}/scripts/one-click/down-compute.sh" ]]; then
  stop_script="${PKG_ROOT}/scripts/one-click/down-compute.sh"
else
  stop_script="${PKG_ROOT}/scripts/one-click/down-with-deps.sh"
fi

if [[ -x "${stop_script}" ]]; then
  log "stopping existing deployment under ${INSTALL_PREFIX}"
  ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
  ONE_CLICK_RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env" \
    "${stop_script}" || true
fi

if [[ "${INSTALL_PREFIX%/}" == "${TOOLBOX_ROOT%/}" ]]; then
  rm -rf \
    "${INSTALL_PREFIX}/network-agent" \
    "${INSTALL_PREFIX}/CubeAPI" \
    "${INSTALL_PREFIX}/CubeMaster" \
    "${INSTALL_PREFIX}/Cubelet" \
    "${INSTALL_PREFIX}/cubeproxy" \
    "${INSTALL_PREFIX}/coredns" \
    "${INSTALL_PREFIX}/support" \
    "${INSTALL_PREFIX}/cube-shim" \
    "${INSTALL_PREFIX}/cube-kernel-scf" \
    "${INSTALL_PREFIX}/cube-image" \
    "${INSTALL_PREFIX}/scripts" \
    "${INSTALL_PREFIX}/sql" \
    "${INSTALL_PREFIX}/.one-click.env"
else
  rm -rf "${INSTALL_PREFIX}"
fi

mkdir -p "${INSTALL_PREFIX}"
if [[ "${DEPLOY_ROLE}" == "compute" ]]; then
  copy_dir_contents "${PKG_ROOT}/network-agent" "${INSTALL_PREFIX}/network-agent"
  copy_dir_contents "${PKG_ROOT}/Cubelet" "${INSTALL_PREFIX}/Cubelet"
  copy_dir_contents "${PKG_ROOT}/cube-shim" "${INSTALL_PREFIX}/cube-shim"
  copy_dir_contents "${PKG_ROOT}/cube-kernel-scf" "${INSTALL_PREFIX}/cube-kernel-scf"
  copy_dir_contents "${PKG_ROOT}/cube-image" "${INSTALL_PREFIX}/cube-image"
  copy_dir_contents "${PKG_ROOT}/scripts" "${INSTALL_PREFIX}/scripts"
else
  cp -a "${PKG_ROOT}/." "${INSTALL_PREFIX}/"
fi

mkdir -p \
  "${INSTALL_PREFIX}/cube-vs/network" \
  "${INSTALL_PREFIX}/cube-snapshot" \
  /data/log/Cubelet \
  /data/log/CubeShim \
  /data/log/CubeVmm \
  /data/cube-shim/disks \
  /data/snapshot_pack/disks

if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
  mkdir -p \
    /data/log/CubeAPI \
    /data/log/CubeMaster \
    /data/log/cube-proxy
fi

RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env"
if [[ -f "${ENV_FILE}" ]]; then
  cp -f "${ENV_FILE}" "${RUNTIME_ENV_FILE}"
else
  : > "${RUNTIME_ENV_FILE}"
fi
upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_DEPLOY_ROLE" "${DEPLOY_ROLE}"
if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_SANDBOX_NODE_IP" "${CUBE_SANDBOX_NODE_IP}"
fi
if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_SANDBOX_ETH_NAME" "${CUBE_SANDBOX_ETH_NAME}"
fi
if [[ -n "${ONE_CLICK_CONTROL_PLANE_IP:-}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_CONTROL_PLANE_IP" "${ONE_CLICK_CONTROL_PLANE_IP}"
fi
if [[ -n "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR:-}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR" "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR}"
fi

chmod +x "${INSTALL_PREFIX}/network-agent/bin/network-agent"
chmod +x "${INSTALL_PREFIX}/Cubelet/bin/"*
chmod +x "${INSTALL_PREFIX}/cube-shim/bin/containerd-shim-cube-rs" "${INSTALL_PREFIX}/cube-shim/bin/cube-runtime"
chmod +x "${INSTALL_PREFIX}/scripts/one-click/"*.sh

if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
  cubelet_config="${INSTALL_PREFIX}/Cubelet/config/config.toml"
  if rg -q '^[[:space:]]*eth_name = "' "${cubelet_config}"; then
    sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${CUBE_SANDBOX_ETH_NAME}\"/" "${cubelet_config}"
    if ! grep -Fq "eth_name = \"${CUBE_SANDBOX_ETH_NAME}\"" "${cubelet_config}"; then
      log "WARNING: failed to patch eth_name in Cubelet config (${cubelet_config})"
    fi
  else
    log "WARNING: Cubelet config missing eth_name key; skipped NIC patch (${cubelet_config})"
  fi
fi

if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
  chmod +x "${INSTALL_PREFIX}/CubeAPI/bin/cube-api"
  chmod +x "${INSTALL_PREFIX}/CubeMaster/bin/cubemaster" "${INSTALL_PREFIX}/CubeMaster/bin/cubemastercli"
fi

ln -sf "${INSTALL_PREFIX}/cube-shim/bin/containerd-shim-cube-rs" /usr/local/bin/containerd-shim-cube-rs
ln -sf "${INSTALL_PREFIX}/cube-shim/bin/cube-runtime" /usr/local/bin/cube-runtime
ln -sf "${INSTALL_PREFIX}/Cubelet/bin/cubecli" /usr/local/bin/cubecli
if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
  ln -sf "${INSTALL_PREFIX}/CubeMaster/bin/cubemastercli" /usr/local/bin/cubemastercli
else
  rm -f /usr/local/bin/cubemastercli
fi

if [[ "${DEPLOY_ROLE}" == "compute" ]]; then
  start_script="${INSTALL_PREFIX}/scripts/one-click/up-compute.sh"
else
  start_script="${INSTALL_PREFIX}/scripts/one-click/up-with-deps.sh"
fi

ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
ONE_CLICK_RUNTIME_ENV_FILE="${RUNTIME_ENV_FILE}" \
  "${start_script}"

if [[ "${ONE_CLICK_RUN_QUICKCHECK:-1}" == "1" ]]; then
  ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
  ONE_CLICK_RUNTIME_ENV_FILE="${RUNTIME_ENV_FILE}" \
    "${INSTALL_PREFIX}/scripts/one-click/quickcheck.sh"
fi

log "install complete (role=${DEPLOY_ROLE})"
print_path_hint
