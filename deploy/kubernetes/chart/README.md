# CubeSandbox Kubernetes/TKE Helm Chart

This chart delivers CubeSandbox on Kubernetes/TKE as chart-managed resources.
It follows the final Big Pod delivery design:

- bootstrap images are separate from runtime images;
- `cube-node` is a DaemonSet Big Pod;
- control-plane and compute/data-plane scheduling are separated through `placement.controlPlane` and `placement.compute`;
- PVM host kernel installation and host reboot are handled by a dedicated Init Container;
- Cube Node host preparation is handled by a second Init Container;
- MySQL schema migration is handled by CubeMaster itself using embedded migrations;
- Cube Master, Cube API, cubemastercli, Cube Proxy Node, WebUI, Template Builder, and Cube Node use separate images.

## Directory

```text
deploy/kubernetes/chart/
  Chart.yaml
  values.yaml
  docs/
    ARCHITECTURE.md
  templates/
```

## Architecture

整体组件关系、安装流程、节点启动流程、DNS/Proxy/Egress 数据流和
external control plane / compute-only 模式见：

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

## Image responsibilities

| Image | Role |
|---|---|
| `cube-pvm-host-bootstrap` | Init Container only. Installs/configures PVM host kernel and may reboot the node. |
| `cube-node-init` | Init Container only. Loads KVM module, prepares host paths, validates `/dev/kvm` and XFS. |
| `cube-node` | Runtime container only. Runs `cubelet` and `network-agent`. |
| `cube-master` | Control-plane master only. Built from `CubeMaster/docker/Dockerfile`; runs `cubemaster`; schema migrations are embedded in the binary. |
| `cube-api` | HTTP API only. Runs `cube-api`. |
| `cubemastercli` | Operational CLI only. Packages the real `CubeMaster/bin/cubemastercli` binary for exec-based operations. |
| `cube-proxy-node` | Data-plane proxy. Reuses `CubeProxy/Dockerfile` and runs as a chart-managed control-plane Deployment when `cubeProxy.enabled=true`. |
| `cube-egress` | CubeEgress transparent outbound proxy. Reuses `CubeEgress/Dockerfile` and runs as a Cube Node sidecar when `cubeEgress.enabled=true`. |
| `cube-egress-net` | Host network rule helper for CubeEgress TPROXY/ip-rule/sysctl setup. |
| `cube-webui` | One-click WebUI static assets and OpenResty runtime. |
| template builder sidecar | Optional template builder with `dockerd`/BuildKit. Uses `docker:27-dind` by default. |

## Node selection

The chart separates placement into dedicated control-plane nodes and compute
nodes. Control-plane Deployments, StatefulSets, and `cube-proxy-node` use
`placement.controlPlane`; `cube-node` and node-local `cube-dns` use
`placement.compute`.

The chart refuses to render host-mutating compute components without
`placement.compute.nodeSelector`. This prevents PVM bootstrap and Cube runtime
setup from running on ordinary nodes. The default compute selector includes
`cube.tencent.com/allow-pvm-bootstrap=true` because the default profile
initializes the PVM host kernel and may reboot selected compute nodes.

Default placement values:

```yaml
global:
  timezone: Asia/Shanghai

storageClass:
  create: true
  name: cube-cbs-wffc
  provisioner: com.tencent.cloud.csi.cbs
  volumeBindingMode: WaitForFirstConsumer

placement:
  controlPlane:
    nodeSelector:
      cube.tencent.com/role: control
      cube.tencent.com/cube-control: "true"
    tolerations:
      - key: cube.tencent.com/control
        operator: Equal
        value: "true"
        effect: NoSchedule

  compute:
    nodeSelector:
      cube.tencent.com/role: compute
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
kubectl label node <control-node>   cube.tencent.com/role=control   cube.tencent.com/cube-control=true   --overwrite

kubectl taint node <control-node>   cube.tencent.com/control=true:NoSchedule   --overwrite

kubectl label node <compute-node>   cube.tencent.com/role=compute   cube.tencent.com/cube-node=true   cube.tencent.com/allow-pvm-bootstrap=true   --overwrite

kubectl taint node <compute-node>   cube.tencent.com/compute=true:NoSchedule   --overwrite
```

