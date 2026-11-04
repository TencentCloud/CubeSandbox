#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# cube-jfs-mount.sh -- mount (or unmount) the shared JuiceFS volume that
# holds backend=jfs volumes and snapshots. Run by cube-sandbox-jfs.service.
#
#   cube-jfs-mount.sh start   run `juicefs mount` in a container
#   cube-jfs-mount.sh stop    unmount
#
# scripts/cube-jfs/init.sh creates the volume once on the control node. The
# scripts/cube-jfs/mount.sh wrapper enables this local mount on each node.
#
# Environment (from .one-click.env):
#   CUBE_JFS_IMAGE          JuiceFS mount image (default
#                           juicedata/mount:ce-v1.4.1)
#   CUBE_JFS_CONTAINER_NAME container name (default cube-sandbox-jfs)
#   CUBE_JFS_META_URL       optional metadata engine override. When empty, use
#                           ONE_CLICK_CONTROL_PLANE_IP on compute nodes or
#                           CUBE_SANDBOX_NODE_IP on the control node.
#   CUBE_JFS_META_PASSWORD  optional metadata password; passed to JuiceFS as
#                           META_PASSWORD instead of exposing it in the URL.
#   CUBE_JFS_MOUNT_PATH     mount point, must match [cow.jfs] mount_path
#                           (default /data/cubelet/jfs)
#   CUBE_JFS_CACHE_DIR      local block cache; put it on the node's NVMe
#                           (default /data/cubelet/jfs-cache)
#   CUBE_JFS_CACHE_SIZE_MB  cache size limit (default 102400)
#   CUBE_JFS_MOUNT_OPTS     extra `juicefs mount` options, e.g. --writeback
#
# Unless CUBE_JFS_MOUNT_OPTS sets them, the mount uses --prefetch 16 and
# --max-fuse-io 1M. A restored VM faults its memory in from many vCPU threads
# at once, and with a cold cache each fault is an object GET; the default
# single prefetch worker and 128K FUSE requests serialise those GETs.
# --max-fuse-io above 128K needs a kernel with FUSE max_pages (4.20+).
#
# The mount also uses --cache-large-write unless CUBE_JFS_MOUNT_OPTS sets it
# (--cache-large-write=false turns it off). JuiceFS keeps only partial blocks
# in the local cache after upload by default. Coalesced pause memory writes
# can fill whole blocks, so keeping them avoids fetching them back from
# object storage on a same-node restore.
#
# With --writeback a closed file may still have blocks staged under
# CUBE_JFS_CACHE_DIR, which another node cannot read yet. Cubelet reports a
# jfs package ready for cross-node restore only once none of its blocks is
# staged, so [cow.jfs] cache_dir must name the same directory.

set -euo pipefail

JFS_IMAGE="${CUBE_JFS_IMAGE:-juicedata/mount:ce-v1.4.1}"
CONTAINER_NAME="${CUBE_JFS_CONTAINER_NAME:-cube-sandbox-jfs}"
MOUNT_PATH="${CUBE_JFS_MOUNT_PATH:-/data/cubelet/jfs}"
CACHE_DIR="${CUBE_JFS_CACHE_DIR:-/data/cubelet/jfs-cache}"
CACHE_SIZE_MB="${CUBE_JFS_CACHE_SIZE_MB:-102400}"
EXTRA_OPTS="${CUBE_JFS_MOUNT_OPTS:-}"
READ_OPTS=""
[[ " ${EXTRA_OPTS} " == *" --prefetch"* ]] || READ_OPTS+=" --prefetch 16"
[[ " ${EXTRA_OPTS} " == *" --max-fuse-io"* ]] || READ_OPTS+=" --max-fuse-io 1M"
[[ " ${EXTRA_OPTS} " == *" --cache-large-write"* ]] || READ_OPTS+=" --cache-large-write"

log() { printf '[cube-jfs] %s\n' "$*"; }
die() { printf '[cube-jfs] ERROR: %s\n' "$*" >&2; exit 1; }

is_fuse_mount() {
  awk -v mp="${MOUNT_PATH}" '$2 == mp && $3 ~ /^fuse/ { found = 1 } END { exit !found }' /proc/mounts
}

# Docker can propagate a FUSE mount back to the host only when the bind source
# is a shared mount. Make the otherwise plain directory a self bind mount so
# we do not have to change propagation for all of /data or the root filesystem.
ensure_shared_mountpoint() {
  if ! mountpoint -q "${MOUNT_PATH}"; then
    mount --bind "${MOUNT_PATH}" "${MOUNT_PATH}"
  fi
  mount --make-shared "${MOUNT_PATH}"
}

