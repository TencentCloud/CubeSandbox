# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Shared validation helpers for one-click installer/runtime scripts.
# Sourced by bash callers; die()/log() fallbacks are defined when absent.
# shellcheck shell=bash

if [[ "${ONE_CLICK_VALIDATION_LIB_LOADED:-0}" == "1" ]]; then
  return 0
fi
ONE_CLICK_VALIDATION_LIB_LOADED=1

if ! type die >/dev/null 2>&1; then
  die() {
    echo "[validation] ERROR: $*" >&2
    exit 1
  }
fi

if ! type log >/dev/null 2>&1; then
  log() {
    echo "[validation] $*" >&2
  }
fi

# Single source of truth for the pre-unification CUBE_EXTERNAL_* -> unified
# CUBE_SANDBOX_* mapping. Each entry is "old:new". Consumed by
# apply_deprecated_external_aliases (below) to migrate live env vars, and by the
# runtime-env strip loop in persist_unified_dep_config (lib/common.sh) to drop
# the legacy keys from disk. Keep this list as the only shell definition so both
# paths stay in lockstep when keys change.
ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS=(
  "CUBE_EXTERNAL_MYSQL_HOST:CUBE_SANDBOX_MYSQL_HOST"
  "CUBE_EXTERNAL_MYSQL_PORT:CUBE_SANDBOX_MYSQL_PORT"
  "CUBE_EXTERNAL_MYSQL_USER:CUBE_SANDBOX_MYSQL_USER"
  "CUBE_EXTERNAL_MYSQL_PASSWORD:CUBE_SANDBOX_MYSQL_PASSWORD"
  "CUBE_EXTERNAL_MYSQL_DB:CUBE_SANDBOX_MYSQL_DB"
  "CUBE_EXTERNAL_REDIS_HOST:CUBE_SANDBOX_REDIS_HOST"
  "CUBE_EXTERNAL_REDIS_PORT:CUBE_SANDBOX_REDIS_PORT"
  "CUBE_EXTERNAL_REDIS_PASSWORD:CUBE_SANDBOX_REDIS_PASSWORD"
  "CUBE_EXTERNAL_REDIS_MASTER_NAME:CUBE_SANDBOX_REDIS_MASTER_NAME"
  "CUBE_EXTERNAL_REDIS_SENTINEL_NODES:CUBE_SANDBOX_REDIS_SENTINEL_NODES"
  "CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD:CUBE_SANDBOX_REDIS_SENTINEL_PASSWORD"
)

# Whether a host points at the local node. The bundled MySQL/Redis containers
# publish on the loopback address, so a loopback host is the *default* signal
# that cube should manage the bundled container. It is only a default, though:
# a user may run their own MySQL/Redis on 127.0.0.1 and ask cube not to start
# its container (see _dep_is_managed below).
#
# The whole IPv4 127.0.0.0/8 range is loopback, so any 127.x.y.z literal (with
# in-range octets) matches, not just 127.0.0.1. A zone/scope ID suffix
# (e.g. ::1%lo, 127.0.0.1%eth0) or a compressed IPv6 form other than ::1
# (e.g. 0:0::1) will NOT match and is treated as non-loopback. Deployments
# target IPv4, so that IPv6 gap is an accepted limitation rather than an
# oversight.
_host_is_loopback() {
  local host="${1:-}"
  case "${host}" in
    "" | localhost | ::1 | 0:0:0:0:0:0:0:1)
      return 0
      ;;
  esac
  # Any 127.0.0.0/8 literal is loopback. Match the dotted-quad shape, then
  # range-check the octets so e.g. 127.999.0.1 or a "127."-prefixed hostname
  # is not misclassified.
  if [[ "${host}" =~ ^127\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]]; then
    local b="${BASH_REMATCH[1]}" c="${BASH_REMATCH[2]}" d="${BASH_REMATCH[3]}"
    (( 10#${b} <= 255 && 10#${c} <= 255 && 10#${d} <= 255 )) && return 0
  fi
  return 1
}

# Resolve whether cube manages the bundled container for a dependency -- a
# separate concern from where clients connect (*_HOST), so a user-run service on
# 127.0.0.1:<port> can be used without cube starting a conflicting container.
#   <unset>/auto    -> managed iff the host is loopback
#   1/true/yes/on   -> force managed (host must be loopback, else hard error)
#   0/false/no/off  -> force unmanaged (connect to an existing service)
# Call directly, NOT via $(...): die() inside a command substitution would only
# exit the subshell and the error would be swallowed.
_dep_is_managed() {
  local managed="$1" host="$2" name="$3"
  # ${managed,,} (lowercasing) requires bash 4+; safe under our glibc/distro
  # baseline, which ships bash 4+ everywhere the one-click installer runs.
  case "${managed,,}" in
    "" | auto)
      _host_is_loopback "${host}"
      return
      ;;
    1 | true | yes | on)
      if ! _host_is_loopback "${host}"; then
        die "${name}=1 (managed bundled container) is incompatible with a non-loopback host '${host}'; use a loopback host for the bundled container or set ${name}=0 to connect to an external service"
      fi
      return 0
      ;;
    0 | false | no | off)
      return 1
      ;;
    *)
      die "invalid ${name}: '${managed}' (expected auto, 1/true/yes/on, or 0/false/no/off)"
      ;;
  esac
}

