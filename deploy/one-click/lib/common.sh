#!/usr/bin/env bash
#
# This file is a sourced library. Do not set shell options here: entrypoint
# scripts/tests that source it are responsible for their own strict mode
# (`set -euo pipefail`) policy.

ONE_CLICK_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${ONE_CLICK_LIB_DIR}/.." && pwd)"
if [[ "${CUBE_SANDBOX_INSTALL_ROOT:-}" != "/usr/local/services/cubetoolbox" ]]; then
  CUBE_SANDBOX_INSTALL_ROOT="/usr/local/services/cubetoolbox"
fi
readonly CUBE_SANDBOX_INSTALL_ROOT

log() {
  echo "[one-click] $*" >&2
}

die() {
  echo "[one-click] ERROR: $*" >&2
  exit 1
}

# warn: print a prominent warning to stderr. Bold-yellow when stderr is a
# terminal and color is not disabled (NO_COLOR unset/empty, TERM != dumb);
# plain text otherwise, so log captures and CI/test `grep` stay clean. The
# first line carries the [one-click] WARNING: prefix; continuation lines are
# indented so commands (e.g. the S3 grep hint) stay copyable.
warn() {
  local color=0
  if [[ -t 2 ]] && [[ -z "${NO_COLOR:-}" ]] && [[ "${TERM:-}" != "dumb" ]]; then
    color=1
  fi
  local line first=1
  while IFS= read -r line; do
    # Blank separator lines stay blank (no continuation indent).
    if [[ -z "${line}" ]]; then
      printf '\n' >&2
      continue
    fi
    if [[ "${first}" -eq 1 ]]; then
      if [[ "${color}" -eq 1 ]]; then
        printf '\033[1;33m[one-click] WARNING: %s\033[0m\n' "${line}" >&2
      else
        printf '%s\n' "[one-click] WARNING: ${line}" >&2
      fi
      first=0
    elif [[ "${color}" -eq 1 ]]; then
      printf '\033[1;33m  %s\033[0m\n' "${line}" >&2
    else
      printf '  %s\n' "${line}" >&2
    fi
  done <<<"$*"
}

# shellcheck source=../scripts/common/validation.sh
source "${ONE_CLICK_DIR}/scripts/common/validation.sh"

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

_version_trim_leading_zeroes() {
  local value="$1"
  value="${value#${value%%[!0]*}}"
  printf '%s\n' "${value:-0}"
}

_version_compare_numbers() {
  local LC_ALL=C
  local left right
  left="$(_version_trim_leading_zeroes "$1")"
  right="$(_version_trim_leading_zeroes "$2")"

  if [[ "${#left}" -lt "${#right}" ]]; then
    printf '%s\n' "-1"
  elif [[ "${#left}" -gt "${#right}" ]]; then
    printf '%s\n' "1"
  elif [[ "${left}" < "${right}" ]]; then
    printf '%s\n' "-1"
  elif [[ "${left}" > "${right}" ]]; then
    printf '%s\n' "1"
  else
    printf '%s\n' "0"
  fi
}

# Parse [v]X.Y.Z[-PRERELEASE][+BUILD] into unit-separator-delimited fields:
# major\037minor\037patch\037prerelease. Return 1 when the core has extra
# fields, non-ASCII digits, or invalid prerelease identifiers.
_version_split_semver() {
  local LC_ALL=C
  local version="$1"
  version="${version#v}"
  version="${version%%+*}"

  local core="${version%%-*}"
  local pre=""
  if [[ "${version}" == *-* ]]; then
    pre="${version#*-}"
    [[ "${pre}" =~ ^[0123456789A-Za-z-]+(\.[0123456789A-Za-z-]+)*$ ]] || return 1
  fi

  local major minor patch extra
  IFS='.' read -r major minor patch extra <<<"${core}"
  [[ -z "${extra:-}" ]] || return 1
  [[ "${major}" =~ ^[0123456789]+$ ]] || return 1
  [[ "${minor}" =~ ^[0123456789]+$ ]] || return 1
  [[ "${patch}" =~ ^[0123456789]+$ ]] || return 1

  printf '%s\037%s\037%s\037%s\n' "${major}" "${minor}" "${patch}" "${pre}"
}

# C-locale lexical fallback for versions that do not match the semver subset.
_version_compare_lexical() {
  local LC_ALL=C
  local left="$1"
  local right="$2"

  if [[ "${left}" < "${right}" ]]; then
    printf '%s\n' "-1"
  elif [[ "${left}" > "${right}" ]]; then
    printf '%s\n' "1"
  else
    printf '%s\n' "0"
  fi
}

_version_compare_prerelease_identifier() {
  local LC_ALL=C
  local left="$1"
  local right="$2"

  if [[ "${left}" == "${right}" ]]; then
    printf '%s\n' "0"
    return 0
  fi

  local left_numeric=0
  local right_numeric=0
  [[ "${left}" =~ ^[0123456789]+$ ]] && left_numeric=1
  [[ "${right}" =~ ^[0123456789]+$ ]] && right_numeric=1

  if [[ "${left_numeric}" == "1" && "${right_numeric}" == "1" ]]; then
    _version_compare_numbers "${left}" "${right}"
  elif [[ "${left_numeric}" == "1" ]]; then
    printf '%s\n' "-1"
  elif [[ "${right_numeric}" == "1" ]]; then
    printf '%s\n' "1"
  else
    local left_prefix="" left_suffix="" right_prefix="" right_suffix=""
    if [[ "${left}" =~ ^([A-Za-z-]+)([0123456789]+)$ ]]; then
      left_prefix="${BASH_REMATCH[1]}"
      left_suffix="${BASH_REMATCH[2]}"
    fi
    if [[ "${right}" =~ ^([A-Za-z-]+)([0123456789]+)$ ]]; then
      right_prefix="${BASH_REMATCH[1]}"
      right_suffix="${BASH_REMATCH[2]}"
    fi

    if [[ -n "${left_prefix}" && "${left_prefix}" == "${right_prefix}" ]]; then
      # Deployment tags often use rc1/rc2; compare matching suffixes numerically
      # so rc10 sorts after rc2, even though strict semver compares them lexically.
      _version_compare_numbers "${left_suffix}" "${right_suffix}"
    else
      _version_compare_lexical "${left}" "${right}"
    fi
  fi
}

_version_compare_prerelease() {
  local left="$1"
  local right="$2"

  if [[ -z "${left}" && -z "${right}" ]]; then
    printf '%s\n' "0"
    return 0
  elif [[ -z "${left}" ]]; then
    printf '%s\n' "1"
    return 0
  elif [[ -z "${right}" ]]; then
    printf '%s\n' "-1"
    return 0
  fi

  local left_ids right_ids
  IFS='.' read -r -a left_ids <<<"${left}"
  IFS='.' read -r -a right_ids <<<"${right}"

  local max_count="${#left_ids[@]}"
  if [[ "${#right_ids[@]}" -gt "${max_count}" ]]; then
    max_count="${#right_ids[@]}"
  fi

  local i cmp
  for ((i = 0; i < max_count; i++)); do
    if [[ "${i}" -ge "${#left_ids[@]}" ]]; then
      printf '%s\n' "-1"
      return 0
    elif [[ "${i}" -ge "${#right_ids[@]}" ]]; then
      printf '%s\n' "1"
      return 0
    fi

    cmp="$(_version_compare_prerelease_identifier "${left_ids[i]}" "${right_ids[i]}")"
    [[ "${cmp}" == "0" ]] || {
      printf '%s\n' "${cmp}"
      return 0
    }
  done

  printf '%s\n' "0"
}

# semver_compare: Compare two semantic versions and print -1, 0, or 1 to stdout.
# The comparison accepts an optional leading "v" and ignores build metadata
# after "+". If either input cannot be parsed as semver, both original inputs
# are compared with a C-locale lexical fallback instead. Set
# ONE_CLICK_VERSION_COMPARE_DEBUG=1 to log fallback diagnostics to stderr.
# Matching prerelease identifiers such as rc1/rc10 sort by numeric suffix to
# match deployment tag conventions.
semver_compare() {
  local left_parts right_parts
  if ! left_parts="$(_version_split_semver "$1")" || ! right_parts="$(_version_split_semver "$2")"; then
    # Preserve deterministic ordering for legacy/non-semver release strings.
    if [[ "${ONE_CLICK_VERSION_COMPARE_DEBUG:-}" == "1" ]]; then
      log "DEBUG: falling back to lexical version comparison for '$1' and '$2'"
    fi
    _version_compare_lexical "$1" "$2"
    return 0
  fi

  local left_major left_minor left_patch left_pre
  local right_major right_minor right_patch right_pre
  local parts_sep=$'\037'
  IFS="${parts_sep}" read -r left_major left_minor left_patch left_pre <<<"${left_parts}"
  IFS="${parts_sep}" read -r right_major right_minor right_patch right_pre <<<"${right_parts}"

  local cmp
  cmp="$(_version_compare_numbers "${left_major}" "${right_major}")"
  [[ "${cmp}" == "0" ]] || {
    printf '%s\n' "${cmp}"
    return 0
  }

  cmp="$(_version_compare_numbers "${left_minor}" "${right_minor}")"
  [[ "${cmp}" == "0" ]] || {
    printf '%s\n' "${cmp}"
    return 0
  }

  cmp="$(_version_compare_numbers "${left_patch}" "${right_patch}")"
  [[ "${cmp}" == "0" ]] || {
    printf '%s\n' "${cmp}"
    return 0
  }

  _version_compare_prerelease "${left_pre}" "${right_pre}"
}

# version_lt: Return success when the first version is lower than the second
# ONLY when both inputs are comparable semantic versions. This is used by the
# upgrade downgrade guard, so legacy/SHA-like versions must not block upgrades.
# Use semver_compare directly when lexical fallback for non-semver labels is
# desired.
version_lt() {
  _version_split_semver "$1" >/dev/null || return 1
  _version_split_semver "$2" >/dev/null || return 1
  [[ "$(semver_compare "$1" "$2")" == "-1" ]]
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

one_click_s3lvol_required_commands() {
  printf '%s\n' \
    nvme \
    python3 \
    truncate
}

s3lvol_nvme_present() {
  command -v nvme >/dev/null 2>&1
}

# Prefer apt, then dnf, then yum (detect_pkg_manager knows only apt/yum).
s3lvol_nvme_pkg_manager() {
  if command -v apt-get >/dev/null 2>&1; then
    printf 'apt'
    return 0
  fi
  if command -v dnf >/dev/null 2>&1; then
    printf 'dnf'
    return 0
  fi
  if command -v yum >/dev/null 2>&1; then
    printf 'yum'
    return 0
  fi
  return 1
}

# Install nvme-cli when s3lvol is enabled and `nvme` is missing.
ensure_nvme_cli() {
  [[ "${ONE_CLICK_ENABLE_S3LVOL:-0}" == "1" ]] || return 0
  if s3lvol_nvme_present; then
    return 0
  fi

  local pm
  if ! pm="$(s3lvol_nvme_pkg_manager)"; then
    die "CubeS3lvol needs nvme-cli (the nvme command) but no apt-get/dnf/yum was found. Install nvme-cli by hand and re-run install.sh"
  fi
  log "installing nvme-cli via ${pm}..."
  if [[ -n "${ONE_CLICK_NVME_CLI_INSTALLER:-}" ]]; then
    "${ONE_CLICK_NVME_CLI_INSTALLER}" "${pm}"
  else
    case "${pm}" in
      apt)
        apt-get update -qq
        apt-get install -y -qq nvme-cli
        ;;
      dnf)
        dnf install -y nvme-cli
        ;;
      yum)
        yum install -y nvme-cli
        ;;
    esac
  fi
  if ! s3lvol_nvme_present; then
    die "installed nvme-cli via ${pm} but nvme is still not in PATH. Install nvme-cli by hand (apt: apt-get install -y nvme-cli; dnf/yum: dnf install -y nvme-cli || yum install -y nvme-cli) and re-run install.sh"
  fi
}

# validate_s3lvol_rpc_client: prove the packaged rpc.py launcher can start
# under the *system* python3. SPDK's unmodified rpc.py needs
# argparse.BooleanOptionalAction (Python 3.9+); Ubuntu 20.04 is 3.8, and
# without the launcher shim rcow_start.sh loops on "target not answering".
# A failure here is a client/interpreter mismatch, not a dead target.
validate_s3lvol_rpc_client() {
  local rpc_py="$1"
  local rpc_dir python_dir out
  [[ -n "${rpc_py}" && -f "${rpc_py}" ]] \
    || die "CubeS3lvol RPC client is missing (${rpc_py:-unset}). The release package must ship scripts/rpc.py"
  rpc_dir="$(cd "$(dirname "${rpc_py}")" && pwd)"
  python_dir="${rpc_dir}/python"
  if ! out="$(
    env PYTHONPATH="${python_dir}${PYTHONPATH:+:${PYTHONPATH}}" \
      python3 "${rpc_py}" --help 2>&1
  )"; then
    die "CubeS3lvol RPC client cannot run under $(python3 --version 2>&1) (${rpc_py}): ${out}. This is a Python/rpc.py incompatibility, not a failure of s3lvol_tgt"
  fi
}

