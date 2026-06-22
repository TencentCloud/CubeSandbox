#!/usr/bin/env bash
#
# This file is a sourced library. Do not set shell options here: entrypoint
# scripts/tests that source it are responsible for their own strict mode
# (`set -euo pipefail`) policy.

ONE_CLICK_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${ONE_CLICK_LIB_DIR}/.." && pwd)"

log() {
  echo "[one-click] $*" >&2
}

die() {
  echo "[one-click] ERROR: $*" >&2
  exit 1
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

# Escape a string so it can be used safely as the replacement text in a sed
# `s|...|...|` expression. Escapes the delimiter '|', backslashes, '&' (the
# whole-match reference) and '"'. Mirrors the escape_sed helper in
# up-support.sh, which escapes for the '/' delimiter instead.
#
# The '"' is escaped because the only caller (patch_cubemaster_external_deps in
# install.sh) embeds the result inside double-quoted sed replacement strings
# such as `pwd: "${value}"`; escaping it keeps a value that itself contains a
# '"' from corrupting the rendered YAML.
#
# SECURITY: embedded newlines / carriage returns are stripped first as
# defense-in-depth. An unescaped newline in the replacement text would
# terminate the sed `s` command and let a crafted value (e.g. a password read
# from .env) inject arbitrary sed commands into the rendered config.
escape_sed() {
  printf '%s' "$1" | tr -d '\n\r' | sed 's/[|\\&"]/\\&/g'
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

upsert_env_kv() {
  local env_file="$1"
  local key="$2"
  local value="$3"
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

validate_interface_name() {
  local value="$1"
  local name="${2:-interface name}"
  [[ -n "${value}" ]] || die "${name} must not be empty"
  # Linux IFNAMSIZ is 16 including NUL, so names are at most 15 bytes. Restrict
  # to characters that are safe in the TOML replacement and shell logs.
  [[ "${value}" =~ ^[A-Za-z0-9_.:-]{1,15}$ ]] \
    || die "invalid ${name}: ${value} (expected 1-15 chars: letters, digits, '_', '.', ':', '-')"
}

patch_cubelet_config_template() {
  local cubelet_config="$1"
  local eth_name="${2:-}"
  local network_cidr="${3:-}"

  ensure_file "${cubelet_config}"
  if [[ -L "${cubelet_config}" ]]; then
    die "refusing to patch a symlink target: ${cubelet_config} -> $(readlink "${cubelet_config}")"
  fi

  if [[ -n "${eth_name}" ]]; then
    validate_interface_name "${eth_name}" "CUBE_SANDBOX_ETH_NAME"
    if grep -Eq '^[[:space:]]*eth_name = "' "${cubelet_config}"; then
      sed -i "s/eth_name = \"[^\"]*\"/eth_name = \"${eth_name}\"/" "${cubelet_config}"
      if ! grep -Fq "eth_name = \"${eth_name}\"" "${cubelet_config}"; then
        log "WARNING: failed to patch eth_name in Cubelet config (${cubelet_config})"
      fi
    else
      log "WARNING: Cubelet config missing eth_name key; skipped NIC patch (${cubelet_config})"
    fi
  fi

  if [[ -n "${network_cidr}" ]]; then
    if grep -Eq '^[[:space:]]*cidr = "' "${cubelet_config}"; then
      sed -i "s|cidr = \"[^\"]*\"|cidr = \"${network_cidr}\"|" "${cubelet_config}"
      if ! grep -Fq "cidr = \"${network_cidr}\"" "${cubelet_config}"; then
        log "WARNING: failed to patch cidr in Cubelet config (${cubelet_config})"
      fi
      log "patched cubevs CIDR: ${network_cidr}"
    else
      log "WARNING: Cubelet config missing cidr key; skipped CIDR patch (${cubelet_config})"
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
# obviously unsafe install prefix. Guards against a mis-set
# ONE_CLICK_INSTALL_PREFIX (e.g. "/" or "/usr", or a foreign dir like
# "/usr/local" / "/var/lib") turning the custom-prefix wipe into a
# system-destroying `rm -rf`. Beyond the root/system/top-level denylist, a
# non-empty existing prefix is only wiped when it is a recognised CubeSandbox
# install (presence of a marker artifact such as .one-click.env / CubeMaster)
# or effectively empty. A lone '.backup' left over from an interrupted upgrade
# is fine only when it is a real directory, not a symlink. Non-existent prefixes
# are allowed (a fresh path the installer is about to create).
assert_safe_install_prefix() {
  local prefix="$1"

  [[ -n "${prefix}" ]] || die "refusing to wipe an empty install prefix"
  [[ "${prefix}" == /* ]] || die "refusing to wipe a non-absolute install prefix: ${prefix}"
  [[ ! -L "${prefix}" ]] || die "refusing to wipe a symlink install prefix: ${prefix}"

  # Normalize: drop a single trailing slash (but keep "/" detectable).
  local norm="${prefix%/}"
  [[ -n "${norm}" ]] || die "refusing to wipe the filesystem root: ${prefix}"
  [[ ! -L "${norm}" ]] || die "refusing to wipe a symlink install prefix: ${prefix}"

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
    die "refusing to wipe a top-level directory: ${prefix} (install prefix must be at least two levels deep)"
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
    die "refusing to wipe custom install prefix ${display}: contains top-level symlink (${symlink}); move it away and retry"
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
      die "refusing to wipe custom install prefix ${display}: directory is not empty and contains no CubeSandbox installation markers (.one-click.env / CubeMaster / CubeAPI / Cubelet). Point ONE_CLICK_INSTALL_PREFIX at a dedicated CubeSandbox prefix, or remove the foreign content first."
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
    || die "failed to stat install prefix before wipe: ${prefix}"

  (
    cd -- "${norm}" || die "failed to enter install prefix: ${prefix}"
    after="$(stat -c '%d:%i' -- .)" \
      || die "failed to stat install prefix after cd: ${prefix}"
    [[ "${before}" == "${after}" ]] \
      || die "install prefix changed while preparing to wipe: ${prefix}"

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
}

new_defaults = parse(new_example)
old_values = parse(old_runtime)
old_baseline_vals = parse(old_baseline) if old_baseline else {}
new_overrides = parse(new_dotenv) if new_dotenv else {}
has_baseline = bool(old_baseline_vals)

added = []
updated_default = []
preserved = []
explicit = []
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
    if key in new_overrides and new_overrides[key] != new_defaults.get(key):
        chosen = new_overrides[key]
        explicit.append(key)
    elif key in old_values:
        ov = old_values[key]
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
    "[one-click] env merge: +%d new, ~%d default-updated, =%d preserved, >%d kept-extra, -%d dropped%s\n" % (
        len(added), len(updated_default), len(preserved), len(extra), len(dropped),
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
# existing install is detected, prompts on a TTY (default: upgrade) and falls
# back to a full reinstall (with a loud warning) when non-interactive.
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

  # Unset mode: default to install, but protect an existing install.
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

  log "WARNING: existing installation detected but running non-interactively without --mode."
  log "WARNING: defaulting to a full REINSTALL; your .one-click.env customizations WILL be reset."
  log "WARNING: to preserve configuration, re-run with --mode=upgrade (or --yes)."
  printf 'install\n'
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
    "Cubelet/config/config.toml" \
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
  # Optional second arg forces skipping host-conflict detection (format
  # validation is always enforced). Defaults to the env bypass flag. The
  # upgrade flow passes 1 here: the preserved CIDR is already in use by this
  # cluster's own cubevs bridge/route, which would otherwise be misdetected as
  # a conflict and block the upgrade.
  local skip_conflict="${2:-${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-0}}"

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
  if [[ "${skip_conflict}" == "1" ]]; then
    log "CUBE_SANDBOX_NETWORK_CIDR conflict check SKIPPED -- CIDR: ${cidr}"
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
