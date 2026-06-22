#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

# Install mode and upgrade-related flags (M3-1/M3-2/M3-3).
#   --mode=install   full reinstall (default; existing config is reset)
#   --mode=upgrade   config-preserving upgrade (requires an existing install)
#   --mode=auto      upgrade when an existing install is detected, else install
# When --mode is omitted and an existing install is detected, the installer
# prompts on a TTY and falls back to a full reinstall (with a warning) when
# running non-interactively.
ONE_CLICK_MODE="${ONE_CLICK_MODE:-}"
ONE_CLICK_ASSUME_YES="${ONE_CLICK_ASSUME_YES:-0}"
ONE_CLICK_ALLOW_DOWNGRADE="${ONE_CLICK_ALLOW_DOWNGRADE:-0}"
ONE_CLICK_ALLOW_ROLE_CHANGE="${ONE_CLICK_ALLOW_ROLE_CHANGE:-0}"

# Parse CLI flags into CLI_* globals (supports both `--flag=value` and
# `--flag value`). The values are applied to the canonical variables here AND
# re-applied after the .env file is sourced below, establishing the precedence:
#   CLI flags > .env file > process environment > built-in defaults.
one_click_parse_args "$@"

apply_cli_overrides() {
  [[ -n "${CLI_MODE}" ]] && ONE_CLICK_MODE="${CLI_MODE}"
  [[ -n "${CLI_ASSUME_YES}" ]] && ONE_CLICK_ASSUME_YES="${CLI_ASSUME_YES}"
  [[ -n "${CLI_ALLOW_DOWNGRADE}" ]] && ONE_CLICK_ALLOW_DOWNGRADE="${CLI_ALLOW_DOWNGRADE}"
  [[ -n "${CLI_ALLOW_ROLE_CHANGE}" ]] && ONE_CLICK_ALLOW_ROLE_CHANGE="${CLI_ALLOW_ROLE_CHANGE}"
  [[ -n "${CLI_NODE_IP}" ]] && export CUBE_SANDBOX_NODE_IP="${CLI_NODE_IP}"
  return 0
}
apply_cli_overrides

case "${ONE_CLICK_MODE}" in
  ""|install|upgrade|auto) ;;
  *) die "unsupported --mode: ${ONE_CLICK_MODE} (expected install|upgrade|auto)" ;;
esac

require_root

ENV_FILE="${ONE_CLICK_ENV_FILE:-${SCRIPT_DIR}/.env}"
if [[ -f "${ENV_FILE}" ]]; then
  load_env_file "${ENV_FILE}"
  # CLI flags must win over .env values: load_env_file uses `set -a; source`,
  # which would otherwise clobber the CLI-provided values set above.
  apply_cli_overrides
  case "${ONE_CLICK_MODE}" in
    ""|install|upgrade|auto) ;;
    *) die "unsupported --mode: ${ONE_CLICK_MODE} (expected install|upgrade|auto)" ;;
  esac
fi

DEPLOY_ROLE="$(one_click_deploy_role)"

# ---- External MySQL / Redis support ----
# Set CUBE_EXTERNAL_MYSQL_HOST / CUBE_EXTERNAL_REDIS_HOST to use external
# services instead of the bundled local Docker containers. Defaults are filled
# after the optional upgrade env merge so they are based on the final runtime
# configuration.
init_external_dep_defaults() {
  CUBE_EXTERNAL_MYSQL_HOST="${CUBE_EXTERNAL_MYSQL_HOST:-}"
  CUBE_EXTERNAL_MYSQL_PORT="${CUBE_EXTERNAL_MYSQL_PORT:-3306}"
  CUBE_EXTERNAL_MYSQL_USER="${CUBE_EXTERNAL_MYSQL_USER:-cube}"
  CUBE_EXTERNAL_MYSQL_PASSWORD="${CUBE_EXTERNAL_MYSQL_PASSWORD:-cube_pass}"
  # Default the external DB name from CUBE_SANDBOX_MYSQL_DB so it resolves to the
  # same value up-with-deps.sh derives independently. Otherwise a custom
  # CUBE_SANDBOX_MYSQL_DB (without an explicit CUBE_EXTERNAL_MYSQL_DB) would make
  # the persisted .one-click.env and the seed step disagree on the database name.
  CUBE_EXTERNAL_MYSQL_DB="${CUBE_EXTERNAL_MYSQL_DB:-${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}}"

  # Mirrors the MySQL behaviour above (patch conf.yaml, persist env, mask local
  # redis unit).
  CUBE_EXTERNAL_REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-}"
  CUBE_EXTERNAL_REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT:-6379}"
  CUBE_EXTERNAL_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD:-ceuhvu123}"
}

# Guard against shipping the example/default credentials to a real external
# server. The defaults (cube_pass / ceuhvu123) are published in env.example and
# are trivially guessable, so warn loudly when an external endpoint is wired up
# without overriding them.
warn_default_external_credentials() {
  if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" && "${CUBE_EXTERNAL_MYSQL_PASSWORD}" == "cube_pass" ]]; then
    log "WARNING: external MySQL (${CUBE_EXTERNAL_MYSQL_HOST}) configured with the default password 'cube_pass'."
    log "WARNING: set CUBE_EXTERNAL_MYSQL_PASSWORD to a strong value in your .env before exposing this deployment."
  fi
  if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" && "${CUBE_EXTERNAL_REDIS_PASSWORD}" == "ceuhvu123" ]]; then
    log "WARNING: external Redis (${CUBE_EXTERNAL_REDIS_HOST}) configured with the default password 'ceuhvu123'."
    log "WARNING: set CUBE_EXTERNAL_REDIS_PASSWORD to a strong value in your .env before exposing this deployment."
  fi
}

TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
INSTALL_PREFIX="${ONE_CLICK_INSTALL_PREFIX:-${TOOLBOX_ROOT}}"

# Resolve install vs upgrade mode and, for upgrades, run preflight + backup and
# build the config-preserving merged env BEFORE any destructive change. The
# merged env is sourced so the rest of the installer operates with the user's
# existing values (ports, CIDR, node IP, role, ...).
PACKAGE_TAR="${ONE_CLICK_PACKAGE_TAR:-${SCRIPT_DIR}/assets/package/sandbox-package.tar.gz}"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

INSTALL_MODE="$(resolve_install_mode "${ONE_CLICK_MODE}" "${INSTALL_PREFIX}" "${ONE_CLICK_ASSUME_YES}")"
log "install mode: ${INSTALL_MODE}"

