#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(CDPATH= cd -- "${CHART_DIR}/../../.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

rendered="${TMP_DIR}/rendered.yaml"
disabled_rendered="${TMP_DIR}/disabled-rendered.yaml"

helm template egress-guard "${CHART_DIR}" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  --set egressProxy.enabled=true \
  --set cubeNode.network.cubeRouter.enabled=true \
  --set cubeEgress.network.enabled=false \
  --set-string 'egressProxy.clusterCIDRs[0]=10.244.0.0/16' \
  > "${rendered}"

helm template egress-disabled "${CHART_DIR}" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  > "${disabled_rendered}"

assert_contains() {
  pattern="$1"
  file="$2"
  if ! grep -Eq "${pattern}" "${file}"; then
    echo "missing expected pattern '${pattern}' in ${file}" >&2
    exit 1
  fi
}

assert_not_contains() {
  pattern="$1"
  file="$2"
  if grep -Eq "${pattern}" "${file}"; then
    echo "unexpected pattern '${pattern}' in ${file}" >&2
    exit 1
  fi
}

assert_contains 'name: egress-guard-cube-egress-configurer' "${rendered}"
assert_contains 'name: egress-guard-cube-egress-proxy-primary' "${rendered}"
assert_contains 'name: egress-guard-cube-egress-proxy-standby' "${rendered}"
assert_contains 'kind: DaemonSet' "${rendered}"
assert_contains 'type: OnDelete' "${rendered}"
assert_not_contains 'name: egress-guard-cube-egress-proxy$' "${rendered}"
assert_contains 'hostNetwork: true' "${rendered}"
assert_contains 'hostPID: true' "${rendered}"
assert_contains 'dnsPolicy: ClusterFirstWithHostNet' "${rendered}"
assert_contains 'CUBE_ROUTER_ENABLE' "${rendered}"
assert_contains 'CUBE_ROUTER_CIDR' "${rendered}"
assert_not_contains 'kind: NetworkPolicy' "${rendered}"
assert_not_contains 'kind: PodDisruptionBudget' "${rendered}"
assert_not_contains 'CUBE_EGRESS_WIREGUARD' "${rendered}"
assert_not_contains 'wireguard' "${rendered}"
assert_contains 'setup-veth.sh' "${rendered}"
assert_contains 'CUBE_EGRESS_ROLE' "${rendered}"
assert_contains 'CUBE_EGRESS_CLUSTER_CIDRS' "${rendered}"
assert_contains '10.244.0.0/16' "${rendered}"
assert_contains 'lookup "v1" "Namespace" "" "cube-egress"' \
  "${CHART_DIR}/templates/egress-proxy-namespace.yaml"
assert_contains 'meta.helm.sh/release-name' \
  "${CHART_DIR}/templates/egress-proxy-namespace.yaml"
assert_contains 'ownedByRelease' \
  "${CHART_DIR}/templates/egress-proxy-namespace.yaml"
assert_contains 'helm.sh/resource-policy: keep' "${rendered}"
assert_contains 'name: egress-guard-cube-egress-cleanup-uninstall' "${rendered}"
assert_contains 'helm.sh/hook: post-delete' "${rendered}"
assert_contains 'helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded' "${rendered}"
assert_contains 'readinessProbe:' "${rendered}"
assert_contains '/tmp/cleanup-complete' "${rendered}"
assert_contains 'name: egress-disabled-cube-egress-cleanup-uninstall' \
  "${disabled_rendered}"
assert_contains 'preStop:' "${rendered}"
assert_contains '/opt/cube-egress/cleanup.sh' "${rendered}"

if helm template missing-egress-cidr "${CHART_DIR}" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  --set egressProxy.enabled=true \
  --set cubeNode.network.cubeRouter.enabled=true \
  --set cubeEgress.network.enabled=false \
  > /dev/null 2>&1; then
  echo "egressProxy.clusterCIDRs must be required" >&2
  exit 1
fi

if helm template invalid-egress-cidr "${CHART_DIR}" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  --set egressProxy.enabled=true \
  --set cubeNode.network.cubeRouter.enabled=true \
  --set cubeEgress.network.enabled=false \
  --set-string 'egressProxy.clusterCIDRs[0]=10.999.0.0/16' \
  > /dev/null 2>&1; then
  echo "invalid egressProxy.clusterCIDRs entry was accepted" >&2
  exit 1
fi

if helm template duplicate-egress-cidr "${CHART_DIR}" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  --set egressProxy.enabled=true \
  --set cubeNode.network.cubeRouter.enabled=true \
  --set cubeEgress.network.enabled=false \
  --set-string 'egressProxy.clusterCIDRs[0]=10.244.0.0/16' \
  --set-string 'egressProxy.clusterCIDRs[1]=10.244.0.0/16' \
  > /dev/null 2>&1; then
  echo "duplicate egressProxy.clusterCIDRs entry was accepted" >&2
  exit 1
fi

assert_contains 'iif cube-router' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'from .*CUBE_ROUTER_NAT_IP.*/32' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'CUBE-EGRESS-MARK' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'ensure_route_mark_ownership' \
  "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'fwmark.*ROUTE_MARK' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'unreachable default' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'peer_is_healthy.*current_iface' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'active peer changed' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'CUBE-EGRESS' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'iptables-restore.*--noflush' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'current_chain' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'expected_chain' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_not_contains 'iptables .* -F CUBE-EGRESS' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_not_contains 'connmark|CONNMARK' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_not_contains 'wireguard|wg syncconf' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'cube-egress-p0' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'cube-egress-p1' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'egress-host0' "${REPO_ROOT}/EgressProxy/proxy/setup-veth.sh"
assert_contains 'VETH_HOST_IP' "${REPO_ROOT}/EgressProxy/proxy/entrypoint.sh"
assert_contains 'source_ip in.*CUBE_ROUTER_NAT_IP.*VETH_HOST_IP' \
  "${REPO_ROOT}/EgressProxy/proxy/entrypoint.sh"
assert_contains 'health-server.sh' "${REPO_ROOT}/EgressProxy/proxy/entrypoint.sh"
assert_contains 'nc -w 2' "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_contains 'preflight_iptables_ownership' \
  "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
assert_not_contains 'cleanup_legacy_tunnels|cube-wg' \
  "${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"

sh "${SCRIPT_DIR}/test-egress-route-failover.sh"
sh "${SCRIPT_DIR}/test-egress-policy-rules.sh"
sh "${SCRIPT_DIR}/test-egress-cleanup.sh"

image_builder="${REPO_ROOT}/deploy/kubernetes/images/build-cube-images.sh"
assert_contains 'cube-egress-configurer' "${image_builder}"
assert_contains 'cube-egress-proxy' "${image_builder}"
assert_contains 'EgressProxy/Dockerfile.configurer' "${image_builder}"
assert_contains 'EgressProxy/Dockerfile.proxy' "${image_builder}"

if [ -d "${REPO_ROOT}/EgressProxy/deploy" ]; then
  echo "EgressProxy/deploy must not exist; deployment belongs to deploy/kubernetes/chart" >&2
  exit 1
fi

assert_not_contains 'generate-egress-wireguard-secret' "${CHART_DIR}/README.md"

echo "EgressProxy chart and node-routing guard passed"
