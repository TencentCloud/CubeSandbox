#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd sed

NETWORK_AGENT_BIN="${TOOLBOX_ROOT}/network-agent/bin/network-agent"
NETWORK_AGENT_CFG="${TOOLBOX_ROOT}/network-agent/network-agent.yaml"
NETWORK_AGENT_STATE_DIR="/data/cubelet/network-agent/state"
NETWORK_AGENT_HEALTH_ADDR="${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}"
NETWORK_AGENT_READY_TIMEOUT="${NETWORK_AGENT_READY_TIMEOUT:-120}"
CUBELET_BIN="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
CUBELET_CONFIG="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
CUBELET_DYNAMICCONF="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"

require_cmd bash
require_cmd curl

test -x "${NETWORK_AGENT_BIN}" || die "network-agent binary missing: ${NETWORK_AGENT_BIN}"
test -x "${CUBELET_BIN}" || die "cubelet binary missing: ${CUBELET_BIN}"
test -f "${NETWORK_AGENT_CFG}" || die "network-agent config missing: ${NETWORK_AGENT_CFG}"
test -f "${CUBELET_CONFIG}" || die "cubelet config missing: ${CUBELET_CONFIG}"
test -f "${CUBELET_DYNAMICCONF}" || die "cubelet dynamic config missing: ${CUBELET_DYNAMICCONF}"
validate_cubelet_cow_startup_deps "${CUBELET_CONFIG}"

ROLE="$(one_click_deploy_role)"
[[ "${ROLE}" == "compute" ]] || die "up-compute.sh requires ONE_CLICK_DEPLOY_ROLE=compute"

CONTROL_PLANE_ADDR="$(resolve_control_plane_cubemaster_addr)"
[[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]] || die "CUBE_SANDBOX_NODE_IP is required for compute role"

grep -Eq "meta_server_endpoint:" "${CUBELET_DYNAMICCONF}" || die "meta_server_endpoint missing in ${CUBELET_DYNAMICCONF}"
sed -i \
  -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${CONTROL_PLANE_ADDR}\"#" \
  "${CUBELET_DYNAMICCONF}"

mkdir -p \
  "${TOOLBOX_ROOT}/cube-vs/network" \
  "${TOOLBOX_ROOT}/cube-snapshot" \
  "${NETWORK_AGENT_STATE_DIR}" \
  /tmp/cube \
  /data/log/Cubelet \
  /data/log/CubeShim \
  /data/log/CubeVmm \
  /data/cube-shim/disks \
  /data/snapshot_pack/disks

"${SCRIPT_DIR}/down-compute.sh" >/dev/null 2>&1 || true

start_with_pidfile \
  "network-agent" \
  "mkdir -p /tmp/cube \"${NETWORK_AGENT_STATE_DIR}\" && \"${NETWORK_AGENT_BIN}\" --cubelet-config \"${CUBELET_CONFIG}\" --state-dir \"${NETWORK_AGENT_STATE_DIR}\""

wait_for_http "http://${NETWORK_AGENT_HEALTH_ADDR}/readyz" "${NETWORK_AGENT_READY_TIMEOUT}" 1 || die "network-agent did not become ready, check logs under ${LOG_DIR}"

start_with_pidfile \
  "cubelet" \
  "export CUBE_SANDBOX_NODE_IP=\"${CUBE_SANDBOX_NODE_IP}\"; \"${CUBELET_BIN}\" --config \"${CUBELET_CONFIG}\" --dynamic-conf-path \"${CUBELET_DYNAMICCONF}\""

refresh_pidfile_from_pattern "cubelet" "^${CUBELET_BIN} --config" 10 1 || log "cubelet pidfile refresh skipped"

# quickcheck.sh now waits for each runtime signal to become ready within a single
# shared budget (CUBE_QUICKCHECK_READY_TIMEOUT), so a single invocation is
# already race-tolerant. Do NOT wrap it in an outer retry loop: that would
# multiply quickcheck's budget on a genuinely broken node.
if "${SCRIPT_DIR}/quickcheck.sh"; then
  log "compute services ready"
  exit 0
fi

die "compute services did not become ready, check logs under ${LOG_DIR}"