do_start() {
  if [[ -z "${CUBE_JFS_META_URL:-}" ]]; then
    if [[ "${ONE_CLICK_DEPLOY_ROLE:-control}" == "compute" && -n "${ONE_CLICK_CONTROL_PLANE_IP:-}" ]]; then
      CUBE_JFS_META_URL="redis://${ONE_CLICK_CONTROL_PLANE_IP}:6380/0"
      log "using metadata URL derived from ONE_CLICK_CONTROL_PLANE_IP"
    elif [[ "${ONE_CLICK_DEPLOY_ROLE:-control}" != "compute" && -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
      CUBE_JFS_META_URL="redis://${CUBE_SANDBOX_NODE_IP}:6380/0"
      log "using metadata URL derived from CUBE_SANDBOX_NODE_IP"
    fi
  fi
  [[ -n "${CUBE_JFS_META_URL:-}" ]] || die "CUBE_JFS_META_URL is not set"
  if [[ -n "${CUBE_JFS_META_PASSWORD:-}" \
      && "${CUBE_JFS_META_URL}" =~ ^[A-Za-z][A-Za-z0-9+.-]*://[^/@]*:[^/@]+@ ]]; then
    die "do not set both CUBE_JFS_META_PASSWORD and a password in CUBE_JFS_META_URL"
  fi
  command -v docker >/dev/null 2>&1 || die "docker is not installed"
  [[ -c /dev/fuse ]] || die "/dev/fuse is not available"
  [[ "${CACHE_SIZE_MB}" =~ ^[0-9]+$ ]] || die "CUBE_JFS_CACHE_SIZE_MB must be an integer (got '${CACHE_SIZE_MB}')"

  mkdir -p "${MOUNT_PATH}" "${CACHE_DIR}"
  if is_fuse_mount; then
    # A stale mount from a crashed client would make `juicefs mount` fail.
    log "clearing previous mount at ${MOUNT_PATH}"
    fusermount -uz "${MOUNT_PATH}" 2>/dev/null || umount -l "${MOUNT_PATH}" 2>/dev/null || true
  fi
  ensure_shared_mountpoint

  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  log "mounting JuiceFS image ${JFS_IMAGE} at ${MOUNT_PATH} (cache ${CACHE_DIR}, ${CACHE_SIZE_MB} MiB)"
  local -a meta_env_args=()
  if [[ -n "${CUBE_JFS_META_PASSWORD:-}" ]]; then
    export META_PASSWORD="${CUBE_JFS_META_PASSWORD}"
    meta_env_args+=(--env META_PASSWORD)
  fi
  # shellcheck disable=SC2086 # READ_OPTS and EXTRA_OPTS are lists of options.
  exec docker run --rm --name "${CONTAINER_NAME}" \
    --privileged \
    "${meta_env_args[@]}" \
    --mount "type=bind,source=${MOUNT_PATH},target=${MOUNT_PATH},bind-propagation=rshared" \
    --mount "type=bind,source=${CACHE_DIR},target=${CACHE_DIR}" \
    --entrypoint juicefs \
    "${JFS_IMAGE}" mount \
    --no-usage-report \
    --cache-dir "${CACHE_DIR}" \
    --cache-size "${CACHE_SIZE_MB}" \
    ${READ_OPTS} \
    ${EXTRA_OPTS} \
    "${CUBE_JFS_META_URL}" "${MOUNT_PATH}"
}

do_stop() {
  if command -v docker >/dev/null 2>&1 && docker container inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
    log "stopping JuiceFS container ${CONTAINER_NAME}"
    docker stop --time 60 "${CONTAINER_NAME}" >/dev/null || true
  fi
  if is_fuse_mount; then
    log "detaching remaining mount at ${MOUNT_PATH}"
    # Busy (a sandbox still holds files open): detach now, the kernel
    # finishes once the last file is closed.
    fusermount -uz "${MOUNT_PATH}" 2>/dev/null || umount -l "${MOUNT_PATH}"
  fi
  # The propagated FUSE mount covered the self bind created at startup.
  mountpoint -q "${MOUNT_PATH}" && umount "${MOUNT_PATH}" || true
}

case "${1:-}" in
  start) do_start ;;
  stop)  do_stop ;;
  *)     die "usage: $0 start|stop" ;;
esac
