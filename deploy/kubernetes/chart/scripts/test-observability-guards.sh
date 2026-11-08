#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  helm template obs "$CHART_DIR" -n cube-system \
    --set-string mysql.password=render-only \
    --set-string mysql.rootPassword=render-only \
    --set-string redis.password=render-only \
    --set observability.enabled=true \
    -s templates/observability.yaml "$@"
}

render > "$TMP_DIR/default.yaml"
if grep -Fq 'name: obs-component-otelcol' "$TMP_DIR/default.yaml"; then
  echo "component log Collector must be disabled by default" >&2
  exit 1
fi
grep -Fq 'path: /-/ready' "$TMP_DIR/default.yaml"
grep -Fq 'path: /-/healthy' "$TMP_DIR/default.yaml"
grep -Fq 'path: /api/health' "$TMP_DIR/default.yaml"
grep -Fq 'path: /ready' "$TMP_DIR/default.yaml"
grep -Fq 'app.kubernetes.io/component: grafana-admin' "$TMP_DIR/default.yaml"
grep -Fq 'key: "admin-password"' "$TMP_DIR/default.yaml"
grep -Fq 'checksum/grafana-admin-secret:' "$TMP_DIR/default.yaml"
echo "OK default probes, generated Grafana secret, and component-log opt-in"

if render --set-string observability.backend.grafana.adminPassword=short \
  > "$TMP_DIR/short-grafana-password.yaml" 2> "$TMP_DIR/short-grafana-password.err"; then
  echo "short Grafana admin passwords must fail Helm rendering" >&2
  exit 1
fi
grep -Fq 'adminPassword must contain at least 16 characters' "$TMP_DIR/short-grafana-password.err"
echo "OK weak inline Grafana passwords rejected"

render --set observability.backend.grafana.existingSecret=external-grafana \
  > "$TMP_DIR/external-grafana-secret.yaml"
grep -Fq 'name: "external-grafana"' "$TMP_DIR/external-grafana-secret.yaml"
if grep -Fq 'app.kubernetes.io/component: grafana-admin' "$TMP_DIR/external-grafana-secret.yaml"; then
  echo "chart must not create a Grafana admin Secret when existingSecret is configured" >&2
  exit 1
fi
echo "OK external Grafana admin Secret supported"

render --set observability.logs.components.enabled=true > "$TMP_DIR/components.yaml"
for container in cube-master cube-api cube-ops cube-templatecenter cubelet cube-proxy cube-lifecycle-manager; do
  grep -Fq "/var/log/pods/cube-system_*_*/${container}/*.log" "$TMP_DIR/components.yaml"
  grep -Fq "value: \"cubesandbox-${container}\"" "$TMP_DIR/components.yaml"
done
grep -Fq 'name: obs-cube-component-otelcol' "$TMP_DIR/components.yaml"
grep -Fq 'mountPath: "/var/log/pods"' "$TMP_DIR/components.yaml"
grep -Fq 'readOnly: true' "$TMP_DIR/components.yaml"
grep -Fq 'allowPrivilegeEscalation: false' "$TMP_DIR/components.yaml"
grep -Fq 'drop: ["ALL"]' "$TMP_DIR/components.yaml"
grep -Fq 'key: cube.tencent.com/cube-control' "$TMP_DIR/components.yaml"
grep -Fq 'key: cube.tencent.com/cube-node' "$TMP_DIR/components.yaml"
echo "OK component log allowlist, read-only mount, security context, and placement"

render --set observability.logs.components.enabled=true \
  --set observability.logs.backend=elasticsearch > "$TMP_DIR/elasticsearch.yaml"
grep -Fq 'elasticsearch/logs:' "$TMP_DIR/elasticsearch.yaml"
grep -Fq 'transform/es_flat:' "$TMP_DIR/elasticsearch.yaml"
grep -Fq 'path: /_cluster/health?wait_for_status=yellow&timeout=1s' "$TMP_DIR/elasticsearch.yaml"
echo "OK Elasticsearch component log export and health probes"

if render --set observability.logs.components.enabled=true \
  --set-string 'observability.logs.components.containers[0]=bad/name' \
  > "$TMP_DIR/invalid.yaml" 2> "$TMP_DIR/invalid.err"; then
  echo "invalid component container names must fail Helm rendering" >&2
  exit 1
fi
grep -Fq 'invalid observability.logs.components.containers entry' "$TMP_DIR/invalid.err"
echo "OK invalid component names rejected"

