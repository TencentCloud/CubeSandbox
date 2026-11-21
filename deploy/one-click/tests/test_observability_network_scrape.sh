#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# The bundled observability stack must collect Cubelet network metrics
# (/v1/metrics/network) alongside the sandbox resource metrics: the
# bring-your-own-Prometheus job pair, and the node Collector's scrape config.
# These subcommands are pure config generation and never touch the system.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
PROM_INSTALLER="${ONE_CLICK_DIR}/observability/prometheus/install.sh"
OTEL_INSTALLER="${ONE_CLICK_DIR}/observability/otelcol/install.sh"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# Prometheus config generation needs a listen address even though nothing starts.
export CUBE_PROMETHEUS_LISTEN_ADDRESS="10.0.0.10:9092"

# Node-list path: both jobs, every listed node, comments skipped.
node_list="${TMP_DIR}/nodes.txt"
printf '192.0.2.10\n# a comment\n192.0.2.12\n' > "${node_list}"
CUBE_METRICS_NODE_LIST="${node_list}" \
  CUBE_METRICS_PROM_CONFIG="${TMP_DIR}/scrape-nodes.yml" \
  bash "${PROM_INSTALLER}" scrape-job >"${TMP_DIR}/scrape-nodes.out" 2>&1

for job in cubesandbox-resource cubesandbox-network; do
  grep -Fq "job_name: ${job}" "${TMP_DIR}/scrape-nodes.yml" || fail "node-list scrape job file missing ${job}"
done
grep -Fq 'metrics_path: /v1/metrics/network' "${TMP_DIR}/scrape-nodes.yml" || fail "node-list scrape job file missing the network metrics path"
for node in 192.0.2.10 192.0.2.12; do
  [[ "$(grep -Fc "targets: [\"${node}:9998\"]" "${TMP_DIR}/scrape-nodes.yml")" -eq 2 ]] || fail "node ${node} must be scraped by both jobs"
done
if grep -Fq 'a comment' "${TMP_DIR}/scrape-nodes.yml"; then
  fail "node-list comments leaked into the scrape config"
fi
echo "OK bring-your-own-Prometheus node list scrapes resource and network endpoints"

# Single-node path (the one-click env flow): same job pair.
CUBE_SANDBOX_NODE_IP="192.0.2.11" \
  CUBE_METRICS_PROM_CONFIG="${TMP_DIR}/scrape-node.yml" \
  bash "${PROM_INSTALLER}" scrape-job >"${TMP_DIR}/scrape-node.out" 2>&1
grep -Fq 'job_name: cubesandbox-network' "${TMP_DIR}/scrape-node.yml" || fail "single-node scrape job file missing the network job"
grep -Fq 'metrics_path: /v1/metrics/network' "${TMP_DIR}/scrape-node.yml" || fail "single-node scrape job file missing the network metrics path"
[[ "$(grep -c 'job_name:' "${TMP_DIR}/scrape-node.yml")" -eq 2 ]] || fail "single-node scrape job file must contain exactly the two Cubelet jobs"
echo "OK bring-your-own-Prometheus single-node scrape covers resource and network endpoints"

# Re-running the installer regenerates both jobs deterministically: an operator
# upgrading must not lose (or have to remember) the network wiring.
cp "${TMP_DIR}/scrape-node.yml" "${TMP_DIR}/scrape-node.expected.yml"
CUBE_SANDBOX_NODE_IP="192.0.2.11" \
  CUBE_METRICS_PROM_CONFIG="${TMP_DIR}/scrape-node.yml" \
  bash "${PROM_INSTALLER}" scrape-job >"${TMP_DIR}/scrape-node-rerun.out" 2>&1
diff -q "${TMP_DIR}/scrape-node.expected.yml" "${TMP_DIR}/scrape-node.yml" >/dev/null || \
  fail "scrape-job re-render is not deterministic; operators upgrading would lose the wiring"
echo "OK scrape-job re-render stays deterministic across upgrades"

# Node Collector: the rendered config scrapes both endpoints.
env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
  CUBE_SANDBOX_NODE_IP=127.0.0.1 \
  CUBE_OTEL_CONFIG_OUTPUT="${TMP_DIR}/otelcol.yaml" \
  CUBE_METRICS_OTLP_HTTP_ENDPOINT=http://127.0.0.1:9092/api/v1/otlp \
  bash "${OTEL_INSTALLER}" render >"${TMP_DIR}/otelcol.out" 2>&1
grep -Fq 'job_name: cubesandbox-resource' "${TMP_DIR}/otelcol.yaml" || fail "Collector config missing the resource scrape job"
grep -Fq 'job_name: cubesandbox-network' "${TMP_DIR}/otelcol.yaml" || fail "Collector config missing the network scrape job"
grep -Fq 'metrics_path: /v1/metrics/network' "${TMP_DIR}/otelcol.yaml" || fail "Collector config missing the network metrics path"
echo "OK node Collector scrapes resource and network endpoints"

echo "PASS observability network-metrics wiring"