# validate_cubelet_s3lvol_startup_deps: check the runtime deps of the
# installed s3lvol_tgt binary. SPDK/DPDK/AWS CRT and OpenSSL are static, so
# `ldd` is the probe for the remaining system libraries (glibc is assumed).
# Runs only when ONE_CLICK_ENABLE_S3LVOL=1 and the binary is actually in
# the package.
validate_cubelet_s3lvol_startup_deps() {
  local s3lvol_bin="$1" # <prefix>/CubeS3lvol/bin/s3lvol_tgt
  [[ -n "${s3lvol_bin}" && -f "${s3lvol_bin}" ]] || return 0
  [[ "${ONE_CLICK_ENABLE_S3LVOL:-0}" == "1" ]] || return 0

  require_cmd ldd

  local cmds=()
  local cmd
  while IFS= read -r cmd; do
    [[ -n "${cmd}" ]] && cmds+=("${cmd}")
  done < <(one_click_s3lvol_required_commands)

  local missing=()
  for cmd in "${cmds[@]}"; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing+=("${cmd}")
    fi
  done
  if [[ "${#missing[@]}" -gt 0 ]]; then
    die "CubeS3lvol startup dependency check failed for ${s3lvol_bin}; missing commands in PATH: ${missing[*]} (required commands: ${cmds[*]})"
  fi

  local missing_libs=()
  while IFS= read -r lib; do
    [[ -n "${lib}" ]] && missing_libs+=("${lib}")
  done < <(ldd "${s3lvol_bin}" 2>/dev/null | awk '/=> not found/{print $1}')

  if [[ "${#missing_libs[@]}" -gt 0 ]]; then
    die "CubeS3lvol startup dependency check failed for ${s3lvol_bin}; missing shared libraries: ${missing_libs[*]}"
  fi

  # Release s3lvol_tgt is built for Haswell/AVX2, not the packager's native
  # CPU. Fail here with that wording instead of DPDK's RTE_MACHINE dump.
  if [[ "$(uname -m)" == "x86_64" ]] && ! grep -qw avx2 /proc/cpuinfo; then
    die "CubeS3lvol release binaries require AVX2 (Haswell baseline). This CPU does not advertise avx2 in /proc/cpuinfo; s3lvol_tgt will not start"
  fi

  # A missing S3 config makes the target fail on first connect, the unit
  # hits StartLimitBurst and the role target gives up. Fail here instead,
  # with the fix spelled out. install.sh writes this file from CUBE_S3_*
  # when ONE_CLICK_ENABLE_S3LVOL=1; a hand-written file is also accepted.
  local s3_cfg="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"
  if [[ ! -f "${s3_cfg}" ]]; then
    die "CubeS3lvol is enabled but ${s3_cfg} is missing and there is no CUBE_S3_ENDPOINT to generate it from. Set CUBE_S3_* (or enable bundled MinIO) or write ${s3_cfg} by hand (or point RCOW_S3_CFG at an existing file) and re-run install.sh"
  fi

  # The subsystem grid is exported with -a (allow_any_host), so the listener
  # must stay on loopback; anywhere else the target is an unauthenticated
  # block device on the network. Fail closed rather than ship that.
  local listen_addr="${RCOW_LISTEN_ADDR:-127.0.0.1}"
  case "${listen_addr}" in
    127.*|localhost|::1) ;;
    *)
      die "CubeS3lvol listens on non-loopback ${listen_addr} but exports subsystems with allow_any_host (-a); refusing to expose an unauthenticated block device. Keep RCOW_LISTEN_ADDR on 127.0.0.1 (or add a hostnqn allowlist to the export path first)"
      ;;
  esac

  local scripts_dir
  scripts_dir="$(cd "$(dirname "${s3lvol_bin}")/../scripts" 2>/dev/null && pwd)" \
    || die "CubeS3lvol scripts directory is missing next to ${s3lvol_bin}"
  validate_s3lvol_rpc_client "${scripts_dir}/rpc.py"

  log "CubeS3lvol startup dependencies OK: ${cmds[*]} + $(ldd "${s3lvol_bin}" 2>/dev/null | awk '/=> \//{n++} END{print n+0}') shared libs resolved; S3 config at ${s3_cfg}"
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

# Installer toggle keys: on/off switches whose this-run value must survive the
# upgrade env merge. The merge treats a bundle-.env value as an explicit
# operator override only when it differs from the new env.example default
# (the `cp env.example .env` safeguard), so flipping one of these keys BACK to
# the template default via .env would otherwise be inexpressible on upgrade.
# For the keys listed here, presence in the bundle .env or in install.sh's
# process environment counts as explicit operator intent and is re-applied
# after the merge (snapshot_one_click_toggles / apply_one_click_toggles).
#
# Opting a key in gives it presence-implies-explicit semantics: a wholesale
# `cp env.example .env` carried into an upgrade resets the key to the template
# default (documented caveat in the one-click README).
ONE_CLICK_TOGGLE_KEYS=(
  ONE_CLICK_ENABLE_S3LVOL
  CUBE_PVM_ENABLE
)

# snapshot_one_click_toggles: capture this-run operator intent for
# ONE_CLICK_TOGGLE_KEYS from the process environment and the bundle .env.
# Must run BEFORE any env file is sourced: the upgrade merge later sources the
# merged old-runtime env, which would otherwise clobber both channels.
#   ONE_CLICK_TOGGLE_ENV_SNAPSHOT     key -> value from the process environment
#   ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT  key -> value from the bundle .env
# (presence in .env is enough — a value equal to the env.example default still
# counts, which is exactly what makes "flip back to default" expressible)
snapshot_one_click_toggles() {
  local env_file="$1"
  local key
  # -g: these are process-wide state read later by apply_one_click_toggles;
  # -A: keys are env-var names, not numeric indexes. NOTE: the declaration and
  # the empty-assignment must be separate statements — bash 4.2 (CentOS 7)
  # mishandles `declare -gA VAR=()` by creating a function-local array.
  declare -gA ONE_CLICK_TOGGLE_ENV_SNAPSHOT
  declare -gA ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT
  ONE_CLICK_TOGGLE_ENV_SNAPSHOT=()
  ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT=()
  for key in "${ONE_CLICK_TOGGLE_KEYS[@]}"; do
    if [[ -n "${!key+x}" ]]; then
      ONE_CLICK_TOGGLE_ENV_SNAPSHOT["${key}"]="${!key}"
    fi
    if [[ -f "${env_file}" ]] && grep -q "^${key}=" "${env_file}"; then
      ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT["${key}"]="$(read_env_key "${env_file}" "${key}")"
    fi
  done
  return 0
}

# apply_one_click_toggles: re-apply the snapshotted operator intent after the
# env files (including the upgrade merge output) have been sourced. Same
# snapshot/replay shape as apply_one_click_database_intent (change both together). Precedence
# mirrors install.sh's documented channel order (CLI flags > .env > process
# environment > defaults): .env key > process environment > merged/loaded
# value. The final value is persisted by install.sh's upsert_env_kv, so this
# is the single place toggle intent is resolved.
apply_one_click_toggles() {
  local key
  for key in "${ONE_CLICK_TOGGLE_KEYS[@]}"; do
    if [[ -n "${ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT[${key}]+x}" ]]; then
      printf -v "${key}" '%s' "${ONE_CLICK_TOGGLE_DOTENV_SNAPSHOT[${key}]}"
      log "toggle ${key}=${!key} (explicit in .env)"
    elif [[ -n "${ONE_CLICK_TOGGLE_ENV_SNAPSHOT[${key}]+x}" ]]; then
      printf -v "${key}" '%s' "${ONE_CLICK_TOGGLE_ENV_SNAPSHOT[${key}]}"
      log "toggle ${key}=${!key} (explicit in process environment)"
    fi
  done
  return 0
}

# Database engine keys are commented out of env.example, so merge_env_three_way
# always re-appends the previous .one-click.env markers as "preserved custom
# settings". Without a this-run snapshot, MySQL↔Postgres (or external→bundled)
# switches expressed only in the new .env die as "mutually exclusive" or keep
# the old engine. Mirror the toggle snapshot: capture .env / process intent
# before merge, then scrub the opposite engine after merge.
ONE_CLICK_DB_INTENT_KEYS=(
  CUBE_DATABASE_DRIVER
  CUBE_EXTERNAL_MYSQL_HOST
  CUBE_EXTERNAL_MYSQL_PORT
  CUBE_EXTERNAL_MYSQL_USER
  CUBE_EXTERNAL_MYSQL_PASSWORD
  CUBE_EXTERNAL_MYSQL_DB
  CUBE_EXTERNAL_POSTGRES_HOST
  CUBE_EXTERNAL_POSTGRES_PORT
  CUBE_EXTERNAL_POSTGRES_USER
  CUBE_EXTERNAL_POSTGRES_PASSWORD
  CUBE_EXTERNAL_POSTGRES_DB
)

# snapshot_one_click_database_intent records process-env values and which DB
# keys are present as active KEY= lines in .env. It must NOT store raw RHS text
# from read_env_key: quoted passwords would round-trip with the quote chars.
# Call capture_one_click_database_dotenv_values after load_env_file so dotenv
# values are the shell-interpreted ones.
snapshot_one_click_database_intent() {
  local env_file="$1"
  local key
  declare -gA ONE_CLICK_DB_ENV_SNAPSHOT
  declare -gA ONE_CLICK_DB_DOTENV_SNAPSHOT
  declare -gA ONE_CLICK_DB_DOTENV_PRESENT
  ONE_CLICK_DB_ENV_SNAPSHOT=()
  ONE_CLICK_DB_DOTENV_SNAPSHOT=()
  ONE_CLICK_DB_DOTENV_PRESENT=()
  for key in "${ONE_CLICK_DB_INTENT_KEYS[@]}"; do
    if [[ -n "${!key+x}" ]]; then
      ONE_CLICK_DB_ENV_SNAPSHOT["${key}"]="${!key}"
    fi
    if [[ -f "${env_file}" ]] && grep -q "^${key}=" "${env_file}"; then
      ONE_CLICK_DB_DOTENV_PRESENT["${key}"]=1
    fi
  done
  return 0
}

# capture_one_click_database_dotenv_values fills DOTENV_SNAPSHOT from the live
# shell environment for keys marked present in .env. Must run after
# load_env_file (+ CLI re-apply) and before the upgrade merge re-sources
# .one-click.env.
capture_one_click_database_dotenv_values() {
  local key
  for key in "${ONE_CLICK_DB_INTENT_KEYS[@]}"; do
    if [[ -n "${ONE_CLICK_DB_DOTENV_PRESENT[${key}]+x}" ]]; then
      ONE_CLICK_DB_DOTENV_SNAPSHOT["${key}"]="${!key-}"
    fi
  done
  return 0
}

_clear_one_click_external_mysql_env() {
  CUBE_EXTERNAL_MYSQL_HOST=""
  CUBE_EXTERNAL_MYSQL_PORT=""
  CUBE_EXTERNAL_MYSQL_USER=""
  CUBE_EXTERNAL_MYSQL_PASSWORD=""
  CUBE_EXTERNAL_MYSQL_DB=""
}

_clear_one_click_external_postgres_env() {
  CUBE_EXTERNAL_POSTGRES_HOST=""
  CUBE_EXTERNAL_POSTGRES_PORT=""
  CUBE_EXTERNAL_POSTGRES_USER=""
  CUBE_EXTERNAL_POSTGRES_PASSWORD=""
  CUBE_EXTERNAL_POSTGRES_DB=""
}

_one_click_db_intent_declared() {
  local key="$1"
  [[ -n "${ONE_CLICK_DB_DOTENV_PRESENT[${key}]+x}" || -n "${ONE_CLICK_DB_ENV_SNAPSHOT[${key}]+x}" ]]
}

