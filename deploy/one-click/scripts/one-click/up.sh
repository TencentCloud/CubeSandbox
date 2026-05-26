#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"

NETWORK_AGENT_BIN="${TOOLBOX_ROOT}/network-agent/bin/network-agent"
NETWORK_AGENT_CFG="${TOOLBOX_ROOT}/network-agent/network-agent.yaml"
NETWORK_AGENT_STATE_DIR="${TOOLBOX_ROOT}/network-agent/state"
NETWORK_AGENT_HEALTH_ADDR="${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}"
NETWORK_AGENT_READY_TIMEOUT="${NETWORK_AGENT_READY_TIMEOUT:-120}"
CUBE_API_BIN="${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"
CUBE_API_LOG_DIR="${CUBE_API_LOG_DIR:-/data/log/CubeAPI}"
CUBE_API_HEALTH_ADDR="${CUBE_API_HEALTH_ADDR:-127.0.0.1:3000}"
CUBEMASTER_BIN="${TOOLBOX_ROOT}/CubeMaster/bin/cubemaster"
CUBEMASTER_CFG="${TOOLBOX_ROOT}/CubeMaster/conf.yaml"
CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_DEFAULT="/data/CubeMaster/storage"
CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_CONFIGURED="${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR:-}"
CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR="${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_CONFIGURED:-${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_DEFAULT}}"
CUBELET_BIN="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
CUBELET_CONFIG="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
CUBELET_DYNAMICCONF="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
CUBE_API_OPTIONAL_EXPORTS=""
CUBELET_OPTIONAL_EXPORTS=""

require_cmd bash
require_cmd curl

test -x "${NETWORK_AGENT_BIN}" || die "network-agent binary missing: ${NETWORK_AGENT_BIN}"
test -x "${CUBE_API_BIN}" || die "cube-api binary missing: ${CUBE_API_BIN}"
test -x "${CUBEMASTER_BIN}" || die "cubemaster binary missing: ${CUBEMASTER_BIN}"
test -x "${CUBELET_BIN}" || die "cubelet binary missing: ${CUBELET_BIN}"
test -f "${NETWORK_AGENT_CFG}" || die "network-agent config missing: ${NETWORK_AGENT_CFG}"
test -f "${CUBEMASTER_CFG}" || die "cubemaster config missing: ${CUBEMASTER_CFG}"
test -f "${CUBELET_CONFIG}" || die "cubelet config missing: ${CUBELET_CONFIG}"
test -f "${CUBELET_DYNAMICCONF}" || die "cubelet dynamic config missing: ${CUBELET_DYNAMICCONF}"
validate_cubelet_cow_startup_deps "${CUBELET_CONFIG}"

mkdir -p "${NETWORK_AGENT_STATE_DIR}" "${CUBE_API_LOG_DIR}" /tmp/cube

CUBEMASTER_ARTIFACT_STORE_EXPORT=""
if [[ -n "${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_CONFIGURED}" ]]; then
  mkdir -p "${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR}"
  CUBEMASTER_ARTIFACT_STORE_EXPORT="export CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR=\"${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR}\";"
elif mkdir -p "${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR}" >/dev/null 2>&1; then
  CUBEMASTER_ARTIFACT_STORE_EXPORT="export CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR=\"${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR}\";"
else
  log "cubemaster artifact store ${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR} unavailable, fallback handled by cubemaster"
fi

if [[ -n "${CUBE_MASTER_ADDR:-}" ]]; then
  CUBE_API_OPTIONAL_EXPORTS+="export CUBE_MASTER_ADDR=\"${CUBE_MASTER_ADDR}\"; "
fi
if [[ -n "${AUTH_CALLBACK_URL:-}" ]]; then
  CUBE_API_OPTIONAL_EXPORTS+="export AUTH_CALLBACK_URL=\"${AUTH_CALLBACK_URL}\"; "
fi
if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  CUBELET_OPTIONAL_EXPORTS+="export CUBE_SANDBOX_NODE_IP=\"${CUBE_SANDBOX_NODE_IP}\"; "
fi

"${SCRIPT_DIR}/down-local.sh" >/dev/null 2>&1 || true

start_with_pidfile \
  "network-agent" \
  "mkdir -p /tmp/cube \"${NETWORK_AGENT_STATE_DIR}\" && \"${NETWORK_AGENT_BIN}\" --cubelet-config \"${CUBELET_CONFIG}\" --state-dir \"${NETWORK_AGENT_STATE_DIR}\""

wait_for_http "http://${NETWORK_AGENT_HEALTH_ADDR}/readyz" "${NETWORK_AGENT_READY_TIMEOUT}" 1 || die "network-agent did not become ready, check logs under ${LOG_DIR}"

start_with_pidfile \
  "cubemaster" \
  "export CUBE_MASTER_CONFIG_PATH=\"${CUBEMASTER_CFG}\"; ${CUBEMASTER_ARTIFACT_STORE_EXPORT} \"${CUBEMASTER_BIN}\""

start_with_pidfile \
  "cube-api" \
  "export LOG_DIR=\"${CUBE_API_LOG_DIR}\" CUBE_API_BIND=\"${CUBE_API_BIND:-0.0.0.0:3000}\" CUBE_API_SANDBOX_DOMAIN=\"${CUBE_API_SANDBOX_DOMAIN:-cube.app}\"; ${CUBE_API_OPTIONAL_EXPORTS}\"${CUBE_API_BIN}\""

start_with_pidfile \
  "cubelet" \
  "${CUBELET_OPTIONAL_EXPORTS}\"${CUBELET_BIN}\" --config \"${CUBELET_CONFIG}\" --dynamic-conf-path \"${CUBELET_DYNAMICCONF}\""
refresh_pidfile_from_pattern "cubelet" "^${CUBELET_BIN} --config" 10 1 || log "cubelet pidfile refresh skipped"

wait_for_http "http://${CUBE_API_HEALTH_ADDR}/health" 30 1 || die "cube-api did not become ready, check logs under ${LOG_DIR}"

for _ in {1..30}; do
  if "${SCRIPT_DIR}/quickcheck.sh" >/dev/null 2>&1; then
    "${SCRIPT_DIR}/quickcheck.sh"
    log "core services ready"
    exit 0
  fi
  sleep 2
done

die "core services did not become ready, check logs under ${LOG_DIR}"