MERGED_ENV=""
ENV_DIFF_FILE=""
UPGRADE_BACKUP_DIR=""
if [[ "${INSTALL_MODE}" == "upgrade" ]]; then
  RUNTIME_ENV_OLD="${INSTALL_PREFIX}/.one-click.env"
  ensure_file "${RUNTIME_ENV_OLD}"

  preflight_upgrade \
    "${INSTALL_PREFIX}" \
    "${SCRIPT_DIR}" \
    "${PACKAGE_TAR}" \
    "${DEPLOY_ROLE}" \
    "${ONE_CLICK_ALLOW_ROLE_CHANGE}" \
    "${ONE_CLICK_ALLOW_DOWNGRADE}"

  # Build the merged env into WORK_DIR (the on-disk config backup is taken later,
  # only after all fail-fast preflights pass, to avoid leaving stray backups).
  MERGED_ENV="${WORK_DIR}/merged.env"
  ENV_DIFF_FILE="${WORK_DIR}/env-diff.txt"

  MERGE_NEW_DOTENV=""
  [[ -f "${ENV_FILE}" ]] && MERGE_NEW_DOTENV="${ENV_FILE}"
  MERGE_OLD_BASELINE=""
  [[ -f "${INSTALL_PREFIX}/env.example" ]] && MERGE_OLD_BASELINE="${INSTALL_PREFIX}/env.example"

  merge_env_three_way \
    "${SCRIPT_DIR}/env.example" \
    "${RUNTIME_ENV_OLD}" \
    "${MERGE_OLD_BASELINE}" \
    "${MERGE_NEW_DOTENV}" \
    "${MERGED_ENV}" \
    "${ENV_DIFF_FILE}"
  if [[ -z "${MERGE_OLD_BASELINE}" ]]; then
    log "note: no env.example baseline from the previous install; used two-way merge. Future upgrades will use a full three-way merge."
  fi

  # Override bundle/default values with the merged (old-priority) env so the
  # rest of the installer keeps the user's existing configuration.
  load_env_file "${MERGED_ENV}"
  DEPLOY_ROLE="$(one_click_deploy_role)"
fi

init_external_dep_defaults

CUBE_PVM_ENABLE="${CUBE_PVM_ENABLE:-0}"
case "${CUBE_PVM_ENABLE}" in
  0|1) ;;
  *) die "unsupported CUBE_PVM_ENABLE: ${CUBE_PVM_ENABLE} (expected 0 or 1)" ;;
esac

print_path_hint() {
  {
    echo
    echo "[one-click] Installed public commands in /usr/local/bin:"
    echo "[one-click]   cube-runtime"
    echo "[one-click]   containerd-shim-cube-rs"
    echo "[one-click]   cubecli"
    echo "[one-click]   cubevsmapdump"
    if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
      echo "[one-click]   cubemastercli"
    fi
    echo
  } >&2
}

detect_installed_role() {
  if [[ ! -f "${INSTALL_PREFIX}/.one-click.env" ]]; then
    return 0
  fi

  sed -n '/^ONE_CLICK_DEPLOY_ROLE=/{s/^ONE_CLICK_DEPLOY_ROLE=//;p;q;}' "${INSTALL_PREFIX}/.one-click.env" 2>/dev/null || true
}

needs_docker_for_install() {
  if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
    return 0
  fi

  local installed_role
  installed_role="$(detect_installed_role)"
  [[ -n "${installed_role}" && "${installed_role}" != "compute" ]]
}

require_any_cmd() {
  local cmd
  for cmd in "$@"; do
    if command -v "${cmd}" >/dev/null 2>&1; then
      return 0
    fi
  done
  die "requires one of commands: $*"
}

install_required_dependencies() {
  log "checking and installing dependencies..."

  if needs_docker_for_install; then
    install_docker
    install_docker_compose
  fi
}

check_dns_preflight() {
  # up-dns/down-dns parse resolv.conf via awk.
  require_cmd awk

  if command -v resolvectl >/dev/null 2>&1; then
    return 0
  fi

  require_cmd systemctl
  local nm_load_state
  nm_load_state="$(systemctl show -p LoadState --value NetworkManager 2>/dev/null || true)"
  [[ "${nm_load_state}" == "loaded" ]] || die "DNS setup requires resolvectl or NetworkManager"

  if ! command -v dnsmasq >/dev/null 2>&1; then
    require_any_cmd dnf yum apt-get
  fi
}

check_proxy_cert_preflight() {
  # mkcert is bundled inside the release package (support/bin/mkcert).
  # up-cube-proxy will copy it to /usr/local/bin/mkcert when not already present.
  :
}

restore_selinux_contexts() {
  command -v restorecon >/dev/null 2>&1 || return 0
  if command -v selinuxenabled >/dev/null 2>&1; then
    selinuxenabled || return 0
  elif [[ ! -d /sys/fs/selinux ]]; then
    return 0
  fi

  log "restoring SELinux contexts under ${INSTALL_PREFIX}"
  restorecon -R "${INSTALL_PREFIX}"
}

one_click_runtime_file_paths() {
  [[ "${DEPLOY_ROLE}" != "compute" ]] || return 0

  printf '%s\n' \
    "${INSTALL_PREFIX}/cubeproxy/global.conf" \
    "${INSTALL_PREFIX}/cubeproxy/nginx.conf" \
    "${INSTALL_PREFIX}/webui/nginx.generated.conf" \
    "${INSTALL_PREFIX}/coredns/Corefile" \
    "${INSTALL_PREFIX}/coredns/resolv.conf.upstream"
}

check_runtime_file_paths_not_directories() {
  local path
  while IFS= read -r path; do
    [[ -n "${path}" ]] || continue
    if [[ ! -d "${path}" ]]; then
      continue
    fi
    if rmdir "${path}" 2>/dev/null; then
      log "removed empty directory at runtime file path: ${path}"
      continue
    fi
    die "runtime file path is a non-empty directory: ${path}; move it away and retry"
  done < <(one_click_runtime_file_paths)
}

generate_cubemaster_config_ports() {
  [[ "${DEPLOY_ROLE}" != "compute" ]] || return 0

  local cfg="${PKG_ROOT}/CubeMaster/conf.yaml"
  local mysql_port="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
  local redis_port="${CUBE_SANDBOX_REDIS_PORT:-6379}"

  ensure_file "${cfg}"
  sed -i \
    -e "s|__CUBE_SANDBOX_MYSQL_PORT__|${mysql_port}|g" \
    -e "s|__CUBE_SANDBOX_REDIS_PORT__|${redis_port}|g" \
    "${cfg}"
}

