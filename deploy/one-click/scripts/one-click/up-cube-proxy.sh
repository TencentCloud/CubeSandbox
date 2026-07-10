#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Bring up cube-proxy on the control node from a pre-published image
# (mirrors up-cube-lifecycle-manager.sh / cube-egress).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./compose-lib.sh
source "${SCRIPT_DIR}/compose-lib.sh"

require_root
require_cmd docker
require_cmd sed
require_cmd ss

CUBE_PROXY_ENABLE="${CUBE_PROXY_ENABLE:-1}"
[[ "${CUBE_PROXY_ENABLE}" == "1" ]] || die "CUBE_PROXY_ENABLE must be 1; cube proxy is required in one-click deployment"

PROXY_DIR="${TOOLBOX_ROOT}/cubeproxy"
CUBE_PROXY_CERT_DIR="${CUBE_PROXY_CERT_DIR:-${PROXY_DIR}/certs}"
CERT_DIR="${CUBE_PROXY_CERT_DIR}"
GLOBAL_TEMPLATE="${PROXY_DIR}/global.conf.template"
GLOBAL_CONF="${PROXY_DIR}/global.conf"
COMPOSE_TEMPLATE="${PROXY_DIR}/docker-compose.yaml.template"
COMPOSE_FILE="${PROXY_DIR}/docker-compose.yaml"

# Image resolution priority mirrors up-cube-lifecycle-manager.sh:
#   1. CUBE_SANDBOX_CUBE_PROXY_IMAGE (explicit operator override)
#   2. MIRROR=cn  → cn.tencentcloudcr.com (China-region pull-through)
#   3. default    → int.tencentcloudcr.com (overseas/international)
#
# The image is published by CubeProxy/Makefile's `make push` to both
# registries under the same :v0.5.1-rc11 tag; either default resolves
# to whatever the operator most recently published.
#
# Compatibility: CUBE_PROXY_IMAGE_TAG is deprecated (local compose build era).
# A non-default leftover value is honored once with a warning; the old default
# cube-proxy:one-click is ignored so upgrades adopt the pre-published image.
CUBE_PROXY_IMAGE_INT_DEFAULT="cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-proxy:v0.5.1-rc11"
CUBE_PROXY_IMAGE_CN_DEFAULT="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-proxy:v0.5.1-rc11"
if [[ -z "${CUBE_SANDBOX_CUBE_PROXY_IMAGE:-}" && -n "${CUBE_PROXY_IMAGE_TAG:-}" ]]; then
  if [[ "${CUBE_PROXY_IMAGE_TAG}" == "cube-proxy:one-click" ]]; then
    log "CUBE_PROXY_IMAGE_TAG=cube-proxy:one-click is deprecated and ignored; set CUBE_SANDBOX_CUBE_PROXY_IMAGE or MIRROR to select the pre-published cube-proxy image"
  else
    log "CUBE_PROXY_IMAGE_TAG is deprecated; using ${CUBE_PROXY_IMAGE_TAG}. Set CUBE_SANDBOX_CUBE_PROXY_IMAGE instead"
    CUBE_SANDBOX_CUBE_PROXY_IMAGE="${CUBE_PROXY_IMAGE_TAG}"
  fi
fi
if [[ -n "${CUBE_SANDBOX_CUBE_PROXY_IMAGE:-}" ]]; then
  CUBE_PROXY_IMAGE="${CUBE_SANDBOX_CUBE_PROXY_IMAGE}"
elif [[ "${MIRROR:-}" == "cn" ]]; then
  CUBE_PROXY_IMAGE="${CUBE_PROXY_IMAGE_CN_DEFAULT}"
else
  CUBE_PROXY_IMAGE="${CUBE_PROXY_IMAGE_INT_DEFAULT}"
fi
CUBE_PROXY_CONTAINER_NAME="${CUBE_PROXY_CONTAINER_NAME:-cube-proxy}"
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
CUBE_PROXY_REDIS_IP="${CUBE_PROXY_REDIS_IP:-127.0.0.1}"
CUBE_PROXY_REDIS_PORT="${CUBE_PROXY_REDIS_PORT:-${CUBE_SANDBOX_REDIS_PORT:-6379}}"
CUBE_PROXY_REDIS_PASSWORD="${CUBE_PROXY_REDIS_PASSWORD:-${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}}"
CUBE_PROXY_HTTPS_PORT="${CUBE_PROXY_HTTPS_PORT:-443}"
CUBE_PROXY_HTTP_PORT="${CUBE_PROXY_HTTP_PORT:-80}"
CUBE_PROXY_SSL_CERT="${CUBE_PROXY_SSL_CERT:-cube.app+3.pem}"
CUBE_PROXY_SSL_KEY="${CUBE_PROXY_SSL_KEY:-cube.app+3-key.pem}"
# Address the /admin/* server (port 8082) binds to. Defaults to the node's
# cluster-reachable IP so cube-lifecycle-manager (running on the control
# node) can push meta / state updates here. Token auth via
# $cube_admin_token below prevents outside abuse; operators may further
# tighten this with iptables / security groups.
CUBE_PROXY_ADMIN_LISTEN="${CUBE_PROXY_ADMIN_LISTEN:-${CUBE_SANDBOX_NODE_IP}}"

