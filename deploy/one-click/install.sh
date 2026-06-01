#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
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
CUBE_PVM_ENABLE="${CUBE_PVM_ENABLE:-0}"
case "${CUBE_PVM_ENABLE}" in
  0|1) ;;
  *) die "unsupported CUBE_PVM_ENABLE: ${CUBE_PVM_ENABLE} (expected 0 or 1)" ;;
esac

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

install_required_dependencies() {
  log "checking and installing dependencies..."
  install_ripgrep

  if needs_docker_for_install; then
    install_docker
    install_docker_compose
  fi
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

one_click_runtime_file_paths() {
  [[ "${DEPLOY_ROLE}" != "compute" ]] || return 0

  printf '%s\n' \
    "${INSTALL_PREFIX}/cubeproxy/global.conf" \
    "${INSTALL_PREFIX}/cubeproxy/nginx.conf" \
    "${INSTALL_PREFIX}/webui/nginx.generated.conf" \
    "${INSTALL_PREFIX}/coredns/Corefile" \
    "${INSTALL_PREFIX}/coredns/resolv.conf.upstream"
}

check_runtime_file_paths_not_directories() {
  local path
  while IFS= read -r path; do
    [[ -n "${path}" ]] || continue
    if [[ ! -d "${path}" ]]; then
      continue
    fi
    if rmdir "${path}" 2>/dev/null; then
      log "removed empty directory at runtime file path: ${path}"
      continue
    fi
    die "runtime file path is a non-empty directory: ${path}; move it away and retry"
  done < <(one_click_runtime_file_paths)
}

generate_cubemaster_config_ports() {
  [[ "${DEPLOY_ROLE}" != "compute" ]] || return 0

  local cfg="${PKG_ROOT}/CubeMaster/conf.yaml"
  local mysql_port="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
  local redis_port="${CUBE_SANDBOX_REDIS_PORT:-6379}"

  ensure_file "${cfg}"
  sed -i \
    -e "s|__CUBE_SANDBOX_MYSQL_PORT__|${mysql_port}|g" \
    -e "s|__CUBE_SANDBOX_REDIS_PORT__|${redis_port}|g" \
    "${cfg}"
}

check_hardware_preflight() {
  if [[ ! -e /dev/kvm ]]; then
    log "KVM is not supported or not enabled (/dev/kvm not found)."
    log ""
    log "If this host cannot expose hardware KVM (for example, it is itself a"
    log "virtual machine without nested virtualization), you can try the"
    log "open-source PVM stack shipped under deploy/pvm/ to turn the current"
    log "guest into a PVM host that provides /dev/kvm to CubeSandbox:"
    log ""
    log "    sudo bash deploy/pvm/pvm_setup.sh"
    log ""
    log "That script will build and install a PVM-enabled host kernel, build a"
    log "matching PVM guest vmlinux, and guide you through the reboot needed to"
    log "switch into the new kernel. After reboot, re-run this installer."
    log ""
    log "WARNING: the open-source kvm-pvm integration is intended for"
    log "development, evaluation and self-built experiments only. It is NOT"
    log "suitable for production workloads -- expect reduced performance,"
    log "limited hardware coverage and no long-term support guarantees."
    die "KVM is not supported or not enabled (/dev/kvm not found)."
  fi

  local mem_total_kb
  mem_total_kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  
  local min_mem_kb=7500000
  if [[ -n "${CUBE_MIN_MEMORY_KB:-}" ]]; then
    if [[ "${CUBE_MIN_MEMORY_KB}" =~ ^[0-9]+$ ]] && [[ "${CUBE_MIN_MEMORY_KB}" -gt 0 ]]; then
      # Enforce that the threshold cannot be lower than the default 8GB (7500000 KB) in the authoritative installer
      if [[ "${CUBE_MIN_MEMORY_KB}" -ge 7500000 ]]; then
        min_mem_kb="${CUBE_MIN_MEMORY_KB}"
      fi
    else
      die "Invalid CUBE_MIN_MEMORY_KB '${CUBE_MIN_MEMORY_KB}' (must be a positive integer greater than 0)."
    fi
  fi

  if [[ "${mem_total_kb}" -lt "${min_mem_kb}" ]]; then
    die "System memory must be at least $((min_mem_kb / 1024 / 1024))GB (found $((mem_total_kb / 1024 / 1024)) GB)."
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

check_cgroup_cpu_preflight() {
  local cgroot="/sys/fs/cgroup"
  local fstype
  fstype="$(stat -fc %T "${cgroot}" 2>/dev/null || echo unknown)"

  # cgroup v1 systems still work via the v1 handle in cubelet; only validate
  # cgroup v2 hosts here (which is what every recent distro defaults to).
  if [[ "${fstype}" != "cgroup2fs" ]]; then
    return 0
  fi

  local controllers=""
  if [[ -r "${cgroot}/cgroup.controllers" ]]; then
    controllers="$(cat "${cgroot}/cgroup.controllers" 2>/dev/null || true)"
  fi
  if ! grep -qw cpu <<<"${controllers}"; then
    die "Kernel cgroup v2 does not expose the 'cpu' controller (cgroup.controllers='${controllers:-<empty>}').
  cubelet cannot set CPU quotas without it.
  See: https://github.com/TencentCloud/CubeSandbox/issues/366"
  fi

  local subtree=""
  if [[ -r "${cgroot}/cgroup.subtree_control" ]]; then
    subtree="$(cat "${cgroot}/cgroup.subtree_control" 2>/dev/null || true)"
  fi
  if grep -qw cpu <<<"${subtree}"; then
    return 0
  fi

  log "cgroup v2 'cpu' controller not enabled on ${cgroot}/cgroup.subtree_control; trying to enable it"
  if printf '+cpu\n' >"${cgroot}/cgroup.subtree_control" 2>/dev/null; then
    log "enabled '+cpu' on ${cgroot}/cgroup.subtree_control"
    return 0
  fi

  die "Failed to enable the cgroup v2 'cpu' controller on ${cgroot}/cgroup.subtree_control.
  On Ubuntu / Debian this is usually caused by 'multipathd' (or another service) running real-time threads under the root cgroup, which blocks '+cpu' with 'Invalid argument'.
  Quick fix:
    systemctl disable --now multipathd.service multipathd.socket
    echo +cpu > ${cgroot}/cgroup.subtree_control
  Full repro and fix: https://github.com/TencentCloud/CubeSandbox/issues/366"
}

check_install_preflight() {
  # install.sh itself.
  require_cmd tar
  require_cmd rg
  require_cmd ss
  require_cmd systemctl

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

select_installed_kernel_vmlinux() {
  local kernel_dir="${INSTALL_PREFIX}/cube-kernel-scf"

  ensure_file "${kernel_dir}/vmlinux"
  if [[ "${CUBE_PVM_ENABLE}" != "1" ]]; then
    log "using ordinary guest kernel: ${kernel_dir}/vmlinux"
    return 0
  fi

  ensure_file "${kernel_dir}/vmlinux-pvm"
  cp -f "${kernel_dir}/vmlinux-pvm" "${kernel_dir}/vmlinux"
  log "CUBE_PVM_ENABLE=1, installed PVM guest kernel as ${kernel_dir}/vmlinux"
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

systemd_target_for_role() {
  local role="$1"
  case "${role}" in
    control)
      printf '%s\n' "cube-sandbox-control.target"
      ;;
    compute)
      printf '%s\n' "cube-sandbox-compute.target"
      ;;
    *)
      die "unsupported role for systemd target: ${role}"
      ;;
  esac
}

stop_existing_systemd_deployment() {
  systemctl disable --now \
    cube-sandbox-control.target \
    cube-sandbox-compute.target >/dev/null 2>&1 || true
}

stop_existing_legacy_deployment() {
  # Legacy bridge for upgrading pre-systemd one-click installs.
  # New installs are systemd-only; this path only stops old nohup/pidfile deployments
  # before the install prefix is replaced.
  local installed_role="$1"
  local legacy_stop_script=""

  if [[ "${installed_role}" == "compute" && -x "${INSTALL_PREFIX}/scripts/one-click/down-compute.sh" ]]; then
    legacy_stop_script="${INSTALL_PREFIX}/scripts/one-click/down-compute.sh"
  elif [[ -x "${INSTALL_PREFIX}/scripts/one-click/down-with-deps.sh" ]]; then
    legacy_stop_script="${INSTALL_PREFIX}/scripts/one-click/down-with-deps.sh"
  fi

  if [[ -n "${legacy_stop_script}" ]]; then
    log "stopping legacy pre-systemd deployment under ${INSTALL_PREFIX}"
    ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
    ONE_CLICK_RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env" \
      "${legacy_stop_script}" || true
  fi
}

install_systemd_units() {
  local install_units_script="${INSTALL_PREFIX}/scripts/systemd/install-units.sh"
  ensure_file "${install_units_script}"
  ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
  ONE_CLICK_RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env" \
    "${install_units_script}"
}

start_systemd_target() {
  local target
  target="$(systemd_target_for_role "${DEPLOY_ROLE}")"
  systemctl disable --now \
    cube-sandbox-control.target \
    cube-sandbox-compute.target >/dev/null 2>&1 || true
  systemctl enable --now "${target}"
}

require_root

# Run critical preflight checks that do not depend on dependency installation first
# to ensure we fail fast before installing or modifying any local system packages.
check_hardware_preflight
check_cubelet_fs_preflight
check_cgroup_cpu_preflight

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

install_required_dependencies
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
validate_cubelet_cow_startup_deps "${PKG_ROOT}/Cubelet/config/config.toml"

installed_role="${DEPLOY_ROLE}"
detected_installed_role="$(detect_installed_role)"
if [[ -n "${detected_installed_role}" ]]; then
  installed_role="${detected_installed_role}"
fi

log "stopping existing systemd deployment under ${INSTALL_PREFIX}"
stop_existing_systemd_deployment
stop_existing_legacy_deployment "${installed_role}"

if [[ "${INSTALL_PREFIX%/}" == "${TOOLBOX_ROOT%/}" ]]; then
  rm -rf \
    "${INSTALL_PREFIX}/network-agent" \
    "${INSTALL_PREFIX}/CubeAPI" \
    "${INSTALL_PREFIX}/CubeMaster" \
    "${INSTALL_PREFIX}/Cubelet" \
    "${INSTALL_PREFIX}/cubeproxy" \
    "${INSTALL_PREFIX}/coredns" \
    "${INSTALL_PREFIX}/webui" \
    "${INSTALL_PREFIX}/support" \
    "${INSTALL_PREFIX}/systemd" \
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
  copy_dir_contents "${PKG_ROOT}/systemd" "${INSTALL_PREFIX}/systemd"
  copy_dir_contents "${PKG_ROOT}/scripts" "${INSTALL_PREFIX}/scripts"
else
  generate_cubemaster_config_ports
  cp -a "${PKG_ROOT}/." "${INSTALL_PREFIX}/"
fi

select_installed_kernel_vmlinux

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
upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_PVM_ENABLE" "${CUBE_PVM_ENABLE}"
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
chmod +x "${INSTALL_PREFIX}/scripts/systemd/"*.sh

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

install_systemd_units
check_runtime_file_paths_not_directories
start_systemd_target

if [[ "${ONE_CLICK_RUN_QUICKCHECK:-1}" == "1" ]]; then
  ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
  ONE_CLICK_RUNTIME_ENV_FILE="${RUNTIME_ENV_FILE}" \
    "${INSTALL_PREFIX}/scripts/one-click/quickcheck.sh"
fi

log "install complete (role=${DEPLOY_ROLE})"
print_path_hint