# When external MySQL/Redis is configured, patch CubeMaster conf.yaml to replace
# the default 127.0.0.1 endpoints with the external connection details. Must run
# after generate_cubemaster_config_ports so the port placeholders are resolved.
patch_cubemaster_external_deps() {
  [[ "${DEPLOY_ROLE}" != "compute" ]] || return 0

  local cfg="${PKG_ROOT}/CubeMaster/conf.yaml"

  # Validate once up front; both branches patch the same file.
  if [[ -z "${CUBE_EXTERNAL_MYSQL_HOST}" && -z "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
    return 0
  fi
  ensure_file "${cfg}"

  if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" ]]; then
    log "patching conf.yaml for external MySQL: ${CUBE_EXTERNAL_MYSQL_HOST}:${CUBE_EXTERNAL_MYSQL_PORT}/${CUBE_EXTERNAL_MYSQL_DB}"
    # SECURITY: escape user-supplied values for the sed '|' delimiter so that a
    # '|', '\', '&' or '"' in a host/user/password does not corrupt conf.yaml
    # or break the double-quoted sed replacement strings below.
    local mysql_addr_esc mysql_user_esc mysql_pwd_esc mysql_db_esc
    mysql_addr_esc="$(escape_sed "${CUBE_EXTERNAL_MYSQL_HOST}:${CUBE_EXTERNAL_MYSQL_PORT}")"
    mysql_user_esc="$(escape_sed "${CUBE_EXTERNAL_MYSQL_USER}")"
    mysql_pwd_esc="$(escape_sed "${CUBE_EXTERNAL_MYSQL_PASSWORD}")"
    mysql_db_esc="$(escape_sed "${CUBE_EXTERNAL_MYSQL_DB}")"
    # Match only on the YAML key prefix ('addr:'/'user:'/'pwd:'/'db_name:') and
    # accept any current value, so these patterns keep working even if the
    # conf.yaml template is regenerated with different defaults. These keys only
    # appear in the MySQL sections (ossdb_config/instance_db_config), so without
    # a trailing 'g' flag each line is patched exactly once and Redis fields
    # (nodes:/password:) are never touched.
    sed -i \
      -e "s|addr: \".*\"|addr: \"${mysql_addr_esc}\"|" \
      -e "s|user: \".*\"|user: \"${mysql_user_esc}\"|" \
      -e "s|pwd: \".*\"|pwd: \"${mysql_pwd_esc}\"|" \
      -e "s|db_name: \".*\"|db_name: \"${mysql_db_esc}\"|" \
      "${cfg}"
  fi

  if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
    log "patching conf.yaml for external Redis: ${CUBE_EXTERNAL_REDIS_HOST}:${CUBE_EXTERNAL_REDIS_PORT}"
    local redis_nodes_esc redis_pwd_esc
    redis_nodes_esc="$(escape_sed "${CUBE_EXTERNAL_REDIS_HOST}:${CUBE_EXTERNAL_REDIS_PORT}")"
    redis_pwd_esc="$(escape_sed "${CUBE_EXTERNAL_REDIS_PASSWORD}")"
    # Match only on the YAML key prefix so these patterns survive template
    # default changes. 'nodes:'/'password:' only appear in the redis* sections,
    # so every Redis endpoint is repointed while MySQL fields stay untouched.
    sed -i \
      -e "s|nodes: \".*\"|nodes: \"${redis_nodes_esc}\"|" \
      -e "s|password: \".*\"|password: \"${redis_pwd_esc}\"|" \
      "${cfg}"
  fi
}

# Fail fast when an external MySQL/Redis endpoint is unreachable or rejects the
# configured credentials. Without this, a misconfigured host/port/password only
# surfaces much later during up-with-deps.sh seeding. The check is best-effort:
# if the corresponding client binary is missing we skip rather than block, since
# the seed step (which requires the client) runs later anyway.
check_external_deps_preflight() {
  local connect_timeout="${ONE_CLICK_EXTERNAL_DEP_TIMEOUT:-5}"

  if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" ]]; then
    if command -v mysqladmin >/dev/null 2>&1; then
      log "checking connectivity to external MySQL ${CUBE_EXTERNAL_MYSQL_HOST}:${CUBE_EXTERNAL_MYSQL_PORT}"
      local mysql_cnf
      # SECURITY: tighten umask before mktemp so the credential file is created
      # 0600 from the start -- this closes the brief race window between mktemp's
      # default (umask-derived) permissions and the chmod 600 below.
      local old_umask
      old_umask="$(umask)"
      umask 077
      mysql_cnf="$(mktemp)"
      umask "${old_umask}"
      # SECURITY: trap on EXIT so the plaintext password is removed even if the
      # script is killed abruptly between here and the explicit rm-f below.
      # Mirrors the trap pattern in up-with-deps.sh.
      trap 'rm -f "${mysql_cnf}"' EXIT
      chmod 600 "${mysql_cnf}"
      cat > "${mysql_cnf}" <<EOF
[client]
password="${CUBE_EXTERNAL_MYSQL_PASSWORD}"
EOF
      if ! mysqladmin --defaults-extra-file="${mysql_cnf}" \
          -h "${CUBE_EXTERNAL_MYSQL_HOST}" \
          -P "${CUBE_EXTERNAL_MYSQL_PORT}" \
          -u "${CUBE_EXTERNAL_MYSQL_USER}" \
          --connect-timeout="${connect_timeout}" ping >/dev/null 2>&1; then
        rm -f "${mysql_cnf}"
        trap - EXIT
        die "cannot reach external MySQL at ${CUBE_EXTERNAL_MYSQL_HOST}:${CUBE_EXTERNAL_MYSQL_PORT} as user '${CUBE_EXTERNAL_MYSQL_USER}'.
  Verify CUBE_EXTERNAL_MYSQL_HOST / _PORT / _USER / _PASSWORD and that the server is reachable from this host."
      fi
      rm -f "${mysql_cnf}"
      trap - EXIT
      log "external MySQL connectivity OK"
    else
      log "mysqladmin not found; skipping external MySQL connectivity preflight"
    fi
  fi

  if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
    if command -v redis-cli >/dev/null 2>&1; then
      log "checking connectivity to external Redis ${CUBE_EXTERNAL_REDIS_HOST}:${CUBE_EXTERNAL_REDIS_PORT}"
      local redis_reply
      if [[ -n "${CUBE_EXTERNAL_REDIS_PASSWORD}" ]]; then
        # SECURITY: PING is NOT an authenticated command. A reachable server that
        # has no 'requirepass' set answers PONG even when a (wrong/extraneous)
        # password is configured, so a misconfigured credential would slip
        # through this preflight and only surface much later when CubeMaster /
        # cube-proxy actually try to use Redis. Validate the credential directly
        # by issuing AUTH and requiring an "OK" reply.
        #
        # The password is fed via stdin (`-x AUTH`) rather than as a command-line
        # argument so it is not exposed in /proc/<pid>/cmdline to other local
        # users; --no-auth-warning keeps redis-cli from echoing it on stderr.
        # --connect-timeout bounds the TCP handshake; --timeout bounds Redis
        # protocol I/O so a middlebox that accepts the socket but stalls the
        # response (broken proxy / overloaded server) cannot hang this preflight
        # indefinitely.
        redis_reply="$(printf '%s' "${CUBE_EXTERNAL_REDIS_PASSWORD}" | redis-cli \
          -h "${CUBE_EXTERNAL_REDIS_HOST}" \
          -p "${CUBE_EXTERNAL_REDIS_PORT}" \
          --connect-timeout "${connect_timeout}" \
          --timeout "${connect_timeout}" \
          --no-auth-warning \
          -x AUTH 2>&1 || true)"
        if [[ "${redis_reply}" != "OK" ]]; then
          die "external Redis at ${CUBE_EXTERNAL_REDIS_HOST}:${CUBE_EXTERNAL_REDIS_PORT} is unreachable or rejected the configured password (AUTH replied: ${redis_reply:-<no response>}).
  Verify CUBE_EXTERNAL_REDIS_HOST / _PORT / _PASSWORD and that the server is reachable from this host."
        fi
      else
        local redis_pong
        redis_pong="$(redis-cli \
          -h "${CUBE_EXTERNAL_REDIS_HOST}" \
          -p "${CUBE_EXTERNAL_REDIS_PORT}" \
          --connect-timeout "${connect_timeout}" \
          --timeout "${connect_timeout}" ping 2>/dev/null || true)"
        if [[ "${redis_pong}" != "PONG" ]]; then
          die "cannot reach external Redis at ${CUBE_EXTERNAL_REDIS_HOST}:${CUBE_EXTERNAL_REDIS_PORT} (PING did not return PONG).
  Verify CUBE_EXTERNAL_REDIS_HOST / _PORT and that the server is reachable from this host."
        fi
      fi
      log "external Redis connectivity OK"
    else
      log "redis-cli not found; skipping external Redis connectivity preflight"
    fi
  fi
}

check_hardware_preflight() {
  if [[ ! -e /dev/kvm ]]; then
    log "KVM is not supported or not enabled (/dev/kvm not found)."
    log ""
    log "If this host cannot expose hardware KVM (for example, it is itself a"
    log "virtual machine without nested virtualization), you can try the"
    log "open-source PVM stack shipped under deploy/pvm/ to turn the current"
    log "guest into a PVM host that provides /dev/kvm to CubeSandbox:"
    log ""
    log "    sudo bash deploy/pvm/pvm_setup.sh"
    log ""
    log "That script will build and install a PVM-enabled host kernel, build a"
    log "matching PVM guest vmlinux, and guide you through the reboot needed to"
    log "switch into the new kernel. After reboot, re-run this installer."
    log ""
    log "WARNING: the open-source kvm-pvm integration is intended for"
    log "development, evaluation and self-built experiments only. It is NOT"
    log "suitable for production workloads -- expect reduced performance,"
    log "limited hardware coverage and no long-term support guarantees."
    die "KVM is not supported or not enabled (/dev/kvm not found)."
  fi

  local mem_total_kb
  mem_total_kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  
  local min_mem_kb=7500000
  if [[ -n "${CUBE_MIN_MEMORY_KB:-}" ]]; then
    if [[ "${CUBE_MIN_MEMORY_KB}" =~ ^[0-9]+$ ]] && [[ "${CUBE_MIN_MEMORY_KB}" -gt 0 ]]; then
      # Enforce that the threshold cannot be lower than the default 8GB (7500000 KB) in the authoritative installer
      if [[ "${CUBE_MIN_MEMORY_KB}" -ge 7500000 ]]; then
        min_mem_kb="${CUBE_MIN_MEMORY_KB}"
      fi
    else
      die "Invalid CUBE_MIN_MEMORY_KB '${CUBE_MIN_MEMORY_KB}' (must be a positive integer greater than 0)."
    fi
  fi

  if [[ "${mem_total_kb}" -lt "${min_mem_kb}" ]]; then
    die "System memory must be at least $((min_mem_kb / 1024 / 1024))GB (found $((mem_total_kb / 1024 / 1024)) GB)."
  fi
}

# Check PVM host consistency: if the kvm_pvm kernel module is loaded,
# CUBE_PVM_ENABLE must be set to 1. Otherwise the installer will use the
# ordinary guest kernel (vmlinux) instead of the PVM-optimized one
# (vmlinux-pvm), which causes VM template creation to fail later with
# minimal error messages.
#
# This check runs after check_hardware_preflight (which validates /dev/kvm)
# and before any filesystem or cgroup checks, so the user gets a clear
# fail-fast message before the installer touches the system.
check_pvm_consistency_preflight() {
  local has_kvm_pvm=0
  if lsmod 2>/dev/null | grep -qE '^kvm_pvm[[:space:]]'; then
    has_kvm_pvm=1
  fi

  # Not a PVM host — nothing to check.
  if [[ "${has_kvm_pvm}" -eq 0 ]]; then
    return 0
  fi

  # PVM host detected, CUBE_PVM_ENABLE is already set correctly.
  if [[ "${CUBE_PVM_ENABLE}" == "1" ]]; then
    log "PVM host detected (kvm_pvm loaded) and CUBE_PVM_ENABLE=1 — proceeding with PVM guest kernel."
    return 0
  fi

  # PVM host detected but CUBE_PVM_ENABLE is NOT set to 1.
  # This is the dangerous case: the installer will use the ordinary guest
  # kernel, and VM template creation will fail later.

  cat >&2 <<'EOF'

╔══════════════════════════════════════════════════════════════════╗
║  [!!] PVM HOST DETECTED -- CUBE_PVM_ENABLE NOT SET             ║
╠══════════════════════════════════════════════════════════════════╣
║                                                                  ║
║  The kvm_pvm kernel module is loaded on this host -- this        ║
║  machine is running as a PVM host.                               ║
║                                                                  ║
║  However, CUBE_PVM_ENABLE is not set to 1. The installer will    ║
║  use the ordinary guest kernel (vmlinux) instead of the PVM-     ║
║  optimized guest kernel (vmlinux-pvm).                           ║
║                                                                  ║
║  [!!] VM template creation will fail with minimal error          ║
║       messages if the wrong guest kernel is used.                ║
║                                                                  ║
║  Solution: re-run with CUBE_PVM_ENABLE=1:                        ║
║                                                                  ║
║    CUBE_PVM_ENABLE=1 ./install.sh                                ║
║                                                                  ║
║  To bypass this check (not recommended):                         ║
║                                                                  ║
║    ONE_CLICK_SKIP_PVM_CHECK=1 ./install.sh                       ║
║                                                                  ║
║  Docs: https://cubesandbox.com/guide/pvm-deploy.html             ║
║                                                                  ║
╚══════════════════════════════════════════════════════════════════╝

EOF

  # Check if the user has explicitly opted to skip this check.
  if [[ "${ONE_CLICK_SKIP_PVM_CHECK:-0}" == "1" ]]; then
    log "ONE_CLICK_SKIP_PVM_CHECK=1 — bypassing PVM consistency check (not recommended)."
    return 0
  fi

  # Non-interactive environment: fail fast with a clear error.
  if [[ ! -t 0 ]]; then
    die "PVM host detected but CUBE_PVM_ENABLE is not 1, and stdin is not a terminal.
Re-run with CUBE_PVM_ENABLE=1 to use the PVM guest kernel, or set
ONE_CLICK_SKIP_PVM_CHECK=1 to bypass this check (not recommended).
See: https://cubesandbox.com/guide/pvm-deploy.html"
  fi

  # Interactive: ask the user to confirm.
  printf '\n%s' "Proceed WITHOUT PVM guest kernel support? This may cause VM template failures. [y/N]: "
  read -r reply
  case "${reply}" in
    [Yy]|[Yy][Ee][Ss])
      log "User acknowledged the risk — proceeding with ordinary guest kernel on PVM host."
      ;;
    *)
      die "Installation aborted. Re-run with CUBE_PVM_ENABLE=1 to use the PVM guest kernel.
See: https://cubesandbox.com/guide/pvm-deploy.html"
      ;;
  esac
}

