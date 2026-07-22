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

LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"

# Version injection for Rust build.rs (shim, cube-runtime) when built on host.
# In CI these are prebuilt via the builder container; for local dev, provide
# consistent fallbacks so all components share the same version information.
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
: "${CUBE_BUILD_TIME:=$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME
RAW_ARTIFACTS_DIR="${SCRIPT_DIR}/assets/kernel-artifacts"

CUBE_KERNEL_VMLINUX="${ONE_CLICK_CUBE_KERNEL_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux}"
CUBE_KERNEL_PVM_VMLINUX="${ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX:-${RAW_ARTIFACTS_DIR}/vmlinux-pvm}"

CUBE_SHIM_BUILD_MODE="${ONE_CLICK_CUBE_SHIM_BUILD_MODE:-local}"

CUBESHIM_BIN_OVERRIDE="${ONE_CLICK_CUBESHIM_BIN:-}"
CUBE_RUNTIME_BIN_OVERRIDE="${ONE_CLICK_CUBE_RUNTIME_BIN:-}"
RUNTIME_CFG_OVERRIDE="${ONE_CLICK_RUNTIME_CFG_SRC:-}"
CUBE_SHIM_WORKSPACE_READY=0

# find_built_binary / remove_path_with_optional_sudo (guest build lives in build-guest-image.sh)
# shellcheck source=./lib/guest-image.sh
source "${SCRIPT_DIR}/lib/guest-image.sh"

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

require_cmd python3

ensure_kernel_vmlinux "${CUBE_KERNEL_VMLINUX}" "${RAW_ARTIFACTS_DIR}"

CUBESHIM_BIN="$(build_cube_shim)"
CUBE_RUNTIME_BIN="$(build_cube_runtime)"

remove_path_with_optional_sudo "${RUNTIME_LAYOUT_DIR}"
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

log "building guest image artifacts via build-guest-image.sh"
OUTPUT_DIR="${RUNTIME_LAYOUT_DIR}/cube-image" \
  "${SCRIPT_DIR}/build-guest-image.sh"
log "copying ordinary guest kernel vmlinux"
copy_file "${CUBE_KERNEL_VMLINUX}" "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-bm"
ensure_file "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux-bm"
ln -sfn "vmlinux-bm" "${RUNTIME_LAYOUT_DIR}/cube-kernel-scf/vmlinux"
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