The chart does not label or taint nodes. The platform operator must prepare node labels and taints before installation.
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
- `cubeNode.pvmGuestKernel.enabled` defaults to `true` and controls the one-click `CUBE_PVM_ENABLE` behavior, selecting `cube-kernel-scf/vmlinux -> vmlinux-pvm` or `vmlinux-bm`;
- `cubeNode.network.autoDetectEthName=true` auto-detects the primary host NIC and patches Cubelet `eth_name`;
- `cubeNode.network.cidr` can patch Cubelet cubevs CIDR when the packaged default conflicts with the host network.

`bootstrap.pvmHostKernel.enabled` also defaults to `true`, so the PVM host
kernel bootstrap Init Container can install/configure the host kernel and
perform the configured coordinated reboot. The default
`bootstrap.pvmHostKernel.bootArgs` is `nopti pti=off` because the current
`kvm_pvm` module does not support host KPTI. `cube-node-init` performs the same
fail-fast style checks as one-click for memory, glibc, cgroup v2 cpu
controller, cubecow dependencies, KVM, XFS, and PVM consistency. It fails when
a host has `kvm_pvm` loaded but `cubeNode.pvmGuestKernel.enabled=false`, or
when `cubeNode.pvmGuestKernel.enabled=true` but the host has not booted a PVM
kernel with `kvm_pvm` loaded.

## Build and push images

```bash
PUSH=1 REGISTRY=ccr.ccs.tencentyun.com/cubesandbox-chart IMAGE_TAG=v0.5.0 ./deploy/kubernetes/images/build-cube-images.sh
```

Cube-owned images default to `imagePullPolicy: Always` because this chart uses the release tag directly and environments are expected to pull the pushed image from the registry during deployment.

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

## Use third-party MySQL or Redis

The chart installs `cube-mysql` StatefulSet only when `mysql.enabled=true` and `mysql.host` is empty.
Set `mysql.host` to use an existing MySQL service; the chart will not install `cube-mysql`.

The chart installs `cube-redis` StatefulSet only when `redis.enabled=true` and `redis.host` is empty.
Set `redis.host` to use an existing Redis service; the chart will not install `cube-redis`.

## CubeMaster configuration

The `cube-master` image uses `CubeMaster/docker/Dockerfile` directly and does not carry a Kubernetes-specific entrypoint or bundled `conf.yaml`.
The chart stores the One-click `CubeMaster/conf.yaml` at `deploy/kubernetes/chart/files/cube-master/conf.yaml`, renders MySQL/Redis values into it, creates a release-scoped Secret named `<release>-master-config`, and mounts it to `/usr/local/services/cubemaster/conf.yaml`; `CUBE_MASTER_CONFIG_PATH` points CubeMaster to that mounted file.

CubeMaster artifact storage maps to `/data/CubeMaster/storage`, matching one-click.
The chart uses PVC-backed persistence by default so state can survive
rescheduling across dedicated control nodes:

```yaml
controlPlane:
  master:
    persistence:
      enabled: true
      hostPath: ""
      storageClassName: cube-cbs-wffc
mysql:
  persistence:
    enabled: true
    hostPath: ""
    storageClassName: cube-cbs-wffc
redis:
  persistence:
    enabled: true
    hostPath: ""
    storageClassName: cube-cbs-wffc
```

Set `storageClassName` / `size` to tune dynamic PVCs, or `existingClaim` to
bind pre-created volumes. Use `hostPath` only for single-node throwaway
environments; multi-control-node deployments must use PVCs or external
MySQL/Redis.

Built-in MySQL and Redis use StatefulSet `volumeClaimTemplates` by default.
For a release named `cube`, the generated claims are owned by the StatefulSet
Pod, such as `mysql-data-cube-mysql-0` and `redis-data-cube-redis-0`.

The default `cube-cbs-wffc` StorageClass uses `WaitForFirstConsumer`, which is
important on TKE multi-zone clusters: CBS disks are provisioned in the same zone
as the selected control node instead of being created in a random zone before
the Pod is scheduled.

## Database migration

The chart does not deliver a separate DB migration Job or image. CubeMaster owns MySQL schema migration and runs its embedded `CubeMaster/pkg/base/dao/migrate/migrations/mysql` migrations during startup.

- CubeMaster uses the configured MySQL endpoint, user, password, and database.
- The chart does not package or maintain SQL files under `files/`; do not add migration SQL copies to the chart.
- CubeMaster records applied versions in `goose_db_version` and serializes concurrent migration attempts through the migration lock implemented by CubeMaster.
- There is no chart-managed SQL data seed, and the one-click single-node seed file `sql/002_seed_single_node.sql` is intentionally not rendered by the chart. Node registration must come from real Cube Node Pods selected by `placement.compute.nodeSelector`.
- When using third-party MySQL, set `mysql.host` and ensure the configured MySQL user can create/alter tables in `mysql.database`.