check_cubelet_fs_preflight() {
  local cubelet_dir="/data/cubelet"

  # Walk up to find the nearest existing ancestor so we can query its filesystem.
  # This covers the case where /data/cubelet (or even /data) does not yet exist.
  local check_path="${cubelet_dir}"
  while [[ ! -e "${check_path}" ]]; do
    local parent
    parent="$(dirname "${check_path}")"
    [[ "${parent}" != "${check_path}" ]] || break
    check_path="${parent}"
  done

  local fs_type
  fs_type="$(df -T "${check_path}" 2>/dev/null | awk 'NR==2 {print $2}')"

  if [[ "${fs_type}" == "xfs" ]]; then
    return 0
  fi

  if [[ -d "${cubelet_dir}" ]] && mountpoint -q "${cubelet_dir}" 2>/dev/null; then
    die "/data/cubelet is a mount point but its filesystem type is '${fs_type}' (requires xfs).
  Please format the underlying partition as XFS and remount it at /data/cubelet:
    mkfs.xfs /dev/<your-partition>
    mount /dev/<your-partition> /data/cubelet
  Troubleshooting: https://github.com/TencentCloud/CubeSandbox/issues/311"
  else
    die "The filesystem that will host /data/cubelet is on '${check_path}' (type: ${fs_type:-unknown}), which is not XFS.
  Cube Sandbox requires the /data/cubelet directory to reside on an XFS filesystem.
  Options:
    1. Mount a dedicated XFS-formatted partition at /data/cubelet:
         mkfs.xfs /dev/<your-partition>
         mount /dev/<your-partition> /data/cubelet
    2. Ensure the parent path (${check_path}) itself is on XFS.
  Troubleshooting: https://github.com/TencentCloud/CubeSandbox/issues/311"
  fi
}

