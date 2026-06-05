#!/usr/bin/env bash
set -euo pipefail

ONE_CLICK_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${ONE_CLICK_LIB_DIR}/.." && pwd)"

log() {
  echo "[one-click] $*" >&2
}

die() {
  echo "[one-click] ERROR: $*" >&2
  exit 1
}

# Avoid `ldd --version | head -1` under strict mode: `head` may exit early and
# SIGPIPE `ldd`, which turns a valid glibc probe into a false failure.
detect_glibc_version() {
  local ldd_output glibc_ver
  if ! ldd_output="$(ldd --version 2>&1)"; then
    return 1
  fi
  glibc_ver="$(awk 'NR == 1 { print $NF; exit }' <<<"${ldd_output}")"
  [[ -n "${glibc_ver}" ]] || return 1
  printf '%s\n' "${glibc_ver}"
}

require_cmd() {
  local cmd="$1"
  command -v "${cmd}" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

one_click_cow_required_commands() {
  printf '%s\n' \
    mkfs.ext4 \
    mount \
    umount \
    losetup
}

cubelet_storage_backend_from_config() {
  local config_path="$1"
  ensure_file "${config_path}"
  sed -nE 's/^[[:space:]]*storage_backend[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "${config_path}" | head -n 1
}

validate_cubelet_cow_startup_deps() {
  local config_path="$1"
  ensure_file "${config_path}"
  require_cmd rg
  require_cmd sed

  local storage_backend
  storage_backend="$(cubelet_storage_backend_from_config "${config_path}")"
  [[ "${storage_backend}" == "cubecow" ]] || return 0

  local cmds=()
  while IFS= read -r cmd; do
    [[ -n "${cmd}" ]] && cmds+=("${cmd}")
  done < <(one_click_cow_required_commands)

  local missing=()
  local cmd
  for cmd in "${cmds[@]}"; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing+=("${cmd}")
    fi
  done

  if [[ "${#missing[@]}" -gt 0 ]]; then
    die "cubelet cubecow startup dependency check failed for ${config_path}; missing commands in PATH: ${missing[*]} (required commands: ${cmds[*]})"
  fi

  log "cubelet cubecow startup dependencies OK: ${cmds[*]}"
}

require_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    die "this script must run as root"
  fi
}

load_env_file() {
  local env_file="$1"
  local had_nounset=0
  [[ -n "${env_file}" ]] || return 0
  [[ -f "${env_file}" ]] || die "env file not found: ${env_file}"
  log "loading env file: ${env_file}"
  [[ $- == *u* ]] && had_nounset=1
  set +u
  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a
  if [[ "${had_nounset}" == "1" ]]; then
    set -u
  fi
}

ensure_file() {
  local path="$1"
  [[ -f "${path}" ]] || die "required file not found: ${path}"
}

ensure_dir() {
  local path="$1"
  [[ -d "${path}" ]] || die "required directory not found: ${path}"
}

copy_file() {
  local src="$1"
  local dst="$2"
  ensure_file "${src}"
  mkdir -p "$(dirname "${dst}")"
  cp -f "${src}" "${dst}"
}

copy_dir_contents() {
  local src="$1"
  local dst="$2"
  ensure_dir "${src}"
  rm -rf "${dst}"
  mkdir -p "${dst}"
  cp -a "${src}/." "${dst}/"
}

latest_git_revision() {
  local repo_root="$1"
  if command -v git >/dev/null 2>&1 && git -C "${repo_root}" rev-parse --short HEAD >/dev/null 2>&1; then
    git -C "${repo_root}" rev-parse --short HEAD
    return 0
  fi
  date +%Y%m%d-%H%M%S
}

container_exists() {
  local name="$1"
  docker ps -a --format '{{.Names}}' | rg -x "${name}" >/dev/null 2>&1
}

wait_for_http() {
  local url="$1"
  local retries="${2:-30}"
  local delay="${3:-2}"
  local i
  for ((i = 1; i <= retries; i++)); do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep "${delay}"
  done
  return 1
}

wait_for_pidfile() {
  local pid_file="$1"
  local retries="${2:-20}"
  local delay="${3:-1}"
  local i
  for ((i = 1; i <= retries; i++)); do
    if [[ -f "${pid_file}" ]]; then
      local pid
      pid="$(<"${pid_file}")"
      if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep "${delay}"
  done
  return 1
}

one_click_deploy_role() {
  local role="${ONE_CLICK_DEPLOY_ROLE:-control}"
  case "${role}" in
    control|compute)
      printf '%s\n' "${role}"
      ;;
    *)
      die "unsupported ONE_CLICK_DEPLOY_ROLE: ${role}"
      ;;
  esac
}