# apply_one_click_database_intent re-applies this-run DB engine intent after the
# upgrade merge. Same snapshot/replay shape as apply_one_click_toggles (change both
# together); DB-specific pieces are the no-intent no-op, host→driver inference, and
# opposite-engine scrub. No-op when neither .env nor the process environment named any
# DB intent key (preserve prior runtime markers).
#
# Per-key resolution matches apply_one_click_toggles: dotenv wins, process env
# fills gaps, otherwise the merge-preserved value is kept. Only the *opposite*
# engine's CUBE_EXTERNAL_* markers are scrubbed so same-engine credentials that
# lived only in .one-click.env are not wiped back to cube_pass defaults.
# When DRIVER is only merge-preserved but this run declares a host for the other
# engine, the host implies the driver (stale DRIVER must not scrub the host).
apply_one_click_database_intent() {
  local key
  local has_intent=0

  if [[ ${#ONE_CLICK_DB_DOTENV_PRESENT[@]} -gt 0 || ${#ONE_CLICK_DB_DOTENV_SNAPSHOT[@]} -gt 0 || ${#ONE_CLICK_DB_ENV_SNAPSHOT[@]} -gt 0 ]]; then
    has_intent=1
  fi
  [[ "${has_intent}" -eq 1 ]] || return 0

  for key in "${ONE_CLICK_DB_INTENT_KEYS[@]}"; do
    if [[ -n "${ONE_CLICK_DB_DOTENV_SNAPSHOT[${key}]+x}" ]]; then
      printf -v "${key}" '%s' "${ONE_CLICK_DB_DOTENV_SNAPSHOT[${key}]}"
    elif [[ -n "${ONE_CLICK_DB_ENV_SNAPSHOT[${key}]+x}" ]]; then
      printf -v "${key}" '%s' "${ONE_CLICK_DB_ENV_SNAPSHOT[${key}]}"
    fi
  done

  if ! _one_click_db_intent_declared CUBE_DATABASE_DRIVER; then
    if _one_click_db_intent_declared CUBE_EXTERNAL_POSTGRES_HOST; then
      CUBE_DATABASE_DRIVER="postgres"
      log "database intent: inferred driver=postgres from this-run CUBE_EXTERNAL_POSTGRES_HOST"
    elif _one_click_db_intent_declared CUBE_EXTERNAL_MYSQL_HOST; then
      CUBE_DATABASE_DRIVER="mysql"
      log "database intent: inferred driver=mysql from this-run CUBE_EXTERNAL_MYSQL_HOST"
    elif [[ -z "${CUBE_DATABASE_DRIVER:-}" ]]; then
      if [[ -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
        CUBE_DATABASE_DRIVER="postgres"
      else
        CUBE_DATABASE_DRIVER="mysql"
      fi
    fi
  elif [[ -z "${CUBE_DATABASE_DRIVER:-}" ]]; then
    if [[ -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
      CUBE_DATABASE_DRIVER="postgres"
    else
      CUBE_DATABASE_DRIVER="mysql"
    fi
  fi

  # Fail fast on this-run contradictions before scrubbing so validate_*'s
  # mutually-exclusive / wrong-driver die paths remain reachable for .env input.
  if _one_click_db_intent_declared CUBE_EXTERNAL_MYSQL_HOST       && _one_click_db_intent_declared CUBE_EXTERNAL_POSTGRES_HOST       && [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" && -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
    die "CUBE_EXTERNAL_MYSQL_HOST and CUBE_EXTERNAL_POSTGRES_HOST are mutually exclusive; set CUBE_DATABASE_DRIVER to select one engine"
  fi
  if _one_click_db_intent_declared CUBE_DATABASE_DRIVER; then
    if [[ "${CUBE_DATABASE_DRIVER}" == "postgres" ]]; then
      if _one_click_db_intent_declared CUBE_EXTERNAL_MYSQL_HOST           && [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]]; then
        die "CUBE_DATABASE_DRIVER=postgres cannot be combined with CUBE_EXTERNAL_MYSQL_HOST"
      fi
    else
      if _one_click_db_intent_declared CUBE_EXTERNAL_POSTGRES_HOST           && [[ -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
        die "CUBE_EXTERNAL_POSTGRES_HOST requires CUBE_DATABASE_DRIVER=postgres"
      fi
    fi
  fi

  if [[ "${CUBE_DATABASE_DRIVER}" == "postgres" ]]; then
    _clear_one_click_external_mysql_env
    log "database intent: driver=postgres host=${CUBE_EXTERNAL_POSTGRES_HOST:-}; cleared opposite CUBE_EXTERNAL_MYSQL_*"
  else
    _clear_one_click_external_postgres_env
    if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]]; then
      log "database intent: external MySQL host=${CUBE_EXTERNAL_MYSQL_HOST}; cleared opposite CUBE_EXTERNAL_POSTGRES_*"
    else
      log "database intent: driver=mysql (no external host); cleared opposite CUBE_EXTERNAL_POSTGRES_*"
    fi
  fi
  return 0
}

# Load build-machine overrides for release-bundle scripts.
# Precedence: ONE_CLICK_BUILD_ENV_FILE > ${ONE_CLICK_DIR}/build.env >
# legacy ONE_CLICK_ENV_FILE / .env (with a migration hint).
load_build_env() {
  local build_env_file legacy_env_file
  if [[ -n "${ONE_CLICK_BUILD_ENV_FILE:-}" ]]; then
    load_env_file "${ONE_CLICK_BUILD_ENV_FILE}"
    return
  fi
  build_env_file="${ONE_CLICK_DIR}/build.env"
  if [[ -f "${build_env_file}" ]]; then
    load_env_file "${build_env_file}"
    return
  fi
  legacy_env_file="${ONE_CLICK_ENV_FILE:-${ONE_CLICK_DIR}/.env}"
  if [[ -f "${legacy_env_file}" ]]; then
    log "build.env not found; falling back to ${legacy_env_file} (copy build.env.example to build.env for build-only overrides)"
    load_env_file "${legacy_env_file}"
  fi
}

ensure_file() {
  local path="$1"
  [[ -f "${path}" ]] || die "required file not found: ${path}"
}

# sha256 hex of a file (no "sha256:" prefix).
file_sha256_hex() {
  local path="$1"
  local digest=""
  [[ -f "${path}" ]] || return 1
  if command -v sha256sum >/dev/null 2>&1; then
    digest="$(sha256sum -- "${path}" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    digest="$(shasum -a 256 -- "${path}" | awk '{print $1}')"
  elif command -v openssl >/dev/null 2>&1; then
    digest="$(openssl dgst -sha256 -- "${path}" | awk '{print $NF}')"
  else
    return 1
  fi
  [[ -n "${digest}" ]] || return 1
  printf '%s\n' "${digest}"
}

# "sha256-<12>" from file content.
file_content_version() {
  local path="$1"
  local digest=""
  digest="$(file_sha256_hex "${path}")" || return 1
  printf 'sha256-%s\n' "${digest:0:12}"
}

# Use tag when set; otherwise derive from file content.
resolve_tagged_or_content_version() {
  local tag="${1:-}"
  local path="${2:-}"
  tag="$(printf '%s' "${tag}" | tr -d '[:space:]')"
  case "${tag}" in
    ""|unknown|UNKNOWN) ;;
    *)
      printf '%s\n' "${tag}"
      return 0
      ;;
  esac
  [[ -n "${path}" && -f "${path}" ]] || return 1
  file_content_version "${path}"
}

# Escape VALUE so it can be used safely as the replacement text in a sed
# `s<delim>...<delim>...<delim>` expression. Escapes backslashes, '&' (the
# whole-match reference), '"' (install.sh embeds results in double-quoted YAML
# snippets), and the substitution delimiter (default '|'). Pass the delimiter
# actually used at the call site (e.g. '#') so values containing it do not
# terminate the command.
#
# SECURITY: embedded newlines / carriage returns are stripped first as
# defense-in-depth. An unescaped newline in the replacement text would
# terminate the sed `s` command and let a crafted value (e.g. a password read
# from .env) inject arbitrary sed commands into the rendered config.
escape_sed() {
  local value="$1"
  local delim="${2:-|}"
  printf '%s' "${value}" | tr -d '\n\r' | sed "s/[\\\\${delim}&\"]/\\\\&/g"
}

# Percent-encode a string for safe use as a URL component (e.g. the userinfo
# section of a connection string). Encodes every byte that is not an RFC 3986
# unreserved character, so values containing '@', ':', '/', '%', etc. do not
# corrupt the resulting URL. Operates byte-wise under the C locale so multibyte
# input is encoded correctly.
urlencode() {
  local LC_ALL=C
  local string="$1"
  local len="${#string}"
  local i char hex out=""
  for (( i = 0; i < len; i++ )); do
    char="${string:i:1}"
    case "${char}" in
      [a-zA-Z0-9._~-])
        out+="${char}"
        ;;
      *)
        printf -v hex '%02X' "'${char}"
        out+="%${hex}"
        ;;
    esac
  done
  printf '%s' "${out}"
}

declared_release_manifest_relpath() {
  local version_file="$1"
  [[ -f "${version_file}" ]] || return 0
  sed -nE 's/^manifest=(.+)$/\1/p' "${version_file}" | head -n 1
}

validate_declared_release_manifest() {
  local bundle_dir="$1"
  local version_file="${bundle_dir}/VERSION.txt"
  local manifest_rel manifest_path

  manifest_rel="$(declared_release_manifest_relpath "${version_file}")"
  [[ -n "${manifest_rel}" ]] || return 0

  case "${manifest_rel}" in
    /* | *..* | */* )
      die "unsupported manifest path declared in ${version_file}: ${manifest_rel}"
      ;;
  esac

  manifest_path="${bundle_dir}/${manifest_rel}"
  ensure_file "${manifest_path}"
  require_cmd python3
  python3 - "${manifest_path}" <<'PY' || die "invalid release manifest: ${manifest_path}"
import json, sys
path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    data = json.load(f)
if not isinstance(data, dict):
    raise ValueError("release manifest root must be a JSON object")
for key in ("components", "guest_image", "kernel"):
    if key not in data:
        raise ValueError(f"release manifest missing required key: {key}")
PY
  log "release manifest contract OK: ${manifest_path}"
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

command_output_has_exact_line() {
  local needle="$1"
  shift

  require_cmd grep

  local output
  output="$("$@" 2>/dev/null || true)"
  [[ -n "${output}" ]] || return 1
  grep -Fxq -- "${needle}" <<<"${output}"
}

container_exists() {
  local name="$1"
  command_output_has_exact_line "${name}" docker ps -a --format '{{.Names}}'
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

# one_click_parse_args: parse install.sh CLI flags into CLI_* globals.
#
# Supports BOTH `--flag=value` and space-separated `--flag value` forms so
# that documented invocations like `--mode upgrade` work as expected. Value
# flags reported missing a value fail fast (no silent empty assignment).
# Unknown tokens are warned about but ignored to preserve backward
# compatibility with existing callers that pass extra positional arguments.
#
# Resets and populates the following globals (caller declares/uses them):
#   CLI_MODE CLI_NODE_IP CLI_ASSUME_YES CLI_ALLOW_DOWNGRADE CLI_ALLOW_ROLE_CHANGE
one_click_parse_args() {
  CLI_MODE=""
  CLI_NODE_IP=""
  CLI_ASSUME_YES=""
  CLI_ALLOW_DOWNGRADE=""
  CLI_ALLOW_ROLE_CHANGE=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --node-ip=*)
        CLI_NODE_IP="${1#--node-ip=}"
        ;;
      --node-ip)
        [[ $# -ge 2 ]] || die "--node-ip requires a value"
        shift
        CLI_NODE_IP="$1"
        ;;
      --mode=*)
        CLI_MODE="${1#--mode=}"
        ;;
      --mode)
        [[ $# -ge 2 ]] || die "--mode requires a value (install|upgrade|auto)"
        shift
        CLI_MODE="$1"
        ;;
      -y|--yes)
        CLI_ASSUME_YES=1
        ;;
      --allow-downgrade)
        CLI_ALLOW_DOWNGRADE=1
        ;;
      --allow-role-change)
        CLI_ALLOW_ROLE_CHANGE=1
        ;;
      *)
        log "WARNING: ignoring unknown argument: $1"
        ;;
    esac
    shift
  done
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

env_value_is_plain_scalar() {
  local value="${1-}"
  [[ -z "${value}" ]] && return 0
  [[ "${value}" =~ ^[A-Za-z0-9_./:@%+=,-]*$ ]]
}

render_env_assignment_value() {
  local key="$1"
  local value="$2"

  if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    die "env value for ${key} must not contain newlines"
  fi

  # Keep shell-safe scalars unquoted so preflight readers that deliberately do
  # not source the file (for example, ONE_CLICK_DEPLOY_ROLE checks during
  # upgrade preflight) continue to see plain values.
  if env_value_is_plain_scalar "${value}"; then
    printf '%s' "${value}"
    return 0
  fi

  # Runtime helpers load .one-click.env via `set -a; source`, so persist any
  # shell-sensitive value in a quoted form that preserves bytes literally.
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//\$/\\$}"
  value="${value//\`/\\\`}"
  printf '"%s"' "${value}"
}

upsert_env_kv() {
  local env_file="$1"
  local key="$2"
  local value="$3"
  local rendered_value
  local tmp_file
  # SECURITY: tighten umask before mktemp so the temp file is created 0600 from
  # the start, closing the race window between creation and the chmod below.
  # The atomic `mv` later replaces the target's inode with this temp file, so a
  # permissive umask (e.g. 0022 -> 0644, or 0000 -> 0666) would otherwise leak
  # every persisted secret (DATABASE_URL, CUBE_EXTERNAL_*_PASSWORD, ...) to other
  # local users -- briefly here and permanently in env_file. Mirrors the pattern
  # in install.sh's check_external_deps_preflight and up-with-deps.sh.
  local old_umask
  old_umask="$(umask)"
  umask 077
  # Create temp file in the same directory as target to guarantee
  # atomic rename across filesystem boundaries (e.g., /tmp on tmpfs
  # and /usr/local on ext4/xfs).
  tmp_file="$(mktemp "${env_file}.XXXXXX")"
  umask "${old_umask}"
  # Defense-in-depth: enforce 0600 explicitly in case the temp file pre-existed
  # with looser permissions or mktemp honored a non-default mode.
  chmod 600 "${tmp_file}"
  local replaced=false
  rendered_value="$(render_env_assignment_value "${key}" "${value}")"

  if [[ -f "${env_file}" ]]; then
    while IFS= read -r line || [[ -n "${line}" ]]; do
      if [[ "${line}" == "${key}="* ]]; then
        printf '%s=%s\n' "${key}" "${rendered_value}" >> "${tmp_file}"
        replaced=true
      else
        printf '%s\n' "${line}" >> "${tmp_file}"
      fi
    done < "${env_file}"
  fi

  if [[ "${replaced}" != "true" ]]; then
    printf '%s=%s\n' "${key}" "${rendered_value}" >> "${tmp_file}"
  fi

  mv -f "${tmp_file}" "${env_file}"
}

# remove_env_kv deletes KEY=... lines from env_file. Used when switching Redis
# modes (Sentinel <-> standalone) so stale sentinel/host keys cannot keep the
# previous mode alive via ":-" fallbacks in downstream scripts.
remove_env_kv() {
  local env_file="$1"
  local key="$2"
  local tmp_file
  local old_umask
  [[ -f "${env_file}" ]] || return 0

  old_umask="$(umask)"
  umask 077
  tmp_file="$(mktemp "${env_file}.XXXXXX")"
  umask "${old_umask}"
  chmod 600 "${tmp_file}"

  while IFS= read -r line || [[ -n "${line}" ]]; do
    if [[ "${line}" == "${key}="* ]]; then
      continue
    fi
    if ! printf '%s\n' "${line}" >> "${tmp_file}"; then
      rm -f "${tmp_file}"
      die "failed to rewrite ${env_file} while removing ${key}"
    fi
  done < "${env_file}"

  mv -f "${tmp_file}" "${env_file}"
}

_remove_env_keys() {
  local env_file="$1"
  shift
  local key
  for key in "$@"; do
    remove_env_kv "${env_file}" "${key}"
  done
}

# patch_cubemaster_instance_db_config rewrites instance_db_config.{driver,addr,user,pwd,db_name}
# in a CubeMaster conf.yaml. Patterns are anchored at line start so keys like
# common.cube_ops_addr (which contain the substring "addr:") are never matched.
patch_cubemaster_instance_db_config() {
  local cfg="$1"
  local driver="$2"
  local addr="$3"
  local user="$4"
  local pwd="$5"
  local db_name="$6"
  local addr_esc user_esc pwd_esc db_esc
  ensure_file "${cfg}"
  addr_esc="$(escape_sed "${addr}")"
  user_esc="$(escape_sed "${user}")"
  pwd_esc="$(escape_sed "${pwd}")"
  db_esc="$(escape_sed "${db_name}")"
  sed -i \
    -e "s|^\([[:space:]]*\)driver: \".*\"|\1driver: \"${driver}\"|" \
    -e "s|^\([[:space:]]*\)addr: \".*\"|\1addr: \"${addr_esc}\"|" \
    -e "s|^\([[:space:]]*\)user: \".*\"|\1user: \"${user_esc}\"|" \
    -e "s|^\([[:space:]]*\)pwd: \".*\"|\1pwd: \"${pwd_esc}\"|" \
    -e "s|^\([[:space:]]*\)db_name: \".*\"|\1db_name: \"${db_esc}\"|" \
    "${cfg}"
}

# one_click_skip_local_mysql is true when an external DB endpoint is configured.
# Key off host presence (not bare CUBE_DATABASE_DRIVER): driver=postgres with an
# empty host must not skip bundled MySQL if this env is later reused on control.
one_click_skip_local_mysql() {
  [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" || -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]
}

# validate_one_click_database_config mirrors Helm database.driver / postgres.*:
# mysql keeps bundled or CUBE_EXTERNAL_MYSQL_*; postgres is external-only and
# requires CUBE_EXTERNAL_POSTGRES_HOST. Engines do not share host/credential keys.
validate_one_click_database_config() {
  local driver="${CUBE_DATABASE_DRIVER:-mysql}"
  case "${driver}" in
    mysql|postgres) ;;
    *)
      die "CUBE_DATABASE_DRIVER must be mysql or postgres (got '${driver}')"
      ;;
  esac
  CUBE_DATABASE_DRIVER="${driver}"

  if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" && -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
    die "CUBE_EXTERNAL_MYSQL_HOST and CUBE_EXTERNAL_POSTGRES_HOST are mutually exclusive; set CUBE_DATABASE_DRIVER to select one engine"
  fi

  if [[ "${driver}" == "postgres" ]]; then
    if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]]; then
      die "CUBE_DATABASE_DRIVER=postgres cannot be combined with CUBE_EXTERNAL_MYSQL_HOST"
    fi
    if [[ -z "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
      die "CUBE_DATABASE_DRIVER=postgres requires CUBE_EXTERNAL_POSTGRES_HOST (one-click never ships a local PostgreSQL)"
    fi
  else
    if [[ -n "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
      die "CUBE_EXTERNAL_POSTGRES_HOST requires CUBE_DATABASE_DRIVER=postgres"
    fi
  fi
}

# persist_one_click_database_runtime_env writes CUBE_DATABASE_DRIVER, the active
# engine's CUBE_EXTERNAL_* markers, and DATABASE_URL for CubeAPI/CubeOps.
# Opposite-engine keys are scrubbed so a driver switch cannot keep the previous
# endpoint alive via ":-" fallbacks.
persist_one_click_database_runtime_env() {
  local env_file="$1"
  local driver="${CUBE_DATABASE_DRIVER:-mysql}"
  local database_url_user database_url_pass database_url_host database_url_port database_url_db
  [[ -n "${env_file}" ]] || die "persist_one_click_database_runtime_env: env file path required"

  # driver=postgres with no host is not a usable external endpoint (compute
  # mirror). Persist bundled MySQL markers so reuse on control cannot skip
  # local MySQL or fail validate on a bare driver=postgres.
  if [[ "${driver}" == "postgres" && -z "${CUBE_EXTERNAL_POSTGRES_HOST:-}" ]]; then
    driver="mysql"
    CUBE_DATABASE_DRIVER="mysql"
  fi

  upsert_env_kv "${env_file}" "CUBE_DATABASE_DRIVER" "${driver}"

  if [[ "${driver}" == "postgres" ]]; then
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_HOST" "${CUBE_EXTERNAL_POSTGRES_HOST}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_PORT" "${CUBE_EXTERNAL_POSTGRES_PORT:-5432}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_USER" "${CUBE_EXTERNAL_POSTGRES_USER:-cube}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_PASSWORD" "${CUBE_EXTERNAL_POSTGRES_PASSWORD:-}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_POSTGRES_DB" "${CUBE_EXTERNAL_POSTGRES_DB:-cube_mvp}"
    database_url_user="$(urlencode "${CUBE_EXTERNAL_POSTGRES_USER:-cube}")"
    database_url_pass="$(urlencode "${CUBE_EXTERNAL_POSTGRES_PASSWORD:-}")"
    database_url_host="$(urlencode "${CUBE_EXTERNAL_POSTGRES_HOST}")"
    database_url_port="$(urlencode "${CUBE_EXTERNAL_POSTGRES_PORT:-5432}")"
    database_url_db="$(urlencode "${CUBE_EXTERNAL_POSTGRES_DB:-cube_mvp}")"
    # Scheme matches Helm cube.databaseURL (postgresql://...).
    upsert_env_kv "${env_file}" "DATABASE_URL" \
      "postgresql://${database_url_user}:${database_url_pass}@${database_url_host}:${database_url_port}/${database_url_db}"
    _remove_env_keys "${env_file}" \
      CUBE_EXTERNAL_MYSQL_HOST \
      CUBE_EXTERNAL_MYSQL_PORT \
      CUBE_EXTERNAL_MYSQL_USER \
      CUBE_EXTERNAL_MYSQL_PASSWORD \
      CUBE_EXTERNAL_MYSQL_DB
  elif [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]]; then
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_HOST" "${CUBE_EXTERNAL_MYSQL_HOST}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_PORT" "${CUBE_EXTERNAL_MYSQL_PORT:-3306}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_USER" "${CUBE_EXTERNAL_MYSQL_USER:-cube}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_PASSWORD" "${CUBE_EXTERNAL_MYSQL_PASSWORD:-}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_MYSQL_DB" "${CUBE_EXTERNAL_MYSQL_DB:-cube_mvp}"
    database_url_user="$(urlencode "${CUBE_EXTERNAL_MYSQL_USER:-cube}")"
    database_url_pass="$(urlencode "${CUBE_EXTERNAL_MYSQL_PASSWORD:-}")"
    database_url_host="$(urlencode "${CUBE_EXTERNAL_MYSQL_HOST}")"
    database_url_port="$(urlencode "${CUBE_EXTERNAL_MYSQL_PORT:-3306}")"
    database_url_db="$(urlencode "${CUBE_EXTERNAL_MYSQL_DB:-cube_mvp}")"
    upsert_env_kv "${env_file}" "DATABASE_URL" \
      "mysql://${database_url_user}:${database_url_pass}@${database_url_host}:${database_url_port}/${database_url_db}"
    _remove_env_keys "${env_file}" \
      CUBE_EXTERNAL_POSTGRES_HOST \
      CUBE_EXTERNAL_POSTGRES_PORT \
      CUBE_EXTERNAL_POSTGRES_USER \
      CUBE_EXTERNAL_POSTGRES_PASSWORD \
      CUBE_EXTERNAL_POSTGRES_DB
  else
    local local_mysql_host="127.0.0.1"
    local local_mysql_port="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
    local local_mysql_user="${CUBE_SANDBOX_MYSQL_USER:-cube}"
    local local_mysql_password="${CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass}"
    local local_mysql_db="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
    upsert_env_kv "${env_file}" "DATABASE_URL" \
      "mysql://$(urlencode "${local_mysql_user}"):$(urlencode "${local_mysql_password}")@$(urlencode "${local_mysql_host}"):$(urlencode "${local_mysql_port}")/$(urlencode "${local_mysql_db}")"
    _remove_env_keys "${env_file}" \
      CUBE_EXTERNAL_MYSQL_HOST \
      CUBE_EXTERNAL_MYSQL_PORT \
      CUBE_EXTERNAL_MYSQL_USER \
      CUBE_EXTERNAL_MYSQL_PASSWORD \
      CUBE_EXTERNAL_MYSQL_DB \
      CUBE_EXTERNAL_POSTGRES_HOST \
      CUBE_EXTERNAL_POSTGRES_PORT \
      CUBE_EXTERNAL_POSTGRES_USER \
      CUBE_EXTERNAL_POSTGRES_PASSWORD \
      CUBE_EXTERNAL_POSTGRES_DB
  fi
}

# persist_one_click_redis_runtime_env writes Redis keys for systemd
# EnvironmentFile consumers (cubeops, cubemaster, cube-proxy, LCM).
# --mode=install often starts from an empty .one-click.env; without
# CUBE_SANDBOX_REDIS_PASSWORD, local Redis still requirepass-es
# (up-support.sh defaults to ceuhvu123) while cubeops AUTH-skips and
# NOAUTH-fails.
#
# Uses the current shell's CUBE_EXTERNAL_* / CUBE_SANDBOX_REDIS_PASSWORD.
# Sentinel and standalone external branches persist
# CUBE_EXTERNAL_REDIS_PASSWORD; the local branch persists the resolved
# sandbox password.
persist_one_click_redis_runtime_env() {
  local env_file="$1"
  [[ -n "${env_file}" ]] || die "persist_one_click_redis_runtime_env: env file path required"

  if [[ -n "${CUBE_EXTERNAL_REDIS_MASTER_NAME:-}" ]]; then
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_MASTER_NAME" "${CUBE_EXTERNAL_REDIS_MASTER_NAME}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_SENTINEL_NODES" "${CUBE_EXTERNAL_REDIS_SENTINEL_NODES:-}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_PASSWORD" "${CUBE_EXTERNAL_REDIS_PASSWORD:-}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD" "${CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD:-}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_MASTER_NAME" "${CUBE_EXTERNAL_REDIS_MASTER_NAME}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_SENTINEL_NODES" "${CUBE_EXTERNAL_REDIS_SENTINEL_NODES:-}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_PASSWORD" "${CUBE_EXTERNAL_REDIS_PASSWORD:-}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_SENTINEL_PASSWORD" "${CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD:-}"
    _remove_env_keys "${env_file}" \
      CUBE_EXTERNAL_REDIS_HOST \
      CUBE_EXTERNAL_REDIS_PORT \
      CUBE_PROXY_REDIS_IP \
      CUBE_PROXY_REDIS_PORT
  elif [[ -n "${CUBE_EXTERNAL_REDIS_HOST:-}" ]]; then
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_HOST" "${CUBE_EXTERNAL_REDIS_HOST}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_PORT" "${CUBE_EXTERNAL_REDIS_PORT:-6379}"
    upsert_env_kv "${env_file}" "CUBE_EXTERNAL_REDIS_PASSWORD" "${CUBE_EXTERNAL_REDIS_PASSWORD:-}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_IP" "${CUBE_EXTERNAL_REDIS_HOST}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_PORT" "${CUBE_EXTERNAL_REDIS_PORT:-6379}"
    upsert_env_kv "${env_file}" "CUBE_PROXY_REDIS_PASSWORD" "${CUBE_EXTERNAL_REDIS_PASSWORD:-}"
    _remove_env_keys "${env_file}" \
      CUBE_EXTERNAL_REDIS_MASTER_NAME \
      CUBE_EXTERNAL_REDIS_SENTINEL_NODES \
      CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD \
      CUBE_PROXY_REDIS_MASTER_NAME \
      CUBE_PROXY_REDIS_SENTINEL_NODES \
      CUBE_PROXY_REDIS_SENTINEL_PASSWORD
  else
    # Back to bundled local Redis: drop every external Redis marker so
    # up-support / proxy / LCM do not keep skipping the local container or
    # wiring Sentinel from a previous install. Persist the password the
    # local container actually uses (operator override or ceuhvu123).
    _remove_env_keys "${env_file}" \
      CUBE_EXTERNAL_REDIS_MASTER_NAME \
      CUBE_EXTERNAL_REDIS_SENTINEL_NODES \
      CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD \
      CUBE_EXTERNAL_REDIS_HOST \
      CUBE_EXTERNAL_REDIS_PORT \
      CUBE_EXTERNAL_REDIS_PASSWORD \
      CUBE_PROXY_REDIS_MASTER_NAME \
      CUBE_PROXY_REDIS_SENTINEL_NODES \
      CUBE_PROXY_REDIS_SENTINEL_PASSWORD \
      CUBE_PROXY_REDIS_IP \
      CUBE_PROXY_REDIS_PORT \
      CUBE_PROXY_REDIS_PASSWORD
    upsert_env_kv "${env_file}" "CUBE_SANDBOX_REDIS_PASSWORD" \
      "${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"
  fi
}

redis_cli_help_output() {
  command -v redis-cli >/dev/null 2>&1 || return 1
  redis-cli --help 2>&1 || true
}

redis_cli_help_supports_flag() {
  local help_output="$1"
  local flag="$2"

  printf '%s\n' "${help_output}" | grep -Eq "(^|[[:space:]])${flag}([[:space:]]|$)"
}

run_with_timeout_if_available() {
  local timeout_secs="$1"
  shift

  if command -v timeout >/dev/null 2>&1; then
    if timeout --help 2>&1 | grep -q -- '-k'; then
      timeout -k 1 "${timeout_secs}" "$@"
    else
      timeout "${timeout_secs}" "$@"
    fi
  else
    "$@"
  fi
}

run_redis_preflight_cmd() {
  local use_timeout_wrapper="$1"
  local connect_timeout="$2"
  shift 2

  if [[ "${use_timeout_wrapper}" == "1" ]]; then
    run_with_timeout_if_available "${connect_timeout}" "$@"
  else
    "$@"
  fi
}

validate_interface_name() {
  local value="$1"
  local name="${2:-interface name}"
  [[ -n "${value}" ]] || die "${name} must not be empty"
  # Linux IFNAMSIZ is 16 including NUL, so names are at most 15 bytes. Restrict
  # to characters that are safe in the TOML replacement and shell logs.
  [[ "${value}" =~ ^[A-Za-z0-9_.:-]{1,15}$ ]] \
    || die "invalid ${name}: ${value} (expected 1-15 chars: letters, digits, '_', '.', ':', '-')"
}

validate_bool_01() {
  local value="$1"
  local name="${2:-value}"
  case "${value}" in
    0|1) ;;
    *) die "${name} must be 0 or 1 (got: '${value}')" ;;
  esac
}