# Address of cube-lifecycle-manager reachable from this cube-proxy replica.
# Consumed by the Lua $cube_sidecar_addr variable used by the
# /_sidecar_resume internal sub-location. In the single-control-node
# deployment CLM lives on the same node as cube-proxy (both use host
# networking), so the default points at the local node IP. Override when
# CLM is elsewhere on the intra-cluster network.
CUBE_LCM_HOST="${CUBE_LCM_HOST:-${CUBE_SANDBOX_NODE_IP}}"
CUBE_LCM_PORT="${CUBE_LCM_PORT:-8083}"
CUBE_SIDECAR_NGX_ADDR="${CUBE_SIDECAR_NGX_ADDR:-${CUBE_LCM_HOST}:${CUBE_LCM_PORT}}"

# Optional shared secret accepted by CubeProxy's /admin/* endpoints. When
# admin listens on the node IP (default), setting this to a non-empty
# value gates loopback-bypass abuse; leave empty for single-host dev.
CUBE_ADMIN_TOKEN="${CUBE_ADMIN_TOKEN:-}"

# ── CubeProxy service registration for cube-lifecycle-manager discovery ──
# Enabled by default now that CLM owns lifecycle coordination.
CUBE_PROXY_REGISTRY_ENABLE="${CUBE_PROXY_REGISTRY_ENABLE:-1}"
CUBE_PROXY_ID="${CUBE_PROXY_ID:-${CUBE_SANDBOX_NODE_IP}:8082}"
CUBE_PROXY_ADMIN_URL="${CUBE_PROXY_ADMIN_URL:-http://${CUBE_SANDBOX_NODE_IP}:8082}"
CUBE_PROXY_RESUME_URL="${CUBE_PROXY_RESUME_URL:-${CUBE_PROXY_ADMIN_URL}}"
CUBE_PROXY_NODE_IP="${CUBE_PROXY_NODE_IP:-${CUBE_SANDBOX_NODE_IP}}"
CUBE_PROXY_VERSION="${CUBE_PROXY_VERSION:-}"
CUBE_PROXY_HEARTBEAT_INTERVAL_MS="${CUBE_PROXY_HEARTBEAT_INTERVAL_MS:-5000}"
CUBE_PROXY_REGISTRY_REDIS_HOST="${CUBE_PROXY_REGISTRY_REDIS_HOST:-${CUBE_PROXY_REDIS_IP}}"
CUBE_PROXY_REGISTRY_REDIS_PORT="${CUBE_PROXY_REGISTRY_REDIS_PORT:-${CUBE_PROXY_REDIS_PORT}}"
CUBE_PROXY_REGISTRY_REDIS_PASSWORD="${CUBE_PROXY_REGISTRY_REDIS_PASSWORD:-${CUBE_PROXY_REDIS_PASSWORD}}"
CUBE_PROXY_REGISTRY_REDIS_DB="${CUBE_PROXY_REGISTRY_REDIS_DB:-0}"
COMPOSE_DETACH="${ONE_CLICK_COMPOSE_DETACH:-1}"
MKCERT_BUNDLED_BIN="${TOOLBOX_ROOT}/support/bin/mkcert"
PREPARE_ONLY="${ONE_CLICK_PREPARE_ONLY:-0}"

ensure_dir "${PROXY_DIR}"
mkdir -p "${CERT_DIR}"
ensure_file "${GLOBAL_TEMPLATE}"
ensure_file "${COMPOSE_TEMPLATE}"
[[ -n "${CUBE_SANDBOX_NODE_IP}" ]] || die "CUBE_SANDBOX_NODE_IP is required for cube proxy"
case "${COMPOSE_DETACH}" in
  0|1) ;;
  *) die "unsupported ONE_CLICK_COMPOSE_DETACH: ${COMPOSE_DETACH} (expected 0 or 1)" ;;
esac

install_mkcert() {
  if command -v mkcert >/dev/null 2>&1; then
    return 0
  fi

  local target="/usr/local/bin/mkcert"
  if [[ -x "${MKCERT_BUNDLED_BIN}" ]]; then
    install -m 0755 "${MKCERT_BUNDLED_BIN}" "${target}"
  else
    die "mkcert not found in PATH or bundled location (${MKCERT_BUNDLED_BIN})"
  fi

  command -v mkcert >/dev/null 2>&1 || die "failed to install mkcert from bundled binary"
}