check_cgroup_cpu_preflight() {
  local cgroot="/sys/fs/cgroup"
  local fstype
  fstype="$(stat -fc %T "${cgroot}" 2>/dev/null || echo unknown)"

  # cgroup v1 systems still work via the v1 handle in cubelet; only validate
  # cgroup v2 hosts here (which is what every recent distro defaults to).
  if [[ "${fstype}" != "cgroup2fs" ]]; then
    return 0
  fi

  local controllers=""
  if [[ -r "${cgroot}/cgroup.controllers" ]]; then
    controllers="$(cat "${cgroot}/cgroup.controllers" 2>/dev/null || true)"
  fi
  if ! grep -qw cpu <<<"${controllers}"; then
    die "Kernel cgroup v2 does not expose the 'cpu' controller (cgroup.controllers='${controllers:-<empty>}').
  cubelet cannot set CPU quotas without it.
  See: https://github.com/TencentCloud/CubeSandbox/issues/366"
  fi

  local subtree=""
  if [[ -r "${cgroot}/cgroup.subtree_control" ]]; then
    subtree="$(cat "${cgroot}/cgroup.subtree_control" 2>/dev/null || true)"
  fi
  if grep -qw cpu <<<"${subtree}"; then
    return 0
  fi

  log "cgroup v2 'cpu' controller not enabled on ${cgroot}/cgroup.subtree_control; trying to enable it"
  if printf '+cpu\n' >"${cgroot}/cgroup.subtree_control" 2>/dev/null; then
    log "enabled '+cpu' on ${cgroot}/cgroup.subtree_control"
    return 0
  fi

  die "Failed to enable the cgroup v2 'cpu' controller on ${cgroot}/cgroup.subtree_control.
  On Ubuntu / Debian this is usually caused by 'multipathd' (or another service) running real-time threads under the root cgroup, which blocks '+cpu' with 'Invalid argument'.
  Quick fix:
    systemctl disable --now multipathd.service multipathd.socket
    echo +cpu > ${cgroot}/cgroup.subtree_control
  Full repro and fix: https://github.com/TencentCloud/CubeSandbox/issues/366"
}

check_install_preflight() {
  # install.sh itself.
  require_cmd tar
  require_cmd ss
  require_cmd systemctl

  # runtime common helpers used by up/down scripts.
  require_cmd bash
  require_cmd curl
  require_cmd sed
  require_cmd grep
  require_cmd pgrep
  require_cmd date

  if needs_docker_for_install; then
    require_cmd docker
  fi

  # tencent mirror path may mutate /etc/docker/daemon.json via python3.
  if needs_docker_for_install && [[ "${ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR:-0}" == "1" && -f /etc/docker/daemon.json ]]; then
    require_cmd python3
  fi

  if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
    # control role executes up-with-deps -> up-cube-proxy/up-dns.
    require_cmd ip
    check_proxy_cert_preflight
    check_dns_preflight
  fi
}

select_installed_kernel_vmlinux() {
  local kernel_dir="${INSTALL_PREFIX}/cube-kernel-scf"
  local target="vmlinux-bm"

  if [[ "${CUBE_PVM_ENABLE}" == "1" ]]; then
    target="vmlinux-pvm"
  fi

  ensure_file "${kernel_dir}/${target}"
  ln -sfn "${target}" "${kernel_dir}/vmlinux"
  if [[ "${target}" == "vmlinux-pvm" ]]; then
    log "CUBE_PVM_ENABLE=1, selected PVM guest kernel: ${kernel_dir}/vmlinux -> ${target}"
  else
    log "selected ordinary guest kernel: ${kernel_dir}/vmlinux -> ${target}"
  fi
}

configure_tencent_docker_mirror() {
  local enable_mirror="${ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR:-0}"
  local mirror_url="${ONE_CLICK_TENCENT_DOCKER_MIRROR_URL:-https://mirror.ccs.tencentyun.com}"
  local daemon_json="/etc/docker/daemon.json"

  if [[ "${enable_mirror}" != "1" ]]; then
    return 0
  fi

  mkdir -p /etc/docker
  if [[ ! -f "${daemon_json}" ]]; then
    cat >"${daemon_json}" <<EOF
{
  "registry-mirrors": [
    "${mirror_url}"
  ]
}
EOF
  else
    require_cmd python3
    python3 - "${daemon_json}" "${mirror_url}" <<'PY'
import json
import sys
from pathlib import Path

daemon_path = Path(sys.argv[1])
mirror = sys.argv[2]
raw = daemon_path.read_text(encoding="utf-8").strip()
data = json.loads(raw) if raw else {}
mirrors = data.get("registry-mirrors", [])
if isinstance(mirrors, str):
    mirrors = [mirrors]
elif not isinstance(mirrors, list):
    mirrors = []
if mirror not in mirrors:
    mirrors.append(mirror)
data["registry-mirrors"] = mirrors
daemon_path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
PY
  fi

  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl restart docker || die "failed to restart docker"
  else
    service docker restart || die "failed to restart docker"
  fi
}

systemd_target_for_role() {
  local role="$1"
  case "${role}" in
    control)
      printf '%s\n' "cube-sandbox-control.target"
      ;;
    compute)
      printf '%s\n' "cube-sandbox-compute.target"
      ;;
    *)
      die "unsupported role for systemd target: ${role}"
      ;;
  esac
}