# generate_alnum_secret: print a random ${1:-24}-char alphanumeric secret.
# Uses `openssl rand -hex` when available: hex output is shell-safe, and
# truncating in bash (instead of `head -c`) avoids a SIGPIPE under pipefail.
# Falls back to /dev/urandom when openssl is absent.
generate_alnum_secret() {
  local length="${1:-24}"
  local pass=""
  if command -v openssl >/dev/null 2>&1; then
    pass="$(openssl rand -hex "$(( (length + 1) / 2 ))" | tr -d '\n')"
    pass="${pass:0:${length}}"
  elif [[ -r /dev/urandom ]]; then
    pass="$(tr -dc 'A-Za-z0-9' </dev/urandom | dd bs="${length}" count=1 2>/dev/null || true)"
    pass="${pass:0:${length}}"
  fi
  [[ ${#pass} -eq "${length}" ]] || die "failed to generate a ${length}-character secret"
  printf '%s' "${pass}"
}

patch_cubelet_config_template() {
  local cubelet_config="$1"
  local eth_name="${2:-}"
  local network_cidr="${3:-}"
  local cube_router_enable="${4:-}"
  local cube_router_cidr="${5:-}"
  local cube_egress_admin_port="${6:-}"

  ensure_file "${cubelet_config}"
  if [[ -L "${cubelet_config}" ]]; then
    die "refusing to patch a symlink target: ${cubelet_config} -> $(readlink "${cubelet_config}")"
  fi

  if [[ -n "${eth_name}" ]]; then
    validate_interface_name "${eth_name}" "CUBE_SANDBOX_ETH_NAME"
    if grep -Eq '^[[:space:]]*eth_name = "' "${cubelet_config}"; then
      sed -i -E "s|^([[:space:]]*)eth_name = \"[^\"]*\"|\1eth_name = \"${eth_name}\"|" "${cubelet_config}"
      if ! grep -Eq "^[[:space:]]*eth_name = \"${eth_name}\"\$" "${cubelet_config}"; then
        log "WARNING: failed to patch eth_name in Cubelet config (${cubelet_config})"
      fi
    else
      log "WARNING: Cubelet config missing eth_name key; skipped NIC patch (${cubelet_config})"
    fi
  fi

  if [[ -n "${network_cidr}" ]]; then
    if grep -Eq '^[[:space:]]*cidr = "' "${cubelet_config}"; then
      sed -i -E "s|^([[:space:]]*)cidr = \"[^\"]*\"|\1cidr = \"${network_cidr}\"|" "${cubelet_config}"
      if ! grep -Eq "^[[:space:]]*cidr = \"${network_cidr}\"\$" "${cubelet_config}"; then
        log "WARNING: failed to patch cidr in Cubelet config (${cubelet_config})"
      fi
      log "patched cubevs CIDR: ${network_cidr}"
    else
      log "WARNING: Cubelet config missing cidr key; skipped CIDR patch (${cubelet_config})"
    fi
  fi

  if [[ -n "${cube_router_enable}" ]]; then
    validate_bool_01 "${cube_router_enable}" "CUBE_SANDBOX_CUBE_ROUTER_ENABLE"
    local cube_router_enable_toml="false"
    if [[ "${cube_router_enable}" == "1" ]]; then
      cube_router_enable_toml="true"
    fi
    if grep -Eq '^[[:space:]]*cube_router_enable = ' "${cubelet_config}"; then
      sed -i -E "s|^([[:space:]]*)cube_router_enable = .*|\1cube_router_enable = ${cube_router_enable_toml}|" "${cubelet_config}"
      if ! grep -Eq "^[[:space:]]*cube_router_enable = ${cube_router_enable_toml}\$" "${cubelet_config}"; then
        log "WARNING: failed to patch cube_router_enable in Cubelet config (${cubelet_config})"
      fi
      log "patched cube-router enable: ${cube_router_enable_toml}"
    else
      log "WARNING: Cubelet config missing cube_router_enable key; skipped cube-router enable patch (${cubelet_config})"
    fi
  fi

  if [[ -n "${cube_router_cidr}" ]]; then
    if grep -Eq '^[[:space:]]*cube_router_cidr = "' "${cubelet_config}"; then
      sed -i -E "s|^([[:space:]]*)cube_router_cidr = \"[^\"]*\"|\1cube_router_cidr = \"${cube_router_cidr}\"|" "${cubelet_config}"
      if ! grep -Eq "^[[:space:]]*cube_router_cidr = \"${cube_router_cidr}\"\$" "${cubelet_config}"; then
        log "WARNING: failed to patch cube_router_cidr in Cubelet config (${cubelet_config})"
      fi
      log "patched cube-router CIDR: ${cube_router_cidr}"
    else
      log "WARNING: Cubelet config missing cube_router_cidr key; skipped cube-router CIDR patch (${cubelet_config})"
    fi
  fi

  if [[ -n "${cube_egress_admin_port}" ]]; then
    case "${cube_egress_admin_port}" in
      *[!0-9]*|"")
        die "invalid CUBE_EGRESS_ADMIN_PORT: ${cube_egress_admin_port}"
        ;;
    esac
    local cube_egress_admin_url="http://127.0.0.1:${cube_egress_admin_port}"
    if grep -Eq '^[[:space:]]*cube_egress_admin_url = "' "${cubelet_config}"; then
      sed -i -E "s|^([[:space:]]*)cube_egress_admin_url = \"[^\"]*\"|\1cube_egress_admin_url = \"${cube_egress_admin_url}\"|" "${cubelet_config}"
      if ! grep -Eq "^[[:space:]]*cube_egress_admin_url = \"${cube_egress_admin_url}\"\$" "${cubelet_config}"; then
        log "WARNING: failed to patch cube_egress_admin_url in Cubelet config (${cubelet_config})"
      fi
      log "patched cube-egress admin URL: ${cube_egress_admin_url}"
    else
      log "WARNING: Cubelet config missing cube_egress_admin_url key; skipped admin URL patch (${cubelet_config})"
    fi
  fi
}

# ---------------------------------------------------------------------------
# Config-preserving upgrade helpers (M3-1/M3-2/M3-3).
#
# These power install.sh's `--mode upgrade` flow:
#   * detect_existing_install  - is there a prior one-click install?
#   * read_env_key             - read a KEY from an env file without sourcing
#   * read_version_field       - read a field from VERSION.txt
#   * version_lt               - best-effort semver "<" comparison
#   * merge_env_three_way      - merge old runtime env with new env.example
#   * resolve_install_mode     - decide install vs upgrade (with TTY prompt)
#   * preflight_upgrade        - role/downgrade/disk checks before upgrade
#   * backup_before_upgrade    - snapshot config before replacing artifacts
# ---------------------------------------------------------------------------

# assert_safe_install_prefix: refuse to perform a destructive full wipe of an
# obviously unsafe install root. Guards against a bad caller accidentally
# pointing a wipe at "/" or "/usr", or a foreign dir like "/usr/local" /
# "/var/lib", turning the wipe into a system-destroying `rm -rf`. Beyond the root/system/top-level denylist, a
# non-empty existing prefix is only wiped when it is a recognised CubeSandbox
# install (presence of a marker artifact such as .one-click.env / CubeMaster)
# or effectively empty. A lone '.backup' left over from an interrupted upgrade
# is fine only when it is a real directory, not a symlink. Non-existent prefixes
# are allowed (a fresh path the installer is about to create).
assert_safe_install_prefix() {
  local prefix="$1"

  [[ -n "${prefix}" ]] || die "refusing to wipe an empty install root"
  [[ "${prefix}" == /* ]] || die "refusing to wipe a non-absolute install root: ${prefix}"
  [[ ! -L "${prefix}" ]] || die "refusing to wipe a symlink install root: ${prefix}"

  # Normalize: drop a single trailing slash (but keep "/" detectable).
  local norm="${prefix%/}"
  [[ -n "${norm}" ]] || die "refusing to wipe the filesystem root: ${prefix}"
  [[ ! -L "${norm}" ]] || die "refusing to wipe a symlink install root: ${prefix}"

  case "${norm}" in
    /usr|/bin|/sbin|/lib|/lib64|/etc|/var|/boot|/dev|/proc|/sys|/run|/root|/home|/opt)
      die "refusing to wipe a system directory: ${prefix}"
      ;;
  esac

  if [[ -n "${HOME:-}" && "${norm}" == "${HOME%/}" ]]; then
    die "refusing to wipe the home directory: ${prefix}"
  fi

  # Require at least two non-empty path components (e.g. /a/b), so shallow
  # top-level directories cannot be wiped wholesale.
  local trimmed="${norm#/}"
  if [[ "${trimmed}" != */* ]]; then
    die "refusing to wipe a top-level directory: ${prefix} (install root must be at least two levels deep)"
  fi

  # Content sanity check: the custom-prefix wipe deletes every top-level entry
  # except '.backup'. Refuse unless the prefix is a recognised CubeSandbox
  # install (a marker artifact is present) or effectively empty (nothing to
  # destroy; '.backup' alone is accepted only when it is a real directory). This
  # closes the denylist gap -- e.g. /usr/local or /var/lib are deep enough and
  # not blacklisted, but hold foreign content with no CubeSandbox markers.
  if [[ -d "${norm}" ]]; then
    _assert_no_top_level_symlinks "${norm}" "${prefix}"
    _assert_cube_prefix_marker_or_empty "${norm}" "${prefix}"
  fi
}

_assert_no_top_level_symlinks() {
  local dir="$1"
  local display="$2"
  local symlink
  symlink="$(find "${dir}" -mindepth 1 -maxdepth 1 -type l -print -quit 2>/dev/null || true)"
  if [[ -n "${symlink}" ]]; then
    die "refusing to wipe install root ${display}: contains top-level symlink (${symlink}); move it away and retry"
  fi
}

_assert_cube_prefix_marker_or_empty() {
  local dir="$1"
  local display="$2"
  local cube_marker=""
  local m
  for m in .one-click.env CubeMaster CubeAPI Cubelet; do
    if [[ -e "${dir}/${m}" ]]; then
      cube_marker=1
      break
    fi
  done
  if [[ -z "${cube_marker}" ]]; then
    local stray
    stray="$(find "${dir}" -mindepth 1 -maxdepth 1 ! -name '.backup' -print -quit 2>/dev/null || true)"
    if [[ -n "${stray}" ]]; then
      die "refusing to wipe install root ${display}: directory is not empty and contains no CubeSandbox installation markers (.one-click.env / CubeMaster / CubeAPI / Cubelet). Remove the foreign content first."
    fi
  fi
}

wipe_custom_install_prefix_contents() {
  local prefix="$1"
  local norm before after

  assert_safe_install_prefix "${prefix}"
  norm="${prefix%/}"

  if [[ ! -d "${norm}" ]]; then
    mkdir -p "${norm}"
    return 0
  fi

  before="$(stat -c '%d:%i' -- "${norm}")" \
    || die "failed to stat install root before wipe: ${prefix}"

  (
    cd -- "${norm}" || die "failed to enter install root: ${prefix}"
    after="$(stat -c '%d:%i' -- .)" \
      || die "failed to stat install root after cd: ${prefix}"
    [[ "${before}" == "${after}" ]] \
      || die "install root changed while preparing to wipe: ${prefix}"

    # Re-run the marker/empty check against the pinned cwd. This closes the
    # gap between path validation and destructive deletion.
    _assert_no_top_level_symlinks "." "${prefix}"
    _assert_cube_prefix_marker_or_empty "." "${prefix}"
    find . -mindepth 1 -maxdepth 1 ! -name '.backup' -exec rm -rf -- {} +
  )
}

# detect_existing_install: an install is "present" when its runtime env file
# exists under the given prefix.
detect_existing_install() {
  local install_prefix="$1"
  [[ -f "${install_prefix}/.one-click.env" ]]
}

# read_env_key: extract the raw value of an active KEY=VALUE line from a file
# WITHOUT sourcing it (avoids executing arbitrary shell during preflight).
read_env_key() {
  local file="$1"
  local key="$2"
  # Validate the key is a plain env identifier before interpolating it into the
  # sed address; this prevents sed pattern/command injection if a future caller
  # passes user-controlled data.
  [[ "${key}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "invalid env key name: ${key}"
  [[ -f "${file}" ]] || return 0
  sed -n "/^${key}=/{s/^${key}=//;p;q;}" "${file}" 2>/dev/null || true
}

# read_version_field: read `field=value` from a VERSION.txt-style file.
read_version_field() {
  local file="$1"
  local field="$2"
  # Validate the field name before interpolating it into the sed address.
  [[ "${field}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "invalid version field name: ${field}"
  [[ -f "${file}" ]] || return 0
  sed -n "/^${field}=/{s/^${field}=//;p;q;}" "${file}" 2>/dev/null || true
}

# merge_env_three_way: produce a merged runtime env that preserves the user's
# existing values while adopting new keys/defaults from the new env.example.
#
#   merge_env_three_way NEW_EXAMPLE OLD_RUNTIME OLD_BASELINE NEW_DOTENV OUT DIFF
#
# OLD_BASELINE / NEW_DOTENV may be empty strings (absent). The merge is purely
# line-based: every value is preserved with its original right-hand side, so
# shell-sensitive payloads (${VAR} expansions, URLs with ://@, quotes) survive
# untouched. The new env.example provides the structural template (comments,
# ordering, new keys); old-only keys are appended verbatim and never dropped.
merge_env_three_way() {
  local new_example="$1"
  local old_runtime="$2"
  local old_baseline="$3"
  local new_dotenv="$4"
  local out_file="$5"
  local diff_file="$6"

  require_cmd python3
  ensure_file "${new_example}"
  ensure_file "${old_runtime}"

  python3 - "${new_example}" "${old_runtime}" "${old_baseline}" "${new_dotenv}" "${out_file}" "${diff_file}" <<'PY'
import re
import sys

new_example, old_runtime, old_baseline, new_dotenv, out_file, diff_file = sys.argv[1:7]

KV_RE = re.compile(r'^([A-Za-z_][A-Za-z0-9_]*)=(.*)$')


def fail(message):
    sys.stderr.write("[one-click] ERROR: %s\n" % message)
    sys.exit(1)


def read_lines(path, required=True):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return fh.read().splitlines()
    except FileNotFoundError:
        if required:
            fail("env merge input not found: %s" % path)
        return []
    except UnicodeDecodeError:
        fail("env merge input is not valid UTF-8: %s" % path)


def parse(path):
    """Ordered dict key -> raw value for active KEY=VALUE lines (last wins).

    KEY must start at column 0 (KV_RE is anchored); indented `KEY=value` lines
    are treated as structural text and preserved verbatim. The one-click env
    files never indent keys, so this is safe.
    """
    kv = {}
    if not path:
        return kv
    for line in read_lines(path, required=False):
        stripped = line.lstrip()
        if not stripped or stripped.startswith("#"):
            continue
        m = KV_RE.match(line)
        if m:
            kv[m.group(1)] = m.group(2)
    return kv


# Obsolete keys: removed from env.example and no longer read by any component.
# They are actively dropped on upgrade (rather than kept verbatim) so that stale
# plaintext secrets do not linger in the runtime env file. The AgentHub LLM
# config (key/provider/base_url/model/credential_mode) now lives encrypted in the
# database (configured via the WebUI), and the DB master key is auto-bootstrapped
# by CubeAPI, so AGENTHUB_SECRET_KEY is obsolete too.
DEPRECATED_KEYS = {
    "ONE_CLICK_INSTALL_PREFIX",
    "ONE_CLICK_TOOLBOX_ROOT",
    "AGENTHUB_DEEPSEEK_API_KEY",
    "OPENCLAW_DEEPSEEK_API_KEY",
    "AGENTHUB_LLM_API_KEY",
    "OPENCLAW_LLM_API_KEY",
    "AGENTHUB_LLM_PROVIDER",
    "OPENCLAW_LLM_PROVIDER",
    "AGENTHUB_LLM_BASE_URL",
    "OPENCLAW_LLM_BASE_URL",
    "AGENTHUB_LLM_MODEL",
    "OPENCLAW_DEFAULT_MODEL",
    "AGENTHUB_LLM_CREDENTIAL_MODE",
    "AGENTHUB_SECRET_KEY",
    "CUBE_API_DATABASE_URL",
    # cube-proxy now pulls a pre-published image (MIRROR / CUBE_SANDBOX_CUBE_PROXY_IMAGE);
    # the old local-build knobs must not linger as kept-extra after upgrade.
    "CUBE_PROXY_IMAGE_TAG",
    "CUBE_PROXY_BASE_IMAGE",
    # Build-machine knobs used to live in env.example and could leak into
    # .one-click.env. They now live in build.env.example and are unused at
    # install/runtime, so drop them on upgrade instead of keeping them extra.
    "BUILDER_IMAGE",
    "ONE_CLICK_CUBEMASTER_BUILD_MODE",
    "ONE_CLICK_CUBELET_BUILD_MODE",
    "ONE_CLICK_CUBE_API_BUILD_MODE",
    "ONE_CLICK_CUBE_OPS_BUILD_MODE",
    "ONE_CLICK_CUBE_AGENT_BUILD_MODE",
    "ONE_CLICK_CUBE_SHIM_BUILD_MODE",
    "ONE_CLICK_CUBE_INIT_BUILD_MODE",
    "ONE_CLICK_BUILD_JOBS",
    "ONE_CLICK_DISABLE_PIGZ",
    "ONE_CLICK_SEQUENTIAL_WEB_BUILD",
    "CUBE_BUILD_TIME",
    "ONE_CLICK_CUBEMASTER_BIN",
    "ONE_CLICK_CUBEMASTERCLI_BIN",
    "ENVD_LOCAL_PATH",
    "ONE_CLICK_CUBELET_BIN",
    "ONE_CLICK_CUBECLI_BIN",
    "ONE_CLICK_CUBE_API_BIN",
    "ONE_CLICK_CUBE_OPS_BIN",
    "ONE_CLICK_CUBE_AGENT_BIN",
    "ONE_CLICK_CUBE_INIT_BIN",
    "ONE_CLICK_CUBESHIM_BIN",
    "ONE_CLICK_CUBE_RUNTIME_BIN",
    "ONE_CLICK_GUEST_IMAGE_DOCKERFILE",
    "ONE_CLICK_GUEST_IMAGE_CONTEXT_DIR",
    "ONE_CLICK_GUEST_IMAGE_REF",
    "ONE_CLICK_GUEST_IMAGE_VERSION",
    "ONE_CLICK_GUEST_IMAGE_TAR",
    "ONE_CLICK_AGENT_EXT4_OUTPUT_DIR",
    "ONE_CLICK_CUBE_KERNEL_VMLINUX",
    "ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX",
    "ONE_CLICK_RUNTIME_CFG_SRC",
    "ONE_CLICK_GUEST_IMAGE_RESERVED_BYTES",
    "ONE_CLICK_MKCERT_BIN",
    "ONE_CLICK_WEB_DIST_DIR",
}

LEGACY_CUBE_PROXY_CERT_DIR_DEFAULTS = {
    '"${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs"',
    "'${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs'",
    "${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs",
}

# Old one-click default for the locally-built cube-proxy image. Upgrades that
# still carry this exact value drop it (via DEPRECATED_KEYS) and adopt the
# pre-published TCR image selected by MIRROR; only non-default custom tags are
# migrated to CUBE_SANDBOX_CUBE_PROXY_IMAGE.
LEGACY_CUBE_PROXY_IMAGE_TAG_DEFAULT = "cube-proxy:one-click"


def normalize_legacy_value(key, val, tmpl_val):
    if key == "CUBE_PROXY_CERT_DIR" and val in LEGACY_CUBE_PROXY_CERT_DIR_DEFAULTS:
        return tmpl_val, True
    return val, False


new_defaults = parse(new_example)
old_values = parse(old_runtime)
old_baseline_vals = parse(old_baseline) if old_baseline else {}
new_overrides = parse(new_dotenv) if new_dotenv else {}
has_baseline = bool(old_baseline_vals)

added = []
updated_default = []
preserved = []
explicit = []
migrated_legacy = []
dropped = []

out_lines = []
template = read_lines(new_example)

for line in template:
    stripped = line.lstrip()
    if not stripped or stripped.startswith("#"):
        out_lines.append(line)
        continue
    m = KV_RE.match(line)
    if not m:
        out_lines.append(line)
        continue
    key = m.group(1)
    tmpl_val = m.group(2)
    chosen = tmpl_val
    # Treat a new-bundle .env value as an explicit operator override ONLY when it
    # differs from the new env.example default. This is intentional: the common
    # way to create a .env is `cp env.example .env`, which would otherwise make
    # every key an "override" and clobber the user's existing customizations.
    # (Installer toggle keys such as ONE_CLICK_ENABLE_S3LVOL are NOT special-
    # cased here: their this-run intent is captured and re-applied around this
    # merge by snapshot_one_click_toggles / apply_one_click_toggles in
    # install.sh, and persisted via upsert_env_kv afterwards.)
    if key in new_overrides and new_overrides[key] != new_defaults.get(key):
        chosen = new_overrides[key]
        explicit.append(key)
    elif key in old_values:
        ov, migrated = normalize_legacy_value(key, old_values[key], tmpl_val)
        if migrated:
            migrated_legacy.append((key, old_values[key], ov))
        if (has_baseline and key in old_baseline_vals
                and ov == old_baseline_vals[key] and ov != tmpl_val):
            chosen = tmpl_val
            updated_default.append((key, ov, tmpl_val))
        else:
            chosen = ov
            if ov != tmpl_val:
                preserved.append((key, ov))
    else:
        added.append((key, tmpl_val))
    out_lines.append("%s=%s" % (key, chosen))

# Migrate a customized CUBE_PROXY_IMAGE_TAG into CUBE_SANDBOX_CUBE_PROXY_IMAGE
# before the obsolete key is dropped. The old default cube-proxy:one-click is
# NOT migrated: upgrades adopt the pre-published TCR image selected by MIRROR.
legacy_proxy_image_tag = old_values.get("CUBE_PROXY_IMAGE_TAG", "").strip()
if (legacy_proxy_image_tag
        and legacy_proxy_image_tag != LEGACY_CUBE_PROXY_IMAGE_TAG_DEFAULT
        and "CUBE_SANDBOX_CUBE_PROXY_IMAGE" not in old_values):
    old_values["CUBE_SANDBOX_CUBE_PROXY_IMAGE"] = legacy_proxy_image_tag
    migrated_legacy.append((
        "CUBE_PROXY_IMAGE_TAG",
        legacy_proxy_image_tag,
        "CUBE_SANDBOX_CUBE_PROXY_IMAGE=%s" % legacy_proxy_image_tag,
    ))

# Old-only keys (present in old runtime, absent from the new template) are
# host/user specific (NODE_IP, ROLE, control-plane addr, custom vars). Never
# drop them: append verbatim so the running system keeps working.
dropped = [k for k in old_values if k in DEPRECATED_KEYS]
extra = [(k, v) for k, v in old_values.items()
         if k not in new_defaults and k not in DEPRECATED_KEYS]
if extra:
    out_lines.append("")
    out_lines.append("# --- preserved custom settings (not in env.example) ---")
    for k, v in extra:
        out_lines.append("%s=%s" % (k, v))

with open(out_file, "w", encoding="utf-8") as fh:
    fh.write("\n".join(out_lines) + "\n")

# Redact secret-bearing values in the human-readable diff report. The report
# is persisted to the (on-disk) upgrade backup directory, so it must not leak
# passwords/tokens/connection strings in plaintext. The merged output file
# (out_file) intentionally keeps the real values -- it IS the runtime env.
SECRET_RE = re.compile(
    r'(PASSWORD|PASSWD|SECRET|TOKEN|CREDENTIAL|PRIVATE_KEY|DATABASE_URL|API_KEY|ACCESS_KEY|CLIENT_SECRET|AUTH_TOKEN)',
    re.I)


def redact(key, val):
    return "***REDACTED***" if SECRET_RE.search(key) else val


report = []
report.append("env merge report (mode=%s)" % ("three-way" if has_baseline else "two-way-fallback"))
report.append("")
report.append("[added] new keys filled with new defaults: %d" % len(added))
for k, v in added:
    report.append("  + %s=%s" % (k, redact(k, v)))
report.append("[default-updated] untouched keys adopting new default: %d" % len(updated_default))
for k, ov, nv in updated_default:
    report.append("  ~ %s: %s -> %s" % (k, redact(k, ov), redact(k, nv)))
report.append("[preserved] kept your customized values: %d" % len(preserved))
for k, v in preserved:
    report.append("  = %s=%s" % (k, redact(k, v)))
report.append("[migrated-legacy] legacy defaults rewritten to new fixed defaults: %d" % len(migrated_legacy))
for k, ov, nv in migrated_legacy:
    report.append("  ^ %s: %s -> %s" % (k, redact(k, ov), redact(k, nv)))
report.append("[explicit] taken from new .env overrides: %d" % len(explicit))
for k in explicit:
    report.append("  ! %s" % k)
report.append("[kept-extra] old-only keys not in new env.example (kept): %d" % len(extra))
for k, v in extra:
    report.append("  > %s=%s" % (k, redact(k, v)))
report.append("[dropped] obsolete keys removed on upgrade: %d" % len(dropped))
for k in dropped:
    report.append("  - %s" % k)

with open(diff_file, "w", encoding="utf-8") as fh:
    fh.write("\n".join(report) + "\n")

sys.stderr.write(
    "[one-click] env merge: +%d new, ~%d default-updated, =%d preserved, ^%d migrated-legacy, >%d kept-extra, -%d dropped%s\n" % (
        len(added), len(updated_default), len(preserved), len(migrated_legacy), len(extra), len(dropped),
        "" if has_baseline else " (two-way fallback: no baseline)"))
PY
}

# resolve_install_mode: decide between "install" (full reinstall) and
# "upgrade" (config preserving). Prints the resolved mode to stdout; all
# human-facing output goes to stderr so it can be captured via $(...).
#
#   resolve_install_mode REQUESTED_MODE INSTALL_PREFIX ASSUME_YES
#
# REQUESTED_MODE is one of "", install, upgrade, auto. When empty and an
# existing install is detected, defaults to upgrade: prompts on a TTY
# (default: Y) and proceeds with upgrade when non-interactive. Use
# --mode=install to wipe and reinstall.
resolve_install_mode() {
  local requested="$1"
  local install_prefix="$2"
  local assume_yes="$3"

  local existing="no"
  detect_existing_install "${install_prefix}" && existing="yes"

  case "${requested}" in
    install)
      printf 'install\n'
      return 0
      ;;
    upgrade)
      if [[ "${existing}" != "yes" ]]; then
        die "no existing installation found under ${install_prefix} (missing .one-click.env); cannot upgrade. Run without --mode=upgrade for a fresh install."
      fi
      printf 'upgrade\n'
      return 0
      ;;
    auto)
      if [[ "${existing}" == "yes" ]]; then
        printf 'upgrade\n'
      else
        printf 'install\n'
      fi
      return 0
      ;;
  esac

  # Unset mode: fresh install if nothing is present; otherwise preserve config.
  if [[ "${existing}" != "yes" ]]; then
    printf 'install\n'
    return 0
  fi

  if [[ "${assume_yes}" == "1" ]]; then
    log "existing installation detected; --yes given, running config-preserving upgrade."
    printf 'upgrade\n'
    return 0
  fi

  if [[ -t 0 ]]; then
    printf '%s' "[one-click] Existing installation detected under ${install_prefix}.
[one-click] Run a config-preserving UPGRADE (keep your .one-click.env)? [Y/n]: " >&2
    local reply=""
    read -r reply || reply=""
    case "${reply}" in
      [Nn]|[Nn][Oo])
        log "proceeding with full reinstall; existing config WILL be reset."
        printf 'install\n'
        ;;
      *)
        log "proceeding with config-preserving upgrade."
        printf 'upgrade\n'
        ;;
    esac
    return 0
  fi

  log "existing installation detected; running non-interactively without --mode, defaulting to config-preserving upgrade."
  log "to wipe and reinstall, re-run with --mode=install."
  printf 'upgrade\n'
  return 0
}

# preflight_upgrade: fail-fast checks before a config-preserving upgrade.
#
#   preflight_upgrade INSTALL_PREFIX BUNDLE_DIR PACKAGE_TAR NEW_ROLE \
#                     ALLOW_ROLE_CHANGE ALLOW_DOWNGRADE
preflight_upgrade() {
  local install_prefix="$1"
  local bundle_dir="$2"
  local package_tar="$3"
  local new_role="$4"
  local allow_role_change="$5"
  local allow_downgrade="$6"

  if [[ ! -d "${install_prefix}/scripts" ]]; then
    log "WARNING: ${install_prefix}/scripts not found; existing install may be incomplete"
  fi

  local old_role
  old_role="$(read_env_key "${install_prefix}/.one-click.env" ONE_CLICK_DEPLOY_ROLE)"
  old_role="${old_role:-control}"
  if [[ "${old_role}" != "${new_role}" ]]; then
    if [[ "${allow_role_change}" == "1" ]]; then
      log "WARNING: changing node role on upgrade: ${old_role} -> ${new_role} (--allow-role-change)"
    else
      die "refusing to change node role on upgrade: installed=${old_role}, requested=${new_role}. Re-run with the matching role, or pass --allow-role-change to override."
    fi
  fi

  local old_ver new_ver
  old_ver="$(read_version_field "${install_prefix}/VERSION.txt" release_version)"
  new_ver="$(read_version_field "${bundle_dir}/VERSION.txt" release_version)"
  if [[ -n "${old_ver}" && -n "${new_ver}" ]]; then
    log "upgrade version: ${old_ver} -> ${new_ver}"
    if [[ "${old_ver}" == "${new_ver}" ]]; then
      log "note: re-installing the same version (${new_ver})."
    elif version_lt "${new_ver}" "${old_ver}"; then
      if [[ "${allow_downgrade}" == "1" ]]; then
        log "WARNING: downgrade allowed: ${old_ver} -> ${new_ver} (--allow-downgrade)"
      else
        die "refusing to downgrade: installed=${old_ver}, package=${new_ver}. Pass --allow-downgrade to override."
      fi
    fi
  else
    log "version comparison skipped (missing/unparseable VERSION.txt); proceeding."
  fi

  preflight_upgrade_disk_space "${install_prefix}" "${package_tar}"
}

# preflight_upgrade_disk_space: ensure enough free space for extract + copy +
# backup. Best-effort: skips silently when df/stat are unavailable.
preflight_upgrade_disk_space() {
  local install_prefix="$1"
  local package_tar="$2"

  command -v df >/dev/null 2>&1 || { log "df unavailable; skipping disk space preflight"; return 0; }
  command -v stat >/dev/null 2>&1 || { log "stat unavailable; skipping disk space preflight"; return 0; }
  [[ -f "${package_tar}" ]] || return 0

  local pkg_bytes need_kb avail_kb check
  pkg_bytes="$(stat -c %s "${package_tar}" 2>/dev/null || echo 0)"
  # New artifacts + extraction headroom: ~3x the compressed package + 100MB.
  need_kb=$(( (pkg_bytes / 1024) * 3 + 102400 ))

  check="${install_prefix}"
  while [[ ! -e "${check}" ]]; do
    local parent
    parent="$(dirname "${check}")"
    [[ "${parent}" != "${check}" ]] || break
    check="${parent}"
  done

  avail_kb="$(df -Pk "${check}" 2>/dev/null | awk 'NR==2 {print $4}')"
  if [[ -n "${avail_kb}" && "${avail_kb}" =~ ^[0-9]+$ && "${need_kb}" -gt 0 && "${avail_kb}" -lt "${need_kb}" ]]; then
    die "insufficient disk space for upgrade under ${check}: need ~$((need_kb / 1024))MB, available $((avail_kb / 1024))MB"
  fi
  if [[ -n "${avail_kb}" && "${avail_kb}" =~ ^[0-9]+$ ]]; then
    log "disk space preflight OK ($((avail_kb / 1024))MB available, ~$((need_kb / 1024))MB required)"
  fi
}

# backup_before_upgrade: snapshot the runtime env + component configs + version
# metadata into a timestamped backup dir. Prints the backup dir to stdout.
backup_before_upgrade() {
  local install_prefix="$1"
  local ts backup_root backup_dir rel
  ts="$(date +%Y%m%d-%H%M%S)"
  backup_root="${install_prefix}/.backup"
  [[ ! -L "${backup_root}" ]] || die "refusing to use symlink backup directory: ${backup_root}"
  backup_dir="${backup_root}/upgrade-${ts}"
  mkdir -p "${backup_dir}"
  # The backup holds secret-bearing config (.one-click.env, conf files); keep
  # it owner-only so secrets are not world/group readable on disk.
  chmod 700 "${backup_root}" 2>/dev/null || true
  chmod 700 "${backup_dir}" 2>/dev/null || true

  for rel in \
    ".one-click.env" \
    "env.example" \
    "VERSION.txt" \
    "release-manifest.json" \
    "CubeMaster/conf.yaml" \
    "CubeMaster/plugin/volume-cos.conf" \
    "CubeMaster/plugin/volume-s3.conf" \
    "Cubelet/config/config.toml" \
    "Cubelet/plugin/volume-cos.conf" \
    "Cubelet/plugin/volume-s3.conf" \
    "cube-shim/conf/config-cube.toml" \
    "network-agent/network-agent.yaml" \
    "cubeproxy/global.conf" \
    "cubeproxy/nginx.conf" \
    "coredns/Corefile" \
    "coredns/resolv.conf.upstream" \
    "webui/nginx.generated.conf"
  do
    if [[ -f "${install_prefix}/${rel}" ]]; then
      mkdir -p "${backup_dir}/$(dirname "${rel}")"
      cp -a "${install_prefix}/${rel}" "${backup_dir}/${rel}"
      # Secret-bearing config files: restrict to owner-only in the backup.
      case "${rel}" in
        ".one-click.env"|"env.example"|*conf.yaml|*config.toml|*.yaml|*.conf)
          chmod 600 "${backup_dir}/${rel}" 2>/dev/null || true
          ;;
      esac
    fi
  done

  if [[ -d "${install_prefix}/Cubelet/dynamicconf" ]]; then
    mkdir -p "${backup_dir}/Cubelet"
    cp -a "${install_prefix}/Cubelet/dynamicconf" "${backup_dir}/Cubelet/dynamicconf"
  fi

  log "backed up existing config to ${backup_dir}"
  printf '%s\n' "${backup_dir}"
}

