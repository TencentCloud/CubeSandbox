# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Shared helper: keep cubebox_os_image artifacts on the data disk while
# preserving the historical toolbox path via a symlink.
# Callers must define fail()/die(); log() is optional.
#
# Toggle: CUBE_CUBEBOX_OS_IMAGE_ON_DATA=1 (default) enables the softlink.
# Set to 0 to keep a real directory under the toolbox (may fill the root disk).
#
# Final layout when enabled: <toolbox>/cubebox_os_image -> /data/cubebox_os_image
# Matches one-click: large template rootfs on the data disk; cubelet keeps
# using the historical toolbox path.

if [[ "${CUBE_CUBEBOX_OS_IMAGE_LIB_LOADED:-0}" == "1" ]]; then
  return 0 2>/dev/null || exit 0
fi
CUBE_CUBEBOX_OS_IMAGE_LIB_LOADED=1

if ! type fail >/dev/null 2>&1; then
  if type die >/dev/null 2>&1; then
    fail() { die "$@"; }
  else
    fail() {
      echo "[cubebox_os_image] ERROR: $*" >&2
      exit 1
    }
  fi
fi

CUBEBOX_OS_IMAGE_DATA_DIR_DEFAULT="/data/cubebox_os_image"

cubebox_os_image_on_data_enabled() {
  case "${CUBE_CUBEBOX_OS_IMAGE_ON_DATA:-1}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

# Resolve to a canonical absolute path (no .. components).
_cubebox_os_image_resolve() {
  local path="$1"
  if command -v realpath >/dev/null 2>&1; then
    realpath -m "${path}" 2>/dev/null || realpath "${path}"
  else
    readlink -f "${path}"
  fi
}

# True when path is its own mountpoint (bind mounts included).
_cubebox_os_image_is_mountpoint() {
  local path="$1"
  local tgt d1 d2
  [[ -e "${path}" ]] || return 1
  if command -v findmnt >/dev/null 2>&1; then
    tgt="$(findmnt -n -o TARGET -T "${path}" 2>/dev/null || true)"
    if [[ "${tgt}" == "${path}" ]]; then
      return 0
    fi
  fi
  d1="$(stat -c '%d' "${path}" 2>/dev/null)" || return 1
  d2="$(stat -c '%d' "$(dirname "${path}")" 2>/dev/null)" || return 1
  [[ "${d1}" != "${d2}" ]]
}

# True when resolved target is a strict child of dirname(data_path).
# Never migrates the data parent itself or paths that escape via "..".
_cubebox_os_image_trusted_dir() {
  local target="$1"
  local data_path="$2"
  local data_parent resolved_target resolved_parent
  [[ "${target}" == /* ]] || return 1
  [[ -d "${target}" ]] || return 1
  data_parent="$(dirname "${data_path}")"
  resolved_target="$(_cubebox_os_image_resolve "${target}")" || return 1
  resolved_parent="$(_cubebox_os_image_resolve "${data_parent}")" || return 1
  [[ -n "${resolved_target}" && -n "${resolved_parent}" ]] || return 1
  case "${resolved_target}" in
    "${resolved_parent}"/*)
      [[ "${resolved_target}" != "${resolved_parent}" ]] || return 1
      return 0
      ;;
  esac
  return 1
}

_migrate_cubebox_os_image_dir() {
  local src="$1"
  local data_path="$2"
  if _cubebox_os_image_is_mountpoint "${src}"; then
    if declare -F log >/dev/null 2>&1; then
      log "ERROR: refusing to migrate cubebox_os_image: ${src} is a mountpoint"
    fi
    return 1
  fi
  if [[ -e "${data_path}" && ! -d "${data_path}" ]]; then
    if declare -F log >/dev/null 2>&1; then
      log "ERROR: refusing to migrate cubebox_os_image: ${data_path} exists and is not a directory"
    fi
    return 1
  fi
  if [[ ! -d "${data_path}" ]]; then
    mkdir -p "$(dirname "${data_path}")"
    mv "${src}" "${data_path}" || return 1
  else
    if [[ -n "$(find "${src}" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null || true)" ]]; then
      # Propagate cp status: callers must not rm -rf the source on failure.
      cp -a "${src}/." "${data_path}/" || return 1
    fi
  fi
  return 0
}

# ensure_cubebox_os_image_on_data [toolbox_root] [data_dir]

ensure_cubebox_os_image_on_data() {
  local toolbox_root="${1:-${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}}"
  local data_path="${2:-${CUBEBOX_OS_IMAGE_DATA_DIR:-${CUBEBOX_OS_IMAGE_DATA_DIR_DEFAULT}}}"
  local link_path="${toolbox_root%/}/cubebox_os_image"

  if ! cubebox_os_image_on_data_enabled; then
    if declare -F log >/dev/null 2>&1; then
      log "cubebox_os_image data-disk softlink disabled (CUBE_CUBEBOX_OS_IMAGE_ON_DATA=${CUBE_CUBEBOX_OS_IMAGE_ON_DATA:-0})"
    fi
    return 0
  fi

  # Recoverable failures return 1 so chart callers can WARN+continue; one-click
  # callers under set -e still abort.
  if [[ -z "${toolbox_root}" ]]; then
    if declare -F log >/dev/null 2>&1; then
      log "ERROR: toolbox root is empty"
    fi
    return 1
  fi
  if [[ "${data_path}" != /* ]]; then
    if declare -F log >/dev/null 2>&1; then
      log "ERROR: cubebox_os_image data dir must be absolute: ${data_path}"
    fi
    return 1
  fi
  if [[ "${link_path}" != /* ]]; then
    if declare -F log >/dev/null 2>&1; then
      log "ERROR: cubebox_os_image link path must be absolute: ${link_path}"
    fi
    return 1
  fi

  mkdir -p "$(dirname "${data_path}")"
  mkdir -p "${toolbox_root}"

  if [[ -L "${link_path}" ]]; then
    local current
    current="$(readlink "${link_path}")"
    if [[ "${current}" == "${data_path}" ]]; then
      mkdir -p "${data_path}"
      return 0
    fi
    rm -f "${link_path}"
    if _cubebox_os_image_trusted_dir "${current}" "${data_path}"; then
      if declare -F log >/dev/null 2>&1; then
        log "cubebox_os_image: migrating trusted symlink target ${current} -> ${data_path}"
      fi
      if [[ "${current}" != "${data_path}" ]]; then
        if ! _migrate_cubebox_os_image_dir "${current}" "${data_path}"; then
          if declare -F log >/dev/null 2>&1; then
            log "ERROR: failed to migrate cubebox_os_image symlink target ${current}; leaving link dropped"
          fi
          return 1
        fi
        if [[ -d "${current}" && "${current}" != "${data_path}" ]]; then
          if declare -F log >/dev/null 2>&1; then
            log "cubebox_os_image: left previous data dir in place: ${current}"
          fi
        fi
      fi
    else
      if declare -F log >/dev/null 2>&1; then
        log "WARN: dropping cubebox_os_image symlink to untrusted/missing target '${current}'; re-pointing to ${data_path} without migrating"
      fi
    fi
  elif [[ -d "${link_path}" ]]; then
    if ! _migrate_cubebox_os_image_dir "${link_path}" "${data_path}"; then
      if declare -F log >/dev/null 2>&1; then
        log "ERROR: failed to migrate cubebox_os_image to ${data_path}; leaving source in place"
      fi
      return 1
    fi
    if [[ -d "${link_path}" ]]; then
      if _cubebox_os_image_is_mountpoint "${link_path}"; then
        if declare -F log >/dev/null 2>&1; then
          log "ERROR: refusing to remove cubebox_os_image mountpoint after migrate: ${link_path}"
        fi
        return 1
      fi
      rm -rf "${link_path}"
    fi
  elif [[ -e "${link_path}" ]]; then
    if declare -F log >/dev/null 2>&1; then
      log "ERROR: refusing to replace non-directory cubebox_os_image path: ${link_path}"
    fi
    return 1
  fi

  mkdir -p "${data_path}"
  ln -sfn "${data_path}" "${link_path}"

  if declare -F log >/dev/null 2>&1; then
    log "cubebox_os_image ready: ${link_path} -> ${data_path}"
  fi
}
