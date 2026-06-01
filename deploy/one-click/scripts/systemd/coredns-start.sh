#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker
require_cmd sed
require_cmd ip
require_cmd awk

COREDNS_DIR="${TOOLBOX_ROOT}/coredns"
COREFILE_TEMPLATE="${COREDNS_DIR}/Corefile.template"
COREFILE_PATH="${COREDNS_DIR}/Corefile"
RESOLV_UPSTREAM_PATH="${COREDNS_DIR}/resolv.conf.upstream"
COREDNS_CONTAINER="${CUBE_PROXY_COREDNS_CONTAINER:-cube-proxy-coredns}"
COREDNS_IMAGE="${CUBE_PROXY_COREDNS_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/coredns/coredns:1.14.2}"
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
DNS_ANSWER_IP="${CUBE_PROXY_DNS_ANSWER_IP:-${CUBE_SANDBOX_NODE_IP:-127.0.0.1}}"
DEFAULT_COREDNS_BIND_ADDR="${CUBE_PROXY_COREDNS_BIND_ADDR:-127.0.0.54}"
RESOLVED_COREDNS_BIND_ADDR="${CUBE_PROXY_RESOLVED_DNS_ADDR:-169.254.254.53}"
COREDNS_BIND_ADDR="${DEFAULT_COREDNS_BIND_ADDR}"
RESOLVED_LINK_NAME="${CUBE_PROXY_RESOLVED_LINK_NAME:-cube-dns0}"
RESOLVED_LINK_ADDR="${CUBE_PROXY_RESOLVED_LINK_ADDR:-${RESOLVED_COREDNS_BIND_ADDR}/32}"
HOST_DNS_BACKEND="networkmanager-dnsmasq"
PREPARE_ONLY="${ONE_CLICK_PREPARE_ONLY:-0}"

if command -v resolvectl >/dev/null 2>&1; then
  HOST_DNS_BACKEND="systemd-resolved"
  COREDNS_BIND_ADDR="${RESOLVED_COREDNS_BIND_ADDR}"
fi

is_stub_nameserver() {
  local nameserver="$1"
  [[ -n "${nameserver}" ]] || return 0
  [[ "${nameserver}" == 127.* ]] && return 0
  [[ "${nameserver}" == ::1 ]] && return 0
  [[ "${nameserver}" == 0:0:0:0:0:0:0:1 ]] && return 0
  [[ "${nameserver}" == "${COREDNS_BIND_ADDR}" ]] && return 0
  return 1
}

write_upstream_resolv_conf() {
  local src_path="$1"
  local dst_path="$2"
  local tmp_path="${dst_path}.tmp.$$"
  local found_nameserver=1

  [[ -f "${src_path}" ]] || return 1

  prepare_file_output "${dst_path}"
  : > "${tmp_path}"
  while IFS= read -r line || [[ -n "${line}" ]]; do
    case "${line}" in
      nameserver\ *)
        local nameserver="${line#nameserver }"
        nameserver="${nameserver%%#*}"
        nameserver="${nameserver%%;*}"
        nameserver="$(printf '%s' "${nameserver}" | awk '{print $1}')"
        if ! is_stub_nameserver "${nameserver}"; then
          printf 'nameserver %s\n' "${nameserver}" >> "${tmp_path}"
          found_nameserver=0
        fi
        ;;
      search\ *|domain\ *|options\ *|sortlist\ *)
        printf '%s\n' "${line}" >> "${tmp_path}"
        ;;
      \#*|'')
        printf '%s\n' "${line}" >> "${tmp_path}"
        ;;
    esac
  done < "${src_path}"

  if [[ "${found_nameserver}" -ne 0 ]]; then
    rm -f "${tmp_path}"
    return 1
  fi

  mv -f "${tmp_path}" "${dst_path}"
  return 0
}

prepare_upstream_resolv_conf() {
  local src_path
  local -a candidates=(
    "/run/systemd/resolve/resolv.conf"
    "/run/NetworkManager/no-stub-resolv.conf"
    "/var/run/NetworkManager/no-stub-resolv.conf"
    "/etc/resolv.conf"
  )

  for src_path in "${candidates[@]}"; do
    if write_upstream_resolv_conf "${src_path}" "${RESOLV_UPSTREAM_PATH}"; then
      return 0
    fi
  done

  die "failed to determine non-stub upstream DNS servers; checked ${candidates[*]}"
}

link_exists() {
  ip link show dev "$1" >/dev/null 2>&1
}

link_is_dummy() {
  local link_details
  link_details="$(ip -d link show dev "$1" 2>/dev/null || true)"
  [[ "${link_details}" == *" dummy "* || "${link_details}" == *"dummy "* ]]
}

ensure_resolved_link() {
  if link_exists "${RESOLVED_LINK_NAME}"; then
    link_is_dummy "${RESOLVED_LINK_NAME}" || die "existing link ${RESOLVED_LINK_NAME} is not a dummy link"
  elif ! ip link add "${RESOLVED_LINK_NAME}" type dummy; then
    # CoreDNS and host DNS routing may start concurrently under systemd.
    # If another helper created the link first, accept it after validation.
    link_exists "${RESOLVED_LINK_NAME}" || die "failed to create dummy link ${RESOLVED_LINK_NAME}"
    link_is_dummy "${RESOLVED_LINK_NAME}" || die "existing link ${RESOLVED_LINK_NAME} is not a dummy link"
  fi

  ip link set "${RESOLVED_LINK_NAME}" up
  ip addr replace "${RESOLVED_LINK_ADDR}" dev "${RESOLVED_LINK_NAME}"
}

ensure_dir "${COREDNS_DIR}"
ensure_file "${COREFILE_TEMPLATE}"
if [[ "${HOST_DNS_BACKEND}" == "systemd-resolved" ]]; then
  ensure_resolved_link
fi
prepare_upstream_resolv_conf
render_template_atomic \
  "${COREFILE_TEMPLATE}" \
  "${COREFILE_PATH}" \
  -e "s/__CUBE_PROXY_DNS_ANSWER_IP__/$(escape_sed "${DNS_ANSWER_IP}")/g" \
  -e "s/__COREDNS_BIND_ADDR__/$(escape_sed "${COREDNS_BIND_ADDR}")/g"

ensure_bind_mount_file "${RESOLV_UPSTREAM_PATH}"

if [[ "${PREPARE_ONLY}" == "1" ]]; then
  log "coredns runtime files prepared under ${COREDNS_DIR}"
  exit 0
fi

docker_rm_if_exists "${COREDNS_CONTAINER}"
docker create \
  --name "${COREDNS_CONTAINER}" \
  --network host \
  -v "${COREFILE_PATH}:/etc/coredns/Corefile:ro" \
  -v "${RESOLV_UPSTREAM_PATH}:/etc/resolv.conf:ro" \
  "${COREDNS_IMAGE}" \
  -conf /etc/coredns/Corefile >/dev/null

exec docker start -a "${COREDNS_CONTAINER}"