# restore_volume_plugin_config_from_upgrade_backup: on upgrade, put operator-edited
# volume-cos.conf back after the packaged CubeMaster/Cubelet trees are replaced.
# volume-s3.conf is rewritten from CUBE_S3_* so it is not restored.
restore_volume_plugin_config_from_upgrade_backup() {
  local install_prefix="$1"
  local install_mode="$2"
  local backup_dir="$3"
  local rel src dst

  [[ "${install_mode}" == "upgrade" && -n "${backup_dir}" && -d "${backup_dir}" ]] || return 0

  for rel in \
    "CubeMaster/plugin/volume-cos.conf" \
    "Cubelet/plugin/volume-cos.conf"
  do
    src="${backup_dir}/${rel}"
    dst="${install_prefix}/${rel}"
    if [[ -f "${src}" ]]; then
      mkdir -p "$(dirname "${dst}")"
      cp -a "${src}" "${dst}"
      chmod 600 "${dst}" 2>/dev/null || true
      log "restored ${rel} from upgrade backup"
    fi
  done
}

# seed_volume_plugin_config: create volume-cos.conf from the shipped example on
# first install; skip when the file already exists (including after restore).
seed_volume_plugin_config() {
  local install_prefix="$1"
  local deploy_role="$2"
  local plugin_dir example conf

  for plugin_dir in \
    "CubeMaster/plugin" \
    "Cubelet/plugin"
  do
    [[ "${deploy_role}" == "compute" && "${plugin_dir}" == CubeMaster/plugin ]] && continue
    [[ -d "${install_prefix}/${plugin_dir}" ]] || continue
    example="${install_prefix}/${plugin_dir}/volume-cos.conf.example"
    conf="${install_prefix}/${plugin_dir}/volume-cos.conf"
    if [[ ! -f "${conf}" && -f "${example}" ]]; then
      cp -a "${example}" "${conf}"
      chmod 600 "${conf}"
      log "seeded ${plugin_dir}/volume-cos.conf from example (edit COS credentials before use)"
    fi
  done
}

