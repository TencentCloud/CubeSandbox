#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ENV_FILE="${ONE_CLICK_ENV_FILE:-${SCRIPT_DIR}/.env}"
if [[ -f "${ENV_FILE}" ]]; then
  load_env_file "${ENV_FILE}"
fi

WORK_ROOT="${ONE_CLICK_WORK_ROOT:-${SCRIPT_DIR}/.work}"
RUNTIME_LAYOUT_DIR="${ONE_CLICK_RUNTIME_LAYOUT_DIR:-${WORK_ROOT}/runtime-layout}"
GUEST_IMAGE_WORK_DIR="${WORK_ROOT}/guest-image-build"
GUEST_ROOTFS_DIR="${GUEST_IMAGE_WORK_DIR}/rootfs"
GUEST_ROOTFS_TAR="${GUEST_IMAGE_WORK_DIR}/rootfs.tar"
RAW_ARTIFACTS_DIR="${SCRIPT_DIR}/assets/kernel-artifacts"

CUBE_KERNEL_VMLINUX="${ONE_CLICK_CUBE_KERNEL_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux}"
CUBE_KERNEL_PVM_VMLINUX="${ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux-pvm}"
GUEST_IMAGE_DOCKERFILE="${ONE_CLICK_GUEST_IMAGE_DOCKERFILE:-${ROOT_DIR}/deploy/guest-image/Dockerfile}"
GUEST_IMAGE_CONTEXT_DIR="${ONE_CLICK_GUEST_IMAGE_CONTEXT_DIR:-$(dirname "${GUEST_IMAGE_DOCKERFILE}")}"
GUEST_IMAGE_REF="${ONE_CLICK_GUEST_IMAGE_REF:-cube-sandbox-guest-image:one-click}"
GUEST_IMAGE_VERSION="${ONE_CLICK_GUEST_IMAGE_VERSION:-$(latest_git_revision "${ROOT_DIR}")}"

CUBE_AGENT_BUILD_MODE="${ONE_CLICK_CUBE_AGENT_BUILD_MODE:-local}"
CUBE_SHIM_BUILD_MODE="${ONE_CLICK_CUBE_SHIM_BUILD_MODE:-local}"

CUBE_AGENT_BIN_OVERRIDE="${ONE_CLICK_CUBE_AGENT_BIN:-}"
CUBESHIM_BIN_OVERRIDE="${ONE_CLICK_CUBESHIM_BIN:-}"
CUBE_RUNTIME_BIN_OVERRIDE="${ONE_CLICK_CUBE_RUNTIME_BIN:-}"
RUNTIME_CFG_OVERRIDE="${ONE_CLICK_RUNTIME_CFG_SRC:-}"
CUBE_SHIM_WORKSPACE_READY=0

find_built_binary() {
  local base_dir="$1"
  local name="$2"
  local path
  path="$(python3 - "$base_dir" "$name" <<'PY'
import os
import sys

base_dir = sys.argv[1]
name = sys.argv[2]
matches = []
for root, _, files in os.walk(base_dir):
    for file_name in files:
        if file_name != name:
            continue
        path = os.path.join(root, file_name)
        if os.access(path, os.X_OK):
            matches.append(path)
matches.sort(key=lambda p: os.path.getmtime(p))
print(matches[-1] if matches else "")
PY
)"
  [[ -n "${path}" ]] || die "failed to locate built binary '${name}' under ${base_dir}"
  printf '%s\n' "${path}"
}