is_compute_role() {
  [[ "$(one_click_deploy_role)" == "compute" ]]
}

upsert_env_kv() {
  local env_file="$1"
  local key="$2"
  local value="$3"
  local tmp_file
  # Create temp file in the same directory as target to guarantee
  # atomic rename across filesystem boundaries (e.g., /tmp on tmpfs
  # and /usr/local on ext4/xfs).
  tmp_file="$(mktemp "${env_file}.XXXXXX")"
  local replaced=false

  if [[ -f "${env_file}" ]]; then
    while IFS= read -r line || [[ -n "${line}" ]]; do
      if [[ "${line}" == "${key}="* ]]; then
        printf '%s=%s\n' "${key}" "${value}" >> "${tmp_file}"
        replaced=true
      else
        printf '%s\n' "${line}" >> "${tmp_file}"
      fi
    done < "${env_file}"
  fi

  if [[ "${replaced}" != "true" ]]; then
    printf '%s=%s\n' "${key}" "${value}" >> "${tmp_file}"
  fi

  mv -f "${tmp_file}" "${env_file}"
}

detect_pkg_manager() {
  if command -v apt-get >/dev/null 2>&1; then
    printf 'apt'
  elif command -v yum >/dev/null 2>&1; then
    printf 'yum'
  else
    die "unsupported package manager: neither apt-get nor yum found"
  fi
}

install_ripgrep() {
  if command -v rg >/dev/null 2>&1; then
    return 0
  fi
  local pm
  pm="$(detect_pkg_manager)"
  log "installing ripgrep via ${pm}..."
  case "${pm}" in
    apt)
      apt-get update -qq && apt-get install -y -qq ripgrep
      ;;
    yum)
      yum install -y epel-release 2>/dev/null || true
      yum install -y ripgrep
      ;;
  esac
  command -v rg >/dev/null 2>&1 || die "failed to install ripgrep"
}

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    return 0
  fi
  local pm
  pm="$(detect_pkg_manager)"
  log "installing docker via ${pm}..."
  case "${pm}" in
    apt)
      apt-get update -qq
      apt-get install -y -qq docker.io docker-compose
      ;;
    yum)
      yum install -y docker docker-compose
      ;;
  esac
  systemctl enable docker && systemctl start docker
  command -v docker >/dev/null 2>&1 || die "failed to install docker"
}

install_docker_compose() {
  if docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi
  local pm
  pm="$(detect_pkg_manager)"
  log "installing docker-compose via ${pm}..."
  case "${pm}" in
    apt)
      apt-get update -qq && apt-get install -y -qq docker-compose
      ;;
    yum)
      yum install -y docker-compose
      ;;
  esac
  if ! docker compose version >/dev/null 2>&1 && ! command -v docker-compose >/dev/null 2>&1; then
    die "failed to install docker-compose"
  fi
}

install_dependencies() {
  log "checking and installing dependencies..."
  install_ripgrep
  install_docker
  install_docker_compose
}

detect_node_ip() {
  if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
    printf '%s\n' "${CUBE_SANDBOX_NODE_IP}"
    return 0
  fi

  local detected_ip=""
  if command -v ip >/dev/null 2>&1; then
    local detected_iface
    detected_iface="$(detect_primary_interface || true)"
    if [[ -n "${detected_iface}" ]]; then
      detected_ip="$(ip -4 addr show dev "${detected_iface}" 2>/dev/null \
        | grep -oP 'inet \K[0-9.]+' | head -1 || true)"
      if [[ -n "${detected_ip}" ]]; then
        log "auto-detected node IP from ${detected_iface}: ${detected_ip}"
        printf '%s\n' "${detected_ip}"
        return 0
      fi
    fi

    detected_ip="$(ip -4 addr show scope global 2>/dev/null \
      | grep -oP 'inet \K[0-9.]+' | head -1 || true)"
  fi

  if [[ -n "${detected_ip}" ]]; then
    log "auto-detected node IP from first global IPv4 address: ${detected_ip}"
    printf '%s\n' "${detected_ip}"
    return 0
  fi

  die "cannot auto-detect node IP. Please set CUBE_SANDBOX_NODE_IP or pass --node-ip=<ip>"
}