# local_minio_s3_endpoint: the CUBE_S3_ENDPOINT install.sh fills from the
# bundled MinIO. Single source of truth so the MinIO/S3 exclusivity check and
# the fill cannot drift apart.
local_minio_s3_endpoint() {
  local bind port
  bind="${CUBE_SANDBOX_MINIO_API_BIND:-${CUBE_SANDBOX_NODE_IP:-127.0.0.1}}"
  port="${CUBE_SANDBOX_MINIO_API_PORT:-9000}"
  printf 'http://%s:%s' "${bind}" "${port}"
}

# True when CUBE_S3_* is the leftover fill from a previous local-MinIO install
# (not an operator-supplied external store). After a bind/IP change the endpoint
# may still point at the old node IP; matching MinIO root credentials is enough,
# because fill_s3_from_local_minio rewrites CUBE_S3_*.
s3_config_is_local_minio_fill() {
  local expected
  expected="$(local_minio_s3_endpoint)"
  [[ "${CUBE_S3_ENDPOINT:-}" == "${expected}" ]] && return 0
  [[ -n "${CUBE_S3_ACCESS_KEY_ID:-}" \
     && -n "${CUBE_S3_SECRET_ACCESS_KEY:-}" \
     && -n "${CUBE_SANDBOX_MINIO_ROOT_USER:-}" \
     && -n "${CUBE_SANDBOX_MINIO_ROOT_PASSWORD:-}" \
     && "${CUBE_S3_ACCESS_KEY_ID}" == "${CUBE_SANDBOX_MINIO_ROOT_USER}" \
     && "${CUBE_S3_SECRET_ACCESS_KEY}" == "${CUBE_SANDBOX_MINIO_ROOT_PASSWORD}" ]] && return 0
  return 1
}