prepare_proxy_certs() {
  mkdir -p "${CERT_DIR}"
  if [[ -f "${CERT_DIR}/${CUBE_PROXY_SSL_CERT}" && -f "${CERT_DIR}/${CUBE_PROXY_SSL_KEY}" ]]; then
    return 0
  fi

  # Only auto-generate when using mkcert's default file naming. If the user
  # overrode CUBE_PROXY_SSL_CERT/KEY, they are expected to provision the cert
  # files themselves under CERT_DIR.
  if [[ "${CUBE_PROXY_SSL_CERT}" != "cube.app+3.pem" || "${CUBE_PROXY_SSL_KEY}" != "cube.app+3-key.pem" ]]; then
    die "TLS cert/key not found at ${CERT_DIR}/${CUBE_PROXY_SSL_CERT} or ${CERT_DIR}/${CUBE_PROXY_SSL_KEY}; place them manually when overriding CUBE_PROXY_SSL_CERT/KEY"
  fi

  install_mkcert
  (
    cd "${CERT_DIR}"
    mkcert -install
    mkcert cube.app "*.cube.app" localhost 127.0.0.1
  ) >&2
}

prepare_proxy_certs

render_template_atomic \
  "${GLOBAL_TEMPLATE}" \
  "${GLOBAL_CONF}" \
  -e "s/__CUBE_PROXY_REDIS_IP__/$(escape_sed "${CUBE_PROXY_REDIS_IP}")/g" \
  -e "s/__CUBE_PROXY_REDIS_PORT__/$(escape_sed "${CUBE_PROXY_REDIS_PORT}")/g" \
  -e "s/__CUBE_PROXY_REDIS_PASSWORD__/$(escape_sed "${CUBE_PROXY_REDIS_PASSWORD}")/g" \
  -e "s/__CUBE_PROXY_HOST_IP__/$(escape_sed "${CUBE_SANDBOX_NODE_IP}")/g" \
  -e "s#__CUBE_SIDECAR_NGX_ADDR__#$(escape_sed "${CUBE_SIDECAR_NGX_ADDR}" '#')#g" \
  -e "s#__CUBE_ADMIN_TOKEN__#$(escape_sed "${CUBE_ADMIN_TOKEN}" '#')#g"

NGINX_TEMPLATE="${PROXY_DIR}/nginx.conf.template"
NGINX_CONF="${PROXY_DIR}/nginx.conf"
ensure_file "${NGINX_TEMPLATE}"
render_template_atomic \
  "${NGINX_TEMPLATE}" \
  "${NGINX_CONF}" \
  -e "s/__CUBE_PROXY_HTTPS_PORT__/$(escape_sed "${CUBE_PROXY_HTTPS_PORT}")/g" \
  -e "s/__CUBE_PROXY_HTTP_PORT__/$(escape_sed "${CUBE_PROXY_HTTP_PORT}")/g" \
  -e "s/__CUBE_PROXY_ADMIN_LISTEN__/$(escape_sed "${CUBE_PROXY_ADMIN_LISTEN}")/g" \
  -e "s/__CUBE_PROXY_SSL_CERT__/$(escape_sed "${CUBE_PROXY_SSL_CERT}")/g" \
  -e "s/__CUBE_PROXY_SSL_KEY__/$(escape_sed "${CUBE_PROXY_SSL_KEY}")/g"

