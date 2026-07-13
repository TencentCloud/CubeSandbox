#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

usage() {
  cat <<'EOF'
Usage:
  sudo ./deploy-manual.sh /path/to/cube-manual-update-*.tar.gz

Environment overrides:
  ONE_CLICK_RUNTIME_DIR      Runtime dir, default: /var/run/cube-sandbox-one-click
  ONE_CLICK_LOG_DIR          Log dir, default: /var/log/cube-sandbox-one-click
  ONE_CLICK_MANUAL_PACKAGE_TAR
                             Package path if positional arg is omitted
  ONE_CLICK_SKIP_QUICKCHECK  Set to 1 to skip quickcheck after restart

Behavior:
  - backup current systemd-managed core binaries for the installed role
  - extract package and replace binaries
  - restart local systemd-managed core services
  - run quickcheck and print key status
EOF
}

resolve_package_path() {
  local arg_path="${1:-}"
  if [[ -n "${arg_path}" ]]; then
    printf '%s\n' "${arg_path}"
    return 0
  fi
  if [[ -n "${ONE_CLICK_MANUAL_PACKAGE_TAR:-}" ]]; then
    printf '%s\n' "${ONE_CLICK_MANUAL_PACKAGE_TAR}"
    return 0
  fi

  local candidate
  for candidate in \
    "${PWD}"/cube-manual-update-*.tar.gz \
    "${SCRIPT_DIR}"/cube-manual-update-*.tar.gz
  do
    if [[ -f "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  return 1
}

restart_core_services() {
  local role="$1"
  local units=()
  local unit

  if [[ "${role}" == "compute" ]]; then
    units=(
      cube-sandbox-network-agent.service
      cube-sandbox-cubelet.service
    )
  else
    units=(
      cube-sandbox-cubemaster.service
      cube-sandbox-cube-api.service
      cube-sandbox-network-agent.service
      cube-sandbox-cubelet.service
    )
  fi

  for unit in "${units[@]}"; do
    log "restart ${unit}"
    systemctl restart "${unit}"
  done
}

main() {
  if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
  fi

  require_root
  require_cmd tar
  require_cmd install
  require_cmd curl
  require_cmd systemctl

  local package_tar
  package_tar="$(resolve_package_path "${1:-}")" || die "manual update package not specified"
  ensure_file "${package_tar}"

  local install_prefix="${CUBE_SANDBOX_INSTALL_ROOT}"
  local runtime_dir="${ONE_CLICK_RUNTIME_DIR:-/var/run/cube-sandbox-one-click}"
  local log_dir="${ONE_CLICK_LOG_DIR:-/var/log/cube-sandbox-one-click}"
  local backup_dir="${install_prefix}/.backup/manual-update-$(date +%Y%m%d-%H%M%S)"
  local runtime_env_file="${install_prefix}/.one-click.env"
  local role
  local work_dir
  work_dir="$(mktemp -d)"
  trap "rm -rf '${work_dir}'" EXIT

  ensure_dir "${install_prefix}"
  ensure_file "${runtime_env_file}"
  ensure_file "${install_prefix}/scripts/one-click/quickcheck.sh"
  load_env_file "${runtime_env_file}"
  role="$(one_click_deploy_role)"

  mkdir -p "${backup_dir}"
  log "backup current binaries to ${backup_dir}"
  if [[ "${role}" != "compute" ]]; then
    cp -a "${install_prefix}/CubeMaster/bin/cubemaster" "${backup_dir}/"
    cp -a "${install_prefix}/CubeMaster/bin/cubemastercli" "${backup_dir}/"
  fi
  cp -a "${install_prefix}/Cubelet/bin/cubelet" "${backup_dir}/"
  cp -a "${install_prefix}/Cubelet/bin/cubecli" "${backup_dir}/"
  cp -a "${install_prefix}/network-agent/bin/network-agent" "${backup_dir}/"
  if [[ -f "${install_prefix}/network-agent/bin/cubevsmapdump" ]]; then
    cp -a "${install_prefix}/network-agent/bin/cubevsmapdump" "${backup_dir}/"
  fi

  log "extract package ${package_tar}"
  tar -xzf "${package_tar}" -C "${work_dir}"

  if [[ "${role}" != "compute" ]]; then
    ensure_file "${work_dir}/cubemaster"
    ensure_file "${work_dir}/cubemastercli"
  fi
  ensure_file "${work_dir}/cubelet"
  ensure_file "${work_dir}/cubecli"
  ensure_file "${work_dir}/network-agent"
  ensure_file "${work_dir}/cubevsmapdump"

  log "replace binaries under ${install_prefix}"
  if [[ "${role}" != "compute" ]]; then
    install -m 0755 "${work_dir}/cubemaster" "${install_prefix}/CubeMaster/bin/cubemaster"
    install -m 0755 "${work_dir}/cubemastercli" "${install_prefix}/CubeMaster/bin/cubemastercli"
  fi
  install -m 0755 "${work_dir}/cubelet" "${install_prefix}/Cubelet/bin/cubelet"
  install -m 0755 "${work_dir}/cubecli" "${install_prefix}/Cubelet/bin/cubecli"
  install -m 0755 "${work_dir}/network-agent" "${install_prefix}/network-agent/bin/network-agent"
  install -m 0755 "${work_dir}/cubevsmapdump" "${install_prefix}/network-agent/bin/cubevsmapdump"
  ln -sf "${install_prefix}/network-agent/bin/cubevsmapdump" /usr/local/bin/cubevsmapdump

  log "restart local systemd services"
  restart_core_services "${role}"

  if [[ "${ONE_CLICK_SKIP_QUICKCHECK:-0}" != "1" ]]; then
    ONE_CLICK_RUNTIME_DIR="${runtime_dir}" \
    ONE_CLICK_LOG_DIR="${log_dir}" \
      "${install_prefix}/scripts/one-click/quickcheck.sh"
  fi

  if [[ "${role}" != "compute" ]]; then
    log "node metadata after restart"
    curl -fsS http://127.0.0.1:8089/internal/meta/nodes || true
    printf '\n'
  fi

  log "manual update complete"
  log "backup dir: ${backup_dir}"
}

main "$@"
