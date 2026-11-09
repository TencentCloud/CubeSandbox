# CubeObservability

CubeSandbox sandbox observability deployment and operations.

English | [简体中文](../zh/guide/observability.md)

Observability is opt-in for one-click and Kubernetes installs. Enable it in the
one-click environment file or in Helm values; there are no extra installer flags.

Collected by default:

- Metrics: the `cubesandbox_*` resource metrics from Cubelet at
  `127.0.0.1:9998/v1/metrics/resource`.
- Logs: sandbox `stdout` and `stderr` at
  `/data/cubelet/log/<sandbox-id>/{stdout,stderr}`.

Helm can additionally collect CRI logs for an allowlist of CubeSandbox
containers under `/var/log/pods`. This optional Collector DaemonSet is disabled
by default and is available only through the Helm chart, not the one-click/systemd
installer.

Prometheus, Grafana, and one log backend (Loki by default, or Elasticsearch)
ship with both installers. Both can also export to external backends.

Ownership:

- Grafana image and dashboards: `CubeObservability/`;
- one-click CVM/systemd: `deploy/one-click/observability/`;
- Helm Collector and backends: `deploy/kubernetes/chart/`.

## 1. Capabilities, versions, and preflight

### 1.1 Cubelet capability baseline

This solution uses the following data from the current Cubelet:

- Sandbox CPU and memory metrics from `/v1/metrics/resource`. Each per-sandbox
  series carries `sandbox_id`; `guest_workload` series also carry
  `container_id`.
- The node-level `cubesandbox_node_sandbox_count` and
  `cubesandbox_node_sandbox_available_count` gauges. Both return `0` when no
  sandbox is active.
- Sandbox `stdout` and `stderr` logs from
  `/data/cubelet/log/<sandbox-id>/{stdout,stderr}`.

