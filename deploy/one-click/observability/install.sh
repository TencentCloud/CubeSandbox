#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# CubeSandbox one-click Observability Installer & Orchestrator.
# Supports CVM (one-click/systemd) installation. Kubernetes observability is
# installed through deploy/kubernetes/chart; Terraform does not install it.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OTELCOL_VERSION="0.162.0"
PROMETHEUS_VERSION="3.13.4"
LOKI_VERSION="3.7.9"
ELASTICSEARCH_VERSION="8.17.3"
GRAFANA_VERSION="10.4.18"

log() { echo "[cube-observability] $*" >&2; }
die() { echo "[cube-observability] ERROR: $*" >&2; exit 1; }

resolve_observability_asset_root() {
  local candidate
  for candidate in \
    "${CUBE_OBSERVABILITY_ASSET_ROOT:-}" \
    "${ONE_CLICK_ROOT}/CubeObservability" \
    "${SCRIPT_DIR}/../../../CubeObservability"; do
    if [[ -n "${candidate}" &&
          -d "${candidate}/grafana/dashboards" ]]; then
      (cd "${candidate}" && pwd)
      return
    fi
  done
  die "CubeObservability dashboard assets were not found"
}

# Load one-click runtime env (/usr/local/services/cubetoolbox/.one-click.env)
# or local one-click .env without clobbering variables already set in the caller's
# process environment.
load_one_click_env_defaults() {
  local f line key val
  local caller_backend_set=0
  local caller_es_endpoint_set=0
  local caller_logs_otlp_endpoint_set=0
  local caller_backend="${CUBE_LOGS_BACKEND:-}"
  [[ -n "${CUBE_LOGS_BACKEND+x}" ]] && caller_backend_set=1
  if [[ -n "${CUBE_LOGS_ES_ENDPOINT+x}" ||
        -n "${CUBE_LOGS_ELASTICSEARCH_ENDPOINT+x}" ]]; then
    caller_es_endpoint_set=1
  fi
  [[ -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT+x}" ]] && caller_logs_otlp_endpoint_set=1

  for f in \
    "${ONE_CLICK_RUNTIME_ENV_FILE:-/usr/local/services/cubetoolbox/.one-click.env}" \
    "${ONE_CLICK_ENV_FILE:-}" \
    "${ONE_CLICK_ROOT}/.env"; do
    [[ -n "${f}" && -f "${f}" ]] || continue
    while IFS= read -r line || [[ -n "${line}" ]]; do
      line="${line#"${line%%[![:space:]]*}"}"
      [[ -z "${line}" || "${line}" == \#* ]] && continue
      line="${line#export }"
      [[ "${line}" == *=* ]] || continue
      key="${line%%=*}"
      val="${line#*=}"
      [[ "${key}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
      # Preserve caller's explicit environment variable if already set
      if [[ -z "${!key+x}" ]]; then
        # Strip surrounding single or double quotes
        if [[ "${val}" =~ ^\'(.*)\'[[:space:]]*(#.*)?$ ]]; then
          val="${BASH_REMATCH[1]}"
        elif [[ "${val}" =~ ^\"(.*)\"[[:space:]]*(#.*)?$ ]]; then
          val="${BASH_REMATCH[1]}"
        else
          val="${val%%[[:space:]]#*}"
          val="${val%"${val##*[![:space:]]}"}"
        fi
        export "${key}=${val}"
      fi
    done < "${f}"
  done

  # An explicit selector may intentionally switch a host whose persisted
  # runtime still contains the opposite endpoint. Ignore only inherited
  # conflicts; explicitly passing both still reaches logs_backend() and fails.
  if [[ "${caller_backend_set}" == "1" ]]; then
    case "${caller_backend}" in
      loki)
        if [[ "${caller_es_endpoint_set}" == "0" ]]; then
          unset CUBE_LOGS_ES_ENDPOINT CUBE_LOGS_ELASTICSEARCH_ENDPOINT
        fi
        ;;
      elasticsearch|es)
        if [[ "${caller_logs_otlp_endpoint_set}" == "0" ]]; then
          unset CUBE_LOGS_OTLP_HTTP_ENDPOINT
        fi
        ;;
    esac
  fi
}

resolve_one_click_control_ip() {
  if [[ -n "${ONE_CLICK_CONTROL_PLANE_IP:-}" ]]; then
    printf '%s' "${ONE_CLICK_CONTROL_PLANE_IP}"
  elif [[ -n "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR:-}" ]]; then
    printf '%s' "${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR%%:*}"
  elif [[ -n "${ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR:-}" ]]; then
    printf '%s' "${ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR%%:*}"
  fi
}

load_one_click_env_defaults

logs_backend() {
  local backend="${CUBE_LOGS_BACKEND:-}"
  if [[ -z "${backend}" ]]; then
    if [[ -n "${CUBE_LOGS_ES_ENDPOINT:-}" ]]; then
      backend="elasticsearch"
    elif [[ -f /etc/systemd/system/cube-sandbox-elasticsearch.service &&
            ! -f /etc/systemd/system/cube-sandbox-loki.service ]]; then
      # Preserve an existing bundled ES deployment on upgrade. New installs
      # without either unit still default to Loki.
      backend="elasticsearch"
    else
      backend="loki"
    fi
  fi
  case "${backend}" in
    loki)
      [[ -z "${CUBE_LOGS_ES_ENDPOINT:-}" ]] || \
        die "CUBE_LOGS_BACKEND=loki cannot be combined with CUBE_LOGS_ES_ENDPOINT"
      printf 'loki\n'
      ;;
    elasticsearch|es)
      [[ -z "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" ]] || \
        die "CUBE_LOGS_BACKEND=elasticsearch cannot be combined with CUBE_LOGS_OTLP_HTTP_ENDPOINT"
      printf 'elasticsearch\n'
      ;;
    *) die "unsupported CUBE_LOGS_BACKEND: ${backend} (expected loki|elasticsearch)" ;;
  esac
}

usage() {
  cat <<'EOF'
Usage: ./deploy/one-click/observability/install.sh <command> [sub-action]

Commands:
  auto | one-click    Auto-detect role from one-click (.one-click.env / .env):
                      - control role : installs full stack (or cvm if external endpoints are set)
                      - compute role : installs OTel Collector pointing to ONE_CLICK_CONTROL_PLANE_IP
  all                 Install full stack on a single CVM node:
                      Prometheus + selected logs backend (Loki default) + Grafana
                      + OTel Collector (Cubelet sandbox resources + sandbox stdout/stderr logs)
  backend             Install central observability backend on current host:
                      Prometheus + selected logs backend (Loki default) + Grafana
  cvm | otelcol       Install/upgrade OTel Collector systemd service on a CVM compute node
  prometheus          Manage Prometheus 3.13.4 LTS (:9092)     [install|render|scrape-job|uninstall]
                      scrape-job emits a job for an external Prometheus; it installs nothing
  loki                Manage Loki 3.7.9 (:3100, OTLP)          [install|render|uninstall]
  elasticsearch | es  Manage optional Elasticsearch log backend [install|render|uninstall]
  grafana             Manage Grafana OSS 10.4.18 (:3001)       [install|dashboards|render|uninstall]
  dashboards          Import datasources & final dashboards into an existing Grafana via HTTP API
  status              Check Cubelet, Collector, Prometheus, selected logs backend, and Grafana
  render              Render config files without installing   [all|cvm|prometheus|loki|es|grafana]
  uninstall           Uninstall components                     [all|backend|cvm|prometheus|loki|es|grafana]
  versions            Print pinned component versions and dashboard summary

Common Environment Variables:
  ONE_CLICK_ENABLE_OBSERVABILITY    Set to 1 in deploy/one-click/.env to auto-install during one-click install.sh
  ONE_CLICK_OBSERVABILITY_ROLE      Optional role override for 'auto' (auto|all|backend|cvm, default: auto)
  CUBE_OBSERVABILITY_CLUSTER_ID     Cluster label (default: cubesandbox)
  CUBE_LOGS_BACKEND                 Log backend: loki (default) or elasticsearch
  CUBE_METRICS_OTLP_HTTP_ENDPOINT   Prometheus OTLP base URL (e.g. http://<prom-ip>:9092/api/v1/otlp)
  CUBE_METRICS_REMOTE_WRITE_URL     Prometheus Remote Write URL (e.g. http://<prom-ip>:9092/api/v1/write)
  CUBE_LOGS_OTLP_HTTP_ENDPOINT      OTLP logs base URL (default Loki: http://<loki-ip>:3100/otlp)
  CUBE_LOGS_ES_ENDPOINT             Optional Elasticsearch URL instead of OTLP
  CUBE_LOGS_ES_INDEX                Optional Elasticsearch index name (default: cubesandbox-logs)
  GRAFANA_URL                       Existing Grafana URL for 'dashboards' command (default: http://127.0.0.1:3001)
EOF
}

print_versions() {
  cat <<EOF
CubeSandbox Observability Suite — Component Versions:
  - OpenTelemetry Collector Contrib : v${OTELCOL_VERSION}
  - Prometheus LTS                  : v${PROMETHEUS_VERSION} (default :9092, OTLP + Remote Write enabled)
  - Loki (default logs backend)      : v${LOKI_VERSION} (default :3100, native OTLP/HTTP)
  - Elasticsearch (optional)         : v${ELASTICSEARCH_VERSION} (default :9200)
  - Grafana OSS                     : v${GRAFANA_VERSION} (default :3001, schemaVersion: 39)
  - Dashboards                      :
      1) cubesandbox-cluster.json     (cluster metrics, Prometheus)
      2) cubesandbox-node.json        (node metrics, Prometheus)
      3) cubesandbox-sandbox.json     (sandbox metrics, Prometheus)
      4) cubesandbox-logs*.json        (selected log backend)
EOF
}

install_backend() {
  local backend
  backend="$(logs_backend)"
  log "=== [1/3] Installing Prometheus ${PROMETHEUS_VERSION} (:9092) ==="
  "${SCRIPT_DIR}/prometheus/install.sh" install

  case "${backend}" in
    loki)
      log "=== [2/3] Installing Loki ${LOKI_VERSION} (:3100, native OTLP/HTTP) ==="
      "${SCRIPT_DIR}/loki/install.sh" install
      ;;
    elasticsearch)
      log "=== [2/3] Installing Elasticsearch ${ELASTICSEARCH_VERSION} (:9200) ==="
      "${SCRIPT_DIR}/elasticsearch/install.sh" install
      ;;
  esac

  log "=== [3/3] Installing Grafana ${GRAFANA_VERSION} (:3001) & Provisioning Dashboards ==="
  export CUBE_GRAFANA_PROMETHEUS_URL="${CUBE_GRAFANA_PROMETHEUS_URL:-http://127.0.0.1:9092}"
  export CUBE_LOGS_BACKEND="${backend}"
  if [[ "${backend}" == "loki" ]]; then
    export CUBE_GRAFANA_LOKI_URL="${CUBE_GRAFANA_LOKI_URL:-http://127.0.0.1:3100}"
  else
    export CUBE_GRAFANA_ES_URL="${CUBE_GRAFANA_ES_URL:-http://127.0.0.1:9200}"
  fi
  "${SCRIPT_DIR}/grafana/install.sh" install

  # Install and verify the selected backend before stopping the previous one so
  # a backend switch keeps the interruption window short. Historical data and
  # binaries are retained by both uninstall helpers.
  if [[ "${backend}" == "loki" ]]; then
    "${SCRIPT_DIR}/elasticsearch/install.sh" uninstall >/dev/null 2>&1 || true
  else
    "${SCRIPT_DIR}/loki/install.sh" uninstall >/dev/null 2>&1 || true
  fi
}

install_all() {
  local backend
  backend="$(logs_backend)"
  install_backend

  log "=== [4/4] Installing CVM OTel Collector ${OTELCOL_VERSION} ==="
  if [[ -z "${CUBE_METRICS_REMOTE_WRITE_URL:-}" ]]; then
    export CUBE_METRICS_OTLP_HTTP_ENDPOINT="${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-http://127.0.0.1:9092/api/v1/otlp}"
  fi
  if [[ "${backend}" == "loki" ]]; then
    export CUBE_LOGS_OTLP_HTTP_ENDPOINT="${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-http://127.0.0.1:3100/otlp}"
  else
    export CUBE_LOGS_ES_ENDPOINT="${CUBE_LOGS_ES_ENDPOINT:-http://127.0.0.1:9200}"
  fi
  "${SCRIPT_DIR}/otelcol/install.sh" install

  log "=== Full observability stack installed successfully ==="
  check_status
}

install_cvm() {
  local sub_action="${1:-install}"
  local control_ip backend
  control_ip="$(resolve_one_click_control_ip)"
  backend="$(logs_backend)"

  # When running on a one-click compute node (or when ONE_CLICK_CONTROL_PLANE_IP
  # is present in .one-click.env) and endpoints are not explicitly provided,
  # automatically point OTel Collector at the control-plane Prometheus (:9092)
  # and the selected logs backend (Loki :3100 or Elasticsearch :9200).
  if [[ -z "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" && -z "${CUBE_METRICS_REMOTE_WRITE_URL:-}" && -n "${control_ip}" ]]; then
    export CUBE_METRICS_OTLP_HTTP_ENDPOINT="http://${control_ip}:9092/api/v1/otlp"
    log "auto-derived CUBE_METRICS_OTLP_HTTP_ENDPOINT=${CUBE_METRICS_OTLP_HTTP_ENDPOINT} from one-click control plane IP"
  fi
  if [[ -z "${CUBE_LOGS_ES_ENDPOINT:-}" && -z "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" && -n "${control_ip}" ]]; then
    if [[ "${backend}" == "loki" ]]; then
      export CUBE_LOGS_OTLP_HTTP_ENDPOINT="http://${control_ip}:3100/otlp"
      log "auto-derived CUBE_LOGS_OTLP_HTTP_ENDPOINT=${CUBE_LOGS_OTLP_HTTP_ENDPOINT} from one-click control plane IP"
    else
      export CUBE_LOGS_ES_ENDPOINT="http://${control_ip}:9200"
      log "auto-derived CUBE_LOGS_ES_ENDPOINT=${CUBE_LOGS_ES_ENDPOINT} from one-click control plane IP"
    fi
  fi

  "${SCRIPT_DIR}/otelcol/install.sh" "${sub_action}"
}

install_auto() {
  local deploy_role="${ONE_CLICK_DEPLOY_ROLE:-control}"
  local obs_role="${ONE_CLICK_OBSERVABILITY_ROLE:-auto}"
  local has_external_endpoints=0

  if [[ -n "${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}" ||
        -n "${CUBE_METRICS_REMOTE_WRITE_URL:-}" ||
        -n "${CUBE_LOGS_ES_ENDPOINT:-}" ||
        -n "${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-}" ]]; then
    has_external_endpoints=1
  fi

  case "${obs_role}" in
    all)
      log "one-click observability mode: all (role=${deploy_role})"
      install_all
      ;;
    backend)
      log "one-click observability mode: backend (role=${deploy_role})"
      install_backend
      ;;
    cvm)
      log "one-click observability mode: cvm (role=${deploy_role})"
      install_cvm install
      ;;
    auto|"")
      if [[ "${deploy_role}" == "compute" ]]; then
        log "one-click auto-detected compute node (ONE_CLICK_DEPLOY_ROLE=compute) -> installing OTel Collector"
        install_cvm install
      elif [[ "${has_external_endpoints}" == "1" ]]; then
        log "one-click auto-detected external observability endpoints on control node -> installing OTel Collector"
        install_cvm install
      else
        log "one-click auto-detected control/all-in-one node -> installing Prometheus + $(logs_backend) + Grafana + OTel Collector"
        install_all
      fi
      ;;
    *)
      die "unsupported ONE_CLICK_OBSERVABILITY_ROLE: ${obs_role} (expected auto|all|backend|cvm)"
      ;;
  esac
}

