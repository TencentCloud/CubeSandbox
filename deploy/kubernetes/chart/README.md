# CubeSandbox Kubernetes/TKE Helm Chart

This chart delivers CubeSandbox on Kubernetes/TKE as chart-managed resources.

Current compute-plane shape (per compute node):

- **`cube-node` (Big Pod)**: native `apps/v1` DaemonSet; `wait-node-prep` **initContainer** (exits when ready) + `cubelet` with embedded network runtime + optional egress; Pod network (`hostNetwork=false`). Image/template changes recreate the Pod and interrupt sandboxes on that node.
- **`cube-node-installer`**: native DaemonSet that stages shim / kernel / guest into the host toolbox tree.
- **`cube-node-bootstrap`**: native DaemonSet that runs `wait-pvm-host` + `cube-node-init`, then writes `node-prep-ready`.
- **`cube-node-pvm`**: native `apps/v1` DaemonSet scheduled only via `placement.pvm` (`allow-pvm-bootstrap`); installs PVM host kernel and writes fingerprint `pvm-host-ready`. Non-PVM compute nodes never pull this image.

Control-plane vs compute scheduling uses `placement.controlPlane` and `placement.compute`. PVM host install uses `placement.pvm`. MySQL schema migration is embedded in CubeMaster. Control-plane and runtime components use separate images.

## Directory

```text
deploy/kubernetes/chart/
  Chart.yaml
  values.yaml
  templates/
```

## Documentation

Official docs (source of truth; do not keep a parallel copy under this chart):