# Whether a *_MANAGED value force-selects the bundled container (as opposed to
# auto/unset or an explicit 0). Returns 0 (true) for 1/true/yes/on. Used to
# detect the "flip external -> bundled" intent so a stale non-loopback host can
# be reset to the bundled loopback default before _dep_is_managed would die on
# the mismatch.
_dep_force_managed() {
  case "${1,,}" in
    1 | true | yes | on) return 0 ;;
    *) return 1 ;;
  esac
}

# These wrappers (and the *_is_external inverses below) delegate to
# _dep_is_managed, so they inherit its calling convention: invoke directly in an
# `if`/`&&`/`||` test, NOT via $(...). A die() reached through a command
# substitution would only exit the subshell and the error would be swallowed.
mysql_is_managed() {
  _dep_is_managed "${CUBE_SANDBOX_MYSQL_MANAGED:-auto}" "${CUBE_SANDBOX_MYSQL_HOST:-127.0.0.1}" CUBE_SANDBOX_MYSQL_MANAGED
}

redis_is_managed() {
  # Sentinel mode has no fixed master host and is inherently external: cube can
  # only manage the bundled standalone container, never a Sentinel cluster. A
  # non-empty CUBE_SANDBOX_REDIS_MASTER_NAME therefore forces unmanaged, and an
  # explicit *_MANAGED=1/true/yes/on is rejected as contradictory rather than
  # silently overriding it.
  if [[ -n "${CUBE_SANDBOX_REDIS_MASTER_NAME:-}" ]]; then
    if _dep_force_managed "${CUBE_SANDBOX_REDIS_MANAGED:-}"; then
      die "CUBE_SANDBOX_REDIS_MANAGED=${CUBE_SANDBOX_REDIS_MANAGED} (managed bundled container) is incompatible with Sentinel mode (CUBE_SANDBOX_REDIS_MASTER_NAME is set); unset the master name for a bundled container or set CUBE_SANDBOX_REDIS_MANAGED=0"
    fi
    return 1
  fi
  _dep_is_managed "${CUBE_SANDBOX_REDIS_MANAGED:-auto}" "${CUBE_SANDBOX_REDIS_HOST:-127.0.0.1}" CUBE_SANDBOX_REDIS_MANAGED
}

# "External" means cube does not manage the local container, so the container /
# systemd unit / health checks must be skipped and clients use the configured
# host:port directly. It is the logical inverse of "managed".
mysql_is_external() {
  ! mysql_is_managed
}

redis_is_external() {
  ! redis_is_managed
}

# Whether the runtime env file has already completed the CUBE_EXTERNAL_* ->
# CUBE_SANDBOX_* migration, i.e. it no longer carries ANY active legacy
# CUBE_EXTERNAL_* key. Pre-unification installs persisted CUBE_EXTERNAL_* into
# .one-click.env whenever an external endpoint was configured, so a runtime env
# that still has them was written before unification and needs migrating; one
# without them was written by a post-unification install and is already
# migrated. Returns 0 (true, migration complete) when no legacy key is present,
# 1 otherwise. A missing file counts as complete -- there is nothing legacy to
# migrate. The canonical old-key list comes from
# ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS so this never drifts from the migration.
runtime_external_migration_complete() {
  local file="$1"
  [[ -f "${file}" ]] || return 0
  local pair old
  for pair in "${ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS[@]}"; do
    old="${pair%%:*}"
    # Match an active `KEY=` assignment (optionally indented); a commented
    # `#CUBE_EXTERNAL_*` line does not count as still-configured.
    if grep -Eq "^[[:space:]]*${old}=" "${file}"; then
      return 1
    fi
  done
  return 0
}