For the complete metric names, types, units, and labels, see the
[CubeSandbox resource metrics guide on GitHub](https://github.com/tencentcloud/CubeSandbox/blob/master/docs/guide/resource-metrics.md).
The `host_sandbox` series carry `sandbox_id`; `guest_workload` series carry both
`sandbox_id` and `container_id`. The
[Cubelet Prometheus source](https://github.com/tencentcloud/CubeSandbox/blob/master/Cubelet/plugins/cube/internals/resourcemetrics/prometheus.go)
is the implementation reference. Node-count gauges have no dynamic labels and
do not contain a list of Sandbox IDs.

#### Preflight

Run these checks after Cubelet is installed. For a new one-click environment,
complete the first-node steps in [2.1](#21-one-click) first:

```bash
# Cubelet version and service
/usr/local/services/cubetoolbox/Cubelet/bin/cubecli version
sudo systemctl is-active cube-sandbox-cubelet.service

# Sandbox resource endpoint and per-sandbox ID label
curl -fsS http://127.0.0.1:9998/v1/metrics/resource | \
  grep -E '^cubesandbox_(host_sandbox|guest_workload|node_sandbox_)'
curl -fsS http://127.0.0.1:9998/v1/metrics/resource | \
  grep 'sandbox_id='

# Standard sandbox stdout/stderr log path while a sandbox is active
SANDBOX_ID=019abc-example
ls -l "/data/cubelet/log/${SANDBOX_ID}"/{stdout,stderr}
```

The `sandbox_id` check can be empty when no sandbox is active; both
`cubesandbox_node_sandbox_*` gauges should still return `0`. A disabled plugin
or pending first sample can also produce an empty result, so interpret it
together with the plugin configuration and service state.

### 1.2 Installer versions and external compatibility baselines

Loki and Elasticsearch are mutually exclusive sandbox log backends. Loki is
installed by default; Elasticsearch is installed only when explicitly selected
with `CUBE_LOGS_BACKEND=elasticsearch` (or
`observability.logs.backend=elasticsearch` for Helm). Only one log backend is
active in a deployment, and logs are not duplicated.

| Component | Installer-pinned and validated version | External/self-managed baseline | Default port | Role and compatibility notes |
| --- | --- | --- | --- | --- |
| OTel Collector Contrib | `0.162.0` | Prefer `0.162.x` | `127.0.0.1:13133` (health) | Per-node collector. The generated YAML uses the OTLP/HTTP and Elasticsearch exporter configuration from this release. Arbitrary later releases are not implicitly supported; rerun `otelcol validate` after any upgrade. |
| Prometheus | one-click `3.13.4`; Helm `v3.2.1` | Prometheus `3.x` with OTLP and Remote Write receivers | `9092` | Sandbox metrics backend. `--web.enable-otlp-receiver`, `--web.enable-remote-write-receiver`, and the documented `otlp` configuration are required. |
| Loki (default log backend; choose one) | `3.7.9` | Loki `3.x` with native OTLP and structured metadata enabled | `3100` | Default sandbox logs backend. The Collector writes to `/otlp`; Grafana queries it with LogQL. |
| Elasticsearch (optional log backend; choose one) | `8.17.3` | Elasticsearch `8.x` | `9200` | Replaces Loki when explicitly selected; uses raw mapping and the Bulk API. Validate other major releases separately. |
| Grafana OSS | `10.4.18` | `>= 10.4` with dashboard schema `39` support | `3001` | Metrics and logs UI. Validate the selected log data source, three metrics dashboards, and logs dashboard before upgrading. |

Pinned versions keep bundled installs reproducible. An external baseline is not
a guarantee for every future release.

Upstream configuration references:
[Prometheus receiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/receiver/prometheusreceiver),
[filelog receiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/pkg/stanza/operator/input/file),
[Elasticsearch exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/elasticsearchexporter),
[Prometheus OTLP ingestion](https://prometheus.io/docs/guides/opentelemetry/), and
[Loki OTLP ingestion](https://grafana.com/docs/loki/latest/send-data/otel/).

The bundled CVM/systemd installers require 64-bit Linux, systemd, `curl`,
`tar`, and `python3`; Python is used to extract and validate the Loki release
archive safely. Kubernetes and Helm do not require these host binaries.

### 1.3 Loki or Elasticsearch

- **Loki (default):** lower resource overhead, suited to quick deployment and
  primarily label-based log queries.
- **Elasticsearch (optional):** suited to teams with an existing ES operations
  model or a need for full-text search.
- Choose one. In production, prefer an existing team-managed or hosted log
  platform.

Select the backend in `.env` for a new installation:

```dotenv
CUBE_LOGS_BACKEND=loki
# Or:
# CUBE_LOGS_BACKEND=elasticsearch
```

Learn more in the
[Loki documentation](https://grafana.com/docs/loki/latest/),
[Loki OTLP ingestion guide](https://grafana.com/docs/loki/latest/send-data/otel/),
[Elasticsearch logs documentation](https://www.elastic.co/docs/solutions/observability/logs),
and [Elasticsearch Query DSL](https://www.elastic.co/docs/explore-analyze/query-filter/languages/querydsl).

## 2. Enable with a standard deployment

| Deployment | Entrypoint | Enable |
| --- | --- | --- |
| first one-click node (all-in-one by default) | `deploy/one-click/install.sh` | Set `ONE_CLICK_ENABLE_OBSERVABILITY=1`, then run the normal installer |
| one-click compute node | `deploy/one-click/install-compute.sh` | Set `ONE_CLICK_ENABLE_OBSERVABILITY=1` and the first-node address; only the node Collector is installed |
| existing one-click deployment | original `install.sh` / `install-compute.sh` | Update the original `.env`, then run with `--mode=upgrade --yes`; see [2.1 C](#c-enable-observability-in-an-existing-one-click-deployment) |
| Kubernetes Helm | `deploy/kubernetes/chart` | Pass `--set observability.enabled=true` |

### 2.1 one-click

one-click keeps using its existing `.env` and installer entrypoints, and
observability remains disabled by default. The first node is all-in-one by
default. Additional compute nodes send metrics and logs to that first node;
there is no separate monitoring control plane.

#### A. Install the first all-in-one node

1. Enter `deploy/one-click` in a source checkout, or the one-click root of an
   extracted release. Copy the template only for a fresh installation:

```bash
# Source checkout:
cd deploy/one-click
# Extracted release:
# cd /path/to/cube-sandbox-one-click
[[ -f .env ]] || cp env.example .env
chmod 600 .env
```

2. Choose a private IP reachable from later compute nodes, such as
   `10.0.0.10`, and set:

```bash
# .env
CUBE_SANDBOX_NODE_IP=10.0.0.10
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod
CUBE_LOGS_BACKEND=loki
CUBE_GRAFANA_ADMIN_PASSWORD='<random-password-at-least-16-chars>'
```

   The installer rejects an unset or shorter Grafana password. Set
   `CUBE_LOGS_BACKEND=elasticsearch` to use Elasticsearch. The installer
   can detect `CUBE_SANDBOX_NODE_IP` when it is omitted, but confirm the
   resulting address after installation.

3. Run the normal installer:

```bash
sudo ./install.sh
```

4. Record the final first-node address. Additional compute nodes must use this
   reachable private IP:

```bash
grep '^CUBE_SANDBOX_NODE_IP=' \
  /usr/local/services/cubetoolbox/.one-click.env
```

5. Allow only the required network paths:

   - compute nodes to the first node: TCP `9092`, plus `3100` for Loki or
     `9200` for Elasticsearch;
   - administrators to the first node: Grafana `3001`;
   - never expose `9092/3100/9200` directly to the public Internet.

6. Verify the first node:

```bash
sudo /usr/local/services/cubetoolbox/observability/install.sh status
sudo systemctl --no-pager --full status cube-sandbox-otelcol.service
curl -fsS http://127.0.0.1:9092/-/ready
curl -fsS http://127.0.0.1:3100/ready
# Elasticsearch users replace the previous line with:
# curl -fsS http://127.0.0.1:9200/
```

   Open Grafana at `http://10.0.0.10:3001` and check the
   **CubeSandbox sandbox resources** and **CubeSandbox sandbox logs**
   dashboards. Per-sandbox series and logs can be empty when no sandbox is
   active.

#### B. Install additional compute nodes

1. Prepare the same one-click release and an `.env` on each compute node:

```bash
[[ -f .env ]] || cp env.example .env
chmod 600 .env
```

2. Set the first-node address and keep the cluster ID and log backend identical
   to the first node:

```bash
# .env
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.10
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod
CUBE_LOGS_BACKEND=loki
```

   `ONE_CLICK_CONTROL_PLANE_IP` is the first CubeSandbox node's private IP,
   not a newly created monitoring node. `install-compute.sh`
   exports `ONE_CLICK_DEPLOY_ROLE=compute` automatically, so do not repeat it
   in `.env`. With no explicit external endpoints, the installer derives the
   Prometheus and selected log-backend addresses from this IP.

3. Install and inspect the local Collector:

```bash
sudo ./install-compute.sh
sudo systemctl --no-pager --full status cube-sandbox-otelcol.service
sudo journalctl -u cube-sandbox-otelcol.service -n 100 --no-pager
```

4. Create a test sandbox, then query by `node_id` and `sandbox_id` in Grafana on
   the first node. If no data arrives, first check local `127.0.0.1:9998`, the
   log path, and connectivity to the required first-node ports.

#### C. Enable observability in an existing one-click deployment

Use a one-click release that contains the observability deployment files and
keep maintaining the `.env` in the original installation directory. Do not
overwrite it with `env.example`, and do not edit the generated
`.one-click.env` directly.

1. Add or update these values in `.env` on the first node:

```dotenv
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod
CUBE_LOGS_BACKEND=loki
CUBE_GRAFANA_ADMIN_PASSWORD='<random-password-at-least-16-chars>'
```

2. Run the original one-click entrypoint in upgrade mode:

```bash
sudo ./install.sh --mode=upgrade --yes
```

3. After verifying the first node, add or update these values in `.env` on
   every compute node:

```dotenv
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.10
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod
CUBE_LOGS_BACKEND=loki
```

```bash
sudo ./install-compute.sh --mode=upgrade --yes
```

Replace `10.0.0.10` with the first node's reachable private IP, and use the
same cluster ID and log backend on every node. These commands reconcile the
one-click stack and observability components from the original `.env`; back up
the configuration through the normal operations process before running them.
On upgrade, a wholesale copy of `env.example` with its default
`ONE_CLICK_ENABLE_OBSERVABILITY=0` does not disable an already enabled stack.
To explicitly disable it, run
`sudo env ONE_CLICK_ENABLE_OBSERVABILITY=0 ./install.sh --mode=upgrade --yes`
(or use `./install-compute.sh` on a compute node).

#### D. Online installation

Online installation follows the same environment-variable forwarding style as
bundled MinIO:

```bash
# First all-in-one node. Bundled Grafana requires an admin password of at least
# 16 characters; store the generated value in your password manager.
GRAFANA_ADMIN_PASSWORD="$(openssl rand -hex 16)"
echo "Grafana admin password: ${GRAFANA_ADMIN_PASSWORD}"
curl -fsSL https://raw.githubusercontent.com/tencentcloud/CubeSandbox/master/deploy/one-click/online-install.sh \
  | sudo env ONE_CLICK_ENABLE_OBSERVABILITY=1 \
      CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod \
      CUBE_LOGS_BACKEND=loki \
      CUBE_GRAFANA_ADMIN_PASSWORD="${GRAFANA_ADMIN_PASSWORD}" \
      bash

# Compute node
curl -fsSL https://raw.githubusercontent.com/tencentcloud/CubeSandbox/master/deploy/one-click/online-install.sh \
  | sudo env ONE_CLICK_DEPLOY_ROLE=compute \
      ONE_CLICK_CONTROL_PLANE_IP=10.0.0.10 \
      ONE_CLICK_ENABLE_OBSERVABILITY=1 \
      CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod \
      CUBE_LOGS_BACKEND=loki \
      bash
```

`online-install.sh` downloads the release and invokes the same `install.sh`; it
is not a separate deployment topology. For authentication tokens and other
secrets, prefer downloading the release and storing values in a mode-`0600`
`.env` rather than leaving secrets in shell history.

#### E. External backends

```dotenv
ONE_CLICK_OBSERVABILITY_ROLE=auto
CUBE_LOGS_BACKEND=loki
CUBE_METRICS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:9092/api/v1/otlp
CUBE_LOGS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:3100/otlp
```

With explicit external endpoints, `auto` installs only the node Collector, not
local backends. See
[4.1](#41-core-environment-variables-and-export-authentication) for
external-backend authentication.

### 2.2 Kubernetes Helm

Deploy the in-cluster backend and the compute-node Collector:

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system --create-namespace \
  -f /path/to/runtime-values.yaml \
  --set observability.enabled=true \
  --set observability.clusterId=k8s-prod \
  --set observability.logs.backend=loki
```

The chart generates a release-scoped Grafana admin Secret with a random password
and reuses it on upgrades. Read it from the cluster instead of passing
credentials on the command line:

```bash
kubectl get secret -n cube-system \
  -l app.kubernetes.io/component=grafana-admin \
  -o jsonpath='{.items[0].data.admin-password}' | base64 --decode; echo
```

Set `observability.backend.grafana.existingSecret` to a Secret with `admin-user`
and `admin-password` to manage credentials outside Helm. An inline
`adminPassword` must be at least 16 characters.

To also collect component container logs, add
`--set observability.logs.components.enabled=true`, or set it in
`runtime-values.yaml`:

```yaml
observability:
  logs:
    components:
      enabled: true
```

This runs a second Collector that tails only the allowlisted containers
(`observability.logs.components.containers`, default: master, API, Ops,
TemplateCenter, Cubelet, proxy, lifecycle manager) in the release namespace,
from a read-only `/var/log/pods` mount, on nodes matching
`observability.logs.components.nodeSelector`, affinity, and tolerations.
Collection starts at the file end, so existing history is not replayed. Query it
in Grafana with `service_name="cubesandbox-cube-master"`, or use the
**Component logs** row of the logs dashboard. Check component logs for secrets
before enabling, and add redaction when needed.

Collector, Prometheus, log backend, and Grafana Pods carry readiness/liveness
probes, so Kubernetes gates rollout and restarts unhealthy monitoring Pods. This
is not alerting; no Alertmanager rules are installed.

Deploy only the Collector and send data to existing backends:

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system --create-namespace \
  -f /path/to/runtime-values.yaml \
  --set observability.enabled=true \
  --set observability.backend.enabled=false \
  --set observability.clusterId=k8s-prod \
  --set observability.metrics.otlpEndpoint="http://10.0.0.10:9092/api/v1/otlp" \
  --set observability.logs.backend=loki \
  --set observability.logs.otlpEndpoint="http://10.0.0.10:3100/otlp"
```

Use the same `runtime-values.yaml` as a normal chart installation; it must
contain non-default database/Redis credentials and placement settings.
Observability values are additions to that existing install command.
For Elasticsearch, set `observability.logs.backend=elasticsearch` and, for an
external backend, `observability.logs.esEndpoint`. Do not set both log
endpoints.

For authenticated external backends, create a Secret in the release namespace
with your existing secret-management workflow and reference it from
`runtime-values.yaml`. Store the complete `Authorization` header value (for
example `Bearer ...` or Elasticsearch `ApiKey ...`) and do not pass credentials
through `--set`:

```yaml
# Example only. Create these Secrets through the cluster's secret-management workflow.
apiVersion: v1
kind: Secret
metadata:
  name: cube-observability-auth
  namespace: cube-system
type: Opaque
stringData:
  metrics-authorization: "Bearer <metrics-token>"
  metrics-tenant-id: "cube-prod"
  logs-authorization: "Bearer <logs-token>"
  logs-tenant-id: "cube-prod"
---
apiVersion: v1
kind: Secret
metadata:
  name: cube-grafana-admin
  namespace: cube-system
type: Opaque
stringData:
  admin-user: "admin"
  admin-password: "<strong-password>"
```

For an Elasticsearch API key, set `logs-authorization` to `ApiKey <key>`.
Remove a tenant key when the backend does not require that header.

Reference those Secret keys from `runtime-values.yaml`:

```yaml
observability:
  metrics:
    auth:
      existingSecret: cube-observability-auth
      authorizationKey: metrics-authorization
      tenantIdKey: metrics-tenant-id # optional; empty disables X-Scope-OrgID
  logs:
    auth:
      existingSecret: cube-observability-auth
      authorizationKey: logs-authorization
      tenantIdKey: logs-tenant-id # set "" when the backend needs no tenant header
  backend:
    grafana:
      existingSecret: cube-grafana-admin
      adminUserKey: admin-user
      adminPasswordKey: admin-password
```

HTTPS endpoints use the container system CA. The chart does not yet mount a
custom CA Secret; extend the CA mount for a private-CA deployment instead of
disabling certificate verification.

The Helm Collector always uses the host network and scrapes the same-node
Cubelet at `127.0.0.1:9998`. The built-in topology therefore requires the
default `cubeNode.hostNetwork=true`. The chart does not currently discover a
reliable per-node metrics target when Cubelet is moved to the Pod network.

Common values:

| Value | Default | Meaning |
| --- | --- | --- |
| `observability.enabled` | `false` | Observability master switch |
| `observability.clusterId` | `cubesandbox` | Cluster label for metrics and logs |
| `observability.otelCollector.scrapeInterval` | `15s` | Cubelet scrape interval; it must not be shorter than the fixed `10s` scrape timeout |
| `observability.otelCollector.cubeletMetricsTarget` | `127.0.0.1:9998` | Node-local Cubelet endpoint; the default requires `cubeNode.hostNetwork=true` |
| `observability.otelCollector.stateHostPath` | `/var/lib/cube-sandbox/otelcol-k8s` | Per-node persistent filelog offsets, queues, and WAL |
| `observability.backend.enabled` | `true` | Deploy Prometheus, the selected log backend, and Grafana with the Collector |
| `observability.metrics.otlpEndpoint` | `""` | External Prometheus OTLP/HTTP base endpoint |
| `observability.metrics.remoteWriteUrl` | `""` | External Prometheus Remote Write URL; alternative to OTLP |
| `observability.metrics.auth.existingSecret` | `""` | Secret containing metrics Authorization / tenant headers |
| `observability.logs.enabled` | `true` | Collect sandbox `stdout`/`stderr` logs |
| `observability.logs.components.enabled` | `false` | Collect allowlisted component CRI logs from `/var/log/pods` with a second DaemonSet |
| `observability.logs.components.hostPath` | `/var/log/pods` | Absolute, non-root host directory holding the pod log tree |
| `observability.logs.components.containers` | Cube component list | Container-name allowlist; only these directories are tailed |
| `observability.logs.components.nodeSelector` / `affinity` / `tolerations` | Linux nodes; control/compute labels and taints | Placement of the component-log Collector |
| `observability.logs.backend` | `loki` | Log backend: `loki` or `elasticsearch` |
| `observability.logs.otlpEndpoint` | `""` | External OTLP/HTTP base endpoint for `backend=loki` |
| `observability.logs.esEndpoint` | `""` | External Elasticsearch endpoint for `backend=elasticsearch` |
| `observability.logs.auth.existingSecret` | `""` | Secret containing logs Authorization / tenant headers |
| `observability.backend.<component>.persistence.enabled` | `true` | Use a PVC for built-in Prometheus, log backend, and Grafana |
| `observability.backend.grafana.adminPassword` | `""` | Empty generates a random password in a release-scoped Secret; inline values need 16+ characters |
| `observability.backend.grafana.existingSecret` | `""` | Grafana admin Secret (`admin-user` / `admin-password`); preferred in production |

## 3. Standalone installation (optional)

The observability components can also be installed without a one-click or Helm
release. The standalone installer manages `cube-sandbox-otelcol`, Prometheus,
Loki/Elasticsearch, and Grafana, and does not touch CubeMaster, Cubelet, or
other services. Run the [preflight](#preflight) first, then pick a backend node
reachable from the compute nodes.

### 3.1 Single node or first backend node

If the backend node also runs sandboxes, install both the backend and its local
Collector:

```bash
sudo env \
  CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod \
  CUBE_LOGS_BACKEND=loki \
  CUBE_GRAFANA_ADMIN_PASSWORD='REPLACE_WITH_STRONG_PASSWORD' \
  ./deploy/one-click/observability/install.sh all
```

If that node hosts only the backend and runs no sandboxes, omit the local
Collector:

```bash
sudo env \
  CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod \
  CUBE_LOGS_BACKEND=loki \
  CUBE_GRAFANA_ADMIN_PASSWORD='REPLACE_WITH_STRONG_PASSWORD' \
  ./deploy/one-click/observability/install.sh backend
```

Replace the Grafana password before running either command. To use
Elasticsearch, set `CUBE_LOGS_BACKEND=elasticsearch`. Restrict the backend
ports as described in [2.1 A](#a-install-the-first-all-in-one-node).

### 3.2 Other CVM or bare-metal compute nodes

Install the Collector on each compute node, replacing `10.0.0.10` with the
backend node's private IP:

```bash
sudo env \
  CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod \
  CUBE_METRICS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:9092/api/v1/otlp \
  CUBE_LOGS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:3100/otlp \
  ./deploy/one-click/observability/install.sh cvm
```

For Elasticsearch, remove `CUBE_LOGS_OTLP_HTTP_ENDPOINT` and use:

```bash
sudo env \
  CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod \
  CUBE_LOGS_BACKEND=elasticsearch \
  CUBE_METRICS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:9092/api/v1/otlp \
  CUBE_LOGS_ES_ENDPOINT=http://10.0.0.10:9200 \
  ./deploy/one-click/observability/install.sh cvm
```

Add the variables from
[4.1](#41-core-environment-variables-and-export-authentication) when the
external backend requires a token, tenant header, ES API key, or Basic Auth.

### 3.3 Verification and uninstall

```bash
# Full systemd all-in-one installation
sudo ./deploy/one-click/observability/install.sh status

# Collector-only systemd node
sudo systemctl --no-pager --full status cube-sandbox-otelcol.service

# Remove independently installed systemd components; data directories are retained.
sudo ./deploy/one-click/observability/install.sh uninstall all
```

For a Kubernetes release, check the Collector DaemonSets instead (names are
prefixed with the release fullname, `cube` below):

```bash
kubectl -n cube-system rollout status daemonset/cube-node-otelcol
# Only with observability.logs.components.enabled=true
kubectl -n cube-system rollout status daemonset/cube-component-otelcol
```

## 4. OTel Collector configuration

Each node runs `otelcol-contrib 0.162.0` and generates up to two independent
pipelines according to the configured destinations:

| Pipeline | Receiver | Input | Exporter |
| --- | --- | --- | --- |
| `metrics` | `prometheus` | Cubelet `/v1/metrics/resource` | `otlp_http/metrics` or `prometheus_remote_write` |
| `logs/sandbox` | `filelog/sandbox` | `/data/cubelet/log/*/{stdout,stderr}` | `elasticsearch/logs` or `otlp_http/logs` |

These names come from the systemd generator. Helm uses the equivalent
`prometheus/cubelet_resource_metrics`, `filelog/sandbox_stdio`, and `logs`
names, plus one `logs/component_<container>` pipeline per allowlisted container
in the separate component-log Collector.
Implementation details are documented in the
[OTel design](../dev/sandbox-otel-recommendation.md).

### 4.1 Core environment variables and export authentication

| Variable | Default | Example | Meaning |
| --- | --- | --- | --- |
| `CUBE_OBSERVABILITY_CLUSTER_ID` | `cubesandbox` | `cube-prod` | Cluster label written to metrics and logs |
| `CUBE_LOGS_BACKEND` | `loki` | `elasticsearch` | Log-backend selector for one-click/systemd |
| `CUBE_SANDBOX_NODE_IP` | auto-detected | `10.0.0.20` | systemd only: node label; set it when auto-detection is unsuitable |
| `CUBE_METRICS_OTLP_HTTP_ENDPOINT` | empty | `http://10.0.0.10:9092/api/v1/otlp` | OTLP/HTTP base endpoint; omit `/v1/metrics` |
| `CUBE_METRICS_REMOTE_WRITE_URL` | empty | `http://10.0.0.10:9092/api/v1/write` | Prometheus Remote Write URL; alternative to metrics OTLP |
| `CUBE_METRICS_AUTHORIZATION` / `CUBE_METRICS_REMOTE_WRITE_TOKEN` | empty | `Bearer ...` | Metrics authentication for one-click/systemd; choose a complete header or a bare token |
| `CUBE_METRICS_TENANT_ID` | empty | `cube-prod` | Metrics `X-Scope-OrgID` |
| `CUBE_LOGS_ES_ENDPOINT` | empty | `http://10.0.0.10:9200` | Elasticsearch endpoint |
| `CUBE_LOGS_ES_INDEX` | `cubesandbox-logs` | `cube-prod-logs` | Elasticsearch index |
| `CUBE_LOGS_ES_USER` / `CUBE_LOGS_ES_PASSWORD` | empty | `cube` / `...` | one-click/systemd: ES Basic Auth; both values are required |
| `CUBE_LOGS_ES_API_KEY` | empty | `...` | one-click/systemd: ES API key; alternative to Basic Auth |
| `CUBE_LOGS_ES_TLS_INSECURE_SKIP_VERIFY` | `false` | `true` | Skip ES TLS verification for temporary diagnostics only |
| `CUBE_LOGS_OTLP_HTTP_ENDPOINT` | empty | `http://10.0.0.10:3100/otlp` | OTLP/HTTP logs base endpoint; alternative to Elasticsearch |
| `CUBE_LOGS_AUTHORIZATION` / `CUBE_LOGS_TOKEN` | empty | `Bearer ...` | Log authentication for one-click/systemd; choose a complete header or a bare token |
| `CUBE_LOGS_TENANT_ID` | empty | `cube-prod` | Logs `X-Scope-OrgID` |
| `CUBE_LOGS_SANDBOX_ID` | `*` | `019abc-example` | Optional sandbox ID filter for the systemd collector |
| `CUBE_LOGS_FILE_START_AT` | `beginning` | `end` | systemd only: initial file position, either `beginning` or `end` |

The bundled Grafana provisioning reuses these metrics/log credentials for its
datasources. Running `grafana/install.sh dashboards` creates or updates
datasources by UID.

Helm uses the Kubernetes Node name as the node label and does not read these
systemd-only variables. Its fixed
`start_at: beginning` applies only when no saved offset exists. Collector state
is persisted per node at `/var/lib/cube-sandbox/otelcol-k8s`, so Pod replacement
on the same node resumes from `file_storage` instead of rereading existing files.

One-click users can use the `Observability` section already present in
`deploy/one-click/env.example`. After copying it to `.env` as shown in
[2.1](#21-one-click), uncomment optional settings only when needed:

```dotenv
ONE_CLICK_ENABLE_OBSERVABILITY=1
CUBE_OBSERVABILITY_CLUSTER_ID=cube-prod
CUBE_LOGS_BACKEND=loki

# The standard all-in-one/compute topology derives these automatically.
# Set them only for a self-managed or external backend.
CUBE_METRICS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:9092/api/v1/otlp
CUBE_LOGS_OTLP_HTTP_ENDPOINT=http://10.0.0.10:3100/otlp
```

Configure at least one metrics or logs destination. Do not configure both
metrics destinations or both logs destinations at the same time. The standard
one-click topology fills in its bundled backend endpoints automatically. For
Elasticsearch, use `CUBE_LOGS_BACKEND=elasticsearch`,
`CUBE_LOGS_ES_ENDPOINT=http://10.0.0.10:9200`, and optionally
`CUBE_LOGS_ES_INDEX`.

#### Authentication in every topology

The supported one-click and Kubernetes workflows can export to authenticated
**external** metrics and log backends. A minimal Bearer example is:

```dotenv
CUBE_METRICS_OTLP_HTTP_ENDPOINT=https://prometheus.example.com/api/v1/otlp
CUBE_METRICS_AUTHORIZATION='Bearer <metrics-token>'
CUBE_LOGS_OTLP_HTTP_ENDPOINT=https://loki.example.com/otlp
CUBE_LOGS_AUTHORIZATION='Bearer <logs-token>'
```

- the one-click/systemd installer reads these variables from the node's `.env`
  file;
- Helm references a Secret through
  `observability.metrics.auth.existingSecret` and
  `observability.logs.auth.existingSecret`; see [2.2](#22-kubernetes-helm);
- Elasticsearch uses `CUBE_LOGS_ES_API_KEY`, or the paired
  `CUBE_LOGS_ES_USER` and `CUBE_LOGS_ES_PASSWORD`.

An `*_AUTHORIZATION` value is the complete single-line header. An `*_TOKEN`
value is a bare token that the installer converts to a Bearer header; do not
set both forms. These settings configure Collector/Grafana client headers.
They do not enable server-side authentication on bundled Prometheus, Loki, or
Elasticsearch. Put an authentication gateway or reverse proxy in front of a
bundled backend, or use a managed backend.

Official references:
[OTLP HTTP exporter](https://github.com/open-telemetry/opentelemetry-collector/tree/main/exporter/otlphttpexporter),
[Prometheus Remote Write exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/prometheusremotewriteexporter),
[Loki authentication](https://grafana.com/docs/loki/latest/operations/authentication/),
[Elasticsearch API keys](https://www.elastic.co/docs/deploy-manage/api-keys),
and [Kubernetes Secrets](https://kubernetes.io/docs/concepts/configuration/secret/).

Never commit real credentials. Set `.env` to mode `0600`. HTTPS uses the system
CA by default; install or mount a private CA instead of disabling verification.

### 4.2 Config locations and rendering

`render` and installation use the same generator. Values already configured in
the environment are written into the generated YAML, so the YAML does not need
a second manual edit. The one-click wrapper loads `.env` automatically; for a
standalone run, export the variables first or pass them with `env`.

| Topology | Installed configuration | Render with configured values |
| --- | --- | --- |
| systemd | `/etc/cube-sandbox/otelcol/otelcol.yaml` | `./deploy/one-click/observability/install.sh render cvm` |
| Helm | ConfigMaps `<release>-node-otelcol-config` and `<release>-component-otelcol-config` | Use the `helm template` command below |

Helm uses `observability.*` values from `values.yaml` or `--set`; it does not
read `CUBE_*` environment variables automatically.

Even a render-only Helm command must satisfy the chart-wide credential
validation with non-default placeholders:

```bash
helm template cube ./deploy/kubernetes/chart \
  --set observability.enabled=true \
  --set-string mysql.password=render-only \
  --set-string mysql.rootPassword=render-only \
  --set-string redis.password=render-only \
  -s templates/observability.yaml
```

Use `render-only` only for local rendering. Supply real credentials through
secret values for an actual installation.

### 4.3 Generated configuration example

The excerpt below assumes the example values from 4.1 and node address
`10.0.0.20`, sending metrics to Prometheus and logs to Loki. It omits some exporter discover/queue/retry and
`service.telemetry` fields for readability; use `render` for the complete
output. Comments explain environment-variable mappings and are not guaranteed
to appear in the generated file.

This is generated output, not a second configuration source. `render` and
`install` substitute the configured environment values automatically. Rerun
the installation after changing those values.

```yaml
receivers:
  # Scrape sandbox resource metrics from the local Cubelet only.
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
  # CUBE_LOGS_SANDBOX_ID replaces "*"; the default collects every sandbox.
  filelog/sandbox:
    include:
      - /data/cubelet/log/*/stdout
      - /data/cubelet/log/*/stderr
    start_at: beginning # CUBE_LOGS_FILE_START_AT
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

processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 256
  # Values come from CUBE_OBSERVABILITY_CLUSTER_ID and
  # CUBE_SANDBOX_NODE_IP (auto-detected when unset).
  resource/metrics:
    attributes:
      - {key: cluster, value: "cube-prod", action: upsert}
      - {key: node, value: "10.0.0.20", action: upsert}
      - {key: cubesandbox.cluster.id, value: "cube-prod", action: upsert}
      - {key: host.name, value: "10.0.0.20", action: upsert}
  resource/logs_common:
    attributes:
      - {key: host.name, value: "10.0.0.20", action: upsert}
      - {key: cubesandbox.cluster.id, value: "cube-prod", action: upsert}
  attributes/logs_common:
    actions:
      - {key: host_name, value: "10.0.0.20", action: upsert}
      - {key: cubesandbox_cluster_id, value: "cube-prod", action: upsert}
      - {key: cubesandbox_sandbox_id, value: "none", action: insert}
      - {key: log_iostream, value: "none", action: insert}
  resource/sandbox:
    attributes:
      - {key: service.name, value: "cubesandbox-sandbox", action: upsert}
      - {key: cubesandbox.log.source, value: "sandbox-init", action: upsert}
  attributes/sandbox:
    actions:
      - {key: service_name, value: "cubesandbox-sandbox", action: upsert}
      - {key: cubesandbox_log_source, value: "sandbox-init", action: upsert}
  batch:
    timeout: 2s
    send_batch_size: 1024

exporters:
  otlp_http/metrics:
    # CUBE_METRICS_OTLP_HTTP_ENDPOINT; the Collector appends /v1/metrics.
    endpoint: "http://10.0.0.10:9092/api/v1/otlp"
    compression: gzip
    sending_queue:
      enabled: true
      storage: file_storage
    retry_on_failure:
      enabled: true
      max_elapsed_time: 0
  otlp_http/logs:
    # CUBE_LOGS_OTLP_HTTP_ENDPOINT; the Collector appends /v1/logs.
    endpoint: "http://10.0.0.10:3100/otlp"
    compression: gzip
    sending_queue:
      enabled: true
      storage: file_storage
    retry_on_failure:
      enabled: true
      max_elapsed_time: 0

extensions:
  health_check:
    endpoint: 127.0.0.1:13133
  # Persist file offsets and the OTLP sending queue across process restarts.
  file_storage:
    directory: /var/lib/cube-sandbox/otelcol/storage
    create_directory: true

service:
  extensions: [health_check, file_storage]
  pipelines:
    # Metrics and logs are independent pipelines.
    metrics:
      receivers: [prometheus]
      processors: [memory_limiter, resource/metrics, batch]
      exporters: [otlp_http/metrics]
    logs/sandbox:
      receivers: [filelog/sandbox]
      processors: [memory_limiter, resource/logs_common, resource/sandbox, attributes/logs_common, attributes/sandbox, batch]
      exporters: [otlp_http/logs]
```

With Elasticsearch selected, the log exporter becomes `elasticsearch/logs`;
its endpoint and index come from `CUBE_LOGS_ES_ENDPOINT` and
`CUBE_LOGS_ES_INDEX`. The receiver, field extraction, and pipeline stay the
same.

### 4.4 Supported rules and recommended settings

#### Metric and log collection rules

| Type | Supported rule | Configuration |
| --- | --- | --- |
| Sandbox metrics | Scrape only the local Cubelet `/v1/metrics/resource`; includes sandbox CPU, memory, and node sandbox counts | Cluster/node labels and either an OTLP or Remote Write destination; Helm also exposes `observability.otelCollector.scrapeInterval` |
| Sandbox logs | Read `/data/cubelet/log/<sandbox-id>/{stdout,stderr}` line by line and extract the sandbox ID and stream from the path | Send to Loki over OTLP by default, or to Elasticsearch; systemd also exposes `CUBE_LOGS_SANDBOX_ID` and `CUBE_LOGS_FILE_START_AT` |
| Component logs (Helm opt-in) | Read CRI/containerd logs only for `observability.logs.components.containers` from `/var/log/pods`; start at end and use one pipeline per component | Separate read-only component-log Collector DaemonSet on matching control/compute nodes; one-click does not support this option |
| State and retry | Use `file_storage` for file offsets and OTLP queues, a WAL for Remote Write, and an in-memory queue with at most five retries for Elasticsearch | File state lives on the host for systemd and at `/var/lib/cube-sandbox/otelcol-k8s` per Kubernetes node; component logs use a separate `-components` host path |

For the systemd collector, each original log line is kept in `Body`, with a
maximum log-record size of `1MiB`.

Both backends retain these semantic fields (Loki normalizes `.` in OTel
attribute names to `_`):

- `service_name=cubesandbox-sandbox`
- `cubesandbox_log_source=sandbox-init`
- `cubesandbox_cluster_id`
- `host_name`
- `cubesandbox_sandbox_id`
- `log_iostream=stdout|stderr`
- `log_file_path`
- the log body and timestamp

Component CRI records use `service_name=cubesandbox-<container>`,
`cubesandbox_log_source=component`, and
`cubesandbox_component_name=<container>`; they do not contain the sandbox-only
ID and stream fields above.

Loki indexes only the low-cardinality `service_name`,
`cubesandbox_cluster_id`, and `host_name` labels.
`cubesandbox_sandbox_id`, `log_iostream`, and the remaining fields stay as
structured metadata. This avoids creating one high-cardinality stream per
sandbox. Common LogQL filters:

```logql
# All sandbox stderr
{service_name="cubesandbox-sandbox"} | log_iostream="stderr"

# Error-like messages from one sandbox
{service_name="cubesandbox-sandbox"}
  | cubesandbox_sandbox_id="<sandbox-id>"
  |~ "(?i)error|fatal|panic"
```

Elasticsearch documents additionally contain `Body` and `@timestamp`. Common
Lucene filters:

```text
# All sandbox stderr
service_name:"cubesandbox-sandbox" AND log_iostream:"stderr"

  # Error-like messages from sandbox stdout/stderr
cubesandbox_log_source:"sandbox-init" AND Body:(ERROR OR FATAL OR panic)

# One sandbox
cubesandbox_sandbox_id:"<sandbox-id>"
```

#### Recommended Collector settings

| Setting | Built-in value | Recommendation |
| --- | --- | --- |
| Scrape interval / timeout | systemd: `5s / 3s`; Helm: `15s / 10s` | Keep script defaults; Helm intervals must not be shorter than the fixed `10s` timeout, and larger deployments should keep the default `15s` |
| `CUBE_LOGS_FILE_START_AT` (systemd only) | `beginning` | Keep `beginning` to read the current files in full; use `end` to collect only records written after the Collector starts |
| `CUBE_LOGS_SANDBOX_ID` (systemd only) | `*` | Keep `*` normally; specify one ID only for focused troubleshooting or temporary volume reduction |
| Memory limiter | Fixed at `256MiB`; Kubernetes memory request/limit `128Mi / 512Mi` | Start with the defaults; raising only the Pod limit does not change the limiter, which currently has no installer setting |
| Batch | `1024` records; timeout `2s` for scripts and `5s` for Helm | Keep the defaults for normal low-latency delivery |
| OTLP queue | `1000`, disk-backed with `file_storage`, retry without an elapsed-time limit | Ensure the host state directory has enough capacity |
| Elasticsearch queue | In-memory `1000`, `10` consumers, at most `5` retries | Sustained backend outages can eventually drop logs; monitor Collector logs and backend availability |
| Remote Write | WAL-backed | Ensure the WAL directory has enough capacity and monitor remote-write failures |
| Prometheus retention | one-click: `15d / 40GB`; Helm: `15d` | For one-click use `CUBE_PROMETHEUS_RETENTION_TIME` / `CUBE_PROMETHEUS_RETENTION_SIZE`; for Helm use `observability.backend.prometheus.retention`, which currently has no size value |
| Loki retention | one-click and Helm: `15d` | Use `CUBE_LOKI_RETENTION_PERIOD` for one-click or `observability.backend.loki.retention` for Helm |
| Elasticsearch heap | one-click and Helm: `1g` | Tune `CUBE_ES_HEAP_SIZE` or `observability.backend.elasticsearch.javaOpts`, normally keeping heap below 50% of available memory |

Except for settings with an environment variable or Helm value listed above,
scrape timeout, memory limiter, batch, and queue values are installer-owned
defaults and should not be maintained by editing generated files.

Helm's bundled Prometheus, selected log backend, and Grafana use PVCs by
default. Configure each component's `persistence.storageClassName`,
`existingClaim`, or size under `observability.backend`; set
`persistence.enabled=false` only for disposable validation. Use an external
managed backend when high availability is required. Bundled Loki defaults to
`15d`; configure Elasticsearch ILM separately when Elasticsearch is selected.

#### Suggested monitoring expressions

The installer provisions dashboards but does not install Alertmanager,
Prometheus alert rules, Grafana alerts, or Elasticsearch ILM. Suggested PromQL
starting points are shown below. Use `for: 5m` for the CPU, throttling, memory
ratio, and unavailable-sample conditions. Fire the memory-failure counter
immediately or use only a short delay.

```promql
# CPU usage above 80% of a finite sandbox CPU limit
100 * rate(cubesandbox_host_sandbox_cpu_usage_seconds_total[5m])
  / clamp_min(cubesandbox_host_sandbox_cpu_limit_cores, 0.001) > 80

# CPU throttled periods above 20%
100 * rate(cubesandbox_host_sandbox_cpu_throttled_periods_total[5m])
  / clamp_min(rate(cubesandbox_host_sandbox_cpu_periods_total[5m]), 0.001) > 20

# Memory usage above 85% of a finite sandbox memory limit
100 * cubesandbox_host_sandbox_memory_current_bytes
  / clamp_min(cubesandbox_host_sandbox_memory_limit_bytes, 1) > 85

# Any memory-limit failure during the last five minutes
increase(cubesandbox_host_sandbox_memory_failures_total[5m]) > 0

# A sandbox exists but has no available resource sample
cubesandbox_node_sandbox_count
  - cubesandbox_node_sandbox_available_count > 0
```

CPU and memory ratios only apply when a finite resource-limit series exists.

### 4.5 Extending collection to component logs

Helm already collects allowlisted component container logs (see
[2.2](#22-kubernetes-helm)); `deploy/kubernetes/chart/templates/observability.yaml`
is the reference implementation. The one-click/systemd installer collects
sandbox logs only.

Never edit the installed `/etc/cube-sandbox/otelcol/otelcol.yaml` or the ConfigMap:
installation regenerates them. Change the generator instead:

- systemd: `write_otel_config()` in
  `deploy/one-click/observability/otelcol/install.sh`;
- Helm: the Collector ConfigMap and volumes in
  `deploy/kubernetes/chart/templates/observability.yaml`, behind a
  `values.yaml` switch that stays off by default.

Use one receiver and pipeline per component (`filelog/component_cubelet`,
`logs/component_cubelet`) so unrelated formats never share a parser, and set
both the OTel resource attribute and the flat Elasticsearch field:

- `service.name` / `service_name`: stable low-cardinality name, e.g.
  `cubesandbox-cubelet`;
- `cubesandbox.log.source` / `cubesandbox_log_source`: `component`;
- `cubesandbox.component.name` / `cubesandbox_component_name`: e.g. `cubelet`;
- keep the existing `cubesandbox.cluster.id`, `host.name`, `log.file.path`, and
  timestamp fields.

Constraints for any extension:

- read files with `start_at: end` and a dedicated parser for JSON or multiline
  formats; a parser failure must keep the original `Body` and must not block
  other pipelines;
- index only stable labels; keep PID, request ID, and sandbox ID as structured
  metadata;
- mount only the required host directories, read-only; never the host root;
- exclude credentials, tokens, and headers, adding a `redaction` or `transform`
  processor when needed;
- verify `file_storage` offsets across log rotation, restarts, and backend
  outages;
- update the generator, both documentation languages, the dashboards, and the
  Collector `validate` tests, and state which topologies stay unsupported.

## 5. Backends and dashboards

The bundled backend is Prometheus, Grafana, and one log backend. Grafana
provisions three separate metrics dashboards plus one logs dashboard:

- `cubesandbox-cluster.json`: sandbox resource totals for one cluster.
- `cubesandbox-node.json`: sandbox resource totals for one node in that cluster.
- `cubesandbox-sandbox.json`: one sandbox's Host Sandbox and Guest Workload metrics,
  with an optional guest-container filter.
- `cubesandbox-logs.json` (Loki, default) or
  `cubesandbox-logs-elasticsearch.json` (Elasticsearch), both under the UID
  `cubesandbox-logs`: stdout/stderr volume, log query, and a collapsed
  **Component logs** row (Helm opt-in).

Metrics selectors cascade from `cluster` to `node`, `sandbox_id`, and `container_id`.
All 24 currently exported metric families are covered; aggregate CPU rates are
cores, and memory limits are sums of exported finite limits, not physical host
capacity. Host and Guest measurements must not be added together. Missing data
is not zero, and guest rates omit windows with observed epoch changes. The Loki
sandbox filter remains a regex text input for historical IDs; Elasticsearch
selectors cascade from cluster to node to sandbox.

See [1.2](#12-installer-versions-and-external-compatibility-baselines) for
backend versions, default ports, and external-component baselines.

After a one-click/systemd installation, run
`sudo ./deploy/one-click/observability/install.sh status` to check the Collector
and backends. Check the default Loki with
`curl -fsS http://127.0.0.1:3100/ready`; for Elasticsearch, use
`curl -fsS 'http://127.0.0.1:9200/cubesandbox-logs*/_count'`. A sandbox removal
deletes its local log directory, so the Collector must continuously tail and
export logs while the sandbox exists.
