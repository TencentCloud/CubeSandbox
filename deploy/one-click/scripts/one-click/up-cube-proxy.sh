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
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
CUBE_PROXY_REDIS_IP="${CUBE_PROXY_REDIS_IP:-127.0.0.1}"
CUBE_PROXY_REDIS_PORT="${CUBE_PROXY_REDIS_PORT:-${CUBE_SANDBOX_REDIS_PORT:-6379}}"
CUBE_PROXY_REDIS_PASSWORD="${CUBE_PROXY_REDIS_PASSWORD:-${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}}"
CUBE_PROXY_HTTPS_PORT="${CUBE_PROXY_HTTPS_PORT:-443}"
CUBE_PROXY_HTTP_PORT="${CUBE_PROXY_HTTP_PORT:-80}"
CUBE_PROXY_SSL_CERT="${CUBE_PROXY_SSL_CERT:-cube.app+3.pem}"
CUBE_PROXY_SSL_KEY="${CUBE_PROXY_SSL_KEY:-cube.app+3-key.pem}"
COMPOSE_DETACH="${ONE_CLICK_COMPOSE_DETACH:-1}"
MKCERT_BUNDLED_BIN="${TOOLBOX_ROOT}/support/bin/mkcert"
PREPARE_ONLY="${ONE_CLICK_PREPARE_ONLY:-0}"

ensure_dir "${PROXY_DIR}"
ensure_dir "${BUILD_CONTEXT_DIR}"
mkdir -p "${CERT_DIR}"
ensure_file "${BUILD_CONTEXT_DIR}/Dockerfile"
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

escape_sed() {
  printf '%s' "$1" | sed 's/[\/&]/\\&/g'
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
  -e "s/__CUBE_PROXY_HOST_IP__/$(escape_sed "${CUBE_SANDBOX_NODE_IP}")/g"

NGINX_TEMPLATE="${PROXY_DIR}/nginx.conf.template"
NGINX_CONF="${PROXY_DIR}/nginx.conf"
ensure_file "${NGINX_TEMPLATE}"
render_template_atomic \
  "${NGINX_TEMPLATE}" \
  "${NGINX_CONF}" \
  -e "s/__CUBE_PROXY_HTTPS_PORT__/$(escape_sed "${CUBE_PROXY_HTTPS_PORT}")/g" \
  -e "s/__CUBE_PROXY_HTTP_PORT__/$(escape_sed "${CUBE_PROXY_HTTP_PORT}")/g" \
  -e "s/__CUBE_PROXY_SSL_CERT__/$(escape_sed "${CUBE_PROXY_SSL_CERT}")/g" \
  -e "s/__CUBE_PROXY_SSL_KEY__/$(escape_sed "${CUBE_PROXY_SSL_KEY}")/g"

render_template_atomic \
  "${COMPOSE_TEMPLATE}" \
  "${COMPOSE_FILE}" \
  -e "s#__CUBE_PROXY_IMAGE__#$(escape_sed "${CUBE_PROXY_IMAGE_TAG}")#g" \
  -e "s#__CUBE_PROXY_CONTAINER_NAME__#$(escape_sed "${CUBE_PROXY_CONTAINER_NAME}")#g" \
  -e "s#__CUBE_PROXY_BUILD_CONTEXT__#$(escape_sed "${BUILD_CONTEXT_DIR}")#g" \
  -e "s#__CUBE_PROXY_CERT_DIR__#$(escape_sed "${CERT_DIR}")#g" \
  -e "s#__CUBE_PROXY_GLOBAL_CONF__#$(escape_sed "${GLOBAL_CONF}")#g" \
  -e "s#__CUBE_PROXY_NGINX_CONF__#$(escape_sed "${NGINX_CONF}")#g"

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
  if ss -lnt "( sport = :${port} )" | rg -q "LISTEN"; then
    die "port ${port} is already in use; cube-proxy uses host networking and requires it to be free"
  fi
done

compose_run build cube-proxy
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
     ss -lnt "( sport = :${CUBE_PROXY_HTTP_PORT} )" | rg -q "LISTEN"; then
    http_ready=1
  fi
  if [[ "${https_ready}" == "0" ]] && \
     ss -lnt "( sport = :${CUBE_PROXY_HTTPS_PORT} )" | rg -q "LISTEN"; then
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