check_status() {
  local backend
  backend="$(logs_backend)"
  local prom_url="${CUBE_PROMETHEUS_URL:-http://127.0.0.1:9092}"
  local loki_url="${CUBE_LOKI_URL:-${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-http://127.0.0.1:3100/otlp}}"
  loki_url="${loki_url%/}"
  loki_url="${loki_url%/otlp}"
  local graf_url="${GRAFANA_URL:-http://127.0.0.1:3001}"
  local cubelet_bin="/usr/local/services/cubetoolbox/Cubelet/bin/cubelet"
  local cubelet_ver="unknown"

  if [[ -x "${cubelet_bin}" ]]; then
    cubelet_ver="$("${cubelet_bin}" --version 2>/dev/null | head -n 1 | tr -d '\r' || echo "unknown")"
  elif command -v cubelet >/dev/null 2>&1; then
    cubelet_ver="$(cubelet --version 2>/dev/null | head -n 1 | tr -d '\r' || echo "unknown")"
  fi

  echo "============================================================"
  echo " CubeSandbox Observability Health Status"
  echo "============================================================"

  local http_code body_tmp
  body_tmp="$(mktemp)"
  http_code="$(curl -sS --max-time 2 -o "${body_tmp}" -w "%{http_code}" "http://127.0.0.1:9998/v1/metrics/resource" 2>/dev/null || echo "000")"
  if [[ "${http_code}" == "200" ]]; then
    if grep -q "cubesandbox_node_sandbox_count" "${body_tmp}" 2>/dev/null; then
      echo " [OK]   Cubelet Resource Metrics (127.0.0.1:9998/v1/metrics/resource, HTTP 200)"
      echo "        -> Cubelet Version    : ${cubelet_ver}"
    elif grep -Eq "cubesandbox_(host_sandbox|guest_workload)_" "${body_tmp}" 2>/dev/null; then
      echo " [WARN] Cubelet Resource Metrics does not expose cubesandbox_node_sandbox_count"
      echo "        -> Cubelet Version    : ${cubelet_ver}"
      echo "        -> Check              : deployed Cubelet build and resource-metrics plugin configuration"
    else
      echo " [WARN] Cubelet Resource Metrics returned HTTP 200 without sandbox series"
      echo "        -> Cubelet Version    : ${cubelet_ver}"
      echo "        -> Check              : resource-metrics plugin configuration and first-sample readiness"
    fi
  elif [[ "${http_code}" == "404" ]]; then
    echo " [WARN] Cubelet Resource Metrics returned HTTP 404 (127.0.0.1:9998/v1/metrics/resource)"
    echo "        -> Cubelet Version    : ${cubelet_ver}"
    echo "        -> Check              : deployed Cubelet build and resource-metrics endpoint configuration"
  else
    echo " [SKIP] Cubelet Resource Metrics (127.0.0.1:9998 not listening on this host; version=${cubelet_ver})"
  fi
  rm -f "${body_tmp}"

  if curl -fsS --max-time 2 "http://127.0.0.1:13133/" >/dev/null 2>&1; then
    echo " [OK]   OTel Collector Health    (127.0.0.1:13133)"
  else
    echo " [DOWN] OTel Collector Health    (127.0.0.1:13133)"
  fi

  if curl -fsS --max-time 2 "${prom_url%/}/-/ready" >/dev/null 2>&1; then
    echo " [OK]   Prometheus Server        (${prom_url%/}/-/ready)"
  else
    echo " [DOWN] Prometheus Server        (${prom_url%/}/-/ready)"
  fi

  if [[ "${backend}" == "loki" ]]; then
    if curl -fsS --max-time 2 "${loki_url%/}/ready" >/dev/null 2>&1; then
      echo " [OK]   Loki Server              (${loki_url%/}/ready)"
    else
      echo " [DOWN] Loki Server              (${loki_url%/}/ready)"
    fi
  else
    local es_url="${CUBE_LOGS_ES_ENDPOINT:-http://127.0.0.1:9200}"
    if curl -fsS --max-time 2 "${es_url%/}/_cluster/health" >/dev/null 2>&1; then
      echo " [OK]   Elasticsearch Server     (${es_url%/})"
    else
      echo " [DOWN] Elasticsearch Server     (${es_url%/})"
    fi
  fi

  if curl -fsS --max-time 2 "${graf_url%/}/api/health" >/dev/null 2>&1; then
    echo " [OK]   Grafana Server           (${graf_url%/})"
    echo "        -> Cluster Metrics   : ${graf_url%/}/d/cubesandbox-cluster"
    echo "        -> Node Metrics      : ${graf_url%/}/d/cubesandbox-node"
    echo "        -> Sandbox Metrics   : ${graf_url%/}/d/cubesandbox-sandbox"
    echo "        -> Logs Dashboard    : ${graf_url%/}/d/cubesandbox-logs/cubesandbox-logs"
  else
    echo " [DOWN] Grafana Server           (${graf_url%/})"
  fi
  echo "============================================================"
}

