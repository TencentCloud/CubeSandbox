#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
MASTER_ADDR="$(resolve_control_plane_cubemaster_addr)"
NA_HEALTH_ADDR="${NETWORK_AGENT_HEALTH_ADDR:-127.0.0.1:19090}"
CUBE_API_HEALTH_ADDR="${CUBE_API_HEALTH_ADDR:-127.0.0.1:3000}"
ROLE="$(one_click_deploy_role)"
NODE_ID="${CUBE_SANDBOX_NODE_IP:-}"

require_cmd systemctl

check_unit_active() {
  local unit="$1"
  systemctl is-active --quiet "${unit}" || die "expected systemd unit not active: ${unit}"
}

check_container_ready() {
  local container="$1"
  local timeout="${CUBE_QUICKCHECK_CONTAINER_TIMEOUT:-60}"
  local interval=2
  local elapsed=0
  local status
  while :; do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${container}" 2>/dev/null || true)"
    case "${status}" in
      healthy|running)
        return 0
        ;;
      starting)
        ;;
      *)
        die "container is not ready: ${container} (status=${status:-unknown})"
        ;;
    esac
    if (( elapsed >= timeout )); then
      die "container is not ready within ${timeout}s: ${container} (status=${status:-unknown})"
    fi
    sleep "${interval}"
    elapsed=$(( elapsed + interval ))
  done
}

check_bind_mount_source_file() {
  local path="$1"
  [[ -f "${path}" ]] || die "expected bind mount source file not ready: ${path}"
}

echo "[quickcheck] role=${ROLE}"
echo "[quickcheck] cubemaster=${MASTER_ADDR}"
echo "[quickcheck] network-agent-health=${NA_HEALTH_ADDR}"
if [[ "${ROLE}" != "compute" ]]; then
  echo "[quickcheck] cube-api-health=${CUBE_API_HEALTH_ADDR}"
fi

echo "[quickcheck] check systemd units"
check_unit_active cube-sandbox-network-agent.service
check_unit_active cube-sandbox-cubelet.service
if [[ "${ROLE}" != "compute" ]]; then
  check_unit_active cube-sandbox-mysql.service
  check_unit_active cube-sandbox-redis.service
  check_unit_active cube-sandbox-cubemaster.service
  check_unit_active cube-sandbox-cube-api.service
  check_unit_active cube-sandbox-cube-proxy.service
  check_unit_active cube-sandbox-coredns.service
  check_unit_active cube-sandbox-dns.service
  if [[ "${WEB_UI_ENABLE:-1}" == "1" ]]; then
    check_unit_active cube-sandbox-webui.service
  fi
fi

if command -v docker >/dev/null 2>&1 && [[ "${ROLE}" != "compute" ]]; then
  echo "[quickcheck] check container runtime state"
  check_container_ready "${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}"
  check_container_ready "${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}"
  check_container_ready "${CUBE_PROXY_CONTAINER_NAME:-cube-proxy}"
  check_container_ready "${CUBE_PROXY_COREDNS_CONTAINER:-cube-proxy-coredns}"
  if [[ "${WEB_UI_ENABLE:-1}" == "1" ]]; then
    check_container_ready "${WEB_UI_CONTAINER_NAME:-cube-webui}"
  fi
fi

echo "[quickcheck] 1/5 check network-agent healthz"
curl -fsS "http://${NA_HEALTH_ADDR}/healthz" >/dev/null

echo "[quickcheck] 2/5 check network-agent readyz"
curl -fsS "http://${NA_HEALTH_ADDR}/readyz" >/dev/null

echo "[quickcheck] 3/5 check cubemaster /notify/health"
curl -fsS "http://${MASTER_ADDR}/notify/health" >/dev/null

if [[ "${ROLE}" == "compute" ]]; then
  [[ -n "${NODE_ID}" ]] || die "CUBE_SANDBOX_NODE_IP is required for compute quickcheck"
  echo "[quickcheck] 4/5 check cubemaster node registration"
  curl -fsS "http://${MASTER_ADDR}/internal/meta/nodes/${NODE_ID}" | rg -q "\"host_ip\":\"${NODE_ID}\""

  echo "[quickcheck] 5/5 check essential sockets and runtime assets"
  test -S "/data/cubelet/cubelet.sock"
  test -S "/tmp/cube/network-agent-grpc.sock"
  test -f "${TOOLBOX_ROOT}/Cubelet/config/config.toml"
  test -f "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
  test -f "${TOOLBOX_ROOT}/cube-shim/conf/config-cube.toml"
  test -f "${TOOLBOX_ROOT}/cube-kernel-scf/vmlinux"
  test -f "${TOOLBOX_ROOT}/cube-image/cube-guest-image-cpu.img"
else
  echo "[quickcheck] 4/5 check cube-api /health"
  curl -fsS "http://${CUBE_API_HEALTH_ADDR}/health" >/dev/null

  echo "[quickcheck] 5/5 check essential sockets and config"
  test -S "/data/cubelet/cubelet.sock"
  test -S "/tmp/cube/network-agent-grpc.sock"
  test -x "${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"
  test -f "${TOOLBOX_ROOT}/CubeMaster/conf.yaml"
  test -f "${TOOLBOX_ROOT}/Cubelet/config/config.toml"
  test -f "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
  test -f "${TOOLBOX_ROOT}/cube-shim/conf/config-cube.toml"
  check_bind_mount_source_file "${TOOLBOX_ROOT}/cubeproxy/global.conf"
  check_bind_mount_source_file "${TOOLBOX_ROOT}/cubeproxy/nginx.conf"
  check_bind_mount_source_file "${TOOLBOX_ROOT}/coredns/Corefile"
  check_bind_mount_source_file "${TOOLBOX_ROOT}/coredns/resolv.conf.upstream"
  if [[ "${WEB_UI_ENABLE:-1}" == "1" ]]; then
    check_bind_mount_source_file "${TOOLBOX_ROOT}/webui/nginx.generated.conf"
  fi
fi

echo "[quickcheck] OK"
