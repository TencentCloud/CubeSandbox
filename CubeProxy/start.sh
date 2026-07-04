#!/bin/bash
# CubeProxy container entrypoint.
#
# Layout:
#   - Foreground: openresty/nginx (PID 1's main duty after exec)
#   - Background: crond, log rotation

set -u

GLOBAL_CONF_PATH="${CUBE_PROXY_GLOBAL_CONF_PATH:-/usr/local/openresty/nginx/conf/global/global.conf}"
RESOLVER_INCLUDE_PATH="${CUBE_PROXY_RESOLVER_INCLUDE_PATH:-/usr/local/openresty/nginx/conf/includes/resolver.inc}"

die() {
  echo "$(date -Iseconds) FATAL: $*" >&2
  return 1
}

ipv4_literal_is_valid() {
  local value="${1:-}"
  local a b c d octet
  [[ "${value}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r a b c d <<< "${value}"
  for octet in "${a}" "${b}" "${c}" "${d}"; do
    [[ "${octet}" =~ ^[0-9]{1,3}$ ]] || return 1
    (( 10#${octet} >= 0 && 10#${octet} <= 255 )) || return 1
  done
  return 0
}

ipv6_literal_is_valid() {
  local value="${1:-}"
  [[ -n "${value}" ]] || return 1
  value="${value#\[}"
  value="${value%\]}"
  [[ "${value}" == *:* ]] || return 1
  [[ "${value}" =~ ^[0-9A-Fa-f:.]+$ ]] || return 1
  [[ "${value}" != *:::* ]] || return 1
  [[ "${value}" != :* || "${value}" == ::* ]] || return 1
  [[ "${value}" != *: || "${value}" == *:: ]] || return 1

  local compressed=0
  if [[ "${value}" == *::* ]]; then
    compressed=1
    [[ "${value#*::}" != *::* ]] || return 1
  fi

  local -a parts=()
  IFS=: read -r -a parts <<< "${value}"
  local groups=0 index part
  for index in "${!parts[@]}"; do
    part="${parts[${index}]}"
    [[ -n "${part}" ]] || continue
    if [[ "${part}" == *.* ]]; then
      [[ "${index}" -eq $((${#parts[@]} - 1)) ]] || return 1
      ipv4_literal_is_valid "${part}" || return 1
      groups=$((groups + 2))
      continue
    fi
    [[ "${part}" =~ ^[0-9A-Fa-f]{1,4}$ ]] || return 1
    groups=$((groups + 1))
  done

  if [[ "${compressed}" == "1" ]]; then
    (( groups < 8 ))
  else
    (( groups == 8 ))
  fi
}

is_ip_literal() {
  local value="${1:-}"
  ipv4_literal_is_valid "${value}" || ipv6_literal_is_valid "${value}"
}

normalize_resolver_address() {
  local value="${1:-}"
  if ipv4_literal_is_valid "${value}"; then
    printf '%s\n' "${value}"
    return 0
  fi
  if ipv6_literal_is_valid "${value}"; then
    value="${value#\[}"
    value="${value%\]}"
    [[ "${value}" != "::" ]] || return 1
    printf '[%s]\n' "${value}"
    return 0
  fi

  [[ "${value}" =~ ^(.+):([0-9]+)$ ]] || return 1
  local host="${BASH_REMATCH[1]}"
  local port="${BASH_REMATCH[2]}"
  (( 10#${port} >= 1 && 10#${port} <= 65535 )) || return 1
  if ipv4_literal_is_valid "${host}"; then
    printf '%s:%s\n' "${host}" "${port}"
    return 0
  fi
  if [[ "${host}" == \[*\] ]] && ipv6_literal_is_valid "${host}"; then
    host="${host#\[}"
    host="${host%\]}"
    [[ "${host}" != "::" ]] || return 1
    printf '[%s]:%s\n' "${host}" "${port}"
    return 0
  fi
  return 1
}

discover_resolver_addresses() {
  local path="${1:-${CUBE_PROXY_RESOLV_CONF:-/etc/resolv.conf}}"
  [[ -f "${path}" ]] || return 0

  local line keyword nameserver resolver_address
  declare -A seen_addresses=()
  while IFS= read -r line || [[ -n "${line}" ]]; do
    read -r keyword nameserver _ <<< "${line}"
    [[ "${keyword:-}" == "nameserver" ]] || continue
    [[ -n "${nameserver}" ]] || continue
    resolver_address="$(normalize_resolver_address "${nameserver}")" || continue
    [[ -n "${seen_addresses[${resolver_address}]:-}" ]] && continue
    seen_addresses["${resolver_address}"]=1
    printf '%s\n' "${resolver_address}"
  done < "${path}"
}

read_redis_host_from_global_conf() {
  local path="${1:-${GLOBAL_CONF_PATH}}"
  [[ -f "${path}" ]] || return 0

  local line redis_line=""
  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ "${line}" =~ ^[[:space:]]*set[[:space:]]+\$redis_ip[[:space:]]+ ]] || continue
    # nginx executes rewrite-module set directives in order, so the last assignment wins.
    redis_line="${line}"
  done < "${path}"

  [[ -n "${redis_line}" ]] || return 0
  if [[ "${redis_line}" =~ ^[[:space:]]*set[[:space:]]+\$redis_ip[[:space:]]+\"([^\"]*)\"[[:space:]]*\;[[:space:]]*(#.*)?$ ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi

  die "unable to parse global.conf: \$redis_ip from ${path}; expected a quoted static value"
  return 1
}

build_cube_proxy_resolver_include() {
  local resolver_list="${1:-}"
  if [[ -z "${resolver_list}" ]]; then
    printf '%s\n' "# no usable nginx resolver discovered"
    return 0
  fi

  local valid="${CUBE_PROXY_RESOLVER_VALID:-30s}"
  local timeout="${CUBE_PROXY_RESOLVER_TIMEOUT:-5s}"
  local ipv6="${CUBE_PROXY_RESOLVER_IPV6:-off}"
  [[ "${valid}" =~ ^[0-9]+(ms|s|m|h|d|w|M|y)?$ ]] \
    || { die "invalid CUBE_PROXY_RESOLVER_VALID: ${valid}"; return 1; }
  [[ "${timeout}" =~ ^[0-9]+(ms|s|m|h|d|w|M|y)?$ ]] \
    || { die "invalid CUBE_PROXY_RESOLVER_TIMEOUT: ${timeout}"; return 1; }
  [[ "${ipv6}" == "on" || "${ipv6}" == "off" ]] \
    || { die "invalid CUBE_PROXY_RESOLVER_IPV6: ${ipv6} (expected on or off)"; return 1; }

  printf 'resolver %s ipv6=%s valid=%s;\nresolver_timeout %s;\n' \
    "${resolver_list}" "${ipv6}" "${valid}" "${timeout}"
}

ensure_hostname_target_has_resolver() {
  local target="${1:-}"
  local resolver_list="${2:-}"
  local target_var="${3:-target}"

  if ! is_ip_literal "${target}" && [[ -z "${resolver_list}" ]]; then
    die "${target_var} '${target}' is not an IP literal, but no nginx resolver nameserver could be discovered from ${CUBE_PROXY_RESOLV_CONF:-/etc/resolv.conf}. Ensure the container resolv.conf has at least one valid IPv4 or IPv6 nameserver."
    return 1
  fi
}

render_resolver_include() {
  local resolver_list="${1:-}"
  local tmp="${RESOLVER_INCLUDE_PATH}.tmp"

  mkdir -p "$(dirname "${RESOLVER_INCLUDE_PATH}")"
  if ! build_cube_proxy_resolver_include "${resolver_list}" > "${tmp}"; then
    rm -f "${tmp}"
    die "failed to render resolver include: ${RESOLVER_INCLUDE_PATH}" || return 1
  fi
  mv -f "${tmp}" "${RESOLVER_INCLUDE_PATH}"
}

prepare_resolver_include() {
  local resolver_list=""
  local -a resolver_addresses=()
  if [[ -n "${CUBE_PROXY_RESOLVER_ADDRS:-}" ]]; then
    local -a configured_addresses=()
    read -r -a configured_addresses <<< "${CUBE_PROXY_RESOLVER_ADDRS}"
    local configured_address resolver_address
    for configured_address in "${configured_addresses[@]}"; do
      resolver_address="$(normalize_resolver_address "${configured_address}")" \
        || { die "invalid nameserver in CUBE_PROXY_RESOLVER_ADDRS: ${configured_address}"; return 1; }
      resolver_addresses+=("${resolver_address}")
    done
  else
    local discovered_addresses=""
    discovered_addresses="$(discover_resolver_addresses)" || return 1
    if [[ -n "${discovered_addresses}" ]]; then
      mapfile -t resolver_addresses <<< "${discovered_addresses}"
    fi
  fi
  if [[ "${#resolver_addresses[@]}" -gt 0 ]]; then
    resolver_list="${resolver_addresses[*]}"
  fi

  if [[ -z "${resolver_list}" ]]; then
    local redis_host=""
    redis_host="$(read_redis_host_from_global_conf)" || return 1
    if [[ -n "${redis_host}" ]]; then
      ensure_hostname_target_has_resolver \
        "${redis_host}" "${resolver_list}" 'global.conf: $redis_ip' || return 1
    fi

    if [[ "${CUBE_PROXY_REGISTRY_ENABLE:-}" == "1" && -n "${CUBE_PROXY_REGISTRY_REDIS_HOST:-}" ]]; then
      ensure_hostname_target_has_resolver \
        "${CUBE_PROXY_REGISTRY_REDIS_HOST}" "${resolver_list}" \
        "CUBE_PROXY_REGISTRY_REDIS_HOST" || return 1
    fi
  fi

  render_resolver_include "${resolver_list}" || return 1
}

main() {
  prepare_resolver_include || exit 1

  /usr/sbin/crond
  exec /usr/local/openresty/nginx/sbin/nginx
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