detect_primary_interface() {
  # Honor explicit override first.
  if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
    printf '%s\n' "${CUBE_SANDBOX_ETH_NAME}"
    return 0
  fi

  # `ip` is required for auto-detection.
  command -v ip >/dev/null 2>&1 || return 1

  local iface
  # Preferred path: resolve interface from default IPv4 route.
  iface="$(ip -o -4 route show to default 2>/dev/null | awk '{print $5; exit}')"
  if [[ -n "${iface}" ]]; then
    printf '%s\n' "${iface}"
    return 0
  fi

  # Fallback: first non-loopback interface that is currently up.
  iface="$(ip -o link show up 2>/dev/null \
    | awk -F': ' '$2 != "lo" {print $2; exit}' \
    | cut -d@ -f1)"
  [[ -n "${iface}" ]] || return 1
  printf '%s\n' "${iface}"
}

ensure_kernel_vmlinux() {
  local vmlinux_path="$1"
  local default_dir="$2"

  if [[ -f "${vmlinux_path}" ]]; then
    return 0
  fi

  cat >&2 <<EOF

============================================================
  ERROR: Kernel vmlinux file not found!
============================================================

  Missing: ${vmlinux_path}

  The vmlinux file is a required Linux kernel image used to
  boot guest VMs. You must provide it before building.

  How to fix:

    Option A — Place it in the default location:

      cp /path/to/your/vmlinux ${default_dir}/vmlinux

    Option B — Set a custom path via environment variable:

      export ONE_CLICK_CUBE_KERNEL_VMLINUX=/path/to/vmlinux

  Then re-run the build script.

  For more details, see: docs/guide/one-click-deploy.md
============================================================

EOF
  exit 1
}

# ---------------------------------------------------------------------------
# CIDR / network helper functions for CUBE_SANDBOX_NETWORK_CIDR feature.
# Added as part of the cubevs local network CIDR env var configuration plan.
# See docs/plan/cubevs-cidr-env-var-plan.md for details.
# ---------------------------------------------------------------------------

