#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install and manage a single-node Elasticsearch 8.x service for CubeSandbox
# log observability (default port :9200, index: cubesandbox-logs*).
set -euo pipefail

ES_VERSION="${CUBE_ES_VERSION:-8.17.3}"
ES_USER="cube-es"
ES_GROUP="cube-es"
INSTALL_DIR="/usr/local/services/cube-elasticsearch"
CONFIG_DIR="/etc/cube-sandbox/elasticsearch"
DATA_DIR="${CUBE_ES_DATA_DIR:-/var/lib/cube-sandbox/elasticsearch}"
LOG_DIR="${CUBE_ES_LOG_DIR:-/var/log/cube-sandbox/elasticsearch}"
UNIT_NAME="cube-sandbox-elasticsearch.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"

log() { echo "[cube-elasticsearch] $*" >&2; }
die() { echo "[cube-elasticsearch] ERROR: $*" >&2; exit 1; }

require_root() {
  [[ "$(id -u)" -eq 0 ]] || die "run as root"
}

require_single_line() {
  local name="$1"
  local value="$2"
  [[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || die "${name} must be a single line"
}

listen_address() {
  printf '%s' "${CUBE_ES_LISTEN_ADDRESS:-${CUBE_ES_LISTEN_ADDR:-0.0.0.0:9200}}"
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
  if [[ -n "${CUBE_ES_HEALTH_URL:-}" ]]; then
    printf '%s' "${CUBE_ES_HEALTH_URL%/}"
    return
  fi
  local host
  host="$(listen_host)"
  case "${host}" in
    0.0.0.0) host="127.0.0.1" ;;
  esac
  printf 'http://%s:%s' "${host}" "$(listen_port)"
}

arch_name() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'x86_64\n' ;;
    aarch64|arm64) printf 'aarch64\n' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

require_config() {
  local address host port heap
  address="$(listen_address)"
  require_single_line "CUBE_ES_LISTEN_ADDRESS" "${address}"
  [[ "${address}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}:[0-9]+$ ]] || \
    die "CUBE_ES_LISTEN_ADDRESS must be an IPv4 address and port (e.g. 0.0.0.0:9200)"
  host="${address%:*}"
  port="${address##*:}"
  [[ "${port}" -ge 1 && "${port}" -le 65535 ]] || die "Elasticsearch listen port must be between 1 and 65535"
  if [[ "${host}" == "0.0.0.0" ]]; then
    log "WARNING: Elasticsearch security is disabled and it will listen on all interfaces; restrict port ${port} to trusted private networks"
  fi

  heap="${CUBE_ES_HEAP_SIZE:-1g}"
  require_single_line "CUBE_ES_HEAP_SIZE" "${heap}"
  [[ "${heap}" =~ ^[0-9]+[mMgG]$ ]] || die "CUBE_ES_HEAP_SIZE must look like 512m, 1g, 2g, etc."
}

ensure_user() {
  if ! getent group "${ES_GROUP}" >/dev/null; then
    groupadd --system "${ES_GROUP}"
  fi
  if ! id -u "${ES_USER}" >/dev/null 2>&1; then
    useradd --system --gid "${ES_GROUP}" --home-dir "${DATA_DIR}" \
      --shell /sbin/nologin "${ES_USER}"
  fi
}

install_binaries() {
  if [[ -x "${INSTALL_DIR}/bin/elasticsearch" ]] &&
    "${INSTALL_DIR}/bin/elasticsearch" --version 2>/dev/null | grep -q "${ES_VERSION}"; then
    log "Elasticsearch ${ES_VERSION} already installed at ${INSTALL_DIR}"
    return
  fi

  local arch asset base url sha_url tmp expected actual
  arch="$(arch_name)"
  asset="elasticsearch-${ES_VERSION}-linux-${arch}.tar.gz"
  base="https://artifacts.elastic.co/downloads/elasticsearch"
  url="${CUBE_ES_DOWNLOAD_URL:-${base}/${asset}}"
  sha_url="${url}.sha512"
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp:-}"' RETURN

  if [[ -n "${CUBE_ES_TARBALL:-}" ]]; then
    [[ -f "${CUBE_ES_TARBALL}" ]] || die "CUBE_ES_TARBALL not found: ${CUBE_ES_TARBALL}"
    log "installing from local tarball ${CUBE_ES_TARBALL}"
    cp -f "${CUBE_ES_TARBALL}" "${tmp}/${asset}"
  else
    log "downloading ${url}"
    curl -fsSL "${url}" -o "${tmp}/${asset}"
    curl -fsSL "${sha_url}" -o "${tmp}/${asset}.sha512" || \
      die "could not download checksum for ${asset}"
    expected="$(awk '{print $1; exit}' "${tmp}/${asset}.sha512")"
    [[ "${expected}" =~ ^[0-9a-fA-F]{128}$ ]] || die "invalid checksum for ${asset}"
    if command -v sha512sum >/dev/null 2>&1; then
      actual="$(sha512sum "${tmp}/${asset}" | awk '{print $1}')"
    else
      actual="$(shasum -a 512 "${tmp}/${asset}" | awk '{print $1}')"
    fi
    [[ "${actual}" == "${expected}" ]] || die "checksum mismatch for ${asset}"
  fi

  rm -rf "${INSTALL_DIR}"
  mkdir -p "${INSTALL_DIR}"
  tar -C "${INSTALL_DIR}" --strip-components=1 -xzf "${tmp}/${asset}"
  chown -R "${ES_USER}:${ES_GROUP}" "${INSTALL_DIR}"
  rm -rf "${tmp}"
  trap - RETURN
  log "installed Elasticsearch ${ES_VERSION} to ${INSTALL_DIR}"
}

write_es_config() {
  local destination="$1"
  local host port cluster_name
  host="$(listen_host)"
  port="$(listen_port)"
  cluster_name="${CUBE_OBSERVABILITY_CLUSTER_ID:-cubesandbox}-es"
  mkdir -p "$(dirname "${destination}")"
  cat > "${destination}" <<EOF
cluster.name: ${cluster_name}
node.name: node-1
path.data: ${DATA_DIR}
path.logs: ${LOG_DIR}
network.host: ${host}
http.port: ${port}
discovery.type: single-node
xpack.security.enabled: false
xpack.ml.enabled: false
action.auto_create_index: true
EOF
}

write_jvm_options() {
  local destination="$1"
  local heap="${CUBE_ES_HEAP_SIZE:-1g}"
  mkdir -p "$(dirname "${destination}")"
  cat > "${destination}" <<EOF
-Xms${heap}
-Xmx${heap}
EOF
}

write_unit() {
  local destination="$1"
  cat > "${destination}" <<EOF
[Unit]
Description=CubeSandbox Elasticsearch log backend
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${ES_USER}
Group=${ES_GROUP}
Environment="ES_HOME=${INSTALL_DIR}"
Environment="ES_PATH_CONF=${CONFIG_DIR}"
ExecStart=${INSTALL_DIR}/bin/elasticsearch
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
LimitNPROC=4096
LimitMEMLOCK=infinity
TimeoutStartSec=180

[Install]
WantedBy=multi-user.target
EOF
}

apply_index_template() {
  local url="$1"
  local index_prefix="${CUBE_LOGS_ES_INDEX:-cubesandbox-logs}"
  curl -fsS -X PUT "${url}/_index_template/cubesandbox-logs-template" \
    -H "Content-Type: application/json" \
    -d "{
      \"index_patterns\": [\"${index_prefix}*\"],
      \"priority\": 200,
      \"template\": {
        \"settings\": {
          \"number_of_shards\": 1,
          \"number_of_replicas\": 0,
          \"refresh_interval\": \"2s\"
        },
        \"mappings\": {
          \"properties\": {
            \"@timestamp\": { \"type\": \"date\" },
            \"Body\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\", \"ignore_above\": 2048 } } },
            \"SeverityText\": { \"type\": \"keyword\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"service_name\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"cubesandbox_cluster_id\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"host_name\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"cubesandbox_log_source\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"cubesandbox_sandbox_id\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"log_iostream\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } },
            \"log_file_path\": { \"type\": \"text\", \"fields\": { \"keyword\": { \"type\": \"keyword\" } } }
          }
        }
      }
    }" >/dev/null || log "WARNING: could not pre-install index template (ES will auto-map on first write)"
}

