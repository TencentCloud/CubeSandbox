#!/usr/bin/env bash
# Build guest rootfs artifacts (cube-guest-image-cpu.img + version + agent-version).
#
# Usage:
#   OUTPUT_DIR=/path/to/cube-image [ONE_CLICK_CUBE_AGENT_BIN=/path/to/cube-agent] \
#     CUBE_VERSION=v0.6.0 ./deploy/one-click/build-guest-image.sh
#
# When ONE_CLICK_CUBE_AGENT_BIN is unset, cube-agent is built locally (or via
# ONE_CLICK_CUBE_AGENT_BUILD_MODE=docker).
#
# When ONE_CLICK_GUEST_IMAGE_TAR is set to a cube-guest-image-*.tar.gz (same
# layout as the Release / docker asset), extract it into OUTPUT_DIR and skip
# the local docker/mkfs rebuild so CI can reuse the published guest image.
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
OUTPUT_DIR="${OUTPUT_DIR:-${ONE_CLICK_GUEST_IMAGE_OUTPUT_DIR:-}}"
[[ -n "${OUTPUT_DIR}" ]] || die "OUTPUT_DIR (or ONE_CLICK_GUEST_IMAGE_OUTPUT_DIR) is required"

install_prebuilt_guest_image_tar() {
  local archive="${1:?archive path is required}"
  local required=(
    cube-guest-image-cpu.img
    version
    agent-version
  )
  local name

  [[ -f "${archive}" ]] || die "prebuilt guest image archive not found: ${archive}"
  require_cmd tar

  if [[ -e "${OUTPUT_DIR}" ]]; then
    if [[ "${EUID}" -eq 0 ]]; then
      rm -rf "${OUTPUT_DIR}"
    else
      rm -rf "${OUTPUT_DIR}" 2>/dev/null || {
        require_cmd sudo
        sudo rm -rf "${OUTPUT_DIR}"
      }
    fi
  fi
  mkdir -p "${OUTPUT_DIR}"
  log "using prebuilt guest image archive ${archive}"
  tar -xzf "${archive}" -C "${OUTPUT_DIR}"

  for name in "${required[@]}"; do
    ensure_file "${OUTPUT_DIR}/${name}"
  done

  log "guest image artifacts ready from archive: ${OUTPUT_DIR}"
}

if [[ -n "${ONE_CLICK_GUEST_IMAGE_TAR:-}" ]]; then
  install_prebuilt_guest_image_tar "${ONE_CLICK_GUEST_IMAGE_TAR}"
  exit 0
fi

LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
: "${CUBE_BUILD_TIME:=$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME

GUEST_IMAGE_WORK_DIR="${WORK_ROOT}/guest-image-build"
GUEST_ROOTFS_DIR="${GUEST_IMAGE_WORK_DIR}/rootfs"
GUEST_ROOTFS_TAR="${GUEST_IMAGE_WORK_DIR}/rootfs.tar"

GUEST_IMAGE_DOCKERFILE="${ONE_CLICK_GUEST_IMAGE_DOCKERFILE:-${ROOT_DIR}/deploy/guest-image/Dockerfile}"
GUEST_IMAGE_CONTEXT_DIR="${ONE_CLICK_GUEST_IMAGE_CONTEXT_DIR:-$(dirname "${GUEST_IMAGE_DOCKERFILE}")}"
GUEST_IMAGE_REF="${ONE_CLICK_GUEST_IMAGE_REF:-cube-sandbox-guest-image:one-click}"
GUEST_IMAGE_VERSION="${ONE_CLICK_GUEST_IMAGE_VERSION:-${CUBE_VERSION:-${LATEST_RELEASE_TAG:-$(latest_git_revision "${ROOT_DIR}")}}}"

CUBE_AGENT_BUILD_MODE="${ONE_CLICK_CUBE_AGENT_BUILD_MODE:-local}"
CUBE_AGENT_BIN_OVERRIDE="${ONE_CLICK_CUBE_AGENT_BIN:-}"

# shellcheck source=./lib/guest-image.sh
source "${SCRIPT_DIR}/lib/guest-image.sh"

require_cmd python3
require_cmd truncate
require_cmd ldd
require_cmd mkfs.ext4
require_cmd e2fsck
require_cmd resize2fs
require_cmd dumpe2fs
require_cmd docker
require_cmd tar

ensure_mkfs_ext4_supports_populate_dir

AGENT_BIN="$(build_cube_agent)"

remove_path_with_optional_sudo "${GUEST_IMAGE_WORK_DIR}"
mkdir -p "${OUTPUT_DIR}"

log "building guest image artifacts into ${OUTPUT_DIR}"
build_guest_image_artifacts \
  "${OUTPUT_DIR}/cube-guest-image-cpu.img" \
  "${OUTPUT_DIR}/version" \
  "${OUTPUT_DIR}/agent-version"

log "guest image artifacts ready: ${OUTPUT_DIR}"