## cubemastercli operational CLI

`cubemastercli.enabled=true` installs a chart-managed
`<release>-cubemastercli` Deployment. The image contains the real
`CubeMaster/bin/cubemastercli` binary only; it does not provide a wrapper or
fake `ctl` command.

The chart injects `CUBEMASTERCLI_ADDRESS` and `CUBEMASTERCLI_PORT` from the
current CubeMaster endpoint. Because upstream `cubemastercli` does not read
environment variables as flag defaults, commands should pass those values to
the real binary:

```bash
kubectl exec -n cube-system deploy/cube-cubemastercli -- cubemastercli --help
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" template list'
```

The `cubemastercli` image is intentionally independent from `cube-master` and
`cube-node`. It contains CLI/operator tooling only; the runtime images do not
carry this operational entry point.

## Cube Proxy Node

`cube-proxy-node` is a Cube data-plane component. It is enabled by default to match one-click behavior and is installed, upgraded, and uninstalled with the Cube release as a control-plane Deployment.

The default TLS mode is `selfSigned`, matching the one-click mkcert-style test experience. Production environments should provide a real TLS certificate and reserve node host ports 80/443 on selected nodes. The image reuses `CubeProxy/Dockerfile`; the chart does not override nginx with a Kubernetes-only configuration.

`cube-proxy-node` also starts the built-in `cube-proxy-sidecar`. The chart wires the sidecar to the chart-managed or third-party Redis endpoint and to the CubeMaster Kubernetes Service. Do not run a separate unmanaged CubeProxy sidecar.

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

`cube-proxy-node` uses `placement.controlPlane`, matching the one-click control-node placement for CubeProxy. The chart does not create node labels.

`cubeProxy.hostNetwork=true` is also the default. This is required for one-click
parity: CubeProxy must terminate `cube.app` / wildcard traffic on a node-local
host-network endpoint. When the sandbox owner is on a compute node, CubeProxy
uses Redis routing metadata to connect to the owner `HostIP:hostPort`. The
chart patches the image's default nginx listeners to
the configured `cubeProxy.ports.*.containerPort` values, which default to `80`
and `443`.

The chart does not create a `cube-proxy-node` ClusterIP Service. A normal
Kubernetes Service load-balances requests across proxy Pods, which does not
match the one-click model where callers reach an explicit CubeProxy host
endpoint. For sandbox data-plane traffic, point wildcard DNS at a specific
control-node CubeProxy IP or at an external load balancer that preserves the intended
CubeProxy topology.

CubeProxy admin health remains loopback-only inside each Pod, matching the image's nginx admin listener. The chart validates it through Pod readiness/liveness probes rather than exposing it through a Service.

CubeProxy reads sandbox routing metadata from Redis in nginx Lua. Because nginx
does not automatically inherit Kubernetes DNS resolution for Lua cosocket
connections, the chart renders an nginx `resolver` into
`/usr/local/openresty/nginx/conf/global/global.conf`. By default the proxy
discovers resolver addresses from the Pod `/etc/resolv.conf`, which resolves the
chart-managed `cube-redis.<namespace>.svc.cluster.local` Service name and
third-party Redis DNS names. Override only when the cluster requires explicit
DNS servers:

```yaml
cubeProxy:
  resolver:
    addresses:
      - 172.18.0.10
    valid: 30s
    timeout: 5s
    ipv6: false
```

## Cube DNS

`cubeDns.enabled=true` delivers CoreDNS for one-click style sandbox domain
resolution. The default `cubeDns.mode=nodeLocal` runs `cube-dns` as a
hostNetwork DaemonSet on Cube compute nodes selected by `placement.compute`
and listens on `127.0.0.54:53`.
`cube-node` Pods use `dnsPolicy: None` and explicitly set
`dnsConfig.nameservers: [127.0.0.54]`, so `cube.app` and wildcard domains are
resolved inside the Big Pod without modifying host-wide DNS.

Sandbox guest DNS is configured separately from the `cube-node` Pod DNS. The
guest cannot use `127.0.0.54` because that address is the guest loopback inside
the sandbox. By default `cubeDns.sandboxGateway.enabled=true` also binds
node-local `cube-dns` on the compute node HostIP, and
`cubeNode.dns.sandbox.useCubeDns=true` injects that HostIP into Cubelet
`default_dns_servers`.