# A user-supplied CUBE_S3_ENDPOINT means an external store, which cannot be
# combined with installing local MinIO. A previous install's filled local
# endpoint (or matching MinIO credentials after an IP/bind change) is allowed so
# upgrades can re-run; fill_s3_from_local_minio then rewrites CUBE_S3_*.
check_minio_not_combined_with_user_s3() {
  [[ "${CUBE_SANDBOX_MINIO_ENABLED:-}" == "1" ]] || return 0
  [[ -n "${CUBE_S3_ENDPOINT:-}" ]] || return 0
  if s3_config_is_local_minio_fill; then
    return 0
  fi
  die "CUBE_SANDBOX_MINIO_ENABLED=1 cannot be combined with CUBE_S3_ENDPOINT; set CUBE_SANDBOX_MINIO_ENABLED=0 to use an external S3 backend"
}

# warn_compute_s3_missing: warn (do not abort) when a compute node has no S3
# backend configured. Compute nodes never deploy MinIO (install.sh forces
# CUBE_SANDBOX_MINIO_ENABLED=0), so the volume plugin resolves the S3 store
# solely from CUBE_S3_*. Installing without it is allowed, but the volume
# plugin stays disabled until the values are provided and install is re-run.
warn_compute_s3_missing() {
  is_compute_role || return 0
  [[ -n "${CUBE_S3_ENDPOINT:-}" ]] && return 0
  warn "compute node has no CUBE_S3_* backend configured; the S3 volume
plugin will stay disabled until you set one. To fix this:

  1. On the control node, run: grep '^CUBE_S3_' /usr/local/services/cubetoolbox/.one-click.env
     (empty output means the control node itself has no S3 backend)
  2. Paste the output into this node's .env
  3. Re-run: sudo ./install-compute.sh

With bundled MinIO, also allow TCP 9000 from this node to the control node."
  return 0
}

# Print KEY='value' so bash `source` of volume-s3.conf is safe. Apostrophes in
# value become '"'"' (close quote, literal ', reopen). Do not use printf %q:
# Helm emits the same quoting, and $'...' forms would not match.
shell_assign() {
  local key="$1"
  local value="$2"
  printf "%s='%s'\n" "${key}" "${value//\'/\'\"\'\"\'}"
}

# write_volume_s3_conf is always rendered from CUBE_S3_* (never from MinIO
# deploy vars). Local MinIO fills CUBE_S3_* in install.sh before this runs.
write_volume_s3_conf_file() {
  local conf="$1"
  local access_key="$2"
  local secret_key="$3"
  local bucket="$4"
  local endpoint="$5"
  local region="$6"
  local extra_opts="$7"

  mkdir -p "$(dirname "${conf}")"
  {
    shell_assign ACCESS_KEY_ID "${access_key}"
    shell_assign SECRET_ACCESS_KEY "${secret_key}"
    shell_assign BUCKET "${bucket}"
    shell_assign ENDPOINT "${endpoint}"
    shell_assign REGION "${region}"
    if [[ -n "${extra_opts}" ]]; then
      shell_assign S3FS_EXTRA_OPTS "${extra_opts}"
    fi
  } > "${conf}"
  chmod 600 "${conf}"
}

remove_volume_s3_conf() {
  local install_prefix="$1"
  local deploy_role="$2"
  local plugin_dir conf
  for plugin_dir in \
    "CubeMaster/plugin" \
    "Cubelet/plugin"
  do
    [[ "${deploy_role}" == "compute" && "${plugin_dir}" == CubeMaster/plugin ]] && continue
    conf="${install_prefix}/${plugin_dir}/volume-s3.conf"
    [[ -f "${conf}" ]] || continue
    rm -f "${conf}"
    log "removed ${plugin_dir}/volume-s3.conf (no S3 backend configured)"
  done
}

write_volume_s3_conf() {
  local install_prefix="$1"
  local deploy_role="$2"
  local plugin_dir conf

  if [[ -z "${CUBE_S3_ENDPOINT:-}" ]]; then
    remove_volume_s3_conf "${install_prefix}" "${deploy_role}"
    return 0
  fi

  for plugin_dir in \
    "CubeMaster/plugin" \
    "Cubelet/plugin"
  do
    [[ "${deploy_role}" == "compute" && "${plugin_dir}" == CubeMaster/plugin ]] && continue
    [[ -d "${install_prefix}/${plugin_dir}" ]] || continue
    conf="${install_prefix}/${plugin_dir}/volume-s3.conf"
    write_volume_s3_conf_file \
      "${conf}" \
      "${CUBE_S3_ACCESS_KEY_ID:-}" \
      "${CUBE_S3_SECRET_ACCESS_KEY:-}" \
      "${CUBE_S3_BUCKET:-cube-volumes}" \
      "${CUBE_S3_ENDPOINT}" \
      "${CUBE_S3_REGION:-us-east-1}" \
      "${CUBE_S3_S3FS_EXTRA_OPTS:-}"
    log "wrote ${plugin_dir}/volume-s3.conf endpoint=${CUBE_S3_ENDPOINT} bucket=${CUBE_S3_BUCKET:-cube-volumes}"
  done
}

# Sentinel that marks an installer-generated s3.cfg. A file without this line
# is treated as operator-owned and is never overwritten.
S3LVOL_CFG_SENTINEL="generated by cube-sandbox one-click; do not edit"