copy_binary_with_deps() {
  local src_bin="$1"
  local dst_path="$2"
  local dst_root="$3"
  local loader
  local dep
  local ldd_output

  mkdir -p "${dst_root}$(dirname "${dst_path}")"
  cp -L "${src_bin}" "${dst_root}${dst_path}" 2>/dev/null || {
    require_cmd sudo
    sudo cp -L "${src_bin}" "${dst_root}${dst_path}"
  }
  chmod +x "${dst_root}${dst_path}" 2>/dev/null || {
    require_cmd sudo
    sudo chmod +x "${dst_root}${dst_path}"
  }

  ldd_output="$(ldd "${src_bin}" 2>/dev/null || true)"
  while IFS= read -r dep; do
    [[ -n "${dep}" ]] || continue
    mkdir -p "${dst_root}$(dirname "${dep}")"
    cp -L "${dep}" "${dst_root}${dep}" 2>/dev/null || {
      require_cmd sudo
      sudo cp -L "${dep}" "${dst_root}${dep}"
    }
  done < <(printf '%s\n' "${ldd_output}" | awk '
    /=> \// { print $3 }
    /^\// { print $1 }
  ' | sort -u)

  loader="$(printf '%s\n' "${ldd_output}" | awk '/ld-linux|ld-musl/ { print $1 }' | tail -n 1)"
  if [[ -n "${loader}" && -f "${loader}" ]]; then
    mkdir -p "${dst_root}$(dirname "${loader}")"
    cp -L "${loader}" "${dst_root}${loader}" 2>/dev/null || {
      require_cmd sudo
      sudo cp -L "${loader}" "${dst_root}${loader}"
    }
  fi
}

build_cube_agent() {
  if [[ -n "${CUBE_AGENT_BIN_OVERRIDE}" ]]; then
    ensure_file "${CUBE_AGENT_BIN_OVERRIDE}"
    log "using prebuilt cube-agent: ${CUBE_AGENT_BIN_OVERRIDE}"
    printf '%s\n' "${CUBE_AGENT_BIN_OVERRIDE}"
    return 0
  fi

  case "${CUBE_AGENT_BUILD_MODE}" in
    local)
      require_cmd make
      log "building cube-agent via make"
      (cd "${ROOT_DIR}/agent" && make) >&2
      ;;
    docker)
      require_cmd make
      require_cmd docker
      log "building cube-agent via make all-docker"
      (cd "${ROOT_DIR}/agent" && make all-docker) >&2
      ;;
    *)
      die "unsupported ONE_CLICK_CUBE_AGENT_BUILD_MODE: ${CUBE_AGENT_BUILD_MODE}"
      ;;
  esac

  find_built_binary "${ROOT_DIR}/agent/target" "cube-agent"
}

build_cube_shim_workspace() {
  if [[ "${CUBE_SHIM_WORKSPACE_READY}" -eq 1 ]]; then
    return 0
  fi

  case "${CUBE_SHIM_BUILD_MODE}" in
    local)
      require_cmd cargo
      log "building shim workspace via cargo"
      (cd "${ROOT_DIR}/CubeShim" && cargo build --release --locked) >&2
      ;;
    docker)
      require_cmd make
      require_cmd docker
      log "building shim workspace via make all-docker"
      (cd "${ROOT_DIR}/CubeShim" && make all-docker) >&2
      ;;
    *)
      die "unsupported ONE_CLICK_CUBE_SHIM_BUILD_MODE: ${CUBE_SHIM_BUILD_MODE}"
      ;;
  esac

  CUBE_SHIM_WORKSPACE_READY=1
}

build_cube_shim() {
  if [[ -n "${CUBESHIM_BIN_OVERRIDE}" ]]; then
    ensure_file "${CUBESHIM_BIN_OVERRIDE}"
    log "using prebuilt containerd-shim-cube-rs: ${CUBESHIM_BIN_OVERRIDE}"
    printf '%s\n' "${CUBESHIM_BIN_OVERRIDE}"
    return 0
  fi

  build_cube_shim_workspace
  find_built_binary "${ROOT_DIR}/CubeShim/target/release" "containerd-shim-cube-rs"
}

build_cube_runtime() {
  if [[ -n "${CUBE_RUNTIME_BIN_OVERRIDE}" ]]; then
    ensure_file "${CUBE_RUNTIME_BIN_OVERRIDE}"
    log "using prebuilt cube-runtime: ${CUBE_RUNTIME_BIN_OVERRIDE}"
    printf '%s\n' "${CUBE_RUNTIME_BIN_OVERRIDE}"
    return 0
  fi

  build_cube_shim_workspace
  find_built_binary "${ROOT_DIR}/CubeShim/target/release" "cube-runtime"
}

prepare_runtime_config() {
  local out_cfg="$1"
  mkdir -p "$(dirname "${out_cfg}")"
  if [[ -n "${RUNTIME_CFG_OVERRIDE}" ]]; then
    ensure_file "${RUNTIME_CFG_OVERRIDE}"
    log "using runtime config override: ${RUNTIME_CFG_OVERRIDE}"
    cp -f "${RUNTIME_CFG_OVERRIDE}" "${out_cfg}"
    return 0
  fi

  cp -f "${SCRIPT_DIR}/config-cube.toml" "${out_cfg}"
}

ensure_mkfs_ext4_supports_populate_dir() {
  local help_text
  help_text="$(mkfs.ext4 -h 2>&1 || true)"
  [[ "${help_text}" == *"-d "* || "${help_text}" == *"-d"* ]] || \
    die "mkfs.ext4 does not support -d; e2fsprogs is too old for guest image creation"
}

directory_size_bytes() {
  local dir_path="$1"
  python3 - "$dir_path" <<'PY'
import os
import sys

total = 0
for root, dirs, files in os.walk(sys.argv[1]):
    for name in dirs + files:
        path = os.path.join(root, name)
        try:
            stat = os.lstat(path)
        except FileNotFoundError:
            continue
        total += stat.st_size
print(total)
PY
}