stop_existing_systemd_deployment() {
  # Disable + stop the targets first; PartOf= on each child service is
  # supposed to cascade the stop. In practice, units that are stuck in
  # `failed` or `activating` state don't always get cleaned up by the
  # target stop alone — typically `cube-sandbox-cube-egress-net.service`
  # blocked on a missing cube-dev interface, leaving its requirer
  # `cube-sandbox-cube-egress.service` perpetually inactive.
  #
  # So we belt-and-suspenders explicitly stop every cube-sandbox-*
  # service afterwards, which both forces failed units back to inactive
  # and guarantees the next `enable --now <target>` actually re-runs
  # ExecStart instead of returning a "no-op, already active" exit 0.
  systemctl disable --now \
    cube-sandbox-control.target \
    cube-sandbox-compute.target >/dev/null 2>&1 || true
  systemctl reset-failed 'cube-sandbox-*.service' >/dev/null 2>&1 || true
  systemctl stop 'cube-sandbox-*.service' >/dev/null 2>&1 || true
}

stop_existing_legacy_deployment() {
  # Legacy bridge for upgrading pre-systemd one-click installs.
  # New installs are systemd-only; this path only stops old nohup/pidfile deployments
  # before the install prefix is replaced.
  local installed_role="$1"
  local legacy_stop_script=""

  if [[ "${installed_role}" == "compute" && -x "${INSTALL_PREFIX}/scripts/one-click/down-compute.sh" ]]; then
    legacy_stop_script="${INSTALL_PREFIX}/scripts/one-click/down-compute.sh"
  elif [[ -x "${INSTALL_PREFIX}/scripts/one-click/down-with-deps.sh" ]]; then
    legacy_stop_script="${INSTALL_PREFIX}/scripts/one-click/down-with-deps.sh"
  fi

  if [[ -n "${legacy_stop_script}" ]]; then
    log "stopping legacy pre-systemd deployment under ${INSTALL_PREFIX}"
    ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
    ONE_CLICK_RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env" \
      "${legacy_stop_script}" || true
  fi
}

install_systemd_units() {
  local install_units_script="${INSTALL_PREFIX}/scripts/systemd/install-units.sh"
  ensure_file "${install_units_script}"
  ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
  ONE_CLICK_RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env" \
    "${install_units_script}"
}

start_systemd_target() {
  local target
  target="$(systemd_target_for_role "${DEPLOY_ROLE}")"
  systemctl disable --now \
    cube-sandbox-control.target \
    cube-sandbox-compute.target >/dev/null 2>&1 || true
  systemctl enable --now "${target}"
}

# When external MySQL/Redis is configured, mask the local container systemd
# services so the control target never starts (or restarts) them. The target
# only `Wants` these units, so masking is non-fatal for the rest of the stack.
# Must run after install_systemd_units (units installed + daemon-reload) and
# before start_systemd_target.
#
# install-units.sh installs every unit as a *regular file* under
# /etc/systemd/system. A plain `systemctl mask` cannot overlay its /dev/null
# symlink on top of an existing regular file -- it fails with
# "File ... already exists". The previous implementation swallowed that error,
# so the unit only *appeared* masked and was actually left merely "disabled".
# A disabled-but-present unit is still pulled in by the target's Wants=, so the
# local mysql/redis units would start, their ExecStartPost would wait ~60-80s on
# a container that (correctly) was never started, fail, and Restart=on-failure
# loop -- stalling `systemctl enable --now <target>` for many minutes.
# We therefore remove the installed file first so mask can create a *persistent*
# /dev/null override; a later switch back to local re-installs the real file via
# install-units.sh, whose `install` call replaces the /dev/null mask symlink with
# the real unit (unlink + create, not an atomic rename).
mask_local_dep_service() {
  local unit="$1"
  local unit_dir="${ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR:-/etc/systemd/system}"
  systemctl stop "${unit}" >/dev/null 2>&1 || true
  # Removing the unit file is the primary safeguard; mask is belt-and-suspenders.
  # Keep this tolerant under `set -e` so a rare failure here (e.g. a stray
  # directory left at the path) warns rather than aborting the whole install.
  rm -f "${unit_dir}/${unit}" || true
  if ! systemctl mask "${unit}" >/dev/null 2>&1; then
    # The unit file is already gone, so the target's Wants= just resolves to a
    # missing unit and nothing starts now. The only residual risk is that the
    # mask did not persist, so a later install_systemd_units run could restore it.
    log "WARNING: removed ${unit} but failed to persist its mask; a later re-install may restore it"
  fi
}

mask_external_dep_services() {
  if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" ]]; then
    log "masking local MySQL service (external MySQL at ${CUBE_EXTERNAL_MYSQL_HOST} in use)"
    mask_local_dep_service cube-sandbox-mysql.service
  else
    # Re-enable in case a previous install masked it and the user switched back.
    systemctl unmask cube-sandbox-mysql.service >/dev/null 2>&1 || true
  fi

  if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
    log "masking local Redis service (external Redis at ${CUBE_EXTERNAL_REDIS_HOST} in use)"
    mask_local_dep_service cube-sandbox-redis.service
  else
    systemctl unmask cube-sandbox-redis.service >/dev/null 2>&1 || true
  fi

  systemctl daemon-reload >/dev/null 2>&1 || true
}

# Run critical preflight checks that do not depend on dependency installation first
# to ensure we fail fast before installing or modifying any local system packages.
check_hardware_preflight
check_pvm_consistency_preflight
check_cubelet_fs_preflight
check_cgroup_cpu_preflight
check_glibc_preflight

CUBE_SANDBOX_NODE_IP="$(detect_node_ip)"
export CUBE_SANDBOX_NODE_IP
log "using node IP: ${CUBE_SANDBOX_NODE_IP}"
CUBE_SANDBOX_ETH_NAME="${CUBE_SANDBOX_ETH_NAME:-$(detect_primary_interface || true)}"
if [[ -n "${CUBE_SANDBOX_ETH_NAME}" ]]; then
  export CUBE_SANDBOX_ETH_NAME
  log "using primary network interface: ${CUBE_SANDBOX_ETH_NAME}"
else
  log "primary network interface not detected; keeping packaged Cubelet eth_name"
fi

# Validate the effective cubevs CIDR before installing packages or replacing
# the existing deployment. If unset, use CubeSandbox's fixed packaged default.
CUBE_SANDBOX_NETWORK_CIDR="${CUBE_SANDBOX_NETWORK_CIDR:-}"
# On upgrade the CIDR is the cluster's own (preserved from the old install);
# its existing cubevs bridge/route would self-trigger the host-conflict scan,
# so skip conflict detection (format validation still runs) while still
# honoring an explicit user bypass flag.
cidr_skip_conflict=0
if [[ "${INSTALL_MODE}" == "upgrade" || "${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-0}" == "1" ]]; then
  cidr_skip_conflict=1
fi
if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR}" ]]; then
  check_cidr_preflight "${CUBE_SANDBOX_NETWORK_CIDR}" "${cidr_skip_conflict}" "CUBE_SANDBOX_NETWORK_CIDR"
  export CUBE_SANDBOX_NETWORK_CIDR
else
  check_cidr_preflight "192.168.0.0/18" "${cidr_skip_conflict}" "default CubeSandbox network CIDR"
fi

