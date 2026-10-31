#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
CHART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

cat > "${TMP_DIR}/cluster.yaml" <<'EOF'
mysql:
  password: test
  rootPassword: test
redis:
  password: test
  clusterNodes: "redis-1.example:6379,redis-2.example:6379"
EOF
helm template cluster-check "${CHART_DIR}" -f "${TMP_DIR}/cluster.yaml" > "${TMP_DIR}/rendered.yaml"
grep -q 'cluster_nodes: "redis-1.example:6379,redis-2.example:6379"' "${TMP_DIR}/rendered.yaml"
for env_name in REDIS_CLUSTER_NODES CUBE_LCM_REDIS_CLUSTER_NODES CUBE_PROXY_REGISTRY_REDIS_CLUSTER_NODES; do
  grep -q "name: ${env_name}" "${TMP_DIR}/rendered.yaml"
done
if grep -q 'app.kubernetes.io/component: redis$' "${TMP_DIR}/rendered.yaml"; then
  echo "Cluster mode unexpectedly deploys built-in Redis" >&2; exit 1
fi
for override in redis.db=1 redis.masterName=mymaster redis.host=redis.example redis.clusterNodes=bad-endpoint; do
  if helm template cluster-reject "${CHART_DIR}" -f "${TMP_DIR}/cluster.yaml" --set-string "${override}" > /dev/null 2>"${TMP_DIR}/error"; then
    echo "unexpectedly accepted ${override}" >&2; exit 1
  fi
  grep -q 'redis.clusterNodes' "${TMP_DIR}/error"
done
# Existing modes must still render and keep their previous backend selection.
helm template standalone-check "${CHART_DIR}" -f "${TMP_DIR}/cluster.yaml" --set-string redis.clusterNodes= > "${TMP_DIR}/standalone.yaml"
grep -q 'app.kubernetes.io/component: redis$' "${TMP_DIR}/standalone.yaml"
helm template sentinel-check "${CHART_DIR}" -f "${TMP_DIR}/cluster.yaml" --set-string redis.clusterNodes= --set-string redis.masterName=mymaster --set-string redis.sentinelNodes=redis.example:26379 > "${TMP_DIR}/sentinel.yaml"
grep -q 'master_name: "mymaster"' "${TMP_DIR}/sentinel.yaml"
echo "Redis Cluster chart guards passed"