calculate_guest_image_size_bytes() {
  local rootfs_size_bytes="$1"
  local size_step_bytes="$((256 * 1024 * 1024))"
  local reserved_bytes="$((64 * 1024 * 1024))"
  local requested_bytes

  requested_bytes="$((rootfs_size_bytes + reserved_bytes))"
  printf '%s\n' "$(( ((requested_bytes + size_step_bytes - 1) / size_step_bytes) * size_step_bytes ))"
}

run_mkfs_ext4_with_optional_sudo() {
  if [[ "${EUID}" -eq 0 ]]; then
    mkfs.ext4 "$@"
    return 0
  fi

  if mkfs.ext4 "$@" >/dev/null 2>&1; then
    return 0
  fi

  require_cmd sudo
  sudo mkfs.ext4 "$@"
}

remove_path_with_optional_sudo() {
  if [[ "$#" -eq 0 ]]; then
    return 0
  fi

  if [[ "${EUID}" -eq 0 ]]; then
    rm -rf "$@"
    return 0
  fi

  rm -rf "$@" 2>/dev/null || {
    require_cmd sudo
    sudo rm -rf "$@"
  }
}

inject_agent_into_guest_rootfs() {
  local guest_rootfs_dir="$1"
  local init_path="${guest_rootfs_dir}/sbin/init"
  local init_backup_path="${guest_rootfs_dir}/sbin/init.original"
  local rc_local_path="${guest_rootfs_dir}/etc/rc.local"
  local rc_local_tmp="${GUEST_IMAGE_WORK_DIR}/rc.local"
  local hostname_tmp="${GUEST_IMAGE_WORK_DIR}/hostname"
  local hosts_tmp="${GUEST_IMAGE_WORK_DIR}/hosts"
  local resolv_tmp="${GUEST_IMAGE_WORK_DIR}/resolv.conf"

  ensure_file "${AGENT_BIN}"

  mkdir -p "${guest_rootfs_dir}/sbin" "${guest_rootfs_dir}/etc"

  if [[ -e "${init_path}" || -L "${init_path}" ]]; then
    remove_path_with_optional_sudo "${init_backup_path}"
    mv -f "${init_path}" "${init_backup_path}" 2>/dev/null || {
      require_cmd sudo
      sudo mv -f "${init_path}" "${init_backup_path}"
    }
  fi

  copy_binary_with_deps "${AGENT_BIN}" "/sbin/init" "${guest_rootfs_dir}"

  if [[ ! -e "${rc_local_path}" ]]; then
    cat > "${rc_local_tmp}" <<'EOF'
#!/bin/sh
exit 0
EOF
    cp -f "${rc_local_tmp}" "${rc_local_path}" 2>/dev/null || {
      require_cmd sudo
      sudo cp -f "${rc_local_tmp}" "${rc_local_path}"
    }
    chmod +x "${rc_local_path}" 2>/dev/null || {
      require_cmd sudo
      sudo chmod +x "${rc_local_path}"
    }
  fi

  cat > "${hostname_tmp}" <<'EOF'
localhost
EOF
  cp -f "${hostname_tmp}" "${guest_rootfs_dir}/etc/hostname" 2>/dev/null || {
    require_cmd sudo
    sudo cp -f "${hostname_tmp}" "${guest_rootfs_dir}/etc/hostname"
  }

  cat > "${hosts_tmp}" <<'EOF'
127.0.0.1 localhost
EOF
  cp -f "${hosts_tmp}" "${guest_rootfs_dir}/etc/hosts" 2>/dev/null || {
    require_cmd sudo
    sudo cp -f "${hosts_tmp}" "${guest_rootfs_dir}/etc/hosts"
  }

  cat > "${resolv_tmp}" <<'EOF'
nameserver 119.29.29.29
EOF
  if [[ -L "${guest_rootfs_dir}/etc/resolv.conf" ]]; then
    remove_path_with_optional_sudo "${guest_rootfs_dir}/etc/resolv.conf"
  fi
  cp -f "${resolv_tmp}" "${guest_rootfs_dir}/etc/resolv.conf" 2>/dev/null || {
    require_cmd sudo
    sudo cp -f "${resolv_tmp}" "${guest_rootfs_dir}/etc/resolv.conf"
  }
}