# Authoritative migration shim for the pre-unification variable names. The
# external MySQL/Redis endpoint used to be configured through a separate
# CUBE_EXTERNAL_* set; it is now folded into the single CUBE_SANDBOX_* set,
# distinguished by HOST.
#
# CRITICAL: the legacy CUBE_EXTERNAL_* values are AUTHORITATIVE and OVERWRITE the
# corresponding CUBE_SANDBOX_* variable even when the latter is already set. On
# pre-unification installs the real external address/password lived only on
# CUBE_EXTERNAL_*, while CUBE_SANDBOX_* carried the bundled loopback defaults
# (127.0.0.1 / cube_pass / ceuhvu123). Mapping "only when the new var is unset"
# let that bundled default mask the real endpoint -- so an upgrade would silently
# rewrite DATABASE_URL / CUBE_PROXY_REDIS_* to point at 127.0.0.1 with the default
# password. Overwriting unconditionally (and clearing the legacy var afterwards)
# is the only safe migration: anyone who still has CUBE_EXTERNAL_* set intends it
# as their external endpoint. Safe to call repeatedly: the guard makes later
# calls no-ops.
#
# EXCEPTION -- migration already complete: once an install has migrated onto the
# unified vars, it stops persisting CUBE_EXTERNAL_* to the runtime .one-click.env.
# On a later upgrade merge_env_three_way rebuilds the unified endpoint from that
# runtime env, but leftover CUBE_EXTERNAL_* lines the operator forgot to delete
# from their own .env are still loaded into this process's environment. Honouring
# them authoritatively would clobber the freshly merged unified endpoint back to a
# stale value (fslongjin's second-upgrade regression). When ONE_CLICK_EXTERNAL_MIGRATION_COMPLETE=1
# (set by install.sh iff the OLD runtime env carries no CUBE_EXTERNAL_* key), the
# migration is done: clear the leftover legacy vars and warn, but do NOT let them
# overwrite the unified config or flip *_MANAGED.
apply_deprecated_external_aliases() {
  [[ "${ONE_CLICK_EXTERNAL_ALIASES_APPLIED:-0}" == "1" ]] && return 0
  ONE_CLICK_EXTERNAL_ALIASES_APPLIED=1

  local migration_complete="${ONE_CLICK_EXTERNAL_MIGRATION_COMPLETE:-0}"
  local warned=0
  local _alias_mysql_host_mapped=0
  local _alias_redis_host_mapped=0
  local pair old new
  local stale_leftover=0
  for pair in "${ONE_CLICK_LEGACY_EXTERNAL_ALIAS_PAIRS[@]}"; do
    old="${pair%%:*}"
    new="${pair##*:}"
    [[ -n "${!old:-}" ]] || continue
    if [[ "${migration_complete}" == "1" ]]; then
      # Migration already happened on a prior install; the unified vars are
      # authoritative. Discard the leftover legacy var without overwriting.
      unset "${old}"
      stale_leftover=1
      continue
    fi
    # Authoritatively map when the legacy var is set non-empty, overwriting any
    # existing new value, then clear the legacy var so it does not linger and
    # cannot re-trigger on a later resolution pass.
    printf -v "${new}" '%s' "${!old}"
    export "${new?}"
    unset "${old}"
    warned=1
    case "${old}" in
      CUBE_EXTERNAL_MYSQL_HOST) _alias_mysql_host_mapped=1 ;;
      CUBE_EXTERNAL_REDIS_HOST) _alias_redis_host_mapped=1 ;;
    esac
  done

  if [[ "${stale_leftover}" == "1" ]]; then
    log "WARNING: ignoring leftover deprecated CUBE_EXTERNAL_MYSQL_*/CUBE_EXTERNAL_REDIS_* from your .env -- this deployment already migrated to the unified CUBE_SANDBOX_MYSQL_*/CUBE_SANDBOX_REDIS_* config, so the stale legacy values were discarded rather than applied. Delete the CUBE_EXTERNAL_* lines from your .env to silence this warning."
  fi

  # Preserve legacy semantics: under the old scheme any non-empty
  # CUBE_EXTERNAL_*_HOST meant "external", even 127.0.0.1. The new scheme treats
  # a loopback host as managed-by-default, so a legacy loopback external host
  # would otherwise flip to "bundled container". Force MANAGED=0 when a legacy
  # host was provided and the operator has not set MANAGED explicitly, so the
  # mapped config keeps behaving as external. The legacy HOST var was cleared
  # above, so key off the _alias_*_host_mapped flags set during the loop, which
  # record whether a legacy host was mapped.
  if [[ "${_alias_mysql_host_mapped}" == "1" && -z "${CUBE_SANDBOX_MYSQL_MANAGED:-}" ]]; then
    export CUBE_SANDBOX_MYSQL_MANAGED=0
    warned=1
  fi
  if [[ "${_alias_redis_host_mapped}" == "1" && -z "${CUBE_SANDBOX_REDIS_MANAGED:-}" ]]; then
    export CUBE_SANDBOX_REDIS_MANAGED=0
    warned=1
  fi

  # If the operator force-selects the bundled container (*_MANAGED=1/true/yes/on)
  # on a first migration while a legacy CUBE_EXTERNAL_*_HOST still lingers, the
  # loop above just mapped that non-loopback host onto CUBE_SANDBOX_*_HOST --
  # overwriting the loopback reset merge_env_three_way applied for the same
  # flip. That leaves MANAGED=1 + a remote host, which dies in _dep_is_managed on
  # the first mysql_is_managed/redis_is_managed check. Reset the mapped host back
  # to the bundled loopback default so the forced-managed decision is
  # self-consistent, mirroring the merge-time reset for the new-style vars.
  if [[ "${_alias_mysql_host_mapped}" == "1" ]] \
    && _dep_force_managed "${CUBE_SANDBOX_MYSQL_MANAGED:-}" \
    && ! _host_is_loopback "${CUBE_SANDBOX_MYSQL_HOST:-}"; then
    log "WARNING: CUBE_SANDBOX_MYSQL_MANAGED=${CUBE_SANDBOX_MYSQL_MANAGED} forces the bundled container; resetting the migrated external host '${CUBE_SANDBOX_MYSQL_HOST}' to the bundled loopback default 127.0.0.1."
    export CUBE_SANDBOX_MYSQL_HOST=127.0.0.1
    warned=1
  fi
  if [[ "${_alias_redis_host_mapped}" == "1" ]] \
    && _dep_force_managed "${CUBE_SANDBOX_REDIS_MANAGED:-}" \
    && ! _host_is_loopback "${CUBE_SANDBOX_REDIS_HOST:-}"; then
    log "WARNING: CUBE_SANDBOX_REDIS_MANAGED=${CUBE_SANDBOX_REDIS_MANAGED} forces the bundled container; resetting the migrated external host '${CUBE_SANDBOX_REDIS_HOST}' to the bundled loopback default 127.0.0.1."
    export CUBE_SANDBOX_REDIS_HOST=127.0.0.1
    warned=1
  fi

  if [[ "${warned}" == "1" ]]; then
    log "WARNING: CUBE_EXTERNAL_MYSQL_*/CUBE_EXTERNAL_REDIS_* are deprecated and were migrated onto CUBE_SANDBOX_MYSQL_*/CUBE_SANDBOX_REDIS_* (legacy values are authoritative and the old keys were cleared from the runtime env). Set CUBE_SANDBOX_*_HOST to a remote address, or *_MANAGED=0 to connect to a service on the local host, directly instead -- and delete the CUBE_EXTERNAL_* lines from your .env so the deprecated plaintext values do not linger there."
  fi
}