render_template_atomic \
  "${COMPOSE_TEMPLATE}" \
  "${COMPOSE_FILE}" \
  -e "s#__CUBE_PROXY_IMAGE__#$(escape_sed "${CUBE_PROXY_IMAGE}" '#')#g" \
  -e "s#__CUBE_PROXY_CONTAINER_NAME__#$(escape_sed "${CUBE_PROXY_CONTAINER_NAME}" '#')#g" \
  -e "s#__CUBE_PROXY_CERT_DIR__#$(escape_sed "${CERT_DIR}" '#')#g" \
  -e "s#__CUBE_PROXY_GLOBAL_CONF__#$(escape_sed "${GLOBAL_CONF}" '#')#g" \
  -e "s#__CUBE_PROXY_NGINX_CONF__#$(escape_sed "${NGINX_CONF}" '#')#g" \
  -e "s#__CUBE_PROXY_REGISTRY_ENABLE__#$(escape_sed "${CUBE_PROXY_REGISTRY_ENABLE}" '#')#g" \
  -e "s#__CUBE_PROXY_ID__#$(escape_sed "${CUBE_PROXY_ID}" '#')#g" \
  -e "s#__CUBE_PROXY_ADMIN_URL__#$(escape_sed "${CUBE_PROXY_ADMIN_URL}" '#')#g" \
  -e "s#__CUBE_PROXY_RESUME_URL__#$(escape_sed "${CUBE_PROXY_RESUME_URL}" '#')#g" \
  -e "s#__CUBE_PROXY_NODE_IP__#$(escape_sed "${CUBE_PROXY_NODE_IP}" '#')#g" \
  -e "s#__CUBE_PROXY_VERSION__#$(escape_sed "${CUBE_PROXY_VERSION}" '#')#g" \
  -e "s#__CUBE_PROXY_HEARTBEAT_INTERVAL_MS__#$(escape_sed "${CUBE_PROXY_HEARTBEAT_INTERVAL_MS}" '#')#g" \
  -e "s#__CUBE_PROXY_REGISTRY_REDIS_HOST__#$(escape_sed "${CUBE_PROXY_REGISTRY_REDIS_HOST}" '#')#g" \
  -e "s#__CUBE_PROXY_REGISTRY_REDIS_PORT__#$(escape_sed "${CUBE_PROXY_REGISTRY_REDIS_PORT}" '#')#g" \
  -e "s#__CUBE_PROXY_REGISTRY_REDIS_PASSWORD__#$(escape_sed "${CUBE_PROXY_REGISTRY_REDIS_PASSWORD}" '#')#g" \
  -e "s#__CUBE_PROXY_REGISTRY_REDIS_DB__#$(escape_sed "${CUBE_PROXY_REGISTRY_REDIS_DB}" '#')#g"

if [[ "${PREPARE_ONLY}" == "1" ]]; then
  log "cube proxy runtime files prepared under ${PROXY_DIR}"
  exit 0
fi

compose_run down --remove-orphans >/dev/null 2>&1 || true
docker_rm_if_exists "${CUBE_PROXY_CONTAINER_NAME}"

# cube-proxy uses network_mode: host, so HTTP/HTTPS ports must be free on the
# host before we attempt to start the container; otherwise the failure mode is
# a cryptic "address already in use" from nginx inside the container.
for port in "${CUBE_PROXY_HTTP_PORT}" "${CUBE_PROXY_HTTPS_PORT}"; do
  if command_output_contains_fixed_string "LISTEN" ss -lnt "( sport = :${port} )"; then
    die "port ${port} is already in use; cube-proxy uses host networking and requires it to be free"
  fi
done

# Pull explicitly so the failure mode (registry unreachable / not logged in /
# image not pushed yet) surfaces here instead of midway through `compose up`.
# If the image already exists locally and the registry is unreachable, fall
# back to the cached copy (airgap / CUBE_SANDBOX_CUBE_PROXY_IMAGE preload).
log "pulling ${CUBE_PROXY_IMAGE}"
if ! docker pull "${CUBE_PROXY_IMAGE}" >/dev/null; then
  if docker_image_exists "${CUBE_PROXY_IMAGE}"; then
    log "WARN: pull failed but local image exists; using cached copy"
  else
    die "pull failed for ${CUBE_PROXY_IMAGE} and no local copy is cached; check registry reachability and credentials (or set MIRROR=cn / CUBE_SANDBOX_CUBE_PROXY_IMAGE)"
  fi
fi

if [[ "${COMPOSE_DETACH}" == "0" ]]; then
  compose_run up cube-proxy
  exit $?
fi

compose_run up -d cube-proxy

for _ in {1..40}; do
  state="$(docker inspect --format '{{.State.Status}}' "${CUBE_PROXY_CONTAINER_NAME}" 2>/dev/null || true)"
  if [[ "${state}" == "running" ]]; then
    break
  fi
  sleep 2
done
[[ "${state:-}" == "running" ]] || die "cube proxy container failed to start"

http_ready=0
https_ready=0
for _ in {1..30}; do
  if [[ "${http_ready}" == "0" ]] && \
     command_output_contains_fixed_string "LISTEN" ss -lnt "( sport = :${CUBE_PROXY_HTTP_PORT} )"; then
    http_ready=1
  fi
  if [[ "${https_ready}" == "0" ]] && \
     command_output_contains_fixed_string "LISTEN" ss -lnt "( sport = :${CUBE_PROXY_HTTPS_PORT} )"; then
    https_ready=1
  fi
  if [[ "${http_ready}" == "1" && "${https_ready}" == "1" ]]; then
    log "cube proxy listening on ${CUBE_PROXY_HTTP_PORT} and ${CUBE_PROXY_HTTPS_PORT}"
    exit 0
  fi
  sleep 2
done

if [[ "${http_ready}" != "1" ]]; then
  die "cube proxy port ${CUBE_PROXY_HTTP_PORT} (HTTP) did not become ready"
fi
die "cube proxy port ${CUBE_PROXY_HTTPS_PORT} (HTTPS) did not become ready"
