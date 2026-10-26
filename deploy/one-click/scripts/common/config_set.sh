# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Collect CUBE_SET_<n>_{FILE,PATH,VALUE,TYPE} groups and write them into
# allowlisted component configs. Sourced by install.sh after lib/common.sh
# (needs die / log / upsert_env_kv / require_cmd / remove_env_kv).

if [[ "${ONE_CLICK_CONFIG_SET_LIB_LOADED:-0}" == "1" ]]; then
  return 0
fi
ONE_CLICK_CONFIG_SET_LIB_LOADED=1

ONE_CLICK_CONFIG_SET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_CONFIG_SET_PY="${ONE_CLICK_CONFIG_SET_DIR}/config_set.py"
CUBE_SET_THIS_RUN=0
# Same snapshot/replay shape as ONE_CLICK_TOGGLE_* in lib/common.sh.
# declare and empty-assignment stay separate for bash 4.2 (CentOS 7).
declare -gA CUBE_SET_ENV_SNAPSHOT
declare -gA CUBE_SET_DOTENV_SNAPSHOT
CUBE_SET_ENV_SNAPSHOT=()
CUBE_SET_DOTENV_SNAPSHOT=()

if ! type die >/dev/null 2>&1; then
  die() {
    echo "[config-set] ERROR: $*" >&2
    exit 1
  }
fi

if ! type log >/dev/null 2>&1; then
  log() {
    echo "[config-set] $*" >&2
  }
fi

# cube_set_ids: print numerically sorted unique CUBE_SET_<n> indexes present
# in the process environment. n is a positive integer without leading zeros.
cube_set_ids() {
  local var id
  local seen=" "
  for var in $(compgen -e | LC_ALL=C sort); do
    if [[ "${var}" =~ ^CUBE_SET_([1-9][0-9]*)_(FILE|PATH|VALUE|TYPE)$ ]]; then
      id="${BASH_REMATCH[1]}"
      if [[ "${seen}" != *" ${id} "* ]]; then
        printf '%s\n' "${id}"
        seen="${seen}${id} "
      fi
    fi
  done | LC_ALL=C sort -n
}

cube_set_file_allowed_for_role() {
  local file="$1"
  local role="${DEPLOY_ROLE:-control}"
  case "${file}" in
    Cubelet/config/config.toml|Cubelet/dynamicconf/conf.yaml)
      return 0
      ;;
    CubeMaster/conf.yaml|CubeTemplateCenter/conf.yaml)
      if [[ "${role}" == "compute" ]]; then
        return 1
      fi
      return 0
      ;;
    *)
      return 0
      ;;
  esac
}

# snapshot_cube_set_process_env: capture CUBE_SET_* already in the process
# environment. Must run BEFORE any env file is sourced.
snapshot_cube_set_process_env() {
  local var
  CUBE_SET_ENV_SNAPSHOT=()
  for var in $(compgen -e); do
    if [[ "${var}" =~ ^CUBE_SET_([1-9][0-9]*)_(FILE|PATH|VALUE|TYPE)$ ]]; then
      CUBE_SET_ENV_SNAPSHOT["${var}"]="${!var}"
      CUBE_SET_THIS_RUN=1
    fi
  done
  return 0
}

# True when ENV_FILE has an active assignment for VAR (plain, indented, or export).
cube_set_dotenv_has_assignment() {
  local env_file="$1"
  local var="$2"
  grep -Eq "^[[:space:]]*(export[[:space:]]+)?${var}=" "${env_file}"
}

# capture_cube_set_dotenv_values ENV_FILE
# After load_env_file, keep the sourced (quote-stripped) values for keys that
# were present as active assignments in this-run .env.
capture_cube_set_dotenv_values() {
  local env_file="${1:-}"
  local var
  CUBE_SET_DOTENV_SNAPSHOT=()
  [[ -n "${env_file}" && -f "${env_file}" ]] || return 0
  for var in $(compgen -e); do
    if [[ "${var}" =~ ^CUBE_SET_([1-9][0-9]*)_(FILE|PATH|VALUE|TYPE)$ ]]; then
      if cube_set_dotenv_has_assignment "${env_file}" "${var}"; then
        CUBE_SET_DOTENV_SNAPSHOT["${var}"]="${!var}"
        CUBE_SET_THIS_RUN=1
      fi
    fi
  done
  return 0
}