render_configs() {
  local target="${1:-all}"
  local backend
  backend="$(logs_backend)"
  case "${target}" in
    all)
      if [[ "${backend}" == "loki" ]]; then
        export CUBE_LOGS_OTLP_HTTP_ENDPOINT="${CUBE_LOGS_OTLP_HTTP_ENDPOINT:-http://127.0.0.1:3100/otlp}"
      else
        export CUBE_LOGS_ES_ENDPOINT="${CUBE_LOGS_ES_ENDPOINT:-http://127.0.0.1:9200}"
      fi
      local metrics_otlp="${CUBE_METRICS_OTLP_HTTP_ENDPOINT:-}"
      if [[ -z "${metrics_otlp}" && -z "${CUBE_METRICS_REMOTE_WRITE_URL:-}" ]]; then
        metrics_otlp="http://127.0.0.1:9092/api/v1/otlp"
      fi
      CUBE_METRICS_OTLP_HTTP_ENDPOINT="${metrics_otlp}" \
        "${SCRIPT_DIR}/otelcol/install.sh" render
      "${SCRIPT_DIR}/prometheus/install.sh" render
      if [[ "${backend}" == "loki" ]]; then
        "${SCRIPT_DIR}/loki/install.sh" render
      else
        "${SCRIPT_DIR}/elasticsearch/install.sh" render
      fi
      export CUBE_LOGS_BACKEND="${backend}"
      "${SCRIPT_DIR}/grafana/install.sh" render
      ;;
    cvm|otelcol)
      "${SCRIPT_DIR}/otelcol/install.sh" render
      ;;
    prometheus)
      "${SCRIPT_DIR}/prometheus/install.sh" render
      ;;
    loki)
      "${SCRIPT_DIR}/loki/install.sh" render
      ;;
    elasticsearch|es)
      "${SCRIPT_DIR}/elasticsearch/install.sh" render
      ;;
    grafana)
      "${SCRIPT_DIR}/grafana/install.sh" render
      ;;
    *)
      die "unknown render target: ${target} (expected all|cvm|prometheus|loki|es|grafana)"
      ;;
  esac
}