- [Kubernetes Deployment](https://cubesandbox.com/guide/kubernetes/) — overview and install order
- [Helm Install](https://cubesandbox.com/guide/kubernetes/install) — prerequisites through `helm test`
- [Architecture](https://cubesandbox.com/guide/kubernetes/architecture) — component layering, four compute DaemonSets, DNS / Proxy / Egress, compute-only mode
- [Upgrade](https://cubesandbox.com/guide/kubernetes/upgrade) — control plane can roll; compute upgrades recreate the Big Pod and interrupt sandboxes
- [FAQ](https://cubesandbox.com/guide/kubernetes/faq) — common install and runtime issues
- Chinese: [https://cubesandbox.com/zh/guide/kubernetes/](https://cubesandbox.com/zh/guide/kubernetes/)

## Architecture

See the [Architecture](https://cubesandbox.com/guide/kubernetes/architecture) guide for component layering, the four compute DaemonSets, DNS/Proxy/Egress flows, and external control plane / compute-only mode.

## Image responsibilities

| Image | Role |
|---|---|
| `cube-pvm-host-bootstrap` | **cube-node-pvm** init. Installs/configures PVM host kernel and may reboot the node. |
| `cube-node-init` | Bootstrap DaemonSet: `wait-pvm-host` + `cube-node-init`. Loads KVM, prepares host paths, validates `/dev/kvm` and XFS. |
| `cube-wait-node-prep` | Big Pod `wait-node-prep` initContainer (poll `node-prep-ready` then exit), bootstrap write-ready, and PVM hold container. |
| `cube-shim` / `cube-kernel` / `cube-guest` / `cube-agent` | Installer DaemonSet containers; stage artifacts into `/usr/local/services/cubetoolbox` (`cube-image/`, `cube-agent/cube-agent.ext4`, …). |
| `cubelet` | Big Pod runtime container (self-stage then run; includes embedded network runtime and CubeVS tools). |
| `cube-master` | Control-plane master; embedded schema migrations. |
| `cube-api` | External E2B-compatible HTTP API. |
| `cube-ops` | Ops/admin backend (JWT) + WebUI SDK proxy to CubeMaster. |
| `cubemastercli` | Operational CLI for exec-based ops. |
| `cube-proxy` | Data-plane proxy (control-plane placement when enabled). |
| `cube-lifecycle-manager` | Sandbox auto-pause / auto-resume; discovered via Service DNS and Redis. |
| `cube-egress` / `cube-egress-net` | Transparent outbound proxy + host TPROXY helper (Big Pod sidecars). |
| `cube-webui` | WebUI static assets and OpenResty. |

## Node selection

The chart separates placement into dedicated control-plane nodes and compute
nodes. Control-plane Deployments, StatefulSets, and `cube-proxy` use
`placement.controlPlane`; `cube-node` / installer / bootstrap use
`placement.compute`. PVM host install (`cube-node-pvm`) uses `placement.pvm`
and requires `cube.tencent.com/allow-pvm-bootstrap=true` so non-PVM compute
nodes never pull the heavy PVM bootstrap image. In-cluster `*.cube.app` is
wired automatically when CubeProxy is enabled: the chart patches cluster
CoreDNS so the domain rewrites to the CubeProxy Service.

The chart refuses to render host-mutating compute components without
`placement.compute.nodeSelector`. When `bootstrap.pvmHostKernel.enabled=true`,
`placement.pvm.nodeSelector` must include `allow-pvm-bootstrap`, and that key
must **not** appear under `placement.compute`.

Default placement values:

```yaml
global:
  timezone: Asia/Shanghai
  # Non-default cluster DNS domain (empty falls back to cluster.local).
  clusterDomain: ""
  # Optional private-registry host: rewrites the official Cube TCR host
  # on Cube-owned images; other repositories are left as declared.
  imageRegistry: ""

# StorageClass — off by default so PVCs use the cluster's default
# StorageClass. Set create=true + provide provisioner to have the chart
# manage its own SC. See values-tke.yaml for the TKE CBS preset.
storageClass:
  create: false
  name: ""
  provisioner: ""
  volumeBindingMode: WaitForFirstConsumer

placement:
  controlPlane:
    nodeSelector:
      cube.tencent.com/cube-control: "true"
    tolerations:
      - key: cube.tencent.com/control
        operator: Equal
        value: "true"
        effect: NoSchedule

  compute:
    nodeSelector:
      cube.tencent.com/cube-node: "true"
    tolerations:
      - key: cube.tencent.com/compute
        operator: Equal
        value: "true"
        effect: NoSchedule

  pvm:
    nodeSelector:
      cube.tencent.com/cube-node: "true"
      cube.tencent.com/allow-pvm-bootstrap: "true"
    tolerations:
      - key: cube.tencent.com/compute
        operator: Equal
        value: "true"
        effect: NoSchedule
```

Recommended labels:

```bash
kubectl label node <control-node>   cube.tencent.com/cube-control=true   --overwrite

kubectl taint node <control-node>   cube.tencent.com/control=true:NoSchedule   --overwrite

kubectl label node <compute-node>   cube.tencent.com/cube-node=true   --overwrite

kubectl taint node <compute-node>   cube.tencent.com/compute=true:NoSchedule   --overwrite

# Only on nodes that should install the PVM host kernel:
kubectl label node <pvm-compute-node>   cube.tencent.com/allow-pvm-bootstrap=true   --overwrite
# The Helm preflight Hook writes pvm-not-ready=true:NoSchedule on
# fingerprint-unready pvm nodes. For intentional kernel/bootArgs mutate,
# see https://cubesandbox.com/guide/kubernetes/upgrade (value=maintenance).
```

The chart does not label nodes or apply role taints; prepare those before install. The PVM startup-gate taint is written by the preflight Hook when the node is not fingerprint-ready.
All chart-managed Cube containers and init containers receive `TZ` from
`global.timezone`.

## Cubelet data path

`bootstrap.nodeInit.dataCubelet.loopback.enabled` defaults to `true` so the
chart can create and mount a loopback XFS image for `/data/cubelet` during
bootstrap. Production environments that pre-provision `/data/cubelet` as XFS
can set it to `false`.

## Cube Node one-click parity

`cube-node` mirrors the one-click runtime layout:

- runtime tools are available through `/usr/local/bin/containerd-shim-cube-rs`, `/usr/local/bin/cube-runtime`, `/usr/local/bin/cubecli`, and `/usr/local/bin/cubevsmapdump`;
- `cubeNode.network.autoDetectEthName=true` auto-detects the primary host NIC and patches Cubelet `eth_name`;
- `cubeNode.network.cidr` patches Cubelet cubevs/sandbox CIDR (default `172.16.0.0/18`, chosen to avoid common cluster Service CIDR `192.168.0.0/16` while keeping a /18 pool). A Helm `pre-install`/`pre-upgrade` Hook fails fast when this range overlaps the cluster Service CIDR or existing ClusterIPs; set `cubeNode.network.cidrSkipConflictCheck=true` only if you accept that risk.

The `cube-node` Pod defaults `kubectl exec` to its `cubelet` container, where `cubecli` uses the node-local Cubelet and containerd sockets. In a multi-node cluster, select the Pod scheduled on the compute node you want to inspect:

```bash
kubectl get pods -n cube-system \
  -l app.kubernetes.io/component=cube-node -o wide
kubectl exec -n cube-system <cube-node-pod> -- cubecli ls
```

### Guest kernel (bm vs PVM)

What actually decides the guest kernel on a node:

1. **Node decision** — bootstrap writes `effective-pvm` (`1` = PVM guest, `0` = bm). Nodes with `cube.tencent.com/allow-pvm-bootstrap=true` get `1` after the PVM host is ready; others get `0`.
2. **Keep what the node already runs** — if there is no `effective-pvm` yet, an upgrade keeps the previous guest kernel (so Chart defaults cannot silently flip a PVM node to bm).
3. **Chart default for first install** — `cubeNode.pvmGuestKernel.enabled` (default `true`) only matters when the node has no prior choice; it is the first-install intent, not a force switch.

To **turn off PVM on one node**: remove the `allow-pvm-bootstrap` label. Bootstrap then writes `effective-pvm=0` and the guest kernel switches to bm. Setting `pvmGuestKernel.enabled=false` alone does **not** override a node that already runs PVM.

`bootstrap.pvmHostKernel.enabled` (default `true`) installs/configures the **host** PVM kernel on labeled nodes (`cube-node-pvm`) and may reboot. Boot args default to `nopti pti=off` (`kvm_pvm` does not support host KPTI). `cube-node-init` fail-fast checks match one-click (memory, glibc, cgroup v2 cpu, cubecow, KVM, XFS, PVM consistency): it fails if `kvm_pvm` is loaded but PVM is off for that node, or if PVM is on but the host has not booted a PVM kernel.

## Build and push images

```bash
PUSH=1 REGISTRY=cube-sandbox-int.tencentcloudcr.com/cube-sandbox IMAGE_TAG=v0.7.0 ./deploy/kubernetes/images/build-cube-images.sh
```

Cube-owned images default to `imagePullPolicy: IfNotPresent`. To pick up a
registry rebuild that reuses the same tag, change the tag, delete the local
image on the node, or override `pullPolicy: Always` for that image.

If the target registry requires authentication, create a Kubernetes
`kubernetes.io/dockerconfigjson` Secret in the release namespace and pass it to
the chart:

```yaml
imagePullSecrets:
  - name: <registry-pull-secret>
```

## Install

```bash
helm upgrade --install cube ./deploy/kubernetes/chart   -n cube-system   --create-namespace   -f <runtime-values.yaml>   --wait   --timeout 90m
```

> Do not store SSH passwords or node login credentials in this chart. Host mutation is performed through Kubernetes privileged Pods, not through SSH.

## Use third-party MySQL, PostgreSQL, or Redis

`database.driver` picks the control-plane database engine, and each engine keeps its own connection section. The section that does not match the driver is ignored, and no value is inferred across sections.

| Goal | Values |
| --- | --- |
| Built-in MySQL (default) | `database.driver: mysql`, `mysql.enabled: true`, `mysql.host: ""` |
| External MySQL | `database.driver: mysql`, `mysql.host: <addr>` (plus `mysql.port` / `user` / `password` / `database`) |
| External PostgreSQL | `database.driver: postgres`, `postgres.host: <addr>` (plus `postgres.port` / `user` / `password` / `database`) |

The chart installs the `cube-mysql` StatefulSet only when the driver is `mysql`, `mysql.enabled=true`, and `mysql.host` is empty. `mysql.rootPassword` is used only by that StatefulSet.

PostgreSQL is external-only: the chart never deploys it, so `postgres.host` is required. It then skips the built-in MySQL StatefulSet, writes `driver: postgres` into the CubeMaster config, and sets CubeOps/CubeAPI `DATABASE_URL` to a `postgresql://` URL sourced from the release Secret.

```yaml
database:
  driver: postgres
postgres:
  host: "10.0.0.10"
  port: 5432
  user: postgres
  password: "replace-me"
  database: cube_mvp
```

The chart installs `cube-redis` StatefulSet only when `redis.enabled=true` and `redis.host` is empty.
Set `redis.host` to use an existing Redis service; the chart will not install `cube-redis`.
CubeProxy registry heartbeats resolve Redis by hostname, so a redis Pod IP change self-heals.
Use an FQDN or IP literal: nginx's `resolver` does not apply search domains.

## MinIO and the S3 volume plugin

`minio.*` only installs MinIO (image, port, root user/password, bucket, PVC).
`volumeS3.*` is the source of truth for the S3 volume plugin: `volume-s3.conf`
is always rendered from effective S3 fields, never from `minio.*` as a plugin
config source.

When `minio.enabled=true` (the default) and the operator left
`volumeS3.endpoint` / `existingSecret` empty, the chart **fills** those S3
fields from the chart MinIO (in-cluster endpoint, `rootUser` / generated
password, `bucket`, path-style s3fs options) — same model as one-click
`CUBE_S3_*` after local MinIO. Leave `minio.rootPassword` empty to generate a
password on first install (reused from the existing Secret on upgrade).

To use an existing S3-compatible store, set `minio.enabled=false` and
`volumeS3.endpoint` (or `volumeS3.existingSecret`). Combining both fails render.

| Goal | Values |
| --- | --- |
| Built-in MinIO (default) | `minio.enabled: true`, leave `volumeS3.endpoint` empty |
| External S3 | `minio.enabled: false` plus `volumeS3.endpoint` / `accessKeyId` / `secretAccessKey` / `bucket` |
| Existing Secret | `minio.enabled: false` plus `volumeS3.existingSecret: <secret with key volume-s3.conf>` |
| No S3 backend | `minio.enabled: false` and leave `volumeS3.endpoint` empty |

```yaml
# Point the S3 volume plugin at your own store (MinIO off):
minio:
  enabled: false
volumeS3:
  endpoint: "https://s3.example.com"
  accessKeyId: "AKIA..."
  secretAccessKey: "..."
  bucket: "cube-volumes"
  region: us-east-1
  extraOpts: "-ouse_path_request_style"   # path-style endpoints
```

When `minio.rootPassword` is set, it must be at least 8 characters (MinIO
requirement).

## CubeMaster configuration

The `cube-master` image is built like CI from `CubeMaster/docker/Dockerfile` (repository-root context) and does not carry a Kubernetes-specific entrypoint or bundled `conf.yaml`.
The chart stores the One-click `CubeMaster/conf.yaml` at `deploy/kubernetes/chart/files/cube-master/conf.yaml`, renders MySQL/Redis values into it, creates a release-scoped Secret named `<release>-master-config`, and mounts it to `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml` (same path as one-click); `CUBE_MASTER_CONFIG_PATH` points CubeMaster to that mounted file.

CubeMaster artifact storage maps to `/data/CubeMaster/storage`, matching one-click.
The chart uses PVC-backed persistence by default so state can survive
rescheduling across dedicated control nodes:

```yaml
# Optional: pin master / mysql / redis / minio PVCs at once.
persistence:
  storageClassName: ""   # empty → cluster default SC

controlPlane:
  master:
    persistence:
      enabled: true
      hostPath: ""
      storageClassName: ""   # empty → persistence.storageClassName → cluster default
mysql:
  persistence:
    enabled: true
    hostPath: ""
    storageClassName: ""
redis:
  persistence:
    enabled: true
    hostPath: ""
    storageClassName: ""
minio:
  persistence:
    enabled: true
    hostPath: ""
    storageClassName: ""
```

Set `persistence.storageClassName` (or a component-level
`*.persistence.storageClassName`) to a specific class (e.g. `gp3` on EKS,
`premium-rwo` on GKE, `managed-csi` on AKS, `local-path`, or `cube-cbs-wffc`
via `values-tke.yaml`) if you need to pin PVCs. Empty falls back to the
cluster's default StorageClass, which works out of the box for most
self-hosted / EKS / GKE / AKS clusters. Use `hostPath` only for
single-node throwaway environments; multi-control-node deployments must
use PVCs or external MySQL / Redis / S3. `existingClaim` overrides both
`storageClassName` and `hostPath`. Warehouse blobs live in S3 (chart MinIO
by default); CubeOps unpack scratch is an `emptyDir`. `cubeOps.replicas`
may be greater than 1; the default strategy is RollingUpdate with
`maxUnavailable: 0`. If a previous release set `cubeOps.persistence`,
remove it — Helm fails render rather than silently dropping that key.

Do not confuse `storageClass.*` (whether the chart **creates** a
StorageClass) with `persistence.storageClassName` (which SC **name** PVCs
bind to).

Built-in MySQL and Redis use StatefulSet `volumeClaimTemplates` by default.
For a release named `cube`, the generated claims are owned by the StatefulSet
Pod, such as `mysql-data-cube-mysql-0` and `redis-data-cube-redis-0`.

### Tencent Cloud TKE preset

Merge `values-tke.yaml` on top of your runtime values to have the chart
provision a CBS-backed StorageClass named `cube-cbs-wffc` and pin every
PVC to it:

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -f deploy/kubernetes/chart/values-tke.yaml \
  -f runtime-values.yaml \
  -n cube-system --create-namespace
```

The preset uses `volumeBindingMode: WaitForFirstConsumer` so CBS disks
are provisioned in the same zone as the scheduled control-plane Pod on
multi-AZ TKE clusters. On non-TKE clusters do NOT include this file;
provide the cluster's own StorageClass name instead.

It also exposes CubeProxy as a `LoadBalancer` Service (CLB) with Ingress
disabled, and sets TKE CLB annotations for `pass-to-target` plus a
`CHANGE_ME_TKE_CLB_SECURITY_GROUP` sentinel on
`service.cloud.tencent.com/security-groups`. Helm fails render until you
override that value in runtime values (comma-separated for multiple SGs),
or set it to `""` if you manage CLB security groups outside Helm.

### China registry preset (`values-cn.yaml`)

Default Chart values pull Cube images from TCR **int**
(`cube-sandbox-int.tencentcloudcr.com`). Users in mainland China should layer
`values-cn.yaml` so component images, plus mirrored MySQL / Redis / MinIO / kubectl,
come from TCR **cn** (`cube-sandbox-cn.tencentcloudcr.com`):

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -f deploy/kubernetes/chart/values-cn.yaml \
  -f runtime-values.yaml \
  -n cube-system --create-namespace
```

`global.imageRegistry` (set by `values-cn.yaml`) only rewrites the official
Cube TCR hosts, so per-image overrides pointing at other hosts are left as
declared. On TKE in China, also combine with `values-tke.yaml` (StorageClass /
PVC / LoadBalancer only — it does not set `global.imageRegistry`).

## Database migration

The chart does not deliver a separate DB migration Job or image. CubeMaster owns MySQL schema migration and runs its embedded `CubeMaster/pkg/base/dao/migrate/migrations/mysql` migrations during startup.

- CubeMaster uses the configured MySQL endpoint, user, password, and database.
- The chart does not package or maintain SQL files under `files/`; do not add migration SQL copies to the chart.
- CubeMaster records applied versions in `goose_db_version` and serializes concurrent migration attempts through the migration lock implemented by CubeMaster.
- There is no chart-managed SQL data seed, and the one-click single-node seed file `sql/002_seed_single_node.sql` is intentionally not rendered by the chart. Node registration must come from real Cube Node Pods selected by `placement.compute.nodeSelector`.
- When using a third-party database, set `mysql.host` or `postgres.host` (matching `database.driver`) and ensure the configured user can create/alter tables in that database.

## cubemastercli operational CLI

`cubemastercli.enabled=true` installs a chart-managed
`<release>-cubemastercli` Deployment. The image is built like CI from
`CubeMaster/docker/Dockerfile.cubemastercli` and contains the real
`cmd/cubemastercli` binary at `/usr/local/bin/cubemastercli`; it does not
provide a wrapper or fake `ctl` command. Interactive bash sessions may use a
bashrc helper that injects `--address` / `--port` from env; non-interactive
`kubectl exec` commands should still pass those flags explicitly.

The chart injects `CUBEMASTERCLI_ADDRESS` and `CUBEMASTERCLI_PORT` from the
current CubeMaster endpoint. Because upstream `cubemastercli` does not read
environment variables as flag defaults, commands should pass those values to
the real binary:

```bash
kubectl exec -n cube-system deploy/cube-cubemastercli -- cubemastercli --help
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" cubebox list'
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" template list'
```

The `cubemastercli` image is intentionally independent from `cube-master` and
`cube-node`. It contains CLI/operator tooling only; the runtime images do not
carry this operational entry point.

## cubeopscli operational CLI

The `cubeopscli` binary (from CubeOps, for node management: list/isolate/unisolate)
is bundled inside the `cubemastercli` image. There is no separate
`<release>-cubeopscli` Deployment — both CLIs live in the same
`<release>-cubemastercli` Pod.

When CubeOps is enabled (`cubeOps.enabled` or `externalControlPlane.enabled`),
the chart injects `CUBEOPSCLI_ADDRESS` and `CUBEOPSCLI_PORT` into the
cubemastercli Pod so both CLIs are usable via `kubectl exec`:

```bash
kubectl exec -n cube-system deploy/cube-cubemastercli -- cubeopscli --help
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubeopscli --address "$CUBEOPSCLI_ADDRESS" --port "$CUBEOPSCLI_PORT" node list'
```

## Cube Proxy Node

`cube-proxy` is a Cube data-plane component. It is enabled by default to match one-click behavior and is installed, upgraded, and uninstalled with the Cube release as a control-plane Deployment.

The default TLS mode is `selfSigned`, matching the one-click mkcert-style test experience. Production environments should provide a real TLS certificate for CubeProxy. External clients reach `cubeProxy.domain` / `*.domain` through the chart Ingress (SSL passthrough; TLS still terminates in CubeProxy). The image reuses `CubeProxy/Dockerfile`; the chart does not override nginx with a Kubernetes-only configuration.

`cube-proxy` depends on chart-managed `cube-lifecycle-manager` for sandbox auto-pause / auto-resume. The chart wires nginx `$cube_sidecar_addr` to the lifecycle-manager Service, opens the proxy admin listener for in-cluster discovery, and registers each proxy replica in Redis. Do not deploy a separate cube-proxy-sidecar.

### Production TLS Secret

```yaml
cubeProxy:
  enabled: true
  domain: sandbox.example.com
  tls:
    mode: existingSecret
    existingSecret: cube-proxy-certs
    certSecretKey: tls.crt
    keySecretKey: tls.key
```

The Secret keys are mounted to the file names required by the `CubeProxy` image:

- `cube.app+3.pem`
- `cube.app+3-key.pem`

The certificate SAN should cover the sandbox domain used by CubeAPI, typically:

```text
sandbox.example.com
*.sandbox.example.com
```

Keep `controlPlane.api.sandboxDomain` and `cubeProxy.domain` consistent. Configure DNS so the domain and wildcard subdomains resolve to the CubeProxy entrypoint.

### cert-manager TLS

When cert-manager is installed in the cluster, let the chart create a `Certificate`:

```yaml
controlPlane:
  api:
    sandboxDomain: sandbox.example.com
cubeProxy:
  enabled: true
  domain: sandbox.example.com
  tls:
    mode: certManager
    certManager:
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt-prod
      dnsNames:
        - sandbox.example.com
        - "*.sandbox.example.com"
```

Wildcard public certificates usually require a DNS-01 issuer.

### Self-signed TLS for test only

For offline test environments, explicitly opt in to a chart-generated self-signed certificate:

```yaml
cubeProxy:
  enabled: true
  domain: cube.app
  tls:
    mode: selfSigned
    selfSigned:
      dnsNames:
        - cube.app
        - "*.cube.app"
        - localhost
      ipAddresses:
        - 127.0.0.1
```

This mode creates a release-scoped Secret with `tls.crt`, `tls.key`, and `ca.crt`. Import `ca.crt` into clients if browser or SDK trust is required. Do not use this mode for production.

`cube-proxy` uses `placement.controlPlane`. The chart does not create node labels.

CubeProxy runs on the **Pod network** (no `hostNetwork`). Traffic path:

1. External clients → Ingress Controller → ClusterIP Service → CubeProxy Pod
2. In-cluster clients / sandbox guests → CoreDNS rewrite → same ClusterIP Service

TLS for `cube.app` / wildcards still terminates **inside CubeProxy**. The default Ingress annotations enable nginx-ingress SSL passthrough + HTTPS backend; override `cubeProxy.ingress.className` / `annotations` for TKE CLB or other controllers. Set `cubeProxy.ingress.enabled=false` if you manage the entrypoint yourself (keep the Service as backend).

Without an Ingress / cloud LB, set `cubeProxy.service.type` / `controlPlane.api.service.type` to `NodePort` (or `LoadBalancer`) and optionally pin host ports via `cubeProxy.service.nodePorts.*` / `controlPlane.api.service.nodePort` (Kubernetes range `30000-32767`; empty keeps auto-allocation). Explicit `nodePort` values are rejected when `type` is still `ClusterIP`.

When the sandbox owner is on a compute node, CubeProxy still uses Redis routing metadata to connect to the owner `HostIP:hostPort`. The chart patches the image's default nginx listeners to the configured `cubeProxy.ports.*.containerPort` values (default `80` / `443`).

CubeProxy admin is at Pod IP:`adminPort` (default `8082`) for CLM; helm test uses the Service admin port. Probes send the admin token header.

CubeProxy reads sandbox routing metadata from Redis in nginx Lua. Because nginx
does not automatically inherit Kubernetes DNS resolution for Lua cosocket
connections, the chart renders an nginx `resolver` into
`/usr/local/openresty/nginx/conf/global/global.conf`. By default the proxy
discovers resolver addresses from the Pod `/etc/resolv.conf`, which resolves the
chart-managed `cube-redis.<namespace>.svc.cluster.local` Service name and
third-party Redis DNS names. Override only when the cluster requires explicit
DNS servers. Keep `cubeProxy.resolver.valid` well below
`lifecycleManager.heartbeatTTL` (defaults `5s` / `15s`) so a redis restart
does not look like a dead proxy.

```yaml
cubeProxy:
  resolver:
    addresses:
      - 172.18.0.10
    valid: 5s
    timeout: 5s
    ipv6: false
```

## Cluster DNS for sandbox domain

When CubeProxy is enabled, the chart patches **cluster CoreDNS** so
`cubeProxy.domain` / `*.domain` rewrite to the CubeProxy Service FQDN
(ClusterIP). Users only set the domain:

```yaml
cubeProxy:
  domain: cube.app                 # change this if you use a custom domain
  configureClusterDNS: true        # set false only if kube-system/coredns must not be patched
cubeNode:
  dns:
    sandbox:
      followNodeDns: true          # guests use node/cluster DNS (nameservers+search+options)
                                   # explicit nameservers[] overrides and disables follow-node
```

## WebUI and CubeOps

`webui.enabled=true` delivers the one-click WebUI by default and **requires** `cubeOps.enabled=true`:

- `cube-webui` image packages one-click `webui/dist` static assets;
- a chart-rendered nginx config proxies `/opsapi/` and `/cubeapi/v1/` (SDK) to the CubeOps Service (`0.0.0.0:3010` in-pod, ClusterIP);
- `/sandbox/` proxies to CubeProxy; static assets are unchanged;
- the Service listens on port `12088`, matching one-click `WEB_UI_HOST_PORT`.

Warehouse import allow-lists and tokens are the same `CUBE_OPS_WAREHOUSE_*` env vars as one-click. Set them via `cubeOps.warehouse` (empty lists omit the env so CubeOps defaults apply):

```yaml
cubeOps:
  warehouse:
    githubRepos: ["TencentCloud/CubeSandbox"]
    cnbRepos: ["CubeSandbox/CubeSandbox"]
    # Prefer a Secret for private-repo tokens:
    githubTokenSecret:
      name: my-warehouse-tokens
      key: github-token
    cnbTokenSecret:
      name: my-warehouse-tokens
      key: cnb-token
```

These render as `CUBE_OPS_WAREHOUSE_GITHUB_REPOS`, `CUBE_OPS_WAREHOUSE_CNB_REPOS`, `CUBE_OPS_WAREHOUSE_GITHUB_TOKEN`, and `CUBE_OPS_WAREHOUSE_CNB_TOKEN`. Object storage is `cubeOps.s3` (bucket `cube-ops`; endpoint and inline AK/SK fall back to `volumeS3` then chart MinIO). `cubeOps.replicas` may be greater than 1; the default strategy is RollingUpdate with `maxUnavailable: 0`.

Chart MinIO is a single StatefulSet. For warehouse HA, set `cubeOps.s3.endpoint` (and `nodeEndpoint` for compute nodes outside cluster DNS) to external S3/COS. Credentials are always injected via `secretKeyRef` (`s3-access-key-id` / `s3-secret-access-key`). `cubeOps.s3.existingSecret` must use those keys; it cannot reuse `volumeS3.existingSecret` (`volume-s3.conf`). Sharing a store can reuse the endpoint and inline AK/SK only.

When no cubeOps.s3 / volumeS3 / MinIO endpoint is set, Helm still renders; CubeOps starts and returns `501 warehouse_disabled` on warehouse routes. Helm fails when an endpoint is set without CubeOps credentials.

CubeAPI serves external E2B-compatible SDK clients.

Expose the WebUI externally by changing `webui.service.type` or by adding your platform's ingress/load balancer configuration.

## Diagnostics

One-click delivers `cube-diag` scripts on the host. The Kubernetes chart delivers the equivalent operational entry point as a ConfigMap when `diagnostics.enabled=true`:

```bash
kubectl get configmap -n cube-system cube-diagnostics -o jsonpath='{.data.cube-diag-k8s\.sh}' > /tmp/cube-diag-k8s.sh
sh /tmp/cube-diag-k8s.sh cube-system cube
```

The script collects Pods, DaemonSets, Deployments, StatefulSets, Services, Endpoints, Events, Helm values/manifests, Pod descriptions, and recent logs for Cube components into a timestamped directory.

## CubeEgress

`cubeEgress.enabled=true` runs CubeEgress inside the Cube Node Big Pod:

- `cube-egress` mounts `/etc/cube/ca` and exposes the loopback admin API on `127.0.0.1:9091` by default (`cubeEgress.adminPort` / `CUBE_EGRESS_ADMIN_PORT`);
- transparent proxy timeouts default to 300s (`cubeEgress.proxyReadTimeout` / `cubeEgress.proxySendTimeout`, `CUBE_EGRESS_PROXY_READ_TIMEOUT` / `CUBE_EGRESS_PROXY_SEND_TIMEOUT`) so LLM prefill is not cut off at 60s;
- `cube-egress-net` waits for the `cube-dev` interface, applies the upstream `CubeEgress/scripts/cube-proxy-iptables-init.sh` rules, periodically reapplies them, and removes them on Pod termination;
- CubeMaster and CubeAPI both mount the same CA Secret at `/etc/cube/ca` so template CA bake and AgentHub/OpenClaw CA injection use the same trust root.

Default CA mode is `selfSigned`; the chart creates and reuses a release-scoped Secret named `<release>-egress-ca` with:

```text
cube-root-ca.crt
cube-root-ca.key
placeholder.crt
placeholder.key
```

For production CA lifecycle control, pre-create a Secret and use:

```yaml
cubeEgress:
  enabled: true
  ca:
    mode: existingSecret
    existingSecret: cube-egress-ca
```

Do not rotate the CubeEgress CA casually: templates baked with the old CA and sandboxes trusting the old CA must be considered during rotation.

## CubeS3lvol

`cubeS3lvol.enabled=false` by default, same as one-click. Enabling it injects a `cube-s3lvol` sidecar into the Cube Node Big Pod and **recreates that Pod, interrupting sandboxes on the node** — budget about 2 CPU, 18 GiB RAM, and a 512 GiB sparse WAL per compute node (x86_64 needs AVX2).

When enabled, the sidecar:

- runs in its own `cube-s3lvol` image next to cubelet;
- shares a Unix socket (`/var/run/s3lvol/s3lvol.sock`) with cubelet over an in-memory emptyDir, and writes cubelet's `[cow.s3] enable = true` + `socket_path` (a socket path alone does not opt in);
- keeps WAL and logs on the existing `data-cubelet` / `data-log` hostPaths, so a Pod recreate loses nothing;
- reads S3 config from a chart Secret mounted at `/etc/s3lvol/s3.cfg`; an `existingSecret` must contain that key in s3lvol format (not `volume-s3.conf`);
- reuses chart MinIO or `volumeS3` endpoint and credentials by default. The bucket is `cube-s3lvol` and must not be the volume plugin's `cube-volumes` (Helm fails on a shared bucket);
- identifies the node by hashing the full Kubernetes node name (`spec.nodeName`) to `rcow-<8hex>`, so a Pod recreate is not a new machine and IP / dotted node names stay unique. `cubeS3lvol.lvsName` pins the same name on every node — do not set it when more than one node runs the sidecar;
- uses rcow's default CPU mask (`0x3`); set `cubeS3lvol.cpuMask` when cores are isolated.

```yaml
cubeS3lvol:
  enabled: true
  # Optional: explicit S3 (otherwise chart MinIO or volumeS3 is reused).
  # s3:
  #   existingSecret: my-s3lvol-cfg   # key must be s3.cfg
  #   endpoint: https://s3.example.com
  #   accessKeyId: ...
  #   secretAccessKey: ...
  #   bucket: cube-s3lvol
```

## Render and lint

```bash
helm lint ./deploy/kubernetes/chart
helm template cube ./deploy/kubernetes/chart -n cube-system > /tmp/cube-rendered.yaml
```

## Verify

```bash
kubectl get pods -n cube-system -o wide
kubectl get daemonset -n cube-system cube-node
kubectl get daemonset -n cube-system cube-node-pvm
kubectl get deploy -n cube-system cube-proxy
kubectl get sts -n cube-system cube-mysql cube-redis
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node-pvm -c pvm-host-bootstrap --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node-bootstrap -c wait-pvm-host --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node-bootstrap -c cube-node-init --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node -c cubelet --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node -c wait-node-prep --tail=50
kubectl logs -n cube-system deploy/cube-master -c cube-master --tail=100
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" cubebox list'
helm test cube -n cube-system --timeout 20m
```

`helm test` pods except `node-runtime-test` use `cube.testPlacement` (both
plane taints, no nodeSelector): health, cubemastercli, cubeopscli, mysql,
redis, proxy, dns, and node-image. `node-runtime-test` uses
`cube.computePlacement` and is skipped when `cubeNode.enabled=false`.

`proxy-control-test` GETs `/admin/healthz` on the proxy Service admin port with
`X-Cube-Admin-Token` (Secret `cube-admin-token`) and requires HTTP 200.
Dataplane `/` returns 400.

Health / proxy / node-image use `curl -4`; dns-test uses `getent ahostsv4`.
Override `helmTest.image` with curl+sh+awk+getent. `helmTest.dnsImage` is
busybox for node-runtime-test only.

## Upgrade policy

`cube-node` is a native `apps/v1` DaemonSet. Bumping Big Pod runtime images
(`images.cubelet`, `images.waitNodePrep`, …) or changing the Pod template
**recreates** the Big Pod (Pod UID/IP/netns change) and **interrupts sandboxes
on that node**. Artifact images bump only `cube-node-installer`; node-init
images bump `cube-node-bootstrap`; PVM host image bumps only `cube-node-pvm`.
See [Upgrade](https://cubesandbox.com/guide/kubernetes/upgrade).

Set `cubeNode.updateStrategy.type: OnDelete` for fully manual
per-node upgrades.

## Rollback warning

Helm rollback only rolls back Kubernetes resources. It does not undo host kernel, GRUB, udev, fstab, or XFS changes made by the bootstrap DaemonSet.
Prepare a separate host-kernel rollback runbook for production.

## Uninstall cleanup

`helm uninstall cube -n cube-system` removes chart-managed Kubernetes resources, including CubeProxy, CubeDNS, WebUI, CubeEgress, MySQL/Redis when they are chart-managed, and diagnostic ConfigMaps. It intentionally does not remove:

- operator-provided node labels/taints;
- external MySQL/Redis resources;
- hostPath data such as `/data/CubeMaster/storage`, `/data/cubelet`, `/data/cube-shim`, `/data/cube-shared`, `/data/shared`, `/data/snapshot_pack`, and logs;
- host kernel, GRUB, udev, fstab, or XFS changes made by bootstrap containers;
- external DNS or load balancer records.

Clean those items using the platform runbook for the target environment.