# Print the rendered document (split on ---) that contains "$2".
yaml_doc() {
  awk -v needle="$2" '
    /^---/ { if (doc ~ needle) printf "%s", doc; doc = ""; next }
    { doc = doc $0 "\n" }
    END { if (doc ~ needle) printf "%s", doc }
  ' "$1"
}

# kubelet probes must reach the address health_check listens on: loopback
# for the hostNetwork node Collector, the Pod IP for the component Collector.
yaml_doc "$TMP_DIR/components.yaml" 'name: obs-cube-node-otelcol-config' > "$TMP_DIR/node-cm.yaml"
yaml_doc "$TMP_DIR/components.yaml" 'app.kubernetes.io/component: node-otelcol' \
  | yaml_doc /dev/stdin 'kind: DaemonSet' > "$TMP_DIR/node-ds.yaml"
yaml_doc "$TMP_DIR/components.yaml" 'name: obs-cube-component-otelcol-config' > "$TMP_DIR/component-cm.yaml"
yaml_doc "$TMP_DIR/components.yaml" 'app.kubernetes.io/component: component-otelcol' \
  | yaml_doc /dev/stdin 'kind: DaemonSet' > "$TMP_DIR/component-ds.yaml"
for f in node-cm node-ds component-cm component-ds; do
  [ -s "$TMP_DIR/$f.yaml" ] || { echo "could not locate rendered $f document" >&2; exit 1; }
done
grep -Fq 'endpoint: 127.0.0.1:13133' "$TMP_DIR/node-cm.yaml"
grep -Fq 'hostNetwork: true' "$TMP_DIR/node-ds.yaml"
[ "$(grep -c 'host: 127.0.0.1' "$TMP_DIR/node-ds.yaml")" -eq 2 ] || {
  echo "node Collector readiness/liveness probes must target 127.0.0.1" >&2
  exit 1
}
grep -Fq 'endpoint: ${env:POD_IP}:13133' "$TMP_DIR/component-cm.yaml"
grep -Fq 'fieldPath: status.podIP' "$TMP_DIR/component-ds.yaml"
if grep -Fq 'hostNetwork: true' "$TMP_DIR/component-ds.yaml" ||
   grep -Eq '^[[:space:]]+host:' "$TMP_DIR/component-ds.yaml"; then
  echo "component Collector runs on the Pod network; probes must target the Pod IP" >&2
  exit 1
fi
echo "OK Collector probes match their health_check listen addresses"

render --set-string observability.metrics.remoteWriteUrl=http://prom.example:9090/api/v1/write \
  > "$TMP_DIR/remote-write.yaml"
grep -Fq 'prometheus_remote_write:' "$TMP_DIR/remote-write.yaml"
if grep -Fq 'otlp_http/metrics:' "$TMP_DIR/remote-write.yaml"; then
  echo "remoteWriteUrl alone must not also enable the default OTLP metrics exporter" >&2
  exit 1
fi
echo "OK remoteWriteUrl alone renders without the default OTLP endpoint"

if render --set observability.logs.components.enabled=true \
  --set-string observability.logs.components.hostPath=// \
  > "$TMP_DIR/unsafe-path.yaml" 2> "$TMP_DIR/unsafe-path.err"; then
  echo "host root must not be mounted for component logs" >&2
  exit 1
fi
grep -Fq 'hostPath must not be the host root' "$TMP_DIR/unsafe-path.err"
echo "OK unsafe component log host path rejected"

# Grafana reads dashboards from the chart copy; CubeObservability owns the source.
REPO_ROOT="$(dirname "$(dirname "$(dirname "$CHART_DIR")")")"
for dashboard in cubesandbox-cluster cubesandbox-node cubesandbox-sandbox cubesandbox-logs cubesandbox-logs-elasticsearch; do
  src="$REPO_ROOT/CubeObservability/grafana/dashboards/$dashboard.json"
  chart="$CHART_DIR/files/cube-observability/grafana/dashboards/$dashboard.json"
  if ! cmp -s "$src" "$chart"; then
    echo "dashboard drift: $src and $chart differ" >&2
    exit 1
  fi
  python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$src"
done
grep -Fq 'Component logs (Kubernetes Helm opt-in)' \
  "$CHART_DIR/files/cube-observability/grafana/dashboards/cubesandbox-logs.json"
grep -Fq 'Component logs (Kubernetes Helm opt-in)' \
  "$CHART_DIR/files/cube-observability/grafana/dashboards/cubesandbox-logs-elasticsearch.json"
echo "OK dashboards are valid JSON, in sync, and expose the component-log row"
