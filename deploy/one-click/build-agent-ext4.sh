#!/usr/bin/env bash
# Build independent cube-agent.ext4 (+ version) for virtio-pmem1 injection.
#
# Layout inside the ext4 image (open-source):
#   /cube-agent   # musl static binary only (no e2fsprogs)
#
# Usage:
#   OUTPUT_DIR=/path/to/cube-agent [ONE_CLICK_CUBE_AGENT_BIN=/path/to/cube-agent] \
#     CUBE_VERSION=v0.7.0 ./deploy/one-click/build-agent-ext4.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
load_build_env

WORK_ROOT="${ONE_CLICK_WORK_ROOT:-${SCRIPT_DIR}/.work}"
OUTPUT_DIR="${OUTPUT_DIR:-${ONE_CLICK_AGENT_EXT4_OUTPUT_DIR:-}}"
[[ -n "${OUTPUT_DIR}" ]] || die "OUTPUT_DIR (or ONE_CLICK_AGENT_EXT4_OUTPUT_DIR) is required"

LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
: "${CUBE_BUILD_TIME:=$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME

GUEST_IMAGE_WORK_DIR="${WORK_ROOT}/agent-ext4-build"
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

ensure_mkfs_ext4_supports_populate_dir

AGENT_BIN="$(build_cube_agent)"

remove_path_with_optional_sudo "${GUEST_IMAGE_WORK_DIR}"
mkdir -p "${OUTPUT_DIR}" "${GUEST_IMAGE_WORK_DIR}"

log "building cube-agent.ext4 into ${OUTPUT_DIR}"
build_agent_ext4_artifacts \
  "${AGENT_BIN}" \
  "${OUTPUT_DIR}/cube-agent.ext4" \
  "${OUTPUT_DIR}/version"

# Verify 2 MiB alignment.
img_size="$(stat -c '%s' "${OUTPUT_DIR}/cube-agent.ext4" 2>/dev/null || stat -f '%z' "${OUTPUT_DIR}/cube-agent.ext4")"
if (( img_size % (2 * 1024 * 1024) != 0 )); then
  die "cube-agent.ext4 size ${img_size} is not 2 MiB aligned"
fi

log "cube-agent artifacts ready: ${OUTPUT_DIR}"
