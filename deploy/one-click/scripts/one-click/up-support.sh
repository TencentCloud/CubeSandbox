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
MINIO_CONTAINER="${CUBE_SANDBOX_MINIO_CONTAINER:-cube-sandbox-minio}"
MYSQL_IMAGE="${CUBE_SANDBOX_MYSQL_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0}"
REDIS_IMAGE="${CUBE_SANDBOX_REDIS_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine}"
# Image resolution priority mirrors up-cube-proxy.sh / up-cube-lifecycle-manager.sh:
#   1. CUBE_SANDBOX_MINIO_IMAGE (explicit operator override)
#   2. MIRROR=cn → cube-sandbox-cn.tencentcloudcr.com (China-region registry)
#   3. default   → cube-sandbox-int.tencentcloudcr.com (overseas/international)
MINIO_IMAGE_INT_DEFAULT="cube-sandbox-int.tencentcloudcr.com/cube-sandbox/minio:RELEASE.2025-09-07T16-13-09Z"
MINIO_IMAGE_CN_DEFAULT="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/minio:RELEASE.2025-09-07T16-13-09Z"
if [[ -n "${CUBE_SANDBOX_MINIO_IMAGE:-}" ]]; then
  MINIO_IMAGE="${CUBE_SANDBOX_MINIO_IMAGE}"
elif [[ "${MIRROR:-}" == "cn" ]]; then
  MINIO_IMAGE="${MINIO_IMAGE_CN_DEFAULT}"
else
  MINIO_IMAGE="${MINIO_IMAGE_INT_DEFAULT}"
fi
MYSQL_VOLUME="${CUBE_SANDBOX_MYSQL_VOLUME:-cube-sandbox-mysql-data}"
REDIS_VOLUME="${CUBE_SANDBOX_REDIS_VOLUME:-cube-sandbox-redis-data}"
MINIO_VOLUME="${CUBE_SANDBOX_MINIO_VOLUME:-cube-sandbox-minio-data}"
MYSQL_PORT="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
REDIS_PORT="${CUBE_SANDBOX_REDIS_PORT:-6379}"
MINIO_API_PORT="${CUBE_SANDBOX_MINIO_API_PORT:-9000}"
MINIO_CONSOLE_PORT="${CUBE_SANDBOX_MINIO_CONSOLE_PORT:-9001}"
# Publish the S3 API on the node IP so compute-node Cubelets can reach it.
# Fall back to 127.0.0.1 for a lone all-in-one host without a detected node IP.
MINIO_API_BIND="${CUBE_SANDBOX_MINIO_API_BIND:-${CUBE_SANDBOX_NODE_IP:-127.0.0.1}}"
REDIS_PASSWORD="${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"
MYSQL_DB="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
MYSQL_USER="${CUBE_SANDBOX_MYSQL_USER:-cube}"
MYSQL_PASSWORD="${CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass}"
MYSQL_ROOT_PASSWORD="${CUBE_SANDBOX_MYSQL_ROOT_PASSWORD:-cube_root}"
MINIO_ROOT_USER="${CUBE_SANDBOX_MINIO_ROOT_USER:-cubeminio}"
MINIO_ROOT_PASSWORD="${CUBE_SANDBOX_MINIO_ROOT_PASSWORD:-}"
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
      -e "s/__MINIO_CONTAINER__/$(escape_sed "${MINIO_CONTAINER}")/g" \
      -e "s#__MYSQL_IMAGE__#$(escape_sed "${MYSQL_IMAGE}" '#')#g" \
      -e "s#__REDIS_IMAGE__#$(escape_sed "${REDIS_IMAGE}" '#')#g" \
      -e "s#__MINIO_IMAGE__#$(escape_sed "${MINIO_IMAGE}" '#')#g" \
      -e "s/__MYSQL_VOLUME__/$(escape_sed "${MYSQL_VOLUME}")/g" \
      -e "s/__REDIS_VOLUME__/$(escape_sed "${REDIS_VOLUME}")/g" \
      -e "s/__MINIO_VOLUME__/$(escape_sed "${MINIO_VOLUME}")/g" \
      -e "s/__MYSQL_PORT__/$(escape_sed "${MYSQL_PORT}")/g" \
      -e "s/__REDIS_PORT__/$(escape_sed "${REDIS_PORT}")/g" \
      -e "s/__MINIO_API_BIND__/$(escape_sed "${MINIO_API_BIND}")/g" \
      -e "s/__MINIO_API_PORT__/$(escape_sed "${MINIO_API_PORT}")/g" \
      -e "s/__MINIO_CONSOLE_PORT__/$(escape_sed "${MINIO_CONSOLE_PORT}")/g" \
      -e "s/__REDIS_PASSWORD__/$(escape_sed "${REDIS_PASSWORD}")/g" \
      -e "s/__MYSQL_DB__/$(escape_sed "${MYSQL_DB}")/g" \
      -e "s/__MYSQL_USER__/$(escape_sed "${MYSQL_USER}")/g" \
      -e "s/__MYSQL_PASSWORD__/$(escape_sed "${MYSQL_PASSWORD}")/g" \
      -e "s/__MYSQL_ROOT_PASSWORD__/$(escape_sed "${MYSQL_ROOT_PASSWORD}")/g" \
      -e "s/__MINIO_ROOT_USER__/$(escape_sed "${MINIO_ROOT_USER}")/g" \
      -e "s/__MINIO_ROOT_PASSWORD__/$(escape_sed "${MINIO_ROOT_PASSWORD}")/g"
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