build_guest_image_artifacts() {
  local output_img="$1"
  local output_version="$2"
  local rootfs_size_bytes
  local image_size_bytes
  local guest_container_id=""

  ensure_dir "${GUEST_IMAGE_CONTEXT_DIR}"
  ensure_file "${GUEST_IMAGE_DOCKERFILE}"

  mkdir -p "${GUEST_IMAGE_WORK_DIR}" "$(dirname "${output_img}")" "$(dirname "${output_version}")"
  remove_path_with_optional_sudo "${GUEST_ROOTFS_DIR}" "${GUEST_ROOTFS_TAR}"

  log "building guest image from ${GUEST_IMAGE_DOCKERFILE}"
  docker build -t "${GUEST_IMAGE_REF}" -f "${GUEST_IMAGE_DOCKERFILE}" "${GUEST_IMAGE_CONTEXT_DIR}" >&2

  guest_container_id="$(docker create "${GUEST_IMAGE_REF}")"
  trap 'if [[ -n "${guest_container_id:-}" ]]; then docker rm -f "${guest_container_id}" >/dev/null 2>&1 || true; fi' RETURN

  log "exporting guest rootfs from ${GUEST_IMAGE_REF}"
  docker export -o "${GUEST_ROOTFS_TAR}" "${guest_container_id}" >&2

  mkdir -p "${GUEST_ROOTFS_DIR}"
  tar -xf "${GUEST_ROOTFS_TAR}" -C "${GUEST_ROOTFS_DIR}"
  inject_agent_into_guest_rootfs "${GUEST_ROOTFS_DIR}"

  rootfs_size_bytes="$(directory_size_bytes "${GUEST_ROOTFS_DIR}")"
  image_size_bytes="$(calculate_guest_image_size_bytes "${rootfs_size_bytes}")"

  truncate -s "${image_size_bytes}" "${output_img}"
  run_mkfs_ext4_with_optional_sudo -F -d "${GUEST_ROOTFS_DIR}" "${output_img}" >&2

  printf '%s\n' "${GUEST_IMAGE_VERSION}" > "${output_version}"

  docker rm -f "${guest_container_id}" >/dev/null 2>&1 || true
  guest_container_id=""
  remove_path_with_optional_sudo "${GUEST_ROOTFS_DIR}" "${GUEST_ROOTFS_TAR}"
  trap - RETURN
}

require_cmd python3
require_cmd truncate
require_cmd ldd
require_cmd mkfs.ext4
require_cmd docker
require_cmd tar

ensure_kernel_vmlinux "${CUBE_KERNEL_VMLINUX}" "${RAW_ARTIFACTS_DIR}"
ensure_mkfs_ext4_supports_populate_dir

AGENT_BIN="$(build_cube_agent)"
CUBESHIM_BIN="$(build_cube_shim)"
CUBE_RUNTIME_BIN="$(build_cube_runtime)"

remove_path_with_optional_sudo "${RUNTIME_LAYOUT_DIR}" "${GUEST_IMAGE_WORK_DIR}"
mkdir -p \
  "${RUNTIME_LAYOUT_DIR}/cube-shim/bin" \
  "${RUNTIME_LAYOUT_DIR}/cube-shim/conf" \
  "${RUNTIME_LAYOUT_DIR}/cube-image" \
  "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf"

log "copying runtime binaries"
copy_file "${CUBESHIM_BIN}" "${RUNTIME_LAYOUT_DIR}/cube-shim/bin/containerd-shim-cube-rs"
copy_file "${CUBE_RUNTIME_BIN}" "${RUNTIME_LAYOUT_DIR}/cube-shim/bin/cube-runtime"
chmod +x "${RUNTIME_LAYOUT_DIR}/cube-shim/bin/containerd-shim-cube-rs" "${RUNTIME_LAYOUT_DIR}/cube-shim/bin/cube-runtime"
prepare_runtime_config "${RUNTIME_LAYOUT_DIR}/cube-shim/conf/config-cube.toml"

log "building guest image artifacts"
build_guest_image_artifacts \
  "${RUNTIME_LAYOUT_DIR}/cube-image/cube-guest-image-cpu.img" \
  "${RUNTIME_LAYOUT_DIR}/cube-image/version"
log "copying fixed kernel vmlinux"
copy_file "${CUBE_KERNEL_VMLINUX}" "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux"
ensure_file "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux"
if [[ -f "${CUBE_KERNEL_PVM_VMLINUX}" ]]; then
  log "copying PVM kernel vmlinux"
  copy_file "${CUBE_KERNEL_PVM_VMLINUX}" "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-pvm"
  ensure_file "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-pvm"
elif [[ -n "${ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX:-}" ]]; then
  die "PVM kernel vmlinux file not found: ${CUBE_KERNEL_PVM_VMLINUX}"
else
  log "PVM kernel vmlinux not found; packaging ordinary kernel only"
fi

log "runtime layout ready: ${RUNTIME_LAYOUT_DIR}"
