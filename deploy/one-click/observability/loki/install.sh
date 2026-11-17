#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install and manage a single-binary Loki service for CubeSandbox sandbox logs.
# OTel Collector sends logs to Loki's native OTLP/HTTP endpoint at /otlp.
set -euo pipefail

LOKI_VERSION="${CUBE_LOKI_VERSION:-3.7.9}"
LOKI_USER="cube-loki"
LOKI_GROUP="cube-loki"
BIN_PATH="/usr/local/bin/loki"
CONFIG_DIR="/etc/cube-sandbox/loki"
CONFIG_PATH="${CONFIG_DIR}/loki.yaml"
STATE_DIR="${CUBE_LOKI_DATA_DIR:-/var/lib/cube-sandbox/loki}"
UNIT_NAME="cube-sandbox-loki.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"

log() { echo "[cube-loki] $*" >&2; }
die() { echo "[cube-loki] ERROR: $*" >&2; exit 1; }

require_root() {
  [[ "$(id -u)" -eq 0 ]] || die "run as root"
}

require_single_line() {
  local name="$1"
  local value="$2"
  [[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || die "${name} must be a single line"
}

listen_address() {
  printf '%s' "${CUBE_LOKI_LISTEN_ADDRESS:-${CUBE_LOKI_LISTEN_ADDR:-0.0.0.0:3100}}"
}

listen_host() {
  local address
  address="$(listen_address)"
  printf '%s' "${address%:*}"
}

listen_port() {
  local address
  address="$(listen_address)"
  printf '%s' "${address##*:}"
}

health_url() {
  if [[ -n "${CUBE_LOKI_URL:-}" ]]; then
    printf '%s' "${CUBE_LOKI_URL%/}"
    return
  fi
  local host
  host="$(listen_host)"
  case "${host}" in
    0.0.0.0) host="127.0.0.1" ;;
  esac
  printf 'http://%s:%s' "${host}" "$(listen_port)"
}

require_config() {
  local address host port retention amount
  address="$(listen_address)"
  require_single_line "CUBE_LOKI_LISTEN_ADDRESS" "${address}"
  [[ "${address}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}:[0-9]+$ ]] || \
    die "CUBE_LOKI_LISTEN_ADDRESS must be an IPv4 address and port (e.g. 0.0.0.0:3100)"
  host="${address%:*}"
  port="${address##*:}"
  [[ "${port}" -ge 1 && "${port}" -le 65535 ]] || die "Loki listen port must be between 1 and 65535"
  if [[ "${host}" == "0.0.0.0" ]]; then
    log "WARNING: Loki has no built-in authentication and will listen on all interfaces; restrict port ${port} to trusted private networks"
  fi
  retention="${CUBE_LOKI_RETENTION_PERIOD:-15d}"
  require_single_line "CUBE_LOKI_RETENTION_PERIOD" "${retention}"
  [[ "${retention}" =~ ^([0-9]+)(h|d|w)$ ]] || \
    die "CUBE_LOKI_RETENTION_PERIOD must look like 24h, 15d, or 4w"
  amount="${BASH_REMATCH[1]}"
  [[ "${amount}" -ge 1 ]] || die "CUBE_LOKI_RETENTION_PERIOD must be greater than zero"
  require_single_line "CUBE_LOKI_DATA_DIR" "${STATE_DIR}"
  [[ "${STATE_DIR}" =~ ^/[A-Za-z0-9._/-]+$ ]] || \
    die "CUBE_LOKI_DATA_DIR must be an absolute path containing only letters, digits, '.', '_', '-', and '/'"
}

arch_name() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

install_binary() {
  if [[ -n "${CUBE_LOKI_BIN:-}" ]]; then
    [[ -x "${CUBE_LOKI_BIN}" ]] || die "CUBE_LOKI_BIN must identify an executable"
    install -m 0755 "${CUBE_LOKI_BIN}" "${BIN_PATH}"
    return
  fi
  if [[ -x "${BIN_PATH}" ]] &&
    "${BIN_PATH}" -version 2>&1 | grep -Eq "version ${LOKI_VERSION}([[:space:]]|$)"; then
    log "Loki ${LOKI_VERSION} already installed"
    return
  fi

  local arch asset base tmp expected actual
  arch="$(arch_name)"
  asset="loki-linux-${arch}.zip"
  base="https://github.com/grafana/loki/releases/download/v${LOKI_VERSION}"
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp:-}"' RETURN

  if [[ -n "${CUBE_LOKI_ZIP:-}" ]]; then
    [[ -f "${CUBE_LOKI_ZIP}" ]] || die "CUBE_LOKI_ZIP not found: ${CUBE_LOKI_ZIP}"
    cp -f "${CUBE_LOKI_ZIP}" "${tmp}/${asset}"
  else
    log "downloading ${base}/${asset}"
    curl -fsSL "${base}/${asset}" -o "${tmp}/${asset}"
    curl -fsSL "${base}/SHA256SUMS" -o "${tmp}/SHA256SUMS"
    expected="$(awk -v asset="${asset}" '$2 == asset || $2 == "*" asset { print $1; exit }' "${tmp}/SHA256SUMS")"
    [[ -n "${expected}" ]] || die "checksum for ${asset} is missing from SHA256SUMS"
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "${tmp}/${asset}" | awk '{print $1}')"
    else
      actual="$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')"
    fi
    [[ "${actual}" == "${expected}" ]] || die "checksum mismatch for ${asset}"
  fi

  command -v python3 >/dev/null 2>&1 || \
    die "python3 is required to extract and validate the Loki release archive"
  python3 - "${tmp}/${asset}" "${tmp}/extract" <<'PY'
import pathlib
import sys
import zipfile

archive = pathlib.Path(sys.argv[1])
destination = pathlib.Path(sys.argv[2])
destination.mkdir(parents=True, exist_ok=True)
with zipfile.ZipFile(archive) as source:
    source.extractall(destination)
PY
  [[ -f "${tmp}/extract/loki-linux-${arch}" ]] || die "Loki binary missing from ${asset}"
  install -m 0755 "${tmp}/extract/loki-linux-${arch}" "${BIN_PATH}"
  rm -rf "${tmp}"
  trap - RETURN
  log "installed Loki ${LOKI_VERSION} to ${BIN_PATH}"
}

write_loki_config() {
  local destination="$1"
  local retention="${CUBE_LOKI_RETENTION_PERIOD:-15d}"
  mkdir -p "$(dirname "${destination}")"
  cat > "${destination}" <<EOF
auth_enabled: false

server:
  http_listen_address: $(listen_host)
  http_listen_port: $(listen_port)
  grpc_listen_address: 127.0.0.1
  grpc_listen_port: 9096

common:
  instance_addr: 127.0.0.1
  path_prefix: ${STATE_DIR}
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory

schema_config:
  configs:
    - from: "2024-01-01"
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: cubesandbox_
        period: 24h

storage_config:
  filesystem:
    directory: ${STATE_DIR}/chunks

compactor:
  working_directory: ${STATE_DIR}/compactor
  retention_enabled: true
  delete_request_store: filesystem

limits_config:
  allow_structured_metadata: true
  retention_period: ${retention}
  otlp_config:
    resource_attributes:
      ignore_defaults: true
      attributes_config:
        - action: index_label
          attributes:
            - service.name
            - cubesandbox.cluster.id
            - host.name

analytics:
  reporting_enabled: false
EOF
}

write_unit() {
  local destination="$1"
  cat > "${destination}" <<EOF
[Unit]
Description=CubeSandbox Loki log backend
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${LOKI_USER}
Group=${LOKI_GROUP}
ExecStart=${BIN_PATH} -config.file=${CONFIG_PATH}
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
}

ensure_user() {
  if ! getent group "${LOKI_GROUP}" >/dev/null; then
    groupadd --system "${LOKI_GROUP}"
  fi
  if ! id -u "${LOKI_USER}" >/dev/null 2>&1; then
    useradd --system --gid "${LOKI_GROUP}" --home-dir "${STATE_DIR}" \
      --shell /sbin/nologin "${LOKI_USER}"
  fi
}

install_service() {
  require_root
  require_config
  ensure_user
  install_binary

  # Shared /etc/cube-sandbox must stay traversable by every service user.
  install -d -m 0755 -o root -g root "$(dirname "${CONFIG_DIR}")"
  install -d -m 0750 -o "${LOKI_USER}" -g "${LOKI_GROUP}" \
    "${CONFIG_DIR}" "${STATE_DIR}" "${STATE_DIR}/chunks" "${STATE_DIR}/compactor"
  write_loki_config "${CONFIG_PATH}"
  chown "${LOKI_USER}:${LOKI_GROUP}" "${CONFIG_PATH}"
  "${BIN_PATH}" -config.file="${CONFIG_PATH}" -verify-config=true

  write_unit "${UNIT_PATH}"
  systemctl daemon-reload
  systemctl enable "${UNIT_NAME}"
  systemctl restart "${UNIT_NAME}"

  log "waiting for Loki to become ready at $(health_url) ..."
  local i ready="false"
  for i in $(seq 1 45); do
    if curl -fsS --max-time 3 "$(health_url)/ready" >/dev/null 2>&1; then
      ready="true"
      break
    fi
    sleep 2
  done
  [[ "${ready}" == "true" ]] || die "Loki failed to become ready; inspect journalctl -u ${UNIT_NAME}"
  log "Loki ${LOKI_VERSION} is ready at $(health_url) (OTLP base endpoint: $(health_url)/otlp)"
}

render_config() {
  require_config
  local output="${CUBE_LOKI_CONFIG_OUTPUT:-${PWD}/cubesandbox-loki.yaml}"
  write_loki_config "${output}.tmp"
  mv "${output}.tmp" "${output}"
  log "wrote ${output}"
}

uninstall_service() {
  require_root
  systemctl disable --now "${UNIT_NAME}" 2>/dev/null || true
  rm -f "${UNIT_PATH}" "${CONFIG_PATH}"
  systemctl daemon-reload
  log "removed ${UNIT_NAME}; retained ${BIN_PATH} and ${STATE_DIR}"
}

case "${1:-install}" in
  install) install_service ;;
  render) render_config ;;
  uninstall) uninstall_service ;;
  *) die "usage: $0 [install|render|uninstall]" ;;
esac
