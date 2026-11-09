#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install Grafana OSS (default :3001) and automatically provision Prometheus +
# Loki or Elasticsearch datasources and the CubeSandbox dashboards:
#   - cubesandbox-cluster.json / cubesandbox-node.json / cubesandbox-sandbox.json
#   - cubesandbox-logs.json (selected log backend)
# Also supports importing datasources & dashboards into an existing Grafana
# instance via HTTP API (`./install.sh dashboards`).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -n "${CUBE_OBSERVABILITY_DASHBOARD_DIR:-}" ]]; then
  DASHBOARD_DIR="${CUBE_OBSERVABILITY_DASHBOARD_DIR}"
elif [[ -d "${SCRIPT_DIR}/../../CubeObservability/grafana/dashboards" ]]; then
  DASHBOARD_DIR="${SCRIPT_DIR}/../../CubeObservability/grafana/dashboards"
elif [[ -d "${SCRIPT_DIR}/../../../../CubeObservability/grafana/dashboards" ]]; then
  DASHBOARD_DIR="${SCRIPT_DIR}/../../../../CubeObservability/grafana/dashboards"
else
  echo "[cube-grafana] ERROR: CubeObservability dashboard assets were not found" >&2
  exit 1
fi

GRAFANA_VERSION="${CUBE_GRAFANA_VERSION:-10.4.18}"
GRAFANA_USER="cube-grafana"
GRAFANA_GROUP="cube-grafana"
INSTALL_DIR="/usr/local/services/cube-grafana"
CONFIG_DIR="/etc/cube-sandbox/grafana"
PROVISIONING_DIR="${CONFIG_DIR}/provisioning"
ADMIN_PASSWORD_FILE="${CONFIG_DIR}/admin_password"
STATE_DIR="/var/lib/cube-sandbox/grafana"
PROVISIONED_DASHBOARDS_DIR="${STATE_DIR}/dashboards"
LOG_DIR="/var/log/cube-sandbox/grafana"
UNIT_NAME="cube-sandbox-grafana.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"

log() { echo "[cube-grafana] $*" >&2; }
die() { echo "[cube-grafana] ERROR: $*" >&2; exit 1; }

require_root() {
  [[ "$(id -u)" -eq 0 ]] || die "run as root"
}

