#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Bring up cube-lifecycle-manager on the control node. This is a required
# component of the one-click stack; skipping it leaves paused sandboxes
# permanently unreachable.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./cube-lifecycle-manager-compose-lib.sh
source "${SCRIPT_DIR}/cube-lifecycle-manager-compose-lib.sh"

require_root
require_cmd docker
require_cmd sed
require_cmd ss

CUBE_LCM_ENABLE="${CUBE_LCM_ENABLE:-1}"
[[ "${CUBE_LCM_ENABLE}" == "1" ]] \
  || die "CUBE_LCM_ENABLE must be 1; cube-lifecycle-manager is required in one-click deployment"

COMPOSE_TEMPLATE="${CUBE_LCM_DIR}/docker-compose.yaml.template"
COMPOSE_FILE="${CUBE_LCM_DIR}/docker-compose.yaml"

# Image resolution priority mirrors up-cube-egress.sh:
#   1. CUBE_SANDBOX_CUBE_LCM_IMAGE (explicit operator override)
#   2. MIRROR=cn  → cn.tencentcloudcr.com (China-region pull-through)
#   3. default    → int.tencentcloudcr.com (overseas/international)
#
# The image is published by cube-lifecycle-manager/Makefile's `make push`
# to both registries under the same :v0.5.1-rc5 tag; either default resolves
# to whatever the operator most recently published.
CUBE_LCM_IMAGE_INT_DEFAULT="cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:v0.5.1-rc5"
CUBE_LCM_IMAGE_CN_DEFAULT="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:v0.5.1-rc5"
if [[ -n "${CUBE_SANDBOX_CUBE_LCM_IMAGE:-}" ]]; then
  CUBE_LCM_IMAGE="${CUBE_SANDBOX_CUBE_LCM_IMAGE}"
elif [[ "${MIRROR:-}" == "cn" ]]; then
  CUBE_LCM_IMAGE="${CUBE_LCM_IMAGE_CN_DEFAULT}"
else
  CUBE_LCM_IMAGE="${CUBE_LCM_IMAGE_INT_DEFAULT}"
fi
CUBE_LCM_CONTAINER_NAME="${CUBE_LCM_CONTAINER_NAME:-cube-lifecycle-manager}"

CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
[[ -n "${CUBE_SANDBOX_NODE_IP}" ]] \
  || die "CUBE_SANDBOX_NODE_IP is required for cube-lifecycle-manager"

# Shared Redis with CubeMaster; the same knobs CubeProxy already reads.
CUBE_LCM_REDIS_HOST="${CUBE_LCM_REDIS_HOST:-${CUBE_PROXY_REDIS_IP:-127.0.0.1}}"
CUBE_LCM_REDIS_PORT="${CUBE_LCM_REDIS_PORT:-${CUBE_PROXY_REDIS_PORT:-${CUBE_SANDBOX_REDIS_PORT:-6379}}}"
CUBE_LCM_REDIS_ADDR="${CUBE_LCM_REDIS_ADDR:-${CUBE_LCM_REDIS_HOST}:${CUBE_LCM_REDIS_PORT}}"
CUBE_LCM_REDIS_PASSWORD="${CUBE_LCM_REDIS_PASSWORD:-${CUBE_PROXY_REDIS_PASSWORD:-${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}}}"
CUBE_LCM_REDIS_DB="${CUBE_LCM_REDIS_DB:-0}"

# CubeMaster HTTP for pause/resume RPCs. CLM must be able to reach it, so if
# the operator has narrowed CUBEMASTER_HTTP_BIND to 127.0.0.1 we bail out
# early with a diagnosable error rather than surface an opaque "connection
# refused" from CLM's first pause attempt.
CUBEMASTER_HTTP_BIND="${CUBEMASTER_HTTP_BIND:-}"
if [[ -n "${CUBEMASTER_HTTP_BIND}" && "${CUBEMASTER_HTTP_BIND}" == "127.0.0.1" ]]; then
  die "CUBEMASTER_HTTP_BIND=127.0.0.1 is incompatible with cube-lifecycle-manager; unset it or bind CubeMaster to the node IP"
fi
CUBE_LCM_CUBEMASTER_URL="${CUBE_LCM_CUBEMASTER_URL:-http://${CUBE_SANDBOX_NODE_IP}:8089}"

# CLM listens on host network so CubeProxy on other nodes can reach it.
CUBE_LCM_LISTEN_ADDR="${CUBE_LCM_LISTEN_ADDR:-0.0.0.0:8083}"
CUBE_LCM_DEFAULT_IDLE_TIMEOUT="${CUBE_LCM_DEFAULT_IDLE_TIMEOUT:-5m}"
CUBE_LCM_HEARTBEAT_TTL="${CUBE_LCM_HEARTBEAT_TTL:-15s}"
CUBE_LCM_DISCOVERY_REFRESH="${CUBE_LCM_DISCOVERY_REFRESH:-3s}"

# Shared token: same variable up-cube-proxy.sh consumes, so both sides agree.
CUBE_ADMIN_TOKEN="${CUBE_ADMIN_TOKEN:-}"

