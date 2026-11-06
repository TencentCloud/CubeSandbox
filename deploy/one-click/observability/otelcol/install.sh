#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# one-click/systemd Collector for CVM nodes. See docs/guide/observability.md.
# Metrics and logs are independent opt-in pipelines. The script does not
# change Cubelet configuration.
set -euo pipefail

OTELCOL_VERSION="0.162.0"
TOOLBOX_ROOT="/usr/local/services/cubetoolbox"
ENV_FILE="${ONE_CLICK_RUNTIME_ENV_FILE:-${TOOLBOX_ROOT}/.one-click.env}"
# /etc/cube-sandbox is shared with the backend services, which run as their own
# users; keep it 0755 and restrict only this component's subdirectory.
SHARED_CONFIG_ROOT="/etc/cube-sandbox"
CONFIG_DIR="${SHARED_CONFIG_ROOT}/otelcol"
CONFIG_PATH="${CONFIG_DIR}/otelcol.yaml"
LEGACY_CONFIG_PATH="${SHARED_CONFIG_ROOT}/otelcol.yaml"
STATE_DIR="/var/lib/cube-sandbox/otelcol"
PROM_WAL_DIR="${STATE_DIR}/prom_rw"
OTEL_STORAGE_DIR="${STATE_DIR}/storage"
BIN_PATH="/usr/local/bin/otelcol-contrib"
UNIT_NAME="cube-sandbox-otelcol.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"
CUBELET_METRICS_URL="http://127.0.0.1:9998/v1/metrics/resource"

log() { echo "[cube-otelcol] $*" >&2; }
die() { echo "[cube-otelcol] ERROR: $*" >&2; exit 1; }

yaml_dquote() {
  local value="$1"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  printf '"%s"' "${value}"
}

require_safe_label() {
  local name="$1"
  local value="$2"
  [[ "${value}" =~ ^[A-Za-z0-9_.:@/-]+$ ]] || die "${name} contains characters that cannot be written into the collector config"
}

require_safe_sandbox_glob() {
  local name="$1"
  local value="$2"
  [[ "${value}" == "*" || "${value}" =~ ^[A-Za-z0-9_.-]+$ ]] || \
    die "${name} must be '*' or a valid sandbox ID ([A-Za-z0-9_.-]+)"
}

require_single_line() {
  local name="$1"
  local value="$2"
  [[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || die "${name} must be a single line"
}

require_bool() {
  local name="$1"
  local value="$2"
  case "${value}" in
    true|false) ;;
    *) die "${name} must be true or false" ;;
  esac
}

require_start_at() {
  local name="$1"
  local value="$2"
  case "${value}" in
    beginning|end) ;;
    *) die "${name} must be beginning or end" ;;
  esac
}

load_one_click_env() {
  if [[ -f "${ENV_FILE}" ]]; then
    # Values passed by the current installer invocation must win over the
    # persisted one-click runtime file. Snapshot all existing CUBE_* variables,
    # load defaults from disk, then restore the caller's explicit values.
    local key i had_nounset=0
    local caller_es_endpoint_set=0
    local caller_logs_otlp_endpoint_set=0
    local -a caller_keys=()
    local -a caller_values=()
    if [[ -n "${CUBE_LOGS_ES_ENDPOINT+x}" ||
          -n "${CUBE_LOGS_ELASTICSEARCH_ENDPOINT+x}" ]]; then
      caller_es_endpoint_set=1
    fi
    [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT+x}" ]] && caller_logs_otlp_endpoint_set=1
    while IFS= read -r key; do
      caller_keys+=("${key}")
      caller_values+=("${!key}")
    done < <(compgen -A variable CUBE_)

    # shellcheck disable=SC1090
    [[ $- == *u* ]] && had_nounset=1
    set +u
    set -a
    source "${ENV_FILE}"
    set +a
    if [[ "${had_nounset}" == "1" ]]; then
      set -u
    fi
    for i in "${!caller_keys[@]}"; do
      printf -v "${caller_keys[${i}]}" '%s' "${caller_values[${i}]}"
      export "${caller_keys[${i}]}"
    done
    if [[ "${caller_logs_otlp_endpoint_set}" == "1" && "${caller_es_endpoint_set}" == "0" ]]; then
      unset CUBE_LOGS_ES_ENDPOINT CUBE_LOGS_ELASTICSEARCH_ENDPOINT
    elif [[ "${caller_es_endpoint_set}" == "1" && "${caller_logs_otlp_endpoint_set}" == "0" ]]; then
      unset CUBE_LOGS_OTLP_HTTP_ENDPOINT
    fi
    log "loaded ${ENV_FILE}"
  fi
}