validate_ipv4_literal() {
  local value="$1"
  local name="${2:-IPv4 address}"
  local a b c d
  [[ "${value}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    || die "invalid ${name}: ${value} (expected IPv4 address)"
  IFS=. read -r a b c d <<< "${value}"
  local octet
  for octet in "${a}" "${b}" "${c}" "${d}"; do
    [[ "${octet}" =~ ^[0-9]{1,3}$ ]] || die "invalid ${name}: ${value}"
    (( 10#${octet} >= 0 && 10#${octet} <= 255 )) \
      || die "invalid ${name}: ${value} (octet out of range)"
  done
}

validate_host_port() {
  local value="$1"
  local name="${2:-host:port}"
  local host port
  [[ -n "${value}" ]] || die "${name} must not be empty"
  # This value is written into env files and later into YAML/TOML snippets.
  # Keep it intentionally narrow: hostnames/IPv4 plus :port, no quotes,
  # whitespace, slash, comments, or control characters.
  [[ "${value}" =~ ^[A-Za-z0-9.-]+:[0-9]+$ ]] \
    || die "invalid ${name}: ${value} (expected host:port)"
  host="${value%:*}"
  port="${value##*:}"
  [[ -n "${host}" && -n "${port}" ]] || die "invalid ${name}: ${value}"
  (( 10#${port} >= 1 && 10#${port} <= 65535 )) \
    || die "invalid ${name}: ${value} (port out of range)"
}

# Whether a MySQL DSN component (user/password/host/port/db) carries a character
# that would corrupt or inject into a hand-assembled `mysql://...` DATABASE_URL.
# Returns 0 (true) if a metacharacter is present, 1 otherwise. Call it in an
# `if`; do NOT wrap in $(...).
#
# Single source of the guard used by up.sh and cube-api-start.sh, which build a
# DATABASE_URL without urlencode when the env carries no explicit one. The
# bracket expression matches: @ : / # % " ` $ ' \ and any whitespace ([:space:]
# is a POSIX class, valid inside a case-glob bracket). Every backslash is
# load-bearing (it escapes " ` $ ' \ for the shell); do NOT "tidy up"
# backslashes here -- removing them silently weakens the guard.
dsn_component_has_metachar() {
  case "$1" in
    *[@:/#%\"\`\$\'\\[:space:]]*)
      return 0
      ;;
  esac
  return 1
}