# True when this-run .env listed any CUBE_SET_* (replace the persisted set).
# Process-env-only CUBE_SET_* overlays those keys and must not wipe others.
cube_set_dotenv_declared() {
  (( ${#CUBE_SET_DOTENV_SNAPSHOT[@]} > 0 ))
}

cube_set_env_declared() {
  (( ${#CUBE_SET_ENV_SNAPSHOT[@]} > 0 ))
}

# apply_cube_set_intent: after upgrade merge, restore this-run CUBE_SET_*.
# A this-run .env replaces the merged set; process-env only overlays on top.
apply_cube_set_intent() {
  local var
  [[ "${CUBE_SET_THIS_RUN}" == "1" ]] || return 0
  if cube_set_dotenv_declared; then
    for var in $(compgen -e); do
      if [[ "${var}" =~ ^CUBE_SET_[1-9][0-9]*_(FILE|PATH|VALUE|TYPE)$ ]]; then
        unset "${var}"
      fi
    done
    for var in "${!CUBE_SET_DOTENV_SNAPSHOT[@]}"; do
      printf -v "${var}" '%s' "${CUBE_SET_DOTENV_SNAPSHOT[${var}]}"
      export "${var}"
    done
  fi
  # Count-guard: bash 4.2 + set -u treats "${!empty[@]}" as unbound.
  if cube_set_env_declared; then
    for var in "${!CUBE_SET_ENV_SNAPSHOT[@]}"; do
      if cube_set_dotenv_declared && [[ -n "${!var+x}" ]]; then
        continue
      fi
      printf -v "${var}" '%s' "${CUBE_SET_ENV_SNAPSHOT[${var}]}"
      export "${var}"
    done
  fi
  log "CUBE_SET this-run overlay restored (${#CUBE_SET_DOTENV_SNAPSHOT[@]} .env / ${#CUBE_SET_ENV_SNAPSHOT[@]} process)"
  return 0
}

_cube_set_run_groups() {
  local root="$1"
  local extra="${2:-}"
  local id file path value type_name ids action
  local count=0
  [[ -n "${root}" ]] || die "CUBE_SET requires an install root"
  [[ -d "${root}" ]] || die "CUBE_SET root is not a directory: ${root}"
  ids="$(cube_set_ids)"
  [[ -n "${ids}" ]] || return 0
  require_cmd python3
  [[ -f "${ONE_CLICK_CONFIG_SET_PY}" ]] || die "missing ${ONE_CLICK_CONFIG_SET_PY}"

  while IFS= read -r id; do
    [[ -n "${id}" ]] || continue
    file="CUBE_SET_${id}_FILE"
    path="CUBE_SET_${id}_PATH"
    value="CUBE_SET_${id}_VALUE"
    type_name="CUBE_SET_${id}_TYPE"
    if [[ -z "${!file+x}" || -z "${!path+x}" || -z "${!value+x}" ]]; then
      die "CUBE_SET_${id}_FILE, CUBE_SET_${id}_PATH, and CUBE_SET_${id}_VALUE must all be set"
    fi
    if [[ -z "${!file}" || -z "${!path}" ]]; then
      die "CUBE_SET_${id}_FILE and CUBE_SET_${id}_PATH must not be empty"
    fi
    if [[ "${!value}" == *$'\n'* || "${!value}" == *$'\r'* ]]; then
      die "CUBE_SET_${id}_VALUE must not contain a newline"
    fi
    if ! cube_set_file_allowed_for_role "${!file}"; then
      log "WARNING: CUBE_SET_${id} skipped ${!file} (control-plane only; this node is compute)"
      continue
    fi
    action="$(python3 "${ONE_CLICK_CONFIG_SET_PY}" \
      --root "${root}" \
      --file "${!file}" \
      --path "${!path}" \
      --value="${!value}" \
      --type "${!type_name:-auto}" \
      ${extra})" \
      || die "CUBE_SET_${id} failed"
    if [[ "${action}" == "inserted" || "${action}" == "updated" ]]; then
      count=$((count + 1))
    fi
  done <<< "${ids}"

  if [[ "${count}" -gt 0 ]]; then
    if [[ "${extra}" == *"--dry-run"* ]]; then
      log "validated ${count} CUBE_SET override(s) under ${root}"
    else
      log "applied ${count} CUBE_SET override(s) under ${root}"
    fi
  fi
}

# validate_cube_set_overrides ROOT
# Fail-fast before any destructive install step.
validate_cube_set_overrides() {
  _cube_set_run_groups "${1:-}" --dry-run
}

# apply_cube_set_overrides ROOT
apply_cube_set_overrides() {
  _cube_set_run_groups "${1:-}"
}

# persist_cube_set_overrides ENV_FILE
persist_cube_set_overrides() {
  local env_file="${1:-}"
  local id file path value type_name var
  [[ -n "${env_file}" ]] || die "persist_cube_set_overrides requires an env file"
  if cube_set_dotenv_declared; then
    if [[ -f "${env_file}" ]]; then
      while IFS= read -r var; do
        [[ -n "${var}" ]] || continue
        remove_env_kv "${env_file}" "${var}"
      done < <(sed -nE 's/^(CUBE_SET_[1-9][0-9]*_(FILE|PATH|VALUE|TYPE))=.*/\1/p' "${env_file}" | sort -u)
    fi
  fi
  while IFS= read -r id; do
    [[ -n "${id}" ]] || continue
    file="CUBE_SET_${id}_FILE"
    path="CUBE_SET_${id}_PATH"
    value="CUBE_SET_${id}_VALUE"
    type_name="CUBE_SET_${id}_TYPE"
    [[ -n "${!file+x}" && -n "${!path+x}" && -n "${!value+x}" ]] \
      || die "CUBE_SET_${id}_FILE, CUBE_SET_${id}_PATH, and CUBE_SET_${id}_VALUE must all be set"
    if ! cube_set_file_allowed_for_role "${!file}"; then
      continue
    fi
    upsert_env_kv "${env_file}" "CUBE_SET_${id}_FILE" "${!file}"
    upsert_env_kv "${env_file}" "CUBE_SET_${id}_PATH" "${!path}"
    upsert_env_kv "${env_file}" "CUBE_SET_${id}_VALUE" "${!value}"
    if [[ -n "${!type_name:-}" ]]; then
      upsert_env_kv "${env_file}" "CUBE_SET_${id}_TYPE" "${!type_name}"
    else
      remove_env_kv "${env_file}" "CUBE_SET_${id}_TYPE"
    fi
  done < <(cube_set_ids)
}
