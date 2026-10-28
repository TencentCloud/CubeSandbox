#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
# Initialize the shared JuiceFS volume. Run once on the control node.

set -euo pipefail
PREFIX="${CUBE_INSTALL_PREFIX:-/usr/local/services/cubetoolbox}"
ENV_FILE="${PREFIX}/.one-click.env"

log() { printf '[cube-jfs-init] %s\n' "$*"; }
die() { printf '[cube-jfs-init] ERROR: %s\n' "$*" >&2; exit 1; }
[[ "$(id -u)" -eq 0 ]] || die "run as root"
[[ -f "${ENV_FILE}" ]] || die "missing ${ENV_FILE}"
set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a
[[ "${ONE_CLICK_DEPLOY_ROLE:-control}" != "compute" ]] || die "run this script on the control node"
[[ "${ONE_CLICK_ENABLE_JFS:-0}" == "1" ]] || die "set ONE_CLICK_ENABLE_JFS=1 in ${ENV_FILE}"
command -v docker >/dev/null 2>&1 || die "docker is not installed"

if [[ -z "${CUBE_JFS_META_URL:-}" && -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  CUBE_JFS_META_URL="redis://${CUBE_SANDBOX_NODE_IP}:6380/0"
  log "using metadata URL derived from CUBE_SANDBOX_NODE_IP"
fi
if [[ -z "${CUBE_JFS_BUCKET:-}" && "${CUBE_SANDBOX_MINIO_ENABLED:-0}" == "1" \
    && -n "${CUBE_S3_ENDPOINT:-}" ]]; then
  CUBE_JFS_BUCKET="${CUBE_S3_ENDPOINT%/}/cube-jfs"
  log "using bucket derived from bundled MinIO endpoint"
fi

start_local_redis_if_configured() {
  local meta_url="$1"
  local redis_password redis_host redis_port
  # For the documented local deployment, init.sh owns a dedicated Redis on
  # the control-node IP. Other metadata URLs are treated as externally managed.
  if [[ "${meta_url}" =~ ^redis://:([^@]+)@([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+):([0-9]+)/[0-9]+$ ]]; then
    redis_password="${BASH_REMATCH[1]}"
    redis_host="${BASH_REMATCH[2]}"
    redis_port="${BASH_REMATCH[3]}"
  elif [[ "${meta_url}" =~ ^redis://([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+):([0-9]+)/[0-9]+$ \
      && -n "${CUBE_JFS_META_PASSWORD:-}" ]]; then
    redis_password="${CUBE_JFS_META_PASSWORD}"
    redis_host="${BASH_REMATCH[1]}"
    redis_port="${BASH_REMATCH[2]}"
  else
    return 0
  fi
  if ! ip -o -4 addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -qxF "${redis_host}"; then
    return 0
  fi

  local redis_container="cube-sandbox-jfs-redis"
  local redis_volume="cube-sandbox-jfs-redis-data"
  local redis_image="${CUBE_SANDBOX_REDIS_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine}"
  docker volume create "${redis_volume}" >/dev/null
  if docker container inspect "${redis_container}" >/dev/null 2>&1; then
    docker start "${redis_container}" >/dev/null
  else
    log "starting dedicated JuiceFS Redis on ${redis_host}:${redis_port}"
    REDIS_PASSWORD="${redis_password}" docker run -d --restart unless-stopped --name "${redis_container}" \
      --env REDIS_PASSWORD \
      -p "${redis_host}:${redis_port}:6379" \
      -v "${redis_volume}:/data" \
      "${redis_image}" sh -c 'exec redis-server --appendonly yes --requirepass "$REDIS_PASSWORD"' >/dev/null
  fi
  for _ in $(seq 1 30); do
    docker exec "${redis_container}" redis-cli -a "${redis_password}" ping 2>/dev/null | grep -qx PONG && return 0
    sleep 1
  done
  die "dedicated JuiceFS Redis did not become ready; check docker logs ${redis_container}"
}

image="${CUBE_JFS_IMAGE:-juicedata/mount:ce-v1.4.1}"
name="${CUBE_JFS_VOLUME_NAME:-cube}"
storage="${CUBE_JFS_STORAGE:-s3}"
bucket="${CUBE_JFS_BUCKET:-}"
access_key="${CUBE_JFS_ACCESS_KEY:-${CUBE_S3_ACCESS_KEY_ID:-}}"
secret_key="${CUBE_JFS_SECRET_KEY:-${CUBE_S3_SECRET_ACCESS_KEY:-}}"
: "${CUBE_JFS_META_URL:?set CUBE_JFS_META_URL in ${ENV_FILE}}"
if [[ -n "${CUBE_JFS_META_PASSWORD:-}" \
    && "${CUBE_JFS_META_URL}" =~ ^[A-Za-z][A-Za-z0-9+.-]*://[^/@]*:[^/@]+@ ]]; then
  die "do not set both CUBE_JFS_META_PASSWORD and a password in CUBE_JFS_META_URL"
fi
[[ -n "${bucket}" ]] || die "set CUBE_JFS_BUCKET in ${ENV_FILE}"
if [[ -n "${access_key}" && -z "${secret_key}" ]] || [[ -z "${access_key}" && -n "${secret_key}" ]]; then
  die "object-storage access key and secret key must be set together"
fi
start_local_redis_if_configured "${CUBE_JFS_META_URL}"
log "initializing ${name} when absent; an existing volume is not updated"
meta_env_args=()
if [[ -n "${CUBE_JFS_META_PASSWORD:-}" ]]; then
  export META_PASSWORD="${CUBE_JFS_META_PASSWORD}"
  meta_env_args+=(--env META_PASSWORD)
fi
ACCESS_KEY="${access_key}" SECRET_KEY="${secret_key}" \
docker run --rm --env ACCESS_KEY --env SECRET_KEY "${meta_env_args[@]}" --entrypoint juicefs "${image}" format \
  --no-update \
  --storage "${storage}" \
  --bucket "${bucket}" \
  "${CUBE_JFS_META_URL}" "${name}"
docker run --rm "${meta_env_args[@]}" --entrypoint juicefs "${image}" status "${CUBE_JFS_META_URL}" >/dev/null
log "JuiceFS volume is ready"