install_required_dependencies
check_install_preflight
warn_default_external_credentials
check_external_deps_preflight
if needs_docker_for_install; then
  configure_tencent_docker_mirror
fi

ensure_file "${PACKAGE_TAR}"
validate_declared_release_manifest "${SCRIPT_DIR}"

log "extracting package ${PACKAGE_TAR}"
tar -xzf "${PACKAGE_TAR}" -C "${WORK_DIR}"
PKG_ROOT="${WORK_DIR}/sandbox-package"
ensure_dir "${PKG_ROOT}"
validate_cubelet_cow_startup_deps "${PKG_ROOT}/Cubelet/config/config.toml"
patch_cubelet_config_template \
  "${PKG_ROOT}/Cubelet/config/config.toml" \
  "${CUBE_SANDBOX_ETH_NAME:-}" \
  "${CUBE_SANDBOX_NETWORK_CIDR:-}"

installed_role="${DEPLOY_ROLE}"
detected_installed_role="$(detect_installed_role)"
if [[ -n "${detected_installed_role}" ]]; then
  installed_role="${detected_installed_role}"
fi

log "stopping existing systemd deployment under ${INSTALL_PREFIX}"
stop_existing_systemd_deployment
stop_existing_legacy_deployment "${installed_role}"

# Upgrade: snapshot existing config now that all fail-fast preflights have
# passed and right before any destructive change, then stash the env diff.
if [[ "${INSTALL_MODE}" == "upgrade" ]]; then
  UPGRADE_BACKUP_DIR="$(backup_before_upgrade "${INSTALL_PREFIX}")"
  if [[ -n "${ENV_DIFF_FILE}" && -f "${ENV_DIFF_FILE}" ]]; then
    cp -f "${ENV_DIFF_FILE}" "${UPGRADE_BACKUP_DIR}/env-diff.txt"
    log "env merge diff written to ${UPGRADE_BACKUP_DIR}/env-diff.txt"
  fi
fi

if [[ "${INSTALL_PREFIX%/}" == "${TOOLBOX_ROOT%/}" ]]; then
  rm -rf \
    "${INSTALL_PREFIX}/network-agent" \
    "${INSTALL_PREFIX}/CubeAPI" \
    "${INSTALL_PREFIX}/CubeMaster" \
    "${INSTALL_PREFIX}/Cubelet" \
    "${INSTALL_PREFIX}/cubeproxy" \
    "${INSTALL_PREFIX}/coredns" \
    "${INSTALL_PREFIX}/webui" \
    "${INSTALL_PREFIX}/support" \
    "${INSTALL_PREFIX}/systemd" \
    "${INSTALL_PREFIX}/cube-shim" \
    "${INSTALL_PREFIX}/cube-kernel-scf" \
    "${INSTALL_PREFIX}/cube-image" \
    "${INSTALL_PREFIX}/scripts" \
    "${INSTALL_PREFIX}/sql" \
    "${INSTALL_PREFIX}/.one-click.env"
else
  # Full wipe of a custom prefix, but preserve any upgrade backup directory so
  # the config snapshot survives for recovery/rollback.
  wipe_custom_install_prefix_contents "${INSTALL_PREFIX}"
fi

mkdir -p "${INSTALL_PREFIX}"
if [[ "${DEPLOY_ROLE}" == "compute" ]]; then
  copy_dir_contents "${PKG_ROOT}/network-agent" "${INSTALL_PREFIX}/network-agent"
  copy_dir_contents "${PKG_ROOT}/Cubelet" "${INSTALL_PREFIX}/Cubelet"
  copy_dir_contents "${PKG_ROOT}/cube-shim" "${INSTALL_PREFIX}/cube-shim"
  copy_dir_contents "${PKG_ROOT}/cube-kernel-scf" "${INSTALL_PREFIX}/cube-kernel-scf"
  copy_dir_contents "${PKG_ROOT}/cube-image" "${INSTALL_PREFIX}/cube-image"
  copy_dir_contents "${PKG_ROOT}/cube-egress" "${INSTALL_PREFIX}/cube-egress"
  copy_dir_contents "${PKG_ROOT}/systemd" "${INSTALL_PREFIX}/systemd"
  copy_dir_contents "${PKG_ROOT}/scripts" "${INSTALL_PREFIX}/scripts"
else
  generate_cubemaster_config_ports
  patch_cubemaster_external_deps
  cp -a "${PKG_ROOT}/." "${INSTALL_PREFIX}/"
fi

select_installed_kernel_vmlinux

mkdir -p \
  "${INSTALL_PREFIX}/cube-vs/network" \
  "${INSTALL_PREFIX}/cube-snapshot" \
  /data/log/Cubelet \
  /data/log/CubeShim \
  /data/log/CubeVmm \
  /data/cube-shim/disks \
  /data/snapshot_pack/disks

if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
  mkdir -p \
    /data/log/CubeAPI \
    /data/log/CubeMaster \
    /data/log/cube-proxy
fi

RUNTIME_ENV_FILE="${INSTALL_PREFIX}/.one-click.env"
if [[ "${INSTALL_MODE}" == "upgrade" && -n "${MERGED_ENV}" ]]; then
  # Upgrade: write the config-preserving merged env as the runtime env.
  cp -f "${MERGED_ENV}" "${RUNTIME_ENV_FILE}"
elif [[ -f "${ENV_FILE}" ]]; then
  cp -f "${ENV_FILE}" "${RUNTIME_ENV_FILE}"
else
  : > "${RUNTIME_ENV_FILE}"
fi
# SECURITY: this file holds DATABASE_URL and CUBE_EXTERNAL_*_PASSWORD secrets.
# Restrict it to root before any secrets are written so they are never readable
# by other local users. Note that upsert_env_kv rewrites the file via an atomic
# mktemp+mv, which replaces the inode; it sets 0600 on its temp file so this
# mode is preserved across every later upsert rather than reverting to 0644.
chmod 600 "${RUNTIME_ENV_FILE}"

# Install version files so the installed system can report its version.
if [[ -f "${SCRIPT_DIR}/VERSION.txt" ]]; then
  cp -f "${SCRIPT_DIR}/VERSION.txt" "${INSTALL_PREFIX}/VERSION.txt"
  log "installed VERSION.txt to ${INSTALL_PREFIX}/VERSION.txt"
fi
# Persist the env template as a baseline so the NEXT upgrade can perform a full
# three-way merge (distinguishing user-customized values from old defaults).
if [[ -f "${SCRIPT_DIR}/env.example" ]]; then
  cp -f "${SCRIPT_DIR}/env.example" "${INSTALL_PREFIX}/env.example"
  log "installed env.example baseline to ${INSTALL_PREFIX}/env.example"
fi
manifest_rel="$(declared_release_manifest_relpath "${SCRIPT_DIR}/VERSION.txt")"
if [[ -n "${manifest_rel}" ]]; then
  cp -f "${SCRIPT_DIR}/${manifest_rel}" "${INSTALL_PREFIX}/release-manifest.json"
  ensure_file "${INSTALL_PREFIX}/release-manifest.json"
  log "installed ${manifest_rel} to ${INSTALL_PREFIX}/release-manifest.json"