require_single_line() {
  local name="$1"
  local value="$2"
  [[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || die "${name} must be a single line"
}

require_http_url() {
  local name="$1"
  local value="$2"
  [[ "${value}" == http://* || "${value}" == https://* ]] || \
    die "${name} must start with http:// or https://"
  require_single_line "${name}" "${value}"
  [[ "${value}" != *[[:space:]\"\\]* ]] || \
    die "${name} contains whitespace, quotes, or backslashes"
}

json_escape() {
  local value="$1"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\t'/\\t}
  value=${value//$'\r'/\\r}
  value=${value//$'\n'/\\n}
  printf '%s' "${value}"
}

json_http_header_names() {
  local authorization="$1"
  local tenant="$2"
  local tenant_slot=1
  if [[ -n "${authorization}" ]]; then
    printf ',"httpHeaderName1":"Authorization"'
    tenant_slot=2
  fi
  if [[ -n "${tenant}" ]]; then
    printf ',"httpHeaderName%s":"X-Scope-OrgID"' "${tenant_slot}"
  fi
}

json_http_header_values() {
  local authorization="$1"
  local tenant="$2"
  local tenant_slot=1
  local separator=""
  if [[ -n "${authorization}" ]]; then
    printf '"httpHeaderValue1":"%s"' "$(json_escape "${authorization}")"
    separator=","
    tenant_slot=2
  fi
  if [[ -n "${tenant}" ]]; then
    printf '%s"httpHeaderValue%s":"%s"' "${separator}" "${tenant_slot}" "$(json_escape "${tenant}")"
  fi
}

yaml_dquote() {
  local value="$1"
  # Grafana expands $VAR in provisioning files; $$ preserves a literal dollar.
  value="${value//\$/\$\$}"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  printf '"%s"' "${value}"
}

require_bool() {
  local name="$1"
  local value="$2"
  [[ "${value}" == "true" || "${value}" == "false" ]] || die "${name} must be true or false"
}

metrics_authorization() {
  if [[ -n "${CUBE_METRICS_AUTHORIZATION:-}" ]]; then
    printf '%s' "${CUBE_METRICS_AUTHORIZATION}"
  elif [[ -n "${CUBE_METRICS_REMOTE_WRITE_TOKEN:-}" ]]; then
    printf 'Bearer %s' "${CUBE_METRICS_REMOTE_WRITE_TOKEN}"
  fi
}

logs_authorization() {
  if [[ -n "${CUBE_LOGS_AUTHORIZATION:-}" ]]; then
    printf '%s' "${CUBE_LOGS_AUTHORIZATION}"
  elif [[ -n "${CUBE_LOGS_TOKEN:-}" ]]; then
    printf 'Bearer %s' "${CUBE_LOGS_TOKEN}"
  fi
}

listen_address() {
  printf '%s' "${CUBE_GRAFANA_LISTEN_ADDRESS:-${CUBE_GRAFANA_LISTEN_ADDR:-0.0.0.0:3001}}"
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
  if [[ -n "${CUBE_GRAFANA_URL:-}" ]]; then
    printf '%s' "${CUBE_GRAFANA_URL%/}"
    return
  fi
  local host
  host="$(listen_host)"
  case "${host}" in
    0.0.0.0) host="127.0.0.1" ;;
  esac
  printf 'http://%s:%s' "${host}" "$(listen_port)"
}

prometheus_url() {
  printf '%s' "${CUBE_GRAFANA_PROMETHEUS_URL:-${CUBE_PROMETHEUS_URL:-http://127.0.0.1:9092}}"
}

logs_backend() {
  local backend="${CUBE_LOGS_BACKEND:-}"
  if [[ -z "${backend}" && -n "${CUBE_GRAFANA_ES_URL:-${CUBE_LOGS_ES_ENDPOINT:-}}" ]]; then
    backend="elasticsearch"
  elif [[ -z "${backend}" &&
          -f /etc/systemd/system/cube-sandbox-elasticsearch.service &&
          ! -f /etc/systemd/system/cube-sandbox-loki.service ]]; then
    backend="elasticsearch"
  fi
  case "${backend:-loki}" in
    loki) printf 'loki\n' ;;
    elasticsearch|es) printf 'elasticsearch\n' ;;
    *) die "unsupported CUBE_LOGS_BACKEND: ${backend} (expected loki|elasticsearch)" ;;
  esac
}

elasticsearch_url() {
  printf '%s' "${CUBE_GRAFANA_ES_URL:-${CUBE_LOGS_ES_ENDPOINT:-http://127.0.0.1:9200}}"
}

elasticsearch_index() {
  printf '%s' "${CUBE_GRAFANA_ES_INDEX:-${CUBE_LOGS_ES_INDEX:-cubesandbox-logs}* }" | tr -d '[:space:]'
}

loki_url() {
  local value
  value="${CUBE_GRAFANA_LOKI_URL:-${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-http://127.0.0.1:3100}}"
  value="${value%/}"
  value="${value%/otlp}"
  printf '%s' "${value}"
}

logs_dashboard_file() {
  if [[ "$(logs_backend)" == "elasticsearch" ]]; then
    printf '%s/cubesandbox-logs-elasticsearch.json' "${DASHBOARD_DIR}"
  else
    printf '%s/cubesandbox-logs.json' "${DASHBOARD_DIR}"
  fi
}

arch_name() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

grafana_checksum() {
  local arch="$1"
  if [[ -n "${CUBE_GRAFANA_TARBALL_SHA256:-}" ]]; then
    printf '%s\n' "${CUBE_GRAFANA_TARBALL_SHA256}"
    return
  fi
  case "${GRAFANA_VERSION}/${arch}" in
    10.4.18/amd64) printf '%s\n' "3573c833c322a0cade70f73859107e0d6b9470d322975f9c7a9d62d738361f3b" ;;
    10.4.18/arm64) printf '%s\n' "c33e7a878878dc3a25ec44e97764050ce2c3eee10e8e6d9191109b89127c7b5a" ;;
    *) die "no bundled checksum for Grafana ${GRAFANA_VERSION}/${arch}; set CUBE_GRAFANA_TARBALL_SHA256" ;;
  esac
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${file}" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

verify_checksum() {
  local file="$1"
  local expected="$2"
  local actual
  actual="$(sha256_file "${file}")"
  [[ "${actual}" == "${expected}" ]] || \
    die "checksum mismatch for $(basename "${file}"): expected ${expected}, got ${actual}"
}

append_http_header_names_yaml() {
  local destination="$1"
  local authorization="$2"
  local tenant="$3"
  local tenant_slot=1
  if [[ -n "${authorization}" ]]; then
    printf '      httpHeaderName1: "Authorization"\n' >> "${destination}"
    tenant_slot=2
  fi
  if [[ -n "${tenant}" ]]; then
    printf '      httpHeaderName%s: "X-Scope-OrgID"\n' "${tenant_slot}" >> "${destination}"
  fi
}

append_http_header_values_yaml() {
  local destination="$1"
  local authorization="$2"
  local tenant="$3"
  local tenant_slot=1
  if [[ -n "${authorization}" ]]; then
    printf '      httpHeaderValue1: %s\n' "$(yaml_dquote "${authorization}")" >> "${destination}"
    tenant_slot=2
  fi
  if [[ -n "${tenant}" ]]; then
    printf '      httpHeaderValue%s: %s\n' "${tenant_slot}" "$(yaml_dquote "${tenant}")" >> "${destination}"
  fi
}

require_config() {
  local address port
  address="$(listen_address)"
  require_single_line "CUBE_GRAFANA_LISTEN_ADDRESS" "${address}"
  require_single_line "CUBE_GRAFANA_ADMIN_USER" "${CUBE_GRAFANA_ADMIN_USER:-admin}"
  require_single_line "CUBE_GRAFANA_ADMIN_PASSWORD" "${CUBE_GRAFANA_ADMIN_PASSWORD:-}"
  [[ "${CUBE_GRAFANA_ADMIN_USER:-admin}" =~ ^[A-Za-z0-9._@-]+$ ]] || \
    die "CUBE_GRAFANA_ADMIN_USER may contain only letters, digits, '.', '_', '@' and '-'"
  [[ -n "${CUBE_GRAFANA_ADMIN_PASSWORD:-}" ]] || \
    die "set CUBE_GRAFANA_ADMIN_PASSWORD to a random value of at least 16 characters before installing Grafana"
  [[ "${CUBE_GRAFANA_ADMIN_PASSWORD}" != "admin" && ${#CUBE_GRAFANA_ADMIN_PASSWORD} -ge 16 ]] || \
    die "CUBE_GRAFANA_ADMIN_PASSWORD must be a non-default value of at least 16 characters"
  # Grafana's $__file{} expander trims surrounding whitespace.
  [[ "${CUBE_GRAFANA_ADMIN_PASSWORD}" != [[:space:]]* && "${CUBE_GRAFANA_ADMIN_PASSWORD}" != *[[:space:]] ]] || \
    die "CUBE_GRAFANA_ADMIN_PASSWORD must not start or end with whitespace"
  [[ "${address}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}:[0-9]+$ ]] || \
    die "CUBE_GRAFANA_LISTEN_ADDRESS must be an IPv4 address and port (e.g. 0.0.0.0:3001)"
  port="${address##*:}"
  [[ "${port}" -ge 1 && "${port}" -le 65535 ]] || die "Grafana listen port must be between 1 and 65535"
  require_http_url "Prometheus datasource URL" "$(prometheus_url)"
  if [[ "$(logs_backend)" == "loki" ]]; then
    require_http_url "Loki datasource URL" "$(loki_url)"
  else
    require_http_url "Elasticsearch datasource URL" "$(elasticsearch_url)"
  fi
  if [[ -n "${CUBE_METRICS_AUTHORIZATION:-}" && -n "${CUBE_METRICS_REMOTE_WRITE_TOKEN:-}" ]]; then
    die "set only one of CUBE_METRICS_AUTHORIZATION and CUBE_METRICS_REMOTE_WRITE_TOKEN"
  fi
  if [[ -n "${CUBE_LOGS_AUTHORIZATION:-}" && -n "${CUBE_LOGS_TOKEN:-}" ]]; then
    die "set only one of CUBE_LOGS_AUTHORIZATION and CUBE_LOGS_TOKEN"
  fi
  require_single_line "CUBE_METRICS_AUTHORIZATION" "${CUBE_METRICS_AUTHORIZATION:-}"
  require_single_line "CUBE_METRICS_REMOTE_WRITE_TOKEN" "${CUBE_METRICS_REMOTE_WRITE_TOKEN:-}"
  require_single_line "CUBE_METRICS_TENANT_ID" "${CUBE_METRICS_TENANT_ID:-}"
  require_single_line "CUBE_LOGS_AUTHORIZATION" "${CUBE_LOGS_AUTHORIZATION:-}"
  require_single_line "CUBE_LOGS_TOKEN" "${CUBE_LOGS_TOKEN:-}"
  require_single_line "CUBE_LOGS_TENANT_ID" "${CUBE_LOGS_TENANT_ID:-}"
  require_single_line "CUBE_LOGS_ES_USER" "${CUBE_LOGS_ES_USER:-}"
  require_single_line "CUBE_LOGS_ES_PASSWORD" "${CUBE_LOGS_ES_PASSWORD:-}"
  require_single_line "CUBE_LOGS_ES_API_KEY" "${CUBE_LOGS_ES_API_KEY:-}"
  require_bool "CUBE_LOGS_ES_TLS_INSECURE_SKIP_VERIFY" "${CUBE_LOGS_ES_TLS_INSECURE_SKIP_VERIFY:-false}"
  if [[ -n "${CUBE_LOGS_ES_API_KEY:-}" &&
        ( -n "${CUBE_LOGS_ES_USER:-}" || -n "${CUBE_LOGS_ES_PASSWORD:-}" ) ]]; then
    die "set either CUBE_LOGS_ES_API_KEY or CUBE_LOGS_ES_USER/CUBE_LOGS_ES_PASSWORD, not both"
  fi
  if [[ -n "${CUBE_LOGS_ES_USER:-}" && -z "${CUBE_LOGS_ES_PASSWORD:-}" ]] ||
     [[ -z "${CUBE_LOGS_ES_USER:-}" && -n "${CUBE_LOGS_ES_PASSWORD:-}" ]]; then
    die "CUBE_LOGS_ES_USER and CUBE_LOGS_ES_PASSWORD must be set together"
  fi
  if [[ "$(logs_backend)" == "elasticsearch" &&
        -n "$(logs_authorization)" &&
        ( -n "${CUBE_LOGS_ES_USER:-}" || -n "${CUBE_LOGS_ES_API_KEY:-}" ) ]]; then
    die "set either generic logs authorization or Elasticsearch-specific credentials, not both"
  fi
  local dashboard
  for dashboard in cubesandbox-cluster cubesandbox-node cubesandbox-sandbox; do
    [[ -f "${DASHBOARD_DIR}/${dashboard}.json" ]] || die "missing ${DASHBOARD_DIR}/${dashboard}.json"
  done
  [[ -f "${DASHBOARD_DIR}/cubesandbox-logs.json" ]] || die "missing ${DASHBOARD_DIR}/cubesandbox-logs.json"
  [[ -f "${DASHBOARD_DIR}/cubesandbox-logs-elasticsearch.json" ]] || die "missing ${DASHBOARD_DIR}/cubesandbox-logs-elasticsearch.json"
}

ensure_user() {
  if ! getent group "${GRAFANA_GROUP}" >/dev/null; then
    groupadd --system "${GRAFANA_GROUP}"
  fi
  if ! id -u "${GRAFANA_USER}" >/dev/null 2>&1; then
    useradd --system --gid "${GRAFANA_GROUP}" --home-dir "${STATE_DIR}" \
      --shell /sbin/nologin "${GRAFANA_USER}"
  fi
}

install_binaries() {
  if [[ -x "${INSTALL_DIR}/bin/grafana" ]] &&
    "${INSTALL_DIR}/bin/grafana" --version 2>/dev/null | grep -q "${GRAFANA_VERSION}"; then
    log "Grafana ${GRAFANA_VERSION} already installed at ${INSTALL_DIR}"
    return
  fi

  local arch asset url tmp expected_sha256
  arch="$(arch_name)"
  asset="grafana-${GRAFANA_VERSION}.linux-${arch}.tar.gz"
  url="${CUBE_GRAFANA_DOWNLOAD_URL:-https://dl.grafana.com/oss/release/${asset}}"
  expected_sha256="$(grafana_checksum "${arch}")"
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp:-}"' RETURN

  if [[ -n "${CUBE_GRAFANA_TARBALL:-}" ]]; then
    [[ -f "${CUBE_GRAFANA_TARBALL}" ]] || die "CUBE_GRAFANA_TARBALL not found: ${CUBE_GRAFANA_TARBALL}"
    log "installing from local tarball ${CUBE_GRAFANA_TARBALL}"
    cp -f "${CUBE_GRAFANA_TARBALL}" "${tmp}/${asset}"
  else
    log "downloading ${url}"
    curl -fsSL "${url}" -o "${tmp}/${asset}"
  fi
  verify_checksum "${tmp}/${asset}" "${expected_sha256}"

  rm -rf "${INSTALL_DIR}"
  mkdir -p "${INSTALL_DIR}"
  tar -C "${INSTALL_DIR}" --strip-components=1 -xzf "${tmp}/${asset}"
  chown -R "${GRAFANA_USER}:${GRAFANA_GROUP}" "${INSTALL_DIR}"
  rm -rf "${tmp}"
  trap - RETURN
  log "installed Grafana ${GRAFANA_VERSION} to ${INSTALL_DIR}"
}

write_admin_password_file() {
  local destination="$1"
  local old_umask
  mkdir -p "$(dirname "${destination}")"
  old_umask="$(umask)"
  umask 077
  printf '%s' "${CUBE_GRAFANA_ADMIN_PASSWORD}" > "${destination}"
  umask "${old_umask}"
  chmod 0600 "${destination}"
}

# The admin password is never written into grafana.ini: unquoted INI values are
# cut at '#' or ';', and an empty result makes Grafana fall back to the
# defaults.ini password "admin". $__file{} reads the raw bytes instead.
write_grafana_ini() {
  local destination="$1"
  local password_file="$2"
  local admin_user="${CUBE_GRAFANA_ADMIN_USER:-admin}"
  [[ "${password_file}" == /* && "${password_file}" != *[\#\;\}[:cntrl:]]* ]] || \
    die "Grafana admin password file path must be absolute and must not contain '#', ';', '}' or control characters: ${password_file}"
  mkdir -p "$(dirname "${destination}")"
  cat > "${destination}" <<EOF
[paths]
data = ${STATE_DIR}
logs = ${LOG_DIR}
plugins = ${STATE_DIR}/plugins
provisioning = ${PROVISIONING_DIR}

[server]
http_addr = $(listen_host)
http_port = $(listen_port)

[security]
admin_user = ${admin_user}
admin_password = \$__file{${password_file}}

[users]
allow_sign_up = false

[analytics]
reporting_enabled = false
check_for_updates = false
EOF
}

write_provisioning_files() {
  local backend prom_url loki_query_url es_url es_idx datasource_file
  local metrics_auth metrics_tenant logs_auth logs_tenant es_header_auth
  local es_user es_password es_api_key es_tls_skip
  backend="$(logs_backend)"
  prom_url="$(prometheus_url)"
  loki_query_url="$(loki_url)"
  es_url="$(elasticsearch_url)"
  es_idx="$(elasticsearch_index)"
  metrics_auth="$(metrics_authorization)"
  metrics_tenant="${CUBE_METRICS_TENANT_ID:-}"
  logs_auth="$(logs_authorization)"
  logs_tenant="${CUBE_LOGS_TENANT_ID:-}"
  es_user="${CUBE_LOGS_ES_USER:-}"
  es_password="${CUBE_LOGS_ES_PASSWORD:-}"
  es_api_key="${CUBE_LOGS_ES_API_KEY:-}"
  es_tls_skip="${CUBE_LOGS_ES_TLS_INSECURE_SKIP_VERIFY:-false}"
  es_header_auth="${logs_auth}"
  if [[ -n "${es_api_key}" ]]; then
    es_header_auth="ApiKey ${es_api_key}"
  fi

  mkdir -p "${PROVISIONING_DIR}/datasources" "${PROVISIONING_DIR}/dashboards" "${PROVISIONED_DASHBOARDS_DIR}"
  datasource_file="${PROVISIONING_DIR}/datasources/cubesandbox.yaml"

  cat > "${datasource_file}" <<EOF
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    uid: prometheus
    access: proxy
    url: $(yaml_dquote "${prom_url}")
    isDefault: true
    editable: true
    jsonData:
      timeInterval: "5s"
EOF
  append_http_header_names_yaml "${datasource_file}" "${metrics_auth}" "${metrics_tenant}"
  if [[ -n "${metrics_auth}" || -n "${metrics_tenant}" ]]; then
    printf '    secureJsonData:\n' >> "${datasource_file}"
    append_http_header_values_yaml "${datasource_file}" "${metrics_auth}" "${metrics_tenant}"
  fi

  if [[ "${backend}" == "loki" ]]; then
    cat >> "${datasource_file}" <<EOF
  - name: Loki
    type: loki
    uid: loki
    access: proxy
    url: $(yaml_dquote "${loki_query_url}")
    editable: true
    jsonData:
      derivedFields: []
EOF
    append_http_header_names_yaml "${datasource_file}" "${logs_auth}" "${logs_tenant}"
    if [[ -n "${logs_auth}" || -n "${logs_tenant}" ]]; then
      printf '    secureJsonData:\n' >> "${datasource_file}"
      append_http_header_values_yaml "${datasource_file}" "${logs_auth}" "${logs_tenant}"
    fi
  else
    cat >> "${datasource_file}" <<EOF
  - name: Elasticsearch
    type: elasticsearch
    uid: elasticsearch
    access: proxy
    url: $(yaml_dquote "${es_url}")
    database: $(yaml_dquote "${es_idx}")
    editable: true
EOF
    if [[ -n "${es_user}" ]]; then
      cat >> "${datasource_file}" <<EOF
    basicAuth: true
    basicAuthUser: $(yaml_dquote "${es_user}")
EOF
    fi
    cat >> "${datasource_file}" <<EOF
    jsonData:
      index: $(yaml_dquote "${es_idx}")
      timeField: "@timestamp"
      esVersion: "8.0.0"
      logMessageField: "Body"
      logLevelField: "SeverityText"
EOF
    if [[ "${es_tls_skip}" == "true" ]]; then
      printf '      tlsSkipVerify: true\n' >> "${datasource_file}"
    fi
    append_http_header_names_yaml "${datasource_file}" "${es_header_auth}" "${logs_tenant}"
    if [[ -n "${es_header_auth}" || -n "${logs_tenant}" || -n "${es_password}" ]]; then
      printf '    secureJsonData:\n' >> "${datasource_file}"
      append_http_header_values_yaml "${datasource_file}" "${es_header_auth}" "${logs_tenant}"
      if [[ -n "${es_password}" ]]; then
        printf '      basicAuthPassword: %s\n' "$(yaml_dquote "${es_password}")" >> "${datasource_file}"
      fi
    fi
  fi

  cat > "${PROVISIONING_DIR}/dashboards/cubesandbox.yaml" <<EOF
apiVersion: 1
providers:
  - name: CubeSandbox
    orgId: 1
    folder: "CubeSandbox"
    type: file
    disableDeletion: false
    editable: true
    updateIntervalSeconds: 30
    allowUiUpdates: true
    options:
      path: ${PROVISIONED_DASHBOARDS_DIR}
      foldersFromFilesStructure: false
EOF

  local dashboard
  for dashboard in cubesandbox-cluster cubesandbox-node cubesandbox-sandbox; do
    cp -f "${DASHBOARD_DIR}/${dashboard}.json" "${PROVISIONED_DASHBOARDS_DIR}/${dashboard}.json"
  done
  rm -f "${PROVISIONED_DASHBOARDS_DIR}/cubesandbox-host-sandbox.json"
  cp -f "$(logs_dashboard_file)" "${PROVISIONED_DASHBOARDS_DIR}/cubesandbox-logs.json"
}

write_unit() {
  local destination="$1"
  cat > "${destination}" <<EOF
[Unit]
Description=CubeSandbox Grafana visualization server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${GRAFANA_USER}
Group=${GRAFANA_GROUP}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/bin/grafana server \\
  --homepath=${INSTALL_DIR} \\
  --config=${CONFIG_DIR}/grafana.ini
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
}

install_service() {
  require_root
  require_config
  ensure_user
  install_binaries

  # Shared /etc/cube-sandbox must stay traversable by every service user.
  install -d -m 0755 -o root -g root "$(dirname "${CONFIG_DIR}")"
  install -d -m 0750 -o "${GRAFANA_USER}" -g "${GRAFANA_GROUP}" \
    "${CONFIG_DIR}" "${PROVISIONING_DIR}" "${STATE_DIR}" "${PROVISIONED_DASHBOARDS_DIR}" "${LOG_DIR}"
  write_admin_password_file "${ADMIN_PASSWORD_FILE}"
  write_grafana_ini "${CONFIG_DIR}/grafana.ini" "${ADMIN_PASSWORD_FILE}"
  write_provisioning_files
  chown -R "${GRAFANA_USER}:${GRAFANA_GROUP}" "${CONFIG_DIR}" "${STATE_DIR}" "${LOG_DIR}"

  write_unit "${UNIT_PATH}"
  systemctl daemon-reload
  systemctl enable "${UNIT_NAME}"
  systemctl restart "${UNIT_NAME}"

  local i ready="false"
  for i in $(seq 1 30); do
    if curl -fsS --max-time 3 "$(health_url)/api/health" >/dev/null 2>&1; then
      ready="true"
      break
    fi
    sleep 1
  done
  [[ "${ready}" == "true" ]] || die "Grafana failed to start; inspect journalctl -u ${UNIT_NAME}"

  log "Grafana ${GRAFANA_VERSION} is ready at $(health_url)"
  log "Cluster metrics: $(health_url)/d/cubesandbox-cluster"
  log "Node metrics:    $(health_url)/d/cubesandbox-node"
  log "Sandbox metrics: $(health_url)/d/cubesandbox-sandbox"
  log "Logs dashboard:    $(health_url)/d/cubesandbox-logs/cubesandbox-logs"
}

upsert_datasource_api() {
  local target_url="$1"
  local uid="$2"
  local payload="$3"
  shift 3
  local -a auth_args=("$@")
  local status method endpoint

  status="$(curl -sS -o /dev/null -w '%{http_code}' "${auth_args[@]}" \
    "${target_url}/api/datasources/uid/${uid}")"
  case "${status}" in
    200)
      method="PUT"
      endpoint="${target_url}/api/datasources/uid/${uid}"
      ;;
    404)
      method="POST"
      endpoint="${target_url}/api/datasources"
      ;;
    *)
      die "failed to inspect Grafana datasource ${uid}: HTTP ${status}"
      ;;
  esac

  curl -fsS "${auth_args[@]}" -X "${method}" "${endpoint}" \
    -H "Content-Type: application/json" --data-binary @"${payload}" >/dev/null
  log "upserted Grafana datasource ${uid} on ${target_url}"
}

import_dashboards_api() {
  require_config

  local target_url="${GRAFANA_URL:-$(health_url)}"
  target_url="${target_url%/}"
  require_http_url "Grafana API URL" "${target_url}"
  local auth_user="${GRAFANA_USER_AUTH:-${CUBE_GRAFANA_ADMIN_USER:-admin}}"
  local auth_pass="${GRAFANA_PASSWORD_AUTH:-${CUBE_GRAFANA_ADMIN_PASSWORD:-admin}}"
  require_single_line "Grafana API username" "${auth_user}"
  require_single_line "Grafana API password" "${auth_pass}"
  local -a auth_args=()
  if [[ -n "${GRAFANA_API_KEY:-}" ]]; then
    require_single_line "GRAFANA_API_KEY" "${GRAFANA_API_KEY}"
    auth_args=(-H "Authorization: Bearer ${GRAFANA_API_KEY}")
  else
    auth_args=(-u "${auth_user}:${auth_pass}")
  fi

  local metrics_auth
  local metrics_tenant="${CUBE_METRICS_TENANT_ID:-}"
  local logs_auth logs_tenant es_header_auth
  metrics_auth="$(metrics_authorization)"
  logs_auth="$(logs_authorization)"
  logs_tenant="${CUBE_LOGS_TENANT_ID:-}"
  es_header_auth="${logs_auth}"
  if [[ -n "${CUBE_LOGS_ES_API_KEY:-}" ]]; then
    es_header_auth="ApiKey ${CUBE_LOGS_ES_API_KEY}"
  fi

  local datasource_payload
  datasource_payload="$(mktemp)"

  if [[ -n "${CUBE_GRAFANA_PROMETHEUS_URL:-${CUBE_PROMETHEUS_URL:-}}" ]]; then
    local prom_url
    prom_url="$(prometheus_url)"
    cat > "${datasource_payload}" <<EOF
{"name":"Prometheus","type":"prometheus","uid":"prometheus","access":"proxy",
 "url":"$(json_escape "${prom_url}")","isDefault":true,
 "jsonData":{"timeInterval":"5s"$(json_http_header_names "${metrics_auth}" "${metrics_tenant}")},
 "secureJsonData":{$(json_http_header_values "${metrics_auth}" "${metrics_tenant}")}}
EOF
    upsert_datasource_api "${target_url}" "prometheus" "${datasource_payload}" "${auth_args[@]}"
  fi

  if [[ "$(logs_backend)" == "loki" ]]; then
    local loki_query_url
    loki_query_url="$(loki_url)"
    cat > "${datasource_payload}" <<EOF
{"name":"Loki","type":"loki","uid":"loki","access":"proxy",
 "url":"$(json_escape "${loki_query_url}")",
 "jsonData":{"derivedFields":[]$(json_http_header_names "${logs_auth}" "${logs_tenant}")},
 "secureJsonData":{$(json_http_header_values "${logs_auth}" "${logs_tenant}")}}
EOF
    upsert_datasource_api "${target_url}" "loki" "${datasource_payload}" "${auth_args[@]}"
  else
    local es_url es_idx
    local es_basic_json="" es_secure_json
    es_url="$(elasticsearch_url)"
    es_idx="$(elasticsearch_index)"
    if [[ -n "${CUBE_LOGS_ES_USER:-}" ]]; then
      es_basic_json=",\"basicAuth\":true,\"basicAuthUser\":\"$(json_escape "${CUBE_LOGS_ES_USER}")\""
    fi
    es_secure_json="$(json_http_header_values "${es_header_auth}" "${logs_tenant}")"
    if [[ -n "${CUBE_LOGS_ES_PASSWORD:-}" ]]; then
      [[ -z "${es_secure_json}" ]] || es_secure_json+=","
      es_secure_json+="\"basicAuthPassword\":\"$(json_escape "${CUBE_LOGS_ES_PASSWORD}")\""
    fi
    cat > "${datasource_payload}" <<EOF
{"name":"Elasticsearch","type":"elasticsearch","uid":"elasticsearch","access":"proxy",
 "url":"$(json_escape "${es_url}")","database":"$(json_escape "${es_idx}")"${es_basic_json},
 "jsonData":{"index":"$(json_escape "${es_idx}")","timeField":"@timestamp",
 "esVersion":"8.0.0","logMessageField":"Body","logLevelField":"SeverityText",
 "tlsSkipVerify":${CUBE_LOGS_ES_TLS_INSECURE_SKIP_VERIFY:-false}$(json_http_header_names "${es_header_auth}" "${logs_tenant}")},
 "secureJsonData":{${es_secure_json}}}
EOF
    upsert_datasource_api "${target_url}" "elasticsearch" "${datasource_payload}" "${auth_args[@]}"
  fi

  local dash_file payload
  for dash_file in "${DASHBOARD_DIR}/cubesandbox-cluster.json" "${DASHBOARD_DIR}/cubesandbox-node.json" "${DASHBOARD_DIR}/cubesandbox-sandbox.json" "$(logs_dashboard_file)"; do
    payload="$(mktemp)"
    printf '{"dashboard":' > "${payload}"
    cat "${dash_file}" >> "${payload}"
    printf ',"overwrite":true,"folderId":0}\n' >> "${payload}"
    curl -fsS "${auth_args[@]}" -X POST "${target_url}/api/dashboards/db" \
      -H "Content-Type: application/json" \
      --data-binary @"${payload}" >/dev/null
    rm -f "${payload}"
    log "imported $(basename "${dash_file}") into ${target_url}"
  done

  log "Cluster metrics: ${target_url}/d/cubesandbox-cluster"
  log "Node metrics:    ${target_url}/d/cubesandbox-node"
  log "Sandbox metrics: ${target_url}/d/cubesandbox-sandbox"
  log "Logs dashboard:    ${target_url}/d/cubesandbox-logs/cubesandbox-logs"
  rm -f "${datasource_payload}"
}

render_config() {
  require_config
  local output_dir="${CUBE_GRAFANA_CONFIG_OUTPUT_DIR:-${PWD}/grafana-provisioning}"
  umask 077
  mkdir -p "${output_dir}"
  chmod 0700 "${output_dir}"
  PROVISIONING_DIR="${output_dir}/provisioning"
  PROVISIONED_DASHBOARDS_DIR="${output_dir}/dashboards"
  write_admin_password_file "${output_dir}/admin_password"
  write_grafana_ini "${output_dir}/grafana.ini" "${output_dir}/admin_password"
  chmod 0600 "${output_dir}/grafana.ini"
  write_provisioning_files
  log "wrote Grafana provisioning and dashboards to ${output_dir}"
}

uninstall_service() {
  require_root
  systemctl disable --now "${UNIT_NAME}" 2>/dev/null || true
  rm -f "${UNIT_PATH}" "${CONFIG_DIR}/grafana.ini" "${ADMIN_PASSWORD_FILE}"
  rm -rf "${PROVISIONING_DIR}"
  systemctl daemon-reload
  log "removed ${UNIT_NAME}; retained ${INSTALL_DIR} and ${STATE_DIR}"
}

case "${1:-install}" in
  install) install_service ;;
  dashboards|import) import_dashboards_api ;;
  render) render_config ;;
  uninstall) uninstall_service ;;
  *) die "usage: $0 [install|dashboards|render|uninstall]" ;;
esac
