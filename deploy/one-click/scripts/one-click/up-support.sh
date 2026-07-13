#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./support-compose-lib.sh
source "${SCRIPT_DIR}/support-compose-lib.sh"

require_root
require_cmd docker
require_cmd flock
require_cmd sed

MYSQL_CONTAINER="${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}"
REDIS_CONTAINER="${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}"
MYSQL_IMAGE="${CUBE_SANDBOX_MYSQL_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0}"
REDIS_IMAGE="${CUBE_SANDBOX_REDIS_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine}"
MYSQL_VOLUME="${CUBE_SANDBOX_MYSQL_VOLUME:-cube-sandbox-mysql-data}"
REDIS_VOLUME="${CUBE_SANDBOX_REDIS_VOLUME:-cube-sandbox-redis-data}"
MYSQL_PORT="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
REDIS_PORT="${CUBE_SANDBOX_REDIS_PORT:-6379}"
REDIS_PASSWORD="${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"
MYSQL_DB="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
MYSQL_USER="${CUBE_SANDBOX_MYSQL_USER:-cube}"
MYSQL_PASSWORD="${CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass}"
MYSQL_ROOT_PASSWORD="${CUBE_SANDBOX_MYSQL_ROOT_PASSWORD:-cube_root}"
SUPPORT_DIR="${TOOLBOX_ROOT}/support"
SUPPORT_TEMPLATE="${SUPPORT_DIR}/docker-compose.yaml.template"
SUPPORT_COMPOSE_FILE="${SUPPORT_DIR}/docker-compose.yaml"
SUPPORT_SERVICES="${ONE_CLICK_SUPPORT_SERVICES:-}"
COMPOSE_DETACH="${ONE_CLICK_COMPOSE_DETACH:-1}"
PREPARE_ONLY="${ONE_CLICK_PREPARE_ONLY:-0}"
SUPPORT_COMPOSE_LOCK="${RUNTIME_DIR}/support-compose.lock"

ensure_dir "${SUPPORT_DIR}"
ensure_file "${SUPPORT_TEMPLATE}"

render_support_compose() {
  mkdir -p "$(dirname "${SUPPORT_COMPOSE_LOCK}")"
  (
    flock -x 9
    render_template_atomic \
      "${SUPPORT_TEMPLATE}" \
      "${SUPPORT_COMPOSE_FILE}" \
      -e "s/__MYSQL_CONTAINER__/$(escape_sed "${MYSQL_CONTAINER}")/g" \
      -e "s/__REDIS_CONTAINER__/$(escape_sed "${REDIS_CONTAINER}")/g" \
      -e "s#__MYSQL_IMAGE__#$(escape_sed "${MYSQL_IMAGE}" '#')#g" \
      -e "s#__REDIS_IMAGE__#$(escape_sed "${REDIS_IMAGE}" '#')#g" \
      -e "s/__MYSQL_VOLUME__/$(escape_sed "${MYSQL_VOLUME}")/g" \
      -e "s/__REDIS_VOLUME__/$(escape_sed "${REDIS_VOLUME}")/g" \
      -e "s/__MYSQL_PORT__/$(escape_sed "${MYSQL_PORT}")/g" \
      -e "s/__REDIS_PORT__/$(escape_sed "${REDIS_PORT}")/g" \
      -e "s/__REDIS_PASSWORD__/$(escape_sed "${REDIS_PASSWORD}")/g" \
      -e "s/__MYSQL_DB__/$(escape_sed "${MYSQL_DB}")/g" \
      -e "s/__MYSQL_USER__/$(escape_sed "${MYSQL_USER}")/g" \
      -e "s/__MYSQL_PASSWORD__/$(escape_sed "${MYSQL_PASSWORD}")/g" \
      -e "s/__MYSQL_ROOT_PASSWORD__/$(escape_sed "${MYSQL_ROOT_PASSWORD}")/g"
  ) 9>"${SUPPORT_COMPOSE_LOCK}"
}

render_support_compose

if [[ "${PREPARE_ONLY}" == "1" ]]; then
  log "support compose prepared at ${SUPPORT_COMPOSE_FILE}"
  exit 0
fi

case "${COMPOSE_DETACH}" in
  0|1) ;;
  *) die "unsupported ONE_CLICK_COMPOSE_DETACH: ${COMPOSE_DETACH} (expected 0 or 1)" ;;
esac

# When MySQL/Redis is provided externally, never start the matching local
# container. Filter it out of the requested service set; for the legacy
# "all services" path (empty SUPPORT_SERVICES) restrict the run to the local
# services only so we don't accidentally launch a conflicting container.
CUBE_EXTERNAL_MYSQL_HOST="${CUBE_EXTERNAL_MYSQL_HOST:-}"
CUBE_EXTERNAL_REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-}"
if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" || -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
  requested_services="${SUPPORT_SERVICES:-mysql redis}"
  filtered_services=""
  # Split on whitespace into an array so SUPPORT_SERVICES (user-controllable) is
  # not subject to glob expansion / pathname matching while iterating.
  read -ra requested_services_arr <<< "${requested_services}"
  for svc in "${requested_services_arr[@]}"; do
    case "${svc}" in
      mysql)
        if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" ]]; then
          log "using external MySQL (${CUBE_EXTERNAL_MYSQL_HOST}), skipping local mysql container"
          continue
        fi
        ;;
      redis)
        if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
          log "using external Redis (${CUBE_EXTERNAL_REDIS_HOST}), skipping local redis container"
          continue
        fi
        ;;
    esac
    filtered_services="${filtered_services}${filtered_services:+ }${svc}"
  done
  if [[ -z "${filtered_services}" ]]; then
    log "all support services are external; nothing to start under ${SUPPORT_DIR}"
    exit 0
  fi
  SUPPORT_SERVICES="${filtered_services}"
fi

if [[ -z "${SUPPORT_SERVICES}" ]]; then
  support_compose_run down --remove-orphans >/dev/null 2>&1 || true
  docker_rm_if_exists "${MYSQL_CONTAINER}"
  docker_rm_if_exists "${REDIS_CONTAINER}"
  support_compose_run up -d

  wait_for_health "${MYSQL_CONTAINER}" || die "mysql container did not become healthy"
  wait_for_health "${REDIS_CONTAINER}" || die "redis container did not become healthy"

  log "support services ready under ${SUPPORT_DIR}"
  exit 0
fi

# Systemd manages mysql and redis as separate foreground services. In that mode
# do not run compose down here, because it would stop the sibling unit.
for service in ${SUPPORT_SERVICES}; do
  case "${service}" in
    mysql)
      docker_rm_if_exists "${MYSQL_CONTAINER}"
      ;;
    redis)
      docker_rm_if_exists "${REDIS_CONTAINER}"
      ;;
    *)
      die "unsupported support compose service: ${service}"
      ;;
  esac
done

if [[ "${COMPOSE_DETACH}" == "1" ]]; then
  support_compose_run up -d ${SUPPORT_SERVICES}
  for service in ${SUPPORT_SERVICES}; do
    case "${service}" in
      mysql)
        wait_for_health "${MYSQL_CONTAINER}" || die "mysql container did not become healthy"
        ;;
      redis)
        wait_for_health "${REDIS_CONTAINER}" || die "redis container did not become healthy"
        ;;
    esac
  done
  log "support services ready under ${SUPPORT_DIR}: ${SUPPORT_SERVICES}"
  exit 0
fi

support_compose_run up ${SUPPORT_SERVICES}