elif [[ -f "${SCRIPT_DIR}/release-manifest.json" ]]; then
  cp -f "${SCRIPT_DIR}/release-manifest.json" "${INSTALL_PREFIX}/release-manifest.json"
  log "installed release-manifest.json to ${INSTALL_PREFIX}/release-manifest.json"
fi
upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_DEPLOY_ROLE" "${DEPLOY_ROLE}"
upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_PVM_ENABLE" "${CUBE_PVM_ENABLE}"
if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_SANDBOX_NODE_IP" "${CUBE_SANDBOX_NODE_IP}"
fi
if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
  validate_interface_name "${CUBE_SANDBOX_ETH_NAME}" "CUBE_SANDBOX_ETH_NAME"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_SANDBOX_ETH_NAME" "${CUBE_SANDBOX_ETH_NAME}"
fi
if [[ -n "${ONE_CLICK_CONTROL_PLANE_IP:-}" ]]; then
  validate_ipv4_literal "${ONE_CLICK_CONTROL_PLANE_IP}" "ONE_CLICK_CONTROL_PLANE_IP"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_CONTROL_PLANE_IP" "${ONE_CLICK_CONTROL_PLANE_IP}"
fi
if [[ -n "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR:-}" ]]; then
  validate_host_port "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR}" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR" "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR}"
fi
if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_SANDBOX_NETWORK_CIDR" "${CUBE_SANDBOX_NETWORK_CIDR}"
  if [[ -n "${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-}" ]]; then
    case "${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK}" in
      0|1) ;;
      *) die "CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK must be 0 or 1 (got: '${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK}')" ;;
    esac
    upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK" "${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK}"
  fi
fi

# Persist external MySQL config so every systemd unit / helper picks it up
# instead of the local container. The CUBE_EXTERNAL_* markers let quickcheck
# and the up/down helpers skip the local service entirely; DATABASE_URL points
# CubeAPI at the external server (CubeMaster reads the patched conf.yaml).
if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_MYSQL_HOST" "${CUBE_EXTERNAL_MYSQL_HOST}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_MYSQL_PORT" "${CUBE_EXTERNAL_MYSQL_PORT}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_MYSQL_USER" "${CUBE_EXTERNAL_MYSQL_USER}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_MYSQL_PASSWORD" "${CUBE_EXTERNAL_MYSQL_PASSWORD}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_MYSQL_DB" "${CUBE_EXTERNAL_MYSQL_DB}"
  # Percent-encode every URI component so values containing URL metacharacters
  # (@, :, /, #, %, ...) cannot corrupt the connection string. This covers the
  # userinfo (user/password) as well as the host, port, and database name (e.g.
  # a '/' in the db name would otherwise be parsed as a path separator).
  database_url_user="$(urlencode "${CUBE_EXTERNAL_MYSQL_USER}")"
  database_url_pass="$(urlencode "${CUBE_EXTERNAL_MYSQL_PASSWORD}")"
  database_url_host="$(urlencode "${CUBE_EXTERNAL_MYSQL_HOST}")"
  database_url_port="$(urlencode "${CUBE_EXTERNAL_MYSQL_PORT}")"
  database_url_db="$(urlencode "${CUBE_EXTERNAL_MYSQL_DB}")"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "DATABASE_URL" "mysql://${database_url_user}:${database_url_pass}@${database_url_host}:${database_url_port}/${database_url_db}"
fi

# Persist external Redis config. cube-proxy reads CUBE_PROXY_REDIS_* from the
# env file when rendering global.conf (CubeMaster reads the patched conf.yaml).
if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_REDIS_HOST" "${CUBE_EXTERNAL_REDIS_HOST}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_REDIS_PORT" "${CUBE_EXTERNAL_REDIS_PORT}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_EXTERNAL_REDIS_PASSWORD" "${CUBE_EXTERNAL_REDIS_PASSWORD}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_PROXY_REDIS_IP" "${CUBE_EXTERNAL_REDIS_HOST}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_PROXY_REDIS_PORT" "${CUBE_EXTERNAL_REDIS_PORT}"
  upsert_env_kv "${RUNTIME_ENV_FILE}" "CUBE_PROXY_REDIS_PASSWORD" "${CUBE_EXTERNAL_REDIS_PASSWORD}"
fi

chmod +x "${INSTALL_PREFIX}/network-agent/bin/"*
chmod +x "${INSTALL_PREFIX}/Cubelet/bin/"*
chmod +x "${INSTALL_PREFIX}/cube-shim/bin/containerd-shim-cube-rs" "${INSTALL_PREFIX}/cube-shim/bin/cube-runtime"
chmod +x "${INSTALL_PREFIX}/scripts/one-click/"*.sh
chmod +x "${INSTALL_PREFIX}/scripts/systemd/"*.sh
chmod +x "${INSTALL_PREFIX}/scripts/cube-egress/"*.sh 2>/dev/null || true

if [[ -z "${CUBE_SANDBOX_NETWORK_CIDR:-}" ]]; then
  # Log current CIDR for debugging
  current_cidr="$(sed -nE '/^[[:space:]]*cidr[[:space:]]*=[[:space:]]*"/{s/.*"([^"]+)".*/\1/p;q;}' "${INSTALL_PREFIX}/Cubelet/config/config.toml" 2>/dev/null || echo "unknown")"
  log "using cubevs CIDR from config.toml: ${current_cidr} (CUBE_SANDBOX_NETWORK_CIDR not set)"
fi

if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
  chmod +x "${INSTALL_PREFIX}/CubeAPI/bin/cube-api"
  chmod +x "${INSTALL_PREFIX}/CubeMaster/bin/cubemaster" "${INSTALL_PREFIX}/CubeMaster/bin/cubemastercli"
fi

ln -sf "${INSTALL_PREFIX}/cube-shim/bin/containerd-shim-cube-rs" /usr/local/bin/containerd-shim-cube-rs
ln -sf "${INSTALL_PREFIX}/cube-shim/bin/cube-runtime" /usr/local/bin/cube-runtime
ln -sf "${INSTALL_PREFIX}/Cubelet/bin/cubecli" /usr/local/bin/cubecli
ln -sf "${INSTALL_PREFIX}/network-agent/bin/cubevsmapdump" /usr/local/bin/cubevsmapdump
if [[ "${DEPLOY_ROLE}" != "compute" ]]; then
  ln -sf "${INSTALL_PREFIX}/CubeMaster/bin/cubemastercli" /usr/local/bin/cubemastercli
else
  rm -f /usr/local/bin/cubemastercli
fi

restore_selinux_contexts
install_systemd_units
mask_external_dep_services
check_runtime_file_paths_not_directories
start_systemd_target

if [[ "${ONE_CLICK_RUN_QUICKCHECK:-1}" == "1" ]]; then
  ONE_CLICK_TOOLBOX_ROOT="${INSTALL_PREFIX}" \
  ONE_CLICK_RUNTIME_ENV_FILE="${RUNTIME_ENV_FILE}" \
    "${INSTALL_PREFIX}/scripts/one-click/quickcheck.sh"
fi

log "install complete (role=${DEPLOY_ROLE})"
print_path_hint