# Never start a local container that is externalized (MySQL/Redis) or not
# enabled (MinIO). Empty SUPPORT_SERVICES is the default path: expand to
# "mysql redis minio", then drop external/disabled ones so a conflicting
# container is never launched.
CUBE_EXTERNAL_MYSQL_HOST="${CUBE_EXTERNAL_MYSQL_HOST:-}"
CUBE_EXTERNAL_REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-}"
CUBE_EXTERNAL_REDIS_MASTER_NAME="${CUBE_EXTERNAL_REDIS_MASTER_NAME:-}"
CUBE_SANDBOX_MINIO_ENABLED="${CUBE_SANDBOX_MINIO_ENABLED:-1}"
minio_dropped=0
if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" || -n "${CUBE_EXTERNAL_REDIS_HOST}" \
    || -n "${CUBE_EXTERNAL_REDIS_MASTER_NAME}" || "${CUBE_SANDBOX_MINIO_ENABLED}" != "1" \
    || -z "${SUPPORT_SERVICES}" ]]; then
  requested_services="${SUPPORT_SERVICES:-mysql redis minio}"
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
        if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" || -n "${CUBE_EXTERNAL_REDIS_MASTER_NAME}" ]]; then
          if [[ -n "${CUBE_EXTERNAL_REDIS_MASTER_NAME}" ]]; then
            log "using external Redis Sentinel (${CUBE_EXTERNAL_REDIS_MASTER_NAME}), skipping local redis container"
          else
            log "using external Redis (${CUBE_EXTERNAL_REDIS_HOST}), skipping local redis container"
          fi
          continue
        fi
        ;;
      minio)
        if [[ "${CUBE_SANDBOX_MINIO_ENABLED}" != "1" ]]; then
          log "CUBE_SANDBOX_MINIO_ENABLED=${CUBE_SANDBOX_MINIO_ENABLED}; skipping local minio container"
          minio_dropped=1
          continue
        fi
        ;;
    esac
    filtered_services="${filtered_services}${filtered_services:+ }${svc}"
  done
  # MinIO disabled: also remove a leftover container from a previous install,
  # so it does not keep occupying ports 9000/9001.
  if [[ "${minio_dropped}" == "1" ]]; then
    docker_rm_if_exists "${MINIO_CONTAINER}"
  fi
  if [[ -z "${filtered_services}" ]]; then
    log "no local support services to start under ${SUPPORT_DIR}"
    exit 0
  fi
  SUPPORT_SERVICES="${filtered_services}"
fi

# install.sh generates and persists MINIO_ROOT_PASSWORD to .one-click.env. A
# standalone up-support.sh run must supply it, otherwise the rendered compose
# would pass an empty MINIO_ROOT_PASSWORD and MinIO would refuse to start.
if [[ " ${SUPPORT_SERVICES:-} " == *" minio "* && -z "${MINIO_ROOT_PASSWORD}" ]]; then
  die "CUBE_SANDBOX_MINIO_ROOT_PASSWORD is empty; set it or re-run install.sh to generate one"
fi

wait_for_support_service() {
  local service="$1"
  case "${service}" in
    mysql)
      wait_for_health "${MYSQL_CONTAINER}" || die "mysql container did not become healthy"
      ;;
    redis)
      wait_for_health "${REDIS_CONTAINER}" || die "redis container did not become healthy"
      ;;
    minio)
      wait_for_health "${MINIO_CONTAINER}" || die "minio container did not become healthy"
      ;;
  esac
}

# Systemd manages mysql, redis and minio as separate foreground services. In
# that mode do not run compose down here, because it would stop the sibling unit.
for service in ${SUPPORT_SERVICES}; do
  case "${service}" in
    mysql)
      docker_rm_if_exists "${MYSQL_CONTAINER}"
      ;;
    redis)
      docker_rm_if_exists "${REDIS_CONTAINER}"
      ;;
    minio)
      docker_rm_if_exists "${MINIO_CONTAINER}"
      ;;
    *)
      die "unsupported support compose service: ${service}"
      ;;
  esac
done

if [[ "${COMPOSE_DETACH}" == "1" ]]; then
  # shellcheck disable=SC2086
  support_compose_run up -d ${SUPPORT_SERVICES}
  for service in ${SUPPORT_SERVICES}; do
    wait_for_support_service "${service}"
  done
  log "support services ready under ${SUPPORT_DIR}: ${SUPPORT_SERVICES}"
  exit 0
fi

# shellcheck disable=SC2086
support_compose_run up ${SUPPORT_SERVICES}