node_ip() {
  if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
    printf '%s\n' "${CUBE_SANDBOX_NODE_IP}"
    return
  fi
  if command -v ip >/dev/null 2>&1; then
    ip -4 route get 1.1.1.1 2>/dev/null | awk '{
      for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }
    }' || true
  fi
}

require_http_url() {
  local name="$1"
  local value="$2"
  [[ "${value}" == http://* || "${value}" == https://* ]] || die "${name} must start with http:// or https://"
  require_single_line "${name}" "${value}"
}

require_install_config() {
  local es_endpoint="${CUBE_LOGS_ES_ENDPOINT:-${CUBE_LOGS_ELASTICSEARCH_ENDPOINT:-}}"
  [[ -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" || -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" || -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" || -n "${es_endpoint}" ]] || \
    die "set CUBE_METRICS_OTLP_HTTP_ENDPOINT, CUBE_METRICS_REMOTE_WRITE_URL, CUBE_LOGS_ES_ENDPOINT, CUBE_LOGS_OTLP_HTTP_ENDPOINT, or combine metrics + logs"
  if [[ -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" && -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    die "set only one of CUBE_METRICS_OTLP_HTTP_ENDPOINT and CUBE_METRICS_REMOTE_WRITE_URL to avoid duplicate metric ingestion"
  fi
  if [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" && -n "${es_endpoint}" ]]; then
    die "set only one of CUBE_LOGS_ES_ENDPOINT and CUBE_LOGS_OTLP_HTTP_ENDPOINT"
  fi
  local cluster
  cluster="$(cluster_id)"
  require_safe_label "CUBE_OBSERVABILITY_CLUSTER_ID" "${cluster}"
  if [[ -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" ]]; then
    require_http_url "CUBE_METRICS_REMOTE_WRITE_URL" "${CUBE_METRICS_REMOTE_WRITE_URL}"
  fi
  if [[ -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    require_http_url "CUBE_METRICS_OTLP_HTTP_ENDPOINT" "${CUBE_METRICS_OTLP_HTTP_ENDPOINT}"
    case "${CUBE_METRICS_OTLP_HTTP_ENDPOINT%/}" in
      */v1/metrics) die "CUBE_METRICS_OTLP_HTTP_ENDPOINT must be the base endpoint (e.g. http://<prom>:9092/api/v1/otlp); remove /v1/metrics" ;;
    esac
  fi
  if [[ -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" || -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    if [[ -n "${CUBE_METRICS_AUTHORIZATION:-}" && -n "${CUBE_METRICS_REMOTE_WRITE_TOKEN:-}" ]]; then
      die "set only one of CUBE_METRICS_AUTHORIZATION and CUBE_METRICS_REMOTE_WRITE_TOKEN"
    fi
    require_single_line "CUBE_METRICS_AUTHORIZATION" "${CUBE_METRICS_AUTHORIZATION:-}"
    require_single_line "CUBE_METRICS_REMOTE_WRITE_TOKEN" "${CUBE_METRICS_REMOTE_WRITE_TOKEN:-}"
    require_single_line "CUBE_METRICS_TENANT_ID" "${CUBE_METRICS_TENANT_ID:-}"
  fi
  if [[ -n "${es_endpoint}" ]]; then
    require_http_url "CUBE_LOGS_ES_ENDPOINT" "${es_endpoint}"
    require_safe_label "CUBE_LOGS_ES_INDEX" "${CUBE_LOGS_ES_INDEX:-cubesandbox-logs}"
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
  fi
  if [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    require_http_url "CUBE_LOGS_OTLP_HTTP_ENDPOINT" "${CUBE_LOGS_OTLP_HTTP_ENDPOINT}"
    case "${CUBE_LOGS_OTLP_HTTP_ENDPOINT%/}" in
      */v1/logs) die "CUBE_LOGS_OTLP_HTTP_ENDPOINT must be the base endpoint; remove /v1/logs" ;;
    esac
  fi
  if [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    if [[ -n "${CUBE_LOGS_AUTHORIZATION:-}" && -n "${CUBE_LOGS_TOKEN:-}" ]]; then
      die "set only one of CUBE_LOGS_AUTHORIZATION and CUBE_LOGS_TOKEN"
    fi
    require_single_line "CUBE_LOGS_AUTHORIZATION" "${CUBE_LOGS_AUTHORIZATION:-}"
    require_single_line "CUBE_LOGS_TOKEN" "${CUBE_LOGS_TOKEN:-}"
    require_single_line "CUBE_LOGS_TENANT_ID" "${CUBE_LOGS_TENANT_ID:-}"
  fi
  if [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" || -n "${es_endpoint}" ]]; then
    require_safe_sandbox_glob "CUBE_LOGS_SANDBOX_ID" "${CUBE_LOGS_SANDBOX_ID:-*}"
    require_start_at "CUBE_LOGS_FILE_START_AT" "${CUBE_LOGS_FILE_START_AT:-beginning}"
  fi
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

cluster_id() {
  printf '%s' "${CUBE_OBSERVABILITY_CLUSTER_ID:-${CUBE_METRICS_CLUSTER_ID:-cubesandbox}}"
}

check_cubelet_metrics() {
  local body http_code
  body="$(mktemp)"
  http_code="$(curl -sS --max-time 5 -o "${body}" -w '%{http_code}' "${CUBELET_METRICS_URL}" || true)"
  if [[ "${http_code}" != "200" ]]; then
    rm -f "${body}"
    die "Cubelet metrics endpoint ${CUBELET_METRICS_URL} returned ${http_code:-curl-failed}. Start cubelet before installing the collector"
  fi
  rm -f "${body}"
  log "cubelet metrics endpoint returned 200"
}

arch_name() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

install_binary() {
  if [[ -n "${CUBE_OTELCOL_BIN:-}" ]]; then
    [[ -x "${CUBE_OTELCOL_BIN}" ]] || die "CUBE_OTELCOL_BIN is not executable: ${CUBE_OTELCOL_BIN}"
    install -m 0755 "${CUBE_OTELCOL_BIN}" "${BIN_PATH}"
    return
  fi
  if [[ -x "${BIN_PATH}" ]] && "${BIN_PATH}" --version 2>/dev/null | grep -q "${OTELCOL_VERSION}"; then
    log "otelcol-contrib ${OTELCOL_VERSION} already installed"
    return
  fi
  local arch asset base url sha_url expected actual tmp
  arch="$(arch_name)"
  asset="otelcol-contrib_${OTELCOL_VERSION}_linux_${arch}.tar.gz"
  base="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${OTELCOL_VERSION}"
  url="${base}/${asset}"
  sha_url="${url}.sha256"
  tmp="$(mktemp -d)"
  log "downloading ${url}"
  curl -fsSL "${sha_url}" -o "${tmp}/sha256"
  curl -fsSL "${url}" -o "${tmp}/${asset}"
  expected="$(tr -d '[:space:]' < "${tmp}/sha256")"
  actual="$(sha256sum "${tmp}/${asset}" | awk '{print $1}')"
  [[ "${actual}" == "${expected}" ]] || die "checksum mismatch for ${asset}"
  tar -C "${tmp}" -xzf "${tmp}/${asset}"
  install -m 0755 "${tmp}/otelcol-contrib" "${BIN_PATH}"
  rm -rf "${tmp}"
  log "installed ${BIN_PATH}"
}

write_otel_config() {
  local destination="$1"
  local node_value="$2"
  local cluster metrics_url metrics_otlp_endpoint metrics_auth metrics_tenant
  local logs_endpoint es_endpoint es_index es_user es_password es_api_key es_tls_skip
  local logs_enabled logs_exporter_name logs_auth logs_tenant sandbox_glob
  local file_start_at need_file_storage
  cluster="$(cluster_id)"
  metrics_url="${CUBE_METRICS_REMOTE_WRITE_URL:-}"
  metrics_otlp_endpoint="${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}"
  metrics_otlp_endpoint="${metrics_otlp_endpoint%/}"
  metrics_auth="$(metrics_authorization)"
  metrics_tenant="${CUBE_METRICS_TENANT_ID:-}"
  logs_endpoint="${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}"
  logs_endpoint="${logs_endpoint%/}"
  es_endpoint="${CUBE_LOGS_ES_ENDPOINT:-${CUBE_LOGS_ELASTICSEARCH_ENDPOINT:-}}"
  es_endpoint="${es_endpoint%/}"
  es_index="${CUBE_LOGS_ES_INDEX:-cubesandbox-logs}"
  es_user="${CUBE_LOGS_ES_USER:-}"
  es_password="${CUBE_LOGS_ES_PASSWORD:-}"
  es_api_key="${CUBE_LOGS_ES_API_KEY:-}"
  es_tls_skip="${CUBE_LOGS_ES_TLS_INSECURE_SKIP_VERIFY:-false}"
  logs_enabled="false"
  logs_exporter_name=""
  if [[ -n "${es_endpoint}" ]]; then
    logs_enabled="true"
    logs_exporter_name="elasticsearch/logs"
  elif [[ -n "${logs_endpoint}" ]]; then
    logs_enabled="true"
    logs_exporter_name="otlp_http/logs"
  fi
  logs_auth="$(logs_authorization)"
  logs_tenant="${CUBE_LOGS_TENANT_ID:-}"
  sandbox_glob="${CUBE_LOGS_SANDBOX_ID:-*}"
  file_start_at="${CUBE_LOGS_FILE_START_AT:-beginning}"
  need_file_storage="false"
  if [[ "${logs_enabled}" == "true" || -n "${metrics_otlp_endpoint}" ]]; then
    need_file_storage="true"
  fi
  mkdir -p "$(dirname "${destination}")"
  {
    printf 'receivers:\n'
    if [[ -n "${metrics_url}" || -n "${metrics_otlp_endpoint}" ]]; then
      cat <<EOF
  prometheus:
    trim_metric_suffixes: false
    config:
      scrape_configs:
        - job_name: cubesandbox-resource
          scrape_interval: 5s
          scrape_timeout: 3s
          metrics_path: /v1/metrics/resource
          static_configs:
            - targets: ["127.0.0.1:9998"]
EOF
    fi
    if [[ "${logs_enabled}" == "true" ]]; then
      cat <<EOF
  filelog/sandbox:
    include:
      - /data/cubelet/log/${sandbox_glob}/stdout
      - /data/cubelet/log/${sandbox_glob}/stderr
    start_at: ${file_start_at}
    storage: file_storage
    include_file_path: true
    include_file_name: false
    max_log_size: 1MiB
    operators:
      - type: regex_parser
        parse_from: attributes["log.file.path"]
        regex: '^/data/cubelet/log/(?P<sandbox_id>[^/]+)/(?P<stream>stdout|stderr)$'
      - type: copy
        from: attributes["log.file.path"]
        to: attributes.log_file_path
      - type: copy
        from: attributes.sandbox_id
        to: attributes.cubesandbox_sandbox_id
      - type: move
        from: attributes.sandbox_id
        to: resource["cubesandbox.sandbox.id"]
      - type: copy
        from: attributes.stream
        to: attributes.log_iostream
      - type: move
        from: attributes.stream
        to: attributes["log.iostream"]
EOF
    fi
    cat <<EOF
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 256
  batch:
    timeout: 2s
    send_batch_size: 1024
EOF
    if [[ -n "${metrics_otlp_endpoint}" ]]; then
      cat <<EOF
  resource/metrics:
    attributes:
      - key: cluster
        value: $(yaml_dquote "${cluster}")
        action: upsert
      - key: node
        value: $(yaml_dquote "${node_value}")
        action: upsert
      - key: cubesandbox.cluster.id
        value: $(yaml_dquote "${cluster}")
        action: upsert
      - key: host.name
        value: $(yaml_dquote "${node_value}")
        action: upsert
EOF
    fi
    if [[ "${logs_enabled}" == "true" ]]; then
      cat <<EOF
  resource/logs_common:
    attributes:
      - key: host.name
        value: $(yaml_dquote "${node_value}")
        action: upsert
      - key: cubesandbox.cluster.id
        value: $(yaml_dquote "${cluster}")
        action: upsert
  attributes/logs_common:
    actions:
      - key: host_name
        value: $(yaml_dquote "${node_value}")
        action: upsert
      - key: cubesandbox_cluster_id
        value: $(yaml_dquote "${cluster}")
        action: upsert
      - key: cubesandbox_sandbox_id
        value: "none"
        action: insert
      - key: log_iostream
        value: "none"
        action: insert
  resource/sandbox:
    attributes:
      - key: service.name
        value: cubesandbox-sandbox
        action: upsert
      - key: cubesandbox.log.source
        value: sandbox-init
        action: upsert
  attributes/sandbox:
    actions:
      - key: service_name
        value: cubesandbox-sandbox
        action: upsert
      - key: cubesandbox_log_source
        value: sandbox-init
        action: upsert
EOF
    fi
    printf 'exporters:\n'
    if [[ -n "${metrics_url}" ]]; then
      cat <<EOF
  prometheus_remote_write:
    http:
      endpoint: $(yaml_dquote "${metrics_url}")
EOF
      if [[ -n "${metrics_auth}" || -n "${metrics_tenant}" ]]; then
        cat <<EOF
      headers:
EOF
        if [[ -n "${metrics_auth}" ]]; then
          printf '        Authorization: %s\n' "$(yaml_dquote "${metrics_auth}")"
        fi
        if [[ -n "${metrics_tenant}" ]]; then
          printf '        X-Scope-OrgID: %s\n' "$(yaml_dquote "${metrics_tenant}")"
        fi
      fi
      cat <<EOF
    external_labels:
      cluster: $(yaml_dquote "${cluster}")
      node: $(yaml_dquote "${node_value}")
    wal:
      directory: ${PROM_WAL_DIR}
    resource_to_telemetry_conversion:
      enabled: false
EOF
    fi
    if [[ -n "${metrics_otlp_endpoint}" ]]; then
      cat <<EOF
  otlp_http/metrics:
    endpoint: $(yaml_dquote "${metrics_otlp_endpoint}")
    compression: gzip
EOF
      if [[ -n "${metrics_auth}" || -n "${metrics_tenant}" ]]; then
        cat <<EOF
    headers:
EOF
        if [[ -n "${metrics_auth}" ]]; then
          printf '      Authorization: %s\n' "$(yaml_dquote "${metrics_auth}")"
        fi
        if [[ -n "${metrics_tenant}" ]]; then
          printf '      X-Scope-OrgID: %s\n' "$(yaml_dquote "${metrics_tenant}")"
        fi
      fi
      cat <<EOF
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 1000
      block_on_overflow: true
    retry_on_failure:
      enabled: true
      max_elapsed_time: 0
EOF
    fi
    if [[ -n "${es_endpoint}" ]]; then
      cat <<EOF
  elasticsearch/logs:
    endpoints:
      - $(yaml_dquote "${es_endpoint}")
    logs_index: $(yaml_dquote "${es_index}")
    mapping:
      allowed_modes:
        - raw
    discover:
      on_start: false
      interval: 0s
EOF
      if [[ -n "${es_user}" ]]; then
        printf '    user: %s\n' "$(yaml_dquote "${es_user}")"
      fi
      if [[ -n "${es_password}" ]]; then
        printf '    password: %s\n' "$(yaml_dquote "${es_password}")"
      fi
      if [[ -n "${es_api_key}" ]]; then
        printf '    api_key: %s\n' "$(yaml_dquote "${es_api_key}")"
      fi
      if [[ "${es_tls_skip}" == "true" ]]; then
        cat <<EOF
    tls:
      insecure_skip_verify: true
EOF
      fi
      cat <<EOF
    sending_queue:
      enabled: true
      num_consumers: 10
      queue_size: 1000
      block_on_overflow: true
      batch:
        flush_timeout: 2s
        min_size: 0
        max_size: 5000000
        sizer: bytes
    retry:
      enabled: true
      max_retries: 5
      initial_interval: 100ms
      max_interval: 1m
EOF
    fi
    if [[ -n "${logs_endpoint}" ]]; then
      cat <<EOF
  otlp_http/logs:
    endpoint: $(yaml_dquote "${logs_endpoint}")
    compression: gzip
EOF
      if [[ -n "${logs_auth}" || -n "${logs_tenant}" ]]; then
        cat <<EOF
    headers:
EOF
        if [[ -n "${logs_auth}" ]]; then
          printf '      Authorization: %s\n' "$(yaml_dquote "${logs_auth}")"
        fi
        if [[ -n "${logs_tenant}" ]]; then
          printf '      X-Scope-OrgID: %s\n' "$(yaml_dquote "${logs_tenant}")"
        fi
      fi
      cat <<EOF
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 1000
      block_on_overflow: true
    retry_on_failure:
      enabled: true
      max_elapsed_time: 0
EOF
    fi
    cat <<EOF
extensions:
  health_check:
    endpoint: 127.0.0.1:13133
EOF
    if [[ "${need_file_storage}" == "true" ]]; then
      cat <<EOF
  file_storage:
    directory: ${OTEL_STORAGE_DIR}
    create_directory: true
EOF
    fi
    printf 'service:\n'
    if [[ "${need_file_storage}" == "true" ]]; then
      printf '  extensions: [health_check, file_storage]\n'
    else
      printf '  extensions: [health_check]\n'
    fi
    cat <<EOF
  telemetry:
    metrics:
      readers:
        - pull:
            exporter:
              prometheus:
                host: 127.0.0.1
                port: 8888
  pipelines:
EOF
    if [[ -n "${metrics_url}" ]]; then
      cat <<EOF
    metrics:
      receivers: [prometheus]
      processors: [memory_limiter, batch]
      exporters: [prometheus_remote_write]
EOF
    fi
    if [[ -n "${metrics_otlp_endpoint}" ]]; then
      cat <<EOF
    metrics:
      receivers: [prometheus]
      processors: [memory_limiter, resource/metrics, batch]
      exporters: [otlp_http/metrics]
EOF
    fi
    if [[ "${logs_enabled}" == "true" ]]; then
      cat <<EOF
    logs/sandbox:
      receivers: [filelog/sandbox]
      processors: [memory_limiter, resource/logs_common, resource/sandbox, attributes/logs_common, attributes/sandbox, batch]
      exporters: [${logs_exporter_name}]
EOF
    fi
  } > "${destination}.tmp"
  mv "${destination}.tmp" "${destination}"
}

install_collector() {
  [[ "$(id -u)" -eq 0 ]] || die "install must run as root on the cubelet node"
  load_one_click_env
  require_install_config
  local node
  node="$(node_ip)"
  [[ -n "${node}" ]] || die "CUBE_SANDBOX_NODE_IP is empty and the node address could not be read from the route table"
  require_safe_label "node" "${node}"
  if [[ -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" || -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    check_cubelet_metrics
  fi
  install_binary
  install -d -m 0755 -o root -g root "${SHARED_CONFIG_ROOT}"
  install -d -m 0700 -o root -g root "${CONFIG_DIR}"
  mkdir -p "${PROM_WAL_DIR}" "${OTEL_STORAGE_DIR}"
  chmod 0700 "${STATE_DIR}" "${PROM_WAL_DIR}" "${OTEL_STORAGE_DIR}"
  rm -f "${LEGACY_CONFIG_PATH}"
  write_otel_config "${CONFIG_PATH}" "${node}"
  chmod 0600 "${CONFIG_PATH}"
  "${BIN_PATH}" validate --config="file:${CONFIG_PATH}"
  cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=Cube Sandbox node observability collector
After=network-online.target cube-sandbox-cubelet.service
Wants=network-online.target
[Service]
Type=simple
User=root
ExecStart=${BIN_PATH} --config=file:${CONFIG_PATH}
Restart=on-failure
RestartSec=2s
NoNewPrivileges=true
UMask=0077
[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "${UNIT_NAME}"
  systemctl restart "${UNIT_NAME}"
  sleep 2
  systemctl is-active --quiet "${UNIT_NAME}" || die "collector failed to start; journalctl -u ${UNIT_NAME}"
  curl -fsS --max-time 5 http://127.0.0.1:13133/ >/dev/null
  if [[ -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" ]]; then
    log "metrics: scraping Cubelet sandbox resources from ${CUBELET_METRICS_URL} and remote-writing as node=${node}"
  fi
  if [[ -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    log "metrics: scraping Cubelet sandbox resources from ${CUBELET_METRICS_URL} and exporting via OTLP/HTTP (${CUBE_METRICS_OTLP_HTTP_ENDPOINT%/}/v1/metrics) as node=${node}"
  fi
  local es_endpoint="${CUBE_LOGS_ES_ENDPOINT:-${CUBE_LOGS_ELASTICSEARCH_ENDPOINT:-}}"
  if [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" || -n "${es_endpoint}" ]]; then
    if [[ -n "${es_endpoint}" ]]; then
      log "logs: reading sandbox (${CUBE_LOGS_SANDBOX_ID:-*}) records from ${CUBE_LOGS_FILE_START_AT:-beginning} and exporting to Elasticsearch (${es_endpoint}, index=${CUBE_LOGS_ES_INDEX:-cubesandbox-logs}) as node=${node}"
    else
      log "logs: reading sandbox (${CUBE_LOGS_SANDBOX_ID:-*}) records from ${CUBE_LOGS_FILE_START_AT:-beginning} and exporting with OTLP/HTTP as node=${node}"
    fi
  fi
}

render_collector_config() {
  local ACTION="render"
  load_one_click_env
  require_install_config
  local node output tmp_output
  node="$(node_ip)"
  [[ -n "${node}" ]] || node="127.0.0.1"
  require_safe_label "node" "${node}"
  output="${CUBE_OTEL_CONFIG_OUTPUT:-${CUBE_OTEL_CONFIG:-${PWD}/cubesandbox-otelcol.yaml}}"
  umask 077
  mkdir -p "$(dirname "${output}")"
  tmp_output="$(mktemp "${output}.tmp.XXXXXX")"
  if ! write_otel_config "${tmp_output}" "${node}"; then
    rm -f -- "${tmp_output}"
    return 1
  fi
  chmod 0600 "${tmp_output}"
  mv -f -- "${tmp_output}" "${output}"
  log "wrote ${output}"
}

uninstall_collector() {
  [[ "$(id -u)" -eq 0 ]] || die "uninstall must run as root"
  systemctl disable --now "${UNIT_NAME}" 2>/dev/null || true
  rm -f "${UNIT_PATH}" "${CONFIG_PATH}" "${LEGACY_CONFIG_PATH}"
  systemctl daemon-reload
  log "removed ${UNIT_NAME}"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "${1:-install}" in
    install) install_collector ;;
    render) render_collector_config ;;
    uninstall) uninstall_collector ;;
    *) die "usage: $0 [install|render|uninstall]" ;;
  esac
fi