# ip_to_int: Convert an IPv4 dotted-quad string to a 32-bit integer.
# Uses 10# prefix to force base-10 and prevent octal interpretation
# of leading zeros (e.g., 010 -> 8 would be wrong).
ip_to_int() {
  local ip="$1"
  local a b c d

  if ! [[ "${ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    die "ip_to_int: malformed IPv4 address: '${ip}'"
  fi

  IFS=. read -r a b c d <<< "${ip}"
  if [[ -z "${a}" || -z "${b}" || -z "${c}" || -z "${d}" ]]; then
    die "ip_to_int: malformed IPv4 address: '${ip}'"
  fi

  echo "$(( (10#${a} << 24) + (10#${b} << 16) + (10#${c} << 8) + 10#${d} ))"
}

# ip_int_to_dot: Convert a 32-bit integer back to IPv4 dotted-quad string.
ip_int_to_dot() {
  local n="$1"
  echo "$(( (n >> 24) & 255 )).$(( (n >> 16) & 255 )).$(( (n >> 8) & 255 )).$(( n & 255 ))"
}

# _check_cidr_conflict: Detect overlap between the specified CIDR and
# existing host network interfaces + routes. Exits with die() on conflict.
_check_cidr_conflict() {
  local cidr="$1"
  require_cmd ip

  local ip="${cidr%/*}"
  local mask="${cidr#*/}"

  # Compute CIDR range in 32-bit space
  local cidr_net_int
  cidr_net_int=$(ip_to_int "${ip}")
  # NOTE: Use 10# prefix to prevent octal interpretation of leading-zero masks (e.g., /08)
  local host_bits=$(( 32 - 10#${mask} ))
  local cidr_mask_int=$(( (0xFFFFFFFF << host_bits) & 0xFFFFFFFF ))
  local cidr_net_start=$(( cidr_net_int & cidr_mask_int ))
  local cidr_net_end=$(( cidr_net_start | (0xFFFFFFFF & ~cidr_mask_int) ))

  local conflicts=()

  # --- Check interface addresses ---
  # Format: "IP/MASK IFACE" (e.g., "10.0.0.5/24 eth0")
  local line
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    local iface_cidr="${line%% *}"
    local iface_name="${line#* }"

    local iface_ip="${iface_cidr%%/*}"
    local iface_mask="${iface_cidr##*/}"
    # Bare IP (no mask) -> assume /32
    if [[ "${iface_ip}" == "${iface_cidr}" ]]; then
      iface_mask="32"
    fi

    local iface_int
    iface_int=$(ip_to_int "${iface_ip}")
    local iface_host_bits=$(( 32 - iface_mask ))
    local iface_mask_int=$(( (0xFFFFFFFF << iface_host_bits) & 0xFFFFFFFF ))
    local iface_net_start=$(( iface_int & iface_mask_int ))
    local iface_net_end=$(( iface_net_start | (0xFFFFFFFF & ~iface_mask_int) ))

    # Overlap test: two ranges overlap if start_A <= end_B AND end_A >= start_B
    if (( cidr_net_start <= iface_net_end && cidr_net_end >= iface_net_start )); then
      conflicts+=("interface ${iface_name} (${iface_cidr})")
    fi
  done < <(ip -4 addr show scope global 2>/dev/null | awk '/inet / {print $2, $NF}' || true)

  # --- Check routes for overlap ---
  # Use grep -oP to extract ANY CIDR token from each route line (handles
  # policy routes like "from 10.0.0.0/8 table 100" where the CIDR is not
  # the first field).
  local route_text
  route_text="$(ip -4 route show 2>/dev/null || true)"
  if [[ -n "${route_text}" ]]; then
    while IFS= read -r route_cidr; do
      [[ -n "${route_cidr}" ]] || continue

      # Skip well-known non-conflicting ranges
      [[ "${route_cidr}" != 169.254.* ]] || continue
      [[ "${route_cidr}" != 224.* ]] || continue
      [[ "${route_cidr}" != 127.* ]] || continue
      # Skip default route (0.0.0.0/0 should never conflict)
      [[ "${route_cidr}" != "0.0.0.0/0" ]] || continue

      local route_ip="${route_cidr%/*}"
      local route_mask="${route_cidr#*/}"
      [[ "${route_mask}" =~ ^[0-9]+$ ]] || continue

      local route_int
      route_int=$(ip_to_int "${route_ip}")
      local route_host_bits=$(( 32 - route_mask ))
      local route_mask_int=$(( (0xFFFFFFFF << route_host_bits) & 0xFFFFFFFF ))
      local route_net_start=$(( route_int & route_mask_int ))
      local route_net_end=$(( route_net_start | (0xFFFFFFFF & ~route_mask_int) ))

      if (( cidr_net_start <= route_net_end && cidr_net_end >= route_net_start )); then
        conflicts+=("route ${route_cidr}")
      fi
    done < <(echo "${route_text}" | grep -oP '\b[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+\b' || true)
  fi

  if [[ "${#conflicts[@]}" -gt 0 ]]; then
    local conflict_list
    conflict_list="$(printf '\n  - %s' "${conflicts[@]}")"
    die "CUBE_SANDBOX_NETWORK_CIDR '${cidr}' conflicts with existing host network:${conflict_list}

  The cubevs CIDR must not overlap with any existing interface IPs or routes.
  Choose a private IP range that does not conflict, such as:
    10.0.0.0/8      (any subnet within)
    172.16.0.0/12   (any subnet within)
    192.168.0.0/16  (any non-conflicting subnet)

  To bypass this check (not recommended), set:
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1"
  fi
}

# check_cidr_preflight: Validate CIDR format and detect host network conflicts.
# Called during install preflight (before any system modification).
# Only validates if CUBE_SANDBOX_NETWORK_CIDR is set; when unset, does nothing.
#
# SECURITY: Format validation MUST run before the SKIP_CONFLICT_CHECK bypass
# to prevent sed command injection (sed 'w' flag) and env file shell injection.
check_cidr_preflight() {
  local cidr="${1:-}"

  # When env var not set: skip validation, use config.toml default
  if [[ -z "${cidr}" ]]; then
    return 0
  fi

  # ======================================================================
  # FORMAT VALIDATION -- MUST run before any bypass check.
  #
  # The SKIP_CONFLICT_CHECK flag only skips NETWORK CONFLICT detection.
  # Format validation is always enforced to prevent:
  #   - sed 'w' flag file write injection (requires '|' in value)
  #   - shell injection via .one-click.env sourcing
  #   - config.toml corruption
  # ======================================================================

  # 1. Format validation (IPv4 dotted + mask)
  if ! [[ "${cidr}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]]; then
    die "CUBE_SANDBOX_NETWORK_CIDR '${cidr}' is not a valid IPv4 CIDR format (e.g., 10.0.0.0/16)"
  fi

  local ip="${cidr%/*}"
  local mask="${cidr#*/}"

  # 2. Valid IPv4 octets (force base-10 to prevent octal interpretation)
  local octets
  IFS=. read -r o1 o2 o3 o4 <<< "${ip}"
  octets=("${o1}" "${o2}" "${o3}" "${o4}")
  for octet in "${octets[@]}"; do
    # Reject IP octets with more than 3 digits (bash arithmetic overflow)
    if [[ "${#octet}" -gt 3 ]]; then
      die "CUBE_SANDBOX_NETWORK_CIDR '${cidr}' has an invalid IP octet: '${octet}' (max 3 digits)"
    fi
    if (( 10#${octet} < 0 || 10#${octet} > 255 )); then
      die "CUBE_SANDBOX_NETWORK_CIDR '${cidr}' has an invalid IP octet: ${octet}"
    fi
  done

  # 3. Valid mask range [8, 30] (use 10# prefix to prevent octal interpretation)
  if ! [[ "${mask}" =~ ^[0-9]+$ ]] || (( 10#${mask} < 8 || 10#${mask} > 30 )); then
    die "CUBE_SANDBOX_NETWORK_CIDR mask must be between 8 and 30 (got: ${mask})"
  fi

  # 4. Network address alignment check
  local ip_int=0
  for octet in "${octets[@]}"; do
    ip_int=$(( (ip_int << 8) + 10#${octet} ))
  done
  local host_bits=$(( 32 - 10#${mask} ))
  # & 0xFFFFFFFF truncates to 32 bits (bash uses signed 64-bit internally)
  local mask_int=$(( (0xFFFFFFFF << host_bits) & 0xFFFFFFFF ))
  local network_int=$(( ip_int & mask_int ))
  if (( ip_int != network_int )); then
    local suggested
    suggested=$(ip_int_to_dot ${network_int})
    die "CUBE_SANDBOX_NETWORK_CIDR '${cidr}' is not aligned to its network address. Did you mean: ${suggested}/${mask}?"
  fi

  # NOTE: CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK is read from the
  # process environment (not passed as a parameter). This mirrors the
  # existing pattern in the codebase (e.g., detect_node_ip reads
  # CUBE_SANDBOX_NODE_IP directly).

  # ======================================================================
  # CONFLICT DETECTION -- bypassable with SKIP_CONFLICT_CHECK
  #
  # At this point the CIDR is known-valid. Only the host-network overlap
  # check is conditionally skipped.
  # ======================================================================

  # 5. Check bypass flag -- only skips conflict detection, not format validation
  if [[ "${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-0}" == "1" ]]; then
    log "CUBE_SANDBOX_NETWORK_CIDR conflict check SKIPPED (bypass flag set) -- CIDR: ${cidr}"
    return 0
  fi

  # 6. CIDR conflict detection with host interfaces
  _check_cidr_conflict "${cidr}"

  log "CUBE_SANDBOX_NETWORK_CIDR preflight OK: ${cidr}"
}

# check_glibc_preflight: Verify the system glibc version meets the minimum
# requirement (2.31, matching the highest GLIBC_X.Y symbol version required
# by binaries built with the ubuntu:20.04 builder image).  Fails fast to
# prevent installation on unsupported older distributions (Ubuntu 18.04,
# CentOS 7, Debian 10).
check_glibc_preflight() {
  local min_major=2
  local min_minor=31

  local glibc_ver
  if ! glibc_ver="$(detect_glibc_version)"; then
    die "unable to detect glibc version (ldd --version failed)"
  fi

  # glibc version format is MAJOR.MINOR (e.g., 2.31, 2.35).
  # Strip any patch level or distro suffix beyond the second component.
  local major="${glibc_ver%%.*}"
  local minor="${glibc_ver#*.}"
  minor="${minor%%.*}"
  [[ "${minor}" =~ ^[0-9]+$ ]] || minor=0
  [[ "${major}" =~ ^[0-9]+$ ]] || major=0

  if (( major < min_major )) || { (( major == min_major )) && (( minor < min_minor )); }; then
    cat >&2 <<EOF
[one-click] ERROR: glibc version ${glibc_ver} is too old (minimum required: ${min_major}.${min_minor}).
[one-click]
[one-click]   This system has glibc ${glibc_ver}, but Cube Sandbox requires
[one-click]   glibc >= ${min_major}.${min_minor} (Ubuntu 20.04 LTS baseline).
[one-click]
[one-click]   Supported distributions include:
[one-click]     - Ubuntu 20.04+
[one-click]     - Debian 11+
[one-click]     - RHEL / CentOS 8+
[one-click]     - OpenCloudOS 8+
[one-click]
[one-click]   Please upgrade to a newer distribution and retry.
EOF
    exit 3
  fi

  log "glibc version ${glibc_ver} OK (>= ${min_major}.${min_minor})"
}