COMPOSE_DETACH="${ONE_CLICK_COMPOSE_DETACH:-1}"
PREPARE_ONLY="${ONE_CLICK_PREPARE_ONLY:-0}"

ensure_dir "${CUBE_LCM_DIR}"
ensure_file "${COMPOSE_TEMPLATE}"
case "${COMPOSE_DETACH}" in
  0|1) ;;
  *) die "unsupported ONE_CLICK_COMPOSE_DETACH: ${COMPOSE_DETACH} (expected 0 or 1)" ;;
esac

render_template_atomic \
  "${COMPOSE_TEMPLATE}" \
  "${COMPOSE_FILE}" \
  -e "s#__CUBE_LCM_IMAGE__#$(escape_sed "${CUBE_LCM_IMAGE}" '#')#g" \
  -e "s#__CUBE_LCM_CONTAINER_NAME__#$(escape_sed "${CUBE_LCM_CONTAINER_NAME}" '#')#g" \
  -e "s#__CUBE_LCM_REDIS_ADDR__#$(escape_sed "${CUBE_LCM_REDIS_ADDR}" '#')#g" \
  -e "s#__CUBE_LCM_REDIS_PASSWORD__#$(escape_sed "${CUBE_LCM_REDIS_PASSWORD}" '#')#g" \
  -e "s#__CUBE_LCM_REDIS_DB__#$(escape_sed "${CUBE_LCM_REDIS_DB}" '#')#g" \
  -e "s#__CUBE_LCM_CUBEMASTER_URL__#$(escape_sed "${CUBE_LCM_CUBEMASTER_URL}" '#')#g" \
  -e "s#__CUBE_LCM_LISTEN_ADDR__#$(escape_sed "${CUBE_LCM_LISTEN_ADDR}" '#')#g" \
  -e "s#__CUBE_LCM_DEFAULT_IDLE_TIMEOUT__#$(escape_sed "${CUBE_LCM_DEFAULT_IDLE_TIMEOUT}" '#')#g" \
  -e "s#__CUBE_LCM_HEARTBEAT_TTL__#$(escape_sed "${CUBE_LCM_HEARTBEAT_TTL}" '#')#g" \
  -e "s#__CUBE_LCM_DISCOVERY_REFRESH__#$(escape_sed "${CUBE_LCM_DISCOVERY_REFRESH}" '#')#g" \
  -e "s#__CUBE_ADMIN_TOKEN__#$(escape_sed "${CUBE_ADMIN_TOKEN}" '#')#g"

if [[ "${PREPARE_ONLY}" == "1" ]]; then
  log "cube-lifecycle-manager runtime files prepared under ${CUBE_LCM_DIR}"
  exit 0
fi

cube_lcm_compose_run down --remove-orphans >/dev/null 2>&1 || true
docker_rm_if_exists "${CUBE_LCM_CONTAINER_NAME}"

# LISTEN_ADDR has "host:port" form; extract the port for the pre-flight check.
CUBE_LCM_LISTEN_PORT="${CUBE_LCM_LISTEN_ADDR##*:}"
if command_output_contains_fixed_string "LISTEN" ss -lnt "( sport = :${CUBE_LCM_LISTEN_PORT} )"; then
  die "port ${CUBE_LCM_LISTEN_PORT} is already in use; cube-lifecycle-manager uses host networking and requires it to be free"
fi

# Pull explicitly so the failure mode (registry unreachable / not logged in /
# image not pushed yet) surfaces here instead of midway through `compose up`.
# If the image already exists locally and the registry is unreachable, fall
# back to the cached copy (airgap / CUBE_SANDBOX_CUBE_LCM_IMAGE preload).
log "pulling ${CUBE_LCM_IMAGE}"
if ! docker pull "${CUBE_LCM_IMAGE}" >/dev/null; then
  if docker_image_exists "${CUBE_LCM_IMAGE}"; then
    log "WARN: pull failed but local image exists; using cached copy"
  else
    die "pull failed for ${CUBE_LCM_IMAGE} and no local copy is cached; check registry reachability and credentials (or set MIRROR=cn / CUBE_SANDBOX_CUBE_LCM_IMAGE)"
  fi
fi

if [[ "${COMPOSE_DETACH}" == "0" ]]; then
  cube_lcm_compose_run up cube-lifecycle-manager
  exit $?
fi

cube_lcm_compose_run up -d cube-lifecycle-manager

for _ in {1..40}; do
  state="$(docker inspect --format '{{.State.Status}}' "${CUBE_LCM_CONTAINER_NAME}" 2>/dev/null || true)"
  if [[ "${state}" == "running" ]]; then
    break
  fi
  sleep 2
done
[[ "${state:-}" == "running" ]] || die "cube-lifecycle-manager container failed to start"

for _ in {1..30}; do
  if command_output_contains_fixed_string "LISTEN" ss -lnt "( sport = :${CUBE_LCM_LISTEN_PORT} )"; then
    log "cube-lifecycle-manager listening on ${CUBE_LCM_LISTEN_PORT}"
    exit 0
  fi
  sleep 2
done

die "cube-lifecycle-manager port ${CUBE_LCM_LISTEN_PORT} did not become ready"