uninstall_target() {
  local target="${1:-all}"
  case "${target}" in
    all)
      "${SCRIPT_DIR}/otelcol/install.sh" uninstall || true
      "${SCRIPT_DIR}/grafana/install.sh" uninstall || true
      "${SCRIPT_DIR}/loki/install.sh" uninstall || true
      "${SCRIPT_DIR}/elasticsearch/install.sh" uninstall || true
      "${SCRIPT_DIR}/prometheus/install.sh" uninstall || true
      ;;
    backend)
      "${SCRIPT_DIR}/grafana/install.sh" uninstall || true
      "${SCRIPT_DIR}/loki/install.sh" uninstall || true
      "${SCRIPT_DIR}/elasticsearch/install.sh" uninstall || true
      "${SCRIPT_DIR}/prometheus/install.sh" uninstall || true
      ;;
    cvm|otelcol)
      "${SCRIPT_DIR}/otelcol/install.sh" uninstall
      ;;
    prometheus)
      "${SCRIPT_DIR}/prometheus/install.sh" uninstall
      ;;
    loki)
      "${SCRIPT_DIR}/loki/install.sh" uninstall
      ;;
    elasticsearch|es)
      "${SCRIPT_DIR}/elasticsearch/install.sh" uninstall
      ;;
    grafana)
      "${SCRIPT_DIR}/grafana/install.sh" uninstall
      ;;
    *)
      die "unknown uninstall target: ${target} (expected all|backend|cvm|prometheus|loki|es|grafana)"
      ;;
  esac
}

cmd="${1:-help}"
shift || true

case "${cmd}" in
  auto|one-click|one-click-hook)
    install_auto
    ;;
  all|cvm-all)
    install_all
    ;;
  backend)
    install_backend
    ;;
  cvm|otelcol|node)
    install_cvm "${1:-install}"
    ;;
  prometheus|prom)
    "${SCRIPT_DIR}/prometheus/install.sh" "${1:-install}"
    ;;
  loki)
    "${SCRIPT_DIR}/loki/install.sh" "${1:-install}"
    ;;
  elasticsearch|es)
    "${SCRIPT_DIR}/elasticsearch/install.sh" "${1:-install}"
    ;;
  grafana)
    "${SCRIPT_DIR}/grafana/install.sh" "${1:-install}"
    ;;
  dashboards|import-dashboards)
    "${SCRIPT_DIR}/grafana/install.sh" dashboards
    ;;
  status|check)
    check_status
    ;;
  render)
    render_configs "${1:-all}"
    ;;
  uninstall|remove)
    uninstall_target "${1:-all}"
    ;;
  versions|version)
    print_versions
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage
    die "unknown command: ${cmd}"
    ;;
esac
