#!/usr/bin/env bash
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
BUILD_CONTEXT_DIR="${PROXY_DIR}/build-context"
CUBE_PROXY_CERT_DIR="${CUBE_PROXY_CERT_DIR:-${PROXY_DIR}/certs}"
CERT_DIR="${CUBE_PROXY_CERT_DIR}"
GLOBAL_TEMPLATE="${PROXY_DIR}/global.conf.template"
GLOBAL_CONF="${PROXY_DIR}/global.conf"
COMPOSE_TEMPLATE="${PROXY_DIR}/docker-compose.yaml.template"
COMPOSE_FILE="${PROXY_DIR}/docker-compose.yaml"

CUBE_PROXY_IMAGE_TAG="${CUBE_PROXY_IMAGE_TAG:-cube-proxy:one-click}"
CUBE_PROXY_CONTAINER_NAME="${CUBE_PROXY_CONTAINER_NAME:-cube-proxy}"
CUBE_PROXY_HOST_PORT="${CUBE_PROXY_HOST_PORT:-443}"
CUBE_PROXY_HTTP_HOST_PORT="${CUBE_PROXY_HTTP_HOST_PORT:-80}"
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
CUBE_PROXY_REDIS_IP="${CUBE_PROXY_REDIS_IP:-${CUBE_SANDBOX_NODE_IP}}"
CUBE_PROXY_REDIS_PORT="${CUBE_PROXY_REDIS_PORT:-${CUBE_SANDBOX_REDIS_PORT:-6379}}"
CUBE_PROXY_REDIS_PASSWORD="${CUBE_PROXY_REDIS_PASSWORD:-${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}}"
MKCERT_BUNDLED_BIN="${TOOLBOX_ROOT}/support/bin/mkcert"
CAROOT="${CAROOT:-${TOOLBOX_ROOT}/cubeproxy/caroot}"
export CAROOT
ALPINE_MIRROR_URL="${ALPINE_MIRROR_URL:-https://mirrors.tuna.tsinghua.edu.cn/alpine}"
PIP_INDEX_URL="${PIP_INDEX_URL:-https://pypi.tuna.tsinghua.edu.cn/simple}"

ensure_dir "${PROXY_DIR}"
ensure_dir "${BUILD_CONTEXT_DIR}"
mkdir -p "${CERT_DIR}"
ensure_file "${BUILD_CONTEXT_DIR}/Dockerfile.oneclick"
ensure_file "${GLOBAL_TEMPLATE}"
ensure_file "${COMPOSE_TEMPLATE}"
[[ -n "${CUBE_SANDBOX_NODE_IP}" ]] || die "CUBE_SANDBOX_NODE_IP is required for cube proxy"

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

escape_sed() {
  printf '%s' "$1" | sed 's/[\/&]/\\&/g'
}

prepare_proxy_certs() {
  mkdir -p "${CERT_DIR}"
  mkdir -p "${CAROOT}"
  if [[ -f "${CERT_DIR}/cube.app+3.pem" && -f "${CERT_DIR}/cube.app+3-key.pem" ]]; then
    return 0
  fi

  install_mkcert
  (
    cd "${CERT_DIR}"
    mkcert -install
    mkcert cube.app "*.cube.app" localhost 127.0.0.1
  ) >&2
}

prepare_proxy_certs

sed \
  -e "s/__CUBE_PROXY_REDIS_IP__/$(escape_sed "${CUBE_PROXY_REDIS_IP}")/g" \
  -e "s/__CUBE_PROXY_REDIS_PORT__/$(escape_sed "${CUBE_PROXY_REDIS_PORT}")/g" \
  -e "s/__CUBE_PROXY_REDIS_PASSWORD__/$(escape_sed "${CUBE_PROXY_REDIS_PASSWORD}")/g" \
  -e "s/__CUBE_PROXY_HOST_IP__/$(escape_sed "${CUBE_SANDBOX_NODE_IP}")/g" \
  "${GLOBAL_TEMPLATE}" > "${GLOBAL_CONF}"

sed \
  -e "s#__CUBE_PROXY_IMAGE__#$(escape_sed "${CUBE_PROXY_IMAGE_TAG}")#g" \
  -e "s#__CUBE_PROXY_CONTAINER_NAME__#$(escape_sed "${CUBE_PROXY_CONTAINER_NAME}")#g" \
  -e "s#__CUBE_PROXY_BUILD_CONTEXT__#$(escape_sed "${BUILD_CONTEXT_DIR}")#g" \
  -e "s#__CUBE_PROXY_HOST_PORT__#$(escape_sed "${CUBE_PROXY_HOST_PORT}")#g" \
  -e "s#__CUBE_PROXY_HTTP_HOST_PORT__#$(escape_sed "${CUBE_PROXY_HTTP_HOST_PORT}")#g" \
  -e "s#__ALPINE_MIRROR_URL__#$(escape_sed "${ALPINE_MIRROR_URL}")#g" \
  -e "s#__PIP_INDEX_URL__#$(escape_sed "${PIP_INDEX_URL}")#g" \
  -e "s#__CUBE_PROXY_CERT_DIR__#$(escape_sed "${CERT_DIR}")#g" \
  -e "s#__CUBE_PROXY_GLOBAL_CONF__#$(escape_sed "${GLOBAL_CONF}")#g" \
  "${COMPOSE_TEMPLATE}" > "${COMPOSE_FILE}"

compose_run down --remove-orphans >/dev/null 2>&1 || true
compose_run build cube-proxy
compose_run up -d cube-proxy

for _ in {1..40}; do
  state="$(docker inspect --format '{{.State.Status}}' "${CUBE_PROXY_CONTAINER_NAME}" 2>/dev/null || true)"
  if [[ "${state}" == "running" ]]; then
    break
  fi
  sleep 2
done
[[ "${state:-}" == "running" ]] || die "cube proxy container failed to start"

for _ in {1..30}; do
  if ss -lnt "( sport = :${CUBE_PROXY_HOST_PORT} )" | rg -q ":${CUBE_PROXY_HOST_PORT}"; then
    log "cube proxy listening on ${CUBE_PROXY_HOST_PORT}"
    exit 0
  fi
  sleep 2
done

die "cube proxy port ${CUBE_PROXY_HOST_PORT} did not become ready"