CubeVS eBPF egress does not traverse host kube-proxy ClusterIP DNAT, so guests
should not rely on Kubernetes Service ClusterIPs as if they were ordinary
external addresses. The chart no longer renders sandbox service proxy resources
or DNS overrides for them; expose required in-cluster services through the
platform networking layer, AgentWay provider configuration, or an
operator-managed proxy outside this chart.

Set `cubeProxy.advertiseIP` or `cubeDns.answerIP` to return the control-node
CubeProxy entrypoint. If both are empty in node-local mode, `cube-dns` falls
back to the current compute HostIP for compatibility with older local-proxy
topologies; that fallback is not suitable when CubeProxy runs only on control
nodes. Optional `cubeDns.mode=service` keeps the older ClusterIP DNS model,
where callers must explicitly point their DNS policy or upstream DNS to the
`cube-dns` Service. In service mode, set `cubeDns.answerIP` or
`cubeProxy.advertiseIP` to an explicit CubeProxy entrypoint.

The chart does not silently rewrite Kubernetes nodes' host DNS settings.
For external clients, browsers, SDKs, or any Pod that is not explicitly using this `dnsConfig`, configure DNS/LB/Ingress outside the chart so `cubeProxy.domain` and wildcard subdomains resolve to an explicit control-node CubeProxy endpoint or external load balancer.

## WebUI

`webui.enabled=true` delivers the one-click WebUI by default:

- `cube-webui` image packages one-click `webui/dist` static assets;
- a chart-rendered nginx config proxies `/cubeapi/` to the CubeAPI Service;
- the Service listens on port `12088`, matching one-click `WEB_UI_HOST_PORT`.

Expose the WebUI externally by changing `webui.service.type` or by adding your platform's ingress/load balancer configuration.

## Diagnostics

One-click delivers `cube-diag` scripts on the host. The Kubernetes chart delivers the equivalent operational entry point as a ConfigMap when `diagnostics.enabled=true`:

```bash
kubectl get configmap -n cube-system cube-diagnostics -o jsonpath='{.data.cube-diag-k8s\.sh}' > /tmp/cube-diag-k8s.sh
sh /tmp/cube-diag-k8s.sh cube-system cube
```

The script collects Pods, DaemonSets, Deployments, Services, Endpoints, Events, Helm values/manifests, Pod descriptions, and recent logs for Cube components into a timestamped directory.

## CubeEgress

`cubeEgress.enabled=true` runs CubeEgress inside the Cube Node Big Pod:

- `cube-egress` mounts `/etc/cube/ca` and exposes the loopback admin API on `127.0.0.1:9090`;
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

## Render and lint

```bash
helm lint ./deploy/kubernetes/chart
helm template cube ./deploy/kubernetes/chart -n cube-system > /tmp/cube-rendered.yaml
```

## Verify

```bash
kubectl get pods -n cube-system -o wide
kubectl get ds -n cube-system cube-node
kubectl get deploy -n cube-system cube-proxy-node
kubectl get sts -n cube-system cube-mysql cube-redis
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node -c pvm-host-bootstrap --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node -c cube-node-init --tail=100
kubectl logs -n cube-system -l app.kubernetes.io/component=cube-node -c cube-node --tail=100
kubectl logs -n cube-system deploy/cube-master -c cube-master --tail=100
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'
helm test cube -n cube-system --timeout 20m
```

## Upgrade policy

`cubeNode.updateStrategy.type` defaults to `RollingUpdate`. For production
maintenance windows, set it to `OnDelete` if you need to upgrade one compute
node at a time after draining or cleaning Cube sandboxes on that node.

## Rollback warning

Helm rollback only rolls back Kubernetes resources. It does not undo host kernel, GRUB, udev, fstab, or XFS changes made by the bootstrap Init Containers.
Prepare a separate host-kernel rollback runbook for production.

## Uninstall cleanup

`helm uninstall cube -n cube-system` removes chart-managed Kubernetes resources, including CubeProxy, CubeDNS, WebUI, CubeEgress, MySQL/Redis when they are chart-managed, and diagnostic ConfigMaps. It intentionally does not remove:

- operator-provided node labels/taints;
- external MySQL/Redis resources;
- hostPath data such as `/data/CubeMaster/storage`, `/data/cubelet`, `/data/cube-shim`, `/data/snapshot_pack`, and logs;
- host kernel, GRUB, udev, fstab, or XFS changes made by bootstrap containers;
- external DNS or load balancer records.

Clean those items using the platform runbook for the target environment.