# Print KEY="value" for s3.cfg. rcow_cfg_get's sed requires double quotes and
# cannot unescape, so a literal quote in the value is rejected.
toml_assign() {
  local key="$1"
  local value="$2"
  if [[ "${value}" == *\"* ]]; then
    die "s3.cfg value for ${key} must not contain double quotes"
  fi
  printf '%s="%s"\n' "${key}" "${value}"
}

# Strip http(s):// and any path from CUBE_S3_ENDPOINT. s3lvol uses the host
# as the HTTP Host header; the scheme becomes no_tls instead.
s3lvol_host_from_endpoint() {
  local url="$1"
  url="${url#http://}"
  url="${url#https://}"
  url="${url%%/*}"
  printf '%s' "${url}"
}

s3lvol_no_tls_from_endpoint() {
  [[ "${1:-}" == http://* ]]
}

# Whether s3.cfg should set path_style="true".
# CUBE_S3LVOL_PATH_STYLE=1/0 wins; otherwise local MinIO or s3fs path-style opts.
s3lvol_path_style() {
  case "${CUBE_S3LVOL_PATH_STYLE:-}" in
    1|true|TRUE|yes|YES) return 0 ;;
    0|false|FALSE|no|NO) return 1 ;;
  esac
  if s3_config_is_local_minio_fill; then
    return 0
  fi
  [[ "${CUBE_S3_S3FS_EXTRA_OPTS:-}" == *use_path_request_style* ]]
}

# write_s3lvol_cfg_file: render one s3.cfg. path_style/no_tls are 0/1.
write_s3lvol_cfg_file() {
  local conf="$1"
  local access_key="$2"
  local secret_key="$3"
  local bucket="$4"
  local host="$5"
  local region="$6"
  local path_style="$7"
  local no_tls="$8"
  local tmp_file old_umask

  if [[ "${bucket}" == *\"* ]]; then
    die "s3.cfg value for buckets must not contain double quotes"
  fi

  mkdir -p "$(dirname "${conf}")"
  old_umask="$(umask)"
  umask 077
  tmp_file="$(mktemp "${conf}.XXXXXX")"
  chmod 600 "${tmp_file}"
  {
    printf '# %s\n' "${S3LVOL_CFG_SENTINEL}"
    toml_assign access_key_id "${access_key}"
    toml_assign secret_access_key "${secret_key}"
    toml_assign endpoint "${host}"
    toml_assign region "${region}"
    printf 'buckets=["%s"]\n' "${bucket}"
    if [[ "${path_style}" == "1" ]]; then
      toml_assign path_style "true"
    fi
    if [[ "${no_tls}" == "1" ]]; then
      toml_assign no_tls "true"
    fi
  } > "${tmp_file}"
  mv -f "${tmp_file}" "${conf}"
  umask "${old_umask}"
  chmod 600 "${conf}"
}

# write_s3lvol_cfg: generate /data/cubelet/s3.cfg from CUBE_S3_* when
# s3lvol is enabled. Leaves a hand-written file (no sentinel) alone.
write_s3lvol_cfg() {
  local conf host no_tls=0 path_style=0

  [[ "${ONE_CLICK_ENABLE_S3LVOL:-0}" == "1" ]] || return 0

  conf="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"
  if [[ -z "${CUBE_S3_ENDPOINT:-}" ]]; then
    return 0
  fi
  if [[ -f "${conf}" ]] && ! grep -Fq "${S3LVOL_CFG_SENTINEL}" "${conf}"; then
    log "keeping hand-written ${conf} (no one-click sentinel)"
    return 0
  fi

  host="$(s3lvol_host_from_endpoint "${CUBE_S3_ENDPOINT}")"
  [[ -n "${host}" ]] || die "CUBE_S3_ENDPOINT=${CUBE_S3_ENDPOINT} has no host; cannot write ${conf}"
  if s3lvol_no_tls_from_endpoint "${CUBE_S3_ENDPOINT}"; then
    no_tls=1
  fi
  if s3lvol_path_style; then
    path_style=1
  fi

  write_s3lvol_cfg_file \
    "${conf}" \
    "${CUBE_S3_ACCESS_KEY_ID:-}" \
    "${CUBE_S3_SECRET_ACCESS_KEY:-}" \
    "${CUBE_S3LVOL_BUCKET:-cube-s3lvol}" \
    "${host}" \
    "${CUBE_S3_REGION:-us-east-1}" \
    "${path_style}" \
    "${no_tls}"
  log "wrote ${conf} endpoint=${host} bucket=${CUBE_S3LVOL_BUCKET:-cube-s3lvol}"
}

# install_s3_volume_host_deps: install s3fs for the S3 volume plugin.
#
# The plugin binary has a built-in S3 client, so create/destroy need no host
# tool. Only the mount path needs s3fs, and a control node runs Cubelet too in
# single-node deployments, so both roles get it.
install_s3_volume_host_deps() {
  local install_prefix="$1"
  local deploy_role="$2"
  local script

  if [[ "${deploy_role}" == "compute" ]]; then
    script="${install_prefix}/Cubelet/plugin/install-s3-deps.sh"
  else
    script="${install_prefix}/CubeMaster/plugin/install-s3-deps.sh"
    [[ -f "${script}" ]] || script="${install_prefix}/Cubelet/plugin/install-s3-deps.sh"
  fi
  [[ -f "${script}" ]] || {
    log "WARNING: S3 volume install-s3-deps.sh not found; skip host tool install"
    return 0
  }
  chmod +x "${script}"
  if ! "${script}" --s3fs --jq; then
    log "WARNING: S3 volume host deps install failed; s3fs may be missing (volume attach will fail until it is installed)"
  fi
}

# prepare_volume_plugin_install: restore/seed config and ensure plugin binaries
# are executable under CubeMaster/Cubelet plugin directories.
prepare_volume_plugin_install() {
  local install_prefix="$1"
  local install_mode="$2"
  local backup_dir="$3"
  local deploy_role="$4"
  local plugin_bin

  restore_volume_plugin_config_from_upgrade_backup \
    "${install_prefix}" "${install_mode}" "${backup_dir}"
  seed_volume_plugin_config "${install_prefix}" "${deploy_role}"
  write_volume_s3_conf "${install_prefix}" "${deploy_role}"

  for plugin_bin in \
    "${install_prefix}/CubeMaster/plugin/cube-volume-cos" \
    "${install_prefix}/Cubelet/plugin/cube-volume-cos" \
    "${install_prefix}/CubeMaster/plugin/cube-volume-s3" \
    "${install_prefix}/Cubelet/plugin/cube-volume-s3" \
    "${install_prefix}/CubeMaster/plugin/install-deps.sh" \
    "${install_prefix}/Cubelet/plugin/install-deps.sh" \
    "${install_prefix}/CubeMaster/plugin/install-s3-deps.sh" \
    "${install_prefix}/Cubelet/plugin/install-s3-deps.sh"
  do
    [[ -f "${plugin_bin}" ]] || continue
    chmod +x "${plugin_bin}"
  done

  install_s3_volume_host_deps "${install_prefix}" "${deploy_role}"
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
# CIDR / network helper functions for CubeSandbox local network validation.
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

is_cube_tap_netdev() {
  local iface="$1"
  iface="${iface%%@*}"
  # Current: dotted IPv4. Legacy: z + IPv4 (names that still fit IFNAMSIZ).
  [[ "${iface}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    || [[ "${iface}" =~ ^z[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

is_cube_managed_netdev() {
  local iface="$1"
  iface="${iface%%@*}"
  [[ "${iface}" == "cube-dev" || "${iface}" == "cube-router" ]] || is_cube_tap_netdev "${iface}"
}

resolv_conf_candidates() {
  printf '%s\n' \
    "/run/systemd/resolve/resolv.conf" \
    "/run/systemd/resolve/stub-resolv.conf" \
    "/run/NetworkManager/no-stub-resolv.conf" \
    "/var/run/NetworkManager/no-stub-resolv.conf" \
    "/run/resolvconf/resolv.conf" \
    "/etc/resolvconf/run/resolv.conf" \
    "/etc/resolv.conf"
}

canonicalize_resolv_conf_path() {
  local path="$1"
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "${path}" 2>/dev/null || printf '%s\n' "${path}"
    return 0
  fi
  printf '%s\n' "${path}"
}

# _check_cidr_conflict: Detect overlap between the specified CIDR and
# existing host network interfaces, routes and DNS nameservers. Exits with die()
# on conflict.
_check_cidr_conflict() {
  local cidr="$1"
  local cidr_label="${2:-CUBE_SANDBOX_NETWORK_CIDR}"
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
  # cubesandbox's own gateway interface network (e.g., "192.168.0.1/18"),
  # recorded when the residual cube-dev interface is found. Empty otherwise.
  local cubedev_cidr=""

  # --- Check interface addresses ---
  # Format: "IP/MASK IFACE" (e.g., "10.0.0.5/24 eth0")
  local line
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    local iface_cidr="${line%% *}"
    local iface_name="${line#* }"

    # cubesandbox's own dummy gateway (constant name "cube-dev"): record its
    # network for reuse/change detection below and skip -- it is cube's own
    # residue, not a foreign host conflict.
    if [[ "${iface_name}" == "cube-dev" ]]; then
      cubedev_cidr="${iface_cidr}"
      continue
    fi
    # Other cube-managed devices, including the optional cube-router and
    # persistent TAP devices named "<ipv4>" (legacy "z<ipv4>"), are also
    # deployment residue.
    if is_cube_managed_netdev "${iface_name}"; then
      continue
    fi

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
  # Parse line-by-line so we can read each route's output device and skip
  # routes owned by cube-dev (cube's own residue). grep -oP then extracts ANY
  # CIDR token from the surviving line (handles policy routes like
  # "from 10.0.0.0/8 table 100" where the CIDR is not the first field).
  local route_text
  route_text="$(ip -4 route show 2>/dev/null || true)"
  if [[ -n "${route_text}" ]]; then
    local route_line
    while IFS= read -r route_line; do
      [[ -n "${route_line}" ]] || continue

      # Skip routes attached to cubesandbox-managed interfaces.
      if [[ "${route_line}" =~ dev[[:space:]]+([^[:space:]]+) ]]; then
        if is_cube_managed_netdev "${BASH_REMATCH[1]}"; then
          continue
        fi
      fi

      local route_cidr
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
      done < <(echo "${route_line}" | grep -oP '\b[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+\b' || true)
    done < <(echo "${route_text}")
  fi

  # --- Check resolver nameservers for overlap ---
  # A host DNS upstream inside the sandbox CIDR can later route DNS/image-pull
  # traffic into Cube-owned addresses even when routes do not make it obvious.
  local resolv_path
  local seen_resolv_paths=()
  while IFS= read -r resolv_path; do
    [[ -n "${resolv_path}" && -f "${resolv_path}" ]] || continue

    local canonical_resolv_path
    canonical_resolv_path="$(canonicalize_resolv_conf_path "${resolv_path}")"

    local already_seen=0
    local seen_path
    for seen_path in "${seen_resolv_paths[@]}"; do
      if [[ "${seen_path}" == "${canonical_resolv_path}" ]]; then
        already_seen=1
        break
      fi
    done
    [[ "${already_seen}" -eq 0 ]] || continue
    seen_resolv_paths+=("${canonical_resolv_path}")

    local nameserver
    while IFS= read -r nameserver; do
      [[ "${nameserver}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue

      local ns_int
      ns_int=$(ip_to_int "${nameserver}")
      if (( ns_int >= cidr_net_start && ns_int <= cidr_net_end )); then
        conflicts+=("nameserver ${nameserver} (${resolv_path})")
      fi
    done < <(awk '$1 == "nameserver" {print $2}' "${resolv_path}")
  done < <(resolv_conf_candidates)

  # A genuine conflict with a foreign host interface/route/resolver -> hard fail.
  if [[ "${#conflicts[@]}" -gt 0 ]]; then
    local conflict_list
    conflict_list="$(printf '\n  - %s' "${conflicts[@]}")"
    die "${cidr_label} '${cidr}' conflicts with existing host network:${conflict_list}

  The cubevs CIDR must not overlap with any existing interface IPs, routes, or DNS nameservers.
  Choose a private IP range that does not conflict, such as:
    10.0.0.0/8      (any subnet within)
    172.16.0.0/12   (any subnet within)
    192.168.0.0/16  (any non-conflicting subnet)

  To bypass this check (not recommended), set:
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1"
  fi

  # No foreign conflict. If a residual cube-dev exists (leftover from a
  # previous cubesandbox deployment), decide between reuse and CIDR change.
  if [[ -n "${cubedev_cidr}" ]]; then
    local cd_ip="${cubedev_cidr%/*}"
    local cd_mask="${cubedev_cidr#*/}"
    if [[ "${cd_ip}" == "${cubedev_cidr}" ]]; then
      cd_mask="32"
    fi

    local cd_int
    cd_int=$(ip_to_int "${cd_ip}")
    local cd_host_bits=$(( 32 - 10#${cd_mask} ))
    local cd_mask_int=$(( (0xFFFFFFFF << cd_host_bits) & 0xFFFFFFFF ))
    local cd_net_start=$(( cd_int & cd_mask_int ))
    local cd_net_end=$(( cd_net_start | (0xFFFFFFFF & ~cd_mask_int) ))
    local cd_network
    cd_network="$(ip_int_to_dot "${cd_net_start}")"

    if (( cd_net_start == cidr_net_start )) && (( 10#${cd_mask} == 10#${mask} )); then
      # Same network -> reinstall reuse. The residual cube-dev IS this CIDR's
      # gateway; not a conflict.
      log "reusing existing cube-dev network (${cd_network}/${cd_mask}); CIDR self-conflict skipped"
    elif (( cidr_net_start <= cd_net_end && cidr_net_end >= cd_net_start )); then
      # Different network that overlaps the requested CIDR -> disruptive change
      # on a host that already has a cube network. A reboot alone is NOT enough
      # because the systemd target is enabled and cubelet's embedded network
      # runtime rebuilds the old network from config.toml; a deterministic reset
      # is required.
      die "${cidr_label} '${cidr}' overlaps an existing cube-dev network (${cd_network}/${cd_mask}).

  Changing the sandbox CIDR on a host that already has a cube network is
  disruptive: the old cube-dev and the persistent TAP devices are left
  stale. A reboot alone is NOT enough -- the systemd target is enabled and
  cubelet's embedded network runtime rebuilds the old network from config.toml on boot.

  To change the CIDR, fully reset the cube network first:
    sudo systemctl stop 'cube-sandbox-*.target'
    sudo ip link delete cube-dev 2>/dev/null || true
    sudo ip link delete cube-router 2>/dev/null || true
    ip tuntap show | awk -F: '
      \$1 ~ /^(z)?[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+\$/ { print \$1 }
    ' \\
      | xargs -r -n1 -I{} sudo ip tuntap del dev {} mode tap
  then re-run install with the new CIDR.

  Or keep the existing CIDR (${cd_network}/${cd_mask}) to reuse the current network.

  To bypass this check (not recommended), set:
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1"
    fi
    # else: cube-dev exists but does not overlap the requested CIDR -> allow;
    # Cubelet's embedded network runtime will reconcile cube-dev to the new network.
  fi
}

# check_cidr_preflight: Validate CIDR format and detect host network conflicts.
# Called during install preflight before dependency installation or deployment
# replacement. The caller passes either CUBE_SANDBOX_NETWORK_CIDR or the fixed
# packaged default.
#
# SECURITY: Format validation MUST run before the SKIP_CONFLICT_CHECK bypass
# to prevent sed command injection (sed 'w' flag) and env file shell injection.
check_cidr_preflight() {
  local cidr="${1:-}"
  # Optional second arg forces skipping host-conflict detection (format
  # validation is always enforced). Defaults to the env bypass flag. The
  # upgrade flow passes 1 here: the preserved CIDR is already in use by this
  # cluster's own cubevs bridge/route, which would otherwise be misdetected as
  # a conflict and block the upgrade.
  local skip_conflict="${2:-${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-0}}"
  local cidr_label="${3:-CUBE_SANDBOX_NETWORK_CIDR}"
  local max_mask="${4:-24}"
  local min_mask="${5:-16}"

  # Empty CIDR means there is nothing to validate.
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
    die "${cidr_label} '${cidr}' is not a valid IPv4 CIDR format (e.g., 10.0.0.0/16)"
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
      die "${cidr_label} '${cidr}' has an invalid IP octet: '${octet}' (max 3 digits)"
    fi
    if (( 10#${octet} < 0 || 10#${octet} > 255 )); then
      die "${cidr_label} '${cidr}' has an invalid IP octet: ${octet}"
    fi
  done

  # 3. Valid mask range [min_mask, max_mask] (use 10# prefix to prevent octal interpretation)
  if ! [[ "${min_mask}" =~ ^[0-9]+$ ]] || ! [[ "${max_mask}" =~ ^[0-9]+$ ]] \
    || (( 10#${min_mask} < 0 || 10#${max_mask} > 32 || 10#${min_mask} > 10#${max_mask} )); then
    die "${cidr_label} mask range must be valid (got: ${min_mask}-${max_mask})"
  fi
  if ! [[ "${mask}" =~ ^[0-9]+$ ]] || (( 10#${mask} < 10#${min_mask} || 10#${mask} > 10#${max_mask} )); then
    die "${cidr_label} mask must be between ${min_mask} and ${max_mask} (got: ${mask})"
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
    die "${cidr_label} '${cidr}' is not aligned to its network address. Did you mean: ${suggested}/${mask}?"
  fi

  # If the caller does not pass skip_conflict, the env bypass flag controls
  # whether only host-network conflict detection is skipped.

  # ======================================================================
  # CONFLICT DETECTION -- bypassable with SKIP_CONFLICT_CHECK
  #
  # At this point the CIDR is known-valid. Only the host-network overlap
  # check is conditionally skipped.
  # ======================================================================

  # 5. Check bypass flag -- only skips conflict detection, not format validation
  if [[ "${skip_conflict}" == "1" ]]; then
    log "${cidr_label} conflict check SKIPPED -- CIDR: ${cidr}"
    return 0
  fi

  # 6. CIDR conflict detection with host interfaces, routes and resolvers
  _check_cidr_conflict "${cidr}" "${cidr_label}"

  log "${cidr_label} preflight OK: ${cidr}"
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

# check_compute_control_plane_preflight: fail fast when a compute node is
# missing the mandatory control plane address (CubeMaster + CubeOps).
# Mirrors resolve_control_plane_cubemaster_addr / resolve_control_plane_cubeops_addr.
check_compute_control_plane_preflight() {
  local role
  role="$(one_click_deploy_role)"

  [[ "${role}" == "compute" ]] || return 0

  local addr="${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR:-}"
  local ip="${ONE_CLICK_CONTROL_PLANE_IP:-}"
  local cubeops_addr="${ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR:-}"
  local cubemaster_port=8089
  local cubeops_port=3010

  # Guard: when both variables are set they MUST resolve to the same address.
  if [[ -n "${addr}" && -n "${ip}" ]]; then
    local ip_resolved="${ip}:${cubemaster_port}"
    if [[ "${addr}" != "${ip_resolved}" ]]; then
      die "ONE_CLICK_CONTROL_PLANE_IP (resolves to ${ip_resolved}) and ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR (${addr}) conflict. Use only one of them; if you need a custom port, use ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR=<host>:<port>."
    fi
  fi

  if [[ -n "${addr}" ]]; then
    validate_host_port "${addr}" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR"
    log "control plane cubemaster address preflight OK: ${addr}"
  elif [[ -n "${ip}" ]]; then
    validate_ipv4_literal "${ip}" "ONE_CLICK_CONTROL_PLANE_IP"
    validate_host_port "${ip}:${cubemaster_port}" "ONE_CLICK_CONTROL_PLANE_IP-derived cubemaster address"
    log "control plane IP preflight OK: ${ip} (cubemaster port ${cubemaster_port})"
  else
    cat >&2 <<'EOF'

╔══════════════════════════════════════════════════════════════════╗
║  [!!] CONTROL PLANE ADDRESS NOT CONFIGURED                     ║
╠══════════════════════════════════════════════════════════════════╣
║                                                                  ║
║  This is a COMPUTE node (ONE_CLICK_DEPLOY_ROLE=compute).         ║
║  The control plane address is REQUIRED but not configured.       ║
║                                                                  ║
║  Set ONE of these variables in your .env file:                   ║
║                                                                  ║
║    Option A — control plane IP (recommended):                    ║
║      ONE_CLICK_CONTROL_PLANE_IP=<control-plane-ip>               ║
║                                                                  ║
║    Option B — full CubeMaster host:port:                         ║
║      ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR=<host>:<port>       ║
║                                                                  ║
║  Or pass as environment variables:                               ║
║    ONE_CLICK_CONTROL_PLANE_IP=10.0.0.11 ./install-compute.sh     ║
║                                                                  ║
╚══════════════════════════════════════════════════════════════════╝

EOF
    die "ONE_CLICK_CONTROL_PLANE_IP or ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR is required for compute role"
  fi

  # CubeOps address: prefer explicit, otherwise derive from the control
  # plane IP or the CubeMaster addr host (port 3010).
  if [[ -n "${cubeops_addr}" ]]; then
    validate_host_port "${cubeops_addr}" "ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR"
    log "control plane cubeops address preflight OK: ${cubeops_addr}"
  elif [[ -n "${ip}" ]]; then
    validate_host_port "${ip}:${cubeops_port}" "ONE_CLICK_CONTROL_PLANE_IP-derived cubeops address"
    log "control plane cubeops address preflight OK: ${ip}:${cubeops_port} (derived)"
  elif [[ -n "${addr}" ]]; then
    local cubeops_host="${addr%%:*}"
    validate_host_port "${cubeops_host}:${cubeops_port}" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR-derived cubeops address"
    log "control plane cubeops address preflight OK: ${cubeops_host}:${cubeops_port} (derived from cubemaster addr)"
  else
    die "ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR or ONE_CLICK_CONTROL_PLANE_IP is required for CubeOps registration"
  fi
}