install_service() {
  require_root
  require_config
  ensure_user
  install_binaries

  sysctl -w vm.max_map_count=1048576 >/dev/null 2>&1 || true
  if [[ -d /etc/sysctl.d ]]; then
    echo "vm.max_map_count=1048576" > /etc/sysctl.d/99-cube-elasticsearch.conf || true
  fi

  # Shared /etc/cube-sandbox must stay traversable by every service user.
  install -d -m 0755 -o root -g root "$(dirname "${CONFIG_DIR}")"
  install -d -m 0750 -o "${ES_USER}" -g "${ES_GROUP}" "${CONFIG_DIR}" "${CONFIG_DIR}/jvm.options.d" "${DATA_DIR}" "${LOG_DIR}"
  cp -rn "${INSTALL_DIR}/config/"* "${CONFIG_DIR}/" 2>/dev/null || true
  write_es_config "${CONFIG_DIR}/elasticsearch.yml"
  write_jvm_options "${CONFIG_DIR}/jvm.options.d/cube-heap.options"
  chown -R "${ES_USER}:${ES_GROUP}" "${CONFIG_DIR}" "${DATA_DIR}" "${LOG_DIR}"

  write_unit "${UNIT_PATH}"
  systemctl daemon-reload
  systemctl enable "${UNIT_NAME}"
  systemctl restart "${UNIT_NAME}"

  log "waiting for Elasticsearch to become ready at $(health_url) ..."
  local i ready="false"
  for i in $(seq 1 45); do
    if curl -fsS --max-time 3 "$(health_url)/_cluster/health" >/dev/null 2>&1; then
      ready="true"
      break
    fi
    sleep 2
  done
  [[ "${ready}" == "true" ]] || die "Elasticsearch failed to become ready; inspect journalctl -u ${UNIT_NAME}"

  apply_index_template "$(health_url)"
  log "Elasticsearch ${ES_VERSION} is ready at $(health_url) (index pattern: ${CUBE_LOGS_ES_INDEX:-cubesandbox-logs}*)"
}

render_config() {
  require_config
  local output="${CUBE_ES_CONFIG_OUTPUT:-${PWD}/cubesandbox-elasticsearch.yml}"
  write_es_config "${output}.tmp"
  mv "${output}.tmp" "${output}"
  log "wrote ${output}"
}

uninstall_service() {
  require_root
  systemctl disable --now "${UNIT_NAME}" 2>/dev/null || true
  rm -f "${UNIT_PATH}" "${CONFIG_DIR}/elasticsearch.yml" "${CONFIG_DIR}/jvm.options.d/cube-heap.options"
  systemctl daemon-reload
  log "removed ${UNIT_NAME}; retained ${INSTALL_DIR} and ${DATA_DIR}"
}

case "${1:-install}" in
  install) install_service ;;
  render) render_config ;;
  uninstall) uninstall_service ;;
  *) die "usage: $0 [install|render|uninstall]" ;;
esac
