# 架构说明

本文说明 CubeSandbox **Kubernetes / Helm Chart** 的交付形态：组件分层、计算面四个 DaemonSet 的分工、安装与启动顺序，以及 DNS / Proxy / Egress 等运行期链路。

安装步骤见 [Helm 安装](./install.md)。计算面镜像升级见 [升级](./upgrade.md)。排障见 [常见问题](./faq.md)。

::: tip 和「产品架构」的区别
[架构概览](../../architecture/overview.md)讲的是 CubeSandbox **产品组件**（CubeMaster / Cubelet / MicroVM 等）。本页讲的是这些组件在 **K8s 上怎么落盘、怎么调度、怎么启动**。
:::

## 1. 总体分层

| 层级 | 组件 | Kubernetes 形态 | 主要职责 |
| --- | --- | --- | --- |
| 控制面 | CubeMaster | Deployment + Service + Secret + PVC/hostPath | 节点注册、模板/rootfs artifact、内置 DB migration、调度/元数据 |
| 控制面 API | CubeAPI | Deployment + Service | 对外 E2B 兼容 HTTP API；读写 MySQL；访问 CubeMaster |
| 运维后端 | CubeOps | Deployment + Service | JWT 运维 API + WebUI SDK；监听 `0.0.0.0:3010`；读写 MySQL；访问 CubeMaster |
| 管理入口 | WebUI | Deployment + Service + ConfigMap | 静态控制台；`/opsapi/`、`/cubeapi/v1/` 反代到 CubeOps（依赖 `cubeOps.enabled`） |
| 运维入口 | cubemastercli | Deployment | `kubectl exec` 用 CLI；注入本 Release 的 CubeMaster endpoint |
| 依赖存储 | MySQL / Redis | 内置 StatefulSet 或第三方 | 业务数据 / Proxy 与 lifecycle 状态 |
| 计算面 · 运行时 | `cube-node`（Big Pod） | 原生 `apps/v1` DaemonSet | `wait-node-prep` init + cubelet / network-agent + 可选 egress |
| 计算面 · 产物 | `cube-node-installer` | 原生 `apps/v1` DaemonSet | 将 shim / kernel / guest 安装到宿主机 toolbox |
| 计算面 · 节点引导 | `cube-node-bootstrap` | 原生 `apps/v1` DaemonSet | `wait-pvm-host`、`cube-node-init`、写 `node-prep-ready` |
| 计算面 · PVM 宿主机 | `cube-node-pvm` | 原生 `apps/v1` DaemonSet（仅 `placement.pvm`） | PVM host kernel 安装（可 reboot）；管理 L0 污点并写指纹 |
| 数据面入口 | CubeProxy + 集群 DNS | Deployment；可选改写 CoreDNS | HTTP/HTTPS sandbox 入口；`*.domain` 泛解析 |
| 生命周期 | cube-lifecycle-manager | Deployment + ClusterIP | sandbox pause/resume；经 Redis 发现 Proxy 副本 |

默认完整部署：

```mermaid
flowchart TB
  subgraph CP["Control Plane · placement.controlPlane"]
    CM["cube-master"]
    API["cube-api"]
    OPS["cube-ops"]
    WEB["cube-webui"]
    CLI["cubemastercli"]
    MYSQL[("MySQL")]
    REDIS[("Redis")]
    PROXY["cube-proxy"]
  end

  subgraph CLUSTER["Cluster DNS"]
    KDNS["CoreDNS · *.cube.app → Proxy Service"]
  end

  subgraph PVMN["PVM nodes · placement.pvm"]
    PVMDS["cube-node-pvm"]
    PVMREADY["pvm-host-ready fingerprint"]
  end

  subgraph COMPUTE["Compute · placement.compute"]
    subgraph BOOT["cube-node-bootstrap"]
      WAITPVM["init: wait-pvm-host"]
      NINIT["init: cube-node-init"]
      READY["write node-prep-ready"]
    end
    subgraph INST["cube-node-installer"]
      ART["shim / kernel / guest → toolbox"]
    end
    subgraph NODE["cube-node Big Pod"]
      WAIT["init: wait-node-prep（就绪后退出）"]
      RUN["cubelet + network-agent"]
      EG["cube-egress + cube-egress-net"]
    end
  end

  WEB -->|"/opsapi /cubeapi/v1"| OPS
  WEB -->|"/sandbox/"| PROXY
  CLI --> CM
  OPS --> CM
  OPS --> MYSQL
  API --> CM
  API --> MYSQL
  CM --> MYSQL
  CM --> REDIS
  PROXY --> REDIS
  PROXY --> CM
  KDNS --> PROXY
  PVMDS --> PVMREADY
  PVMREADY --> WAITPVM
  WAITPVM --> NINIT
  NINIT --> READY
  READY --> WAIT
  WAIT --> RUN
  ART --> RUN
  RUN --> CM
  EG --> RUN
```

## 2. 资源与镜像职责

### 2.1 控制面

| 资源 | Chart 模板 | 说明 |
| --- | --- | --- |
| `cube-master` | `templates/master.yaml` | `images.master`；挂载 Chart 渲染的 `conf.yaml`；内置 schema migration |
| `cube-master-config` | `templates/master-config-secret.yaml` | `files/cube-master/conf.yaml` 渲染结果 |
| `cube-master-storage` | `master.yaml` / `master-pvc.yaml` | 默认 PVC；可选 existingClaim / hostPath / emptyDir |
| `cube-api` | `templates/api.yaml` | `images.api`（外部 E2B） |
| `cube-ops` | `templates/ops.yaml` | `images.ops`；ClusterIP；bind `0.0.0.0:3010` |
| `cubemastercli` | `templates/cubemastercli.yaml` | `images.cubemastercli` |
| `cube-webui` | `templates/webui.yaml` | `images.webui` + nginx ConfigMap（上游 CubeOps） |
| `cube-secret` | `templates/secret.yaml` | MySQL / Redis / Proxy 等密码 |
| `volume-cos`（可选） | `templates/volume-cos-secret.yaml` | COS 凭证（`volume-cos.conf`）；`volumeCos.enabled` 时挂到 Master + Cubelet |

### 2.2 MySQL / Redis

| 模式 | 行为 |
| --- | --- |
| 内置 MySQL | `mysql.host=""` → StatefulSet + Headless Service；可配 `mysql.persistence.hostPath` |
| 第三方 MySQL | `mysql.host` 非空 → 不装内置 MySQL |
| 内置 Redis | `redis.host=""` 且控制面或 Proxy 需要时安装 |
| 第三方 Redis | `redis.host` 非空 → 不装内置 Redis |

### 2.3 计算面：四个 DaemonSet

`cube-node` / `cube-node-installer` / `cube-node-bootstrap` 用 `placement.compute`（**不含** `allow-pvm-bootstrap`）。`cube-node-pvm` 用 `placement.pvm`（含 `allow-pvm-bootstrap`），因此非 PVM 节点不会拉取 `cube-pvm-host-bootstrap` 大镜像。

四条计算面（Big Pod / installer / bootstrap / PVM）均为原生 `apps/v1` DaemonSet。无状态控制面（master/api/ops/webui/proxy/lifecycle/cubemastercli）为原生 Deployment；MySQL/Redis 继续使用原生 StatefulSet。

#### Big Pod：`cube-node`

- `hostNetwork: false`（Pod 网络）；原生 `apps/v1` DaemonSet。
- **initContainer**：`wait-node-prep`（指纹匹配后 **exit 0**，不作为常驻 sidecar）。
- 镜像 / 资源 / Pod template 变更会 **recreate** Big Pod（PodIP/netns 变化，存量沙箱中断）。详见 [升级](./upgrade.md)。
- **NodeID** = `spec.nodeName`；**Endpoint** = `status.podIP`。
- toolbox **整树** hostPath：`/usr/local/services/cubetoolbox`。

| 容器 | 镜像 | 职责 |
| --- | --- | --- |
| `wait-node-prep`（init） | `images.waitNodePrep` | 只读 hostPath `node-prep-ready` 自描述指纹；匹配后退出，主容器才启动 |
| `network-agent` | `images.networkAgent` | self-stage 后启动 |
| `cubelet` | `images.cubelet` | self-stage 后启动 |
| `cube-egress` / `cube-egress-net` | 对应镜像 | 可选；透明出站 / TPROXY |

**容器名 / volumeMount / securityContext / imagePullPolicy 变更同样 recreate**。

#### Installer：`cube-node-installer`

- 容器：`cube-shim-install` / `cube-kernel-install` / `cube-guest-install`。
- 把镜像里的 shim / kernel / guest **整目录换到** 宿主机 toolbox；换目录期间版本矩阵会短暂标「未完成」，成功后恢复正常。
- 可独立 RollingUpdate；日常升产物 **只 bump Installer 镜像**。

#### Bootstrap：`cube-node-bootstrap`

- init：`wait-pvm-host` → `cube-node-init`；主容器写 `node-prep-ready`。
- `wait-pvm-host`：看节点有没有 `allow-pvm-bootstrap`——有则等 PVM 宿主机就绪并记「本节点用 PVM guest」；没有则记「本节点用 bm guest」。
- 哨兵目录：`/var/lib/cube-node-bootstrap`（与 Big Pod 的 `wait-node-prep` / PVM DS 共享）。
- `hostPID: true`（`nsenter --target 1`）；低频变更；升 node-init **只 bump Bootstrap / nodeInit 镜像**。

#### PVM：`cube-node-pvm`

- 原生 `apps/v1` DaemonSet；仅当 `bootstrap.pvmHostKernel.enabled=true` 时创建；仅调度到 `placement.pvm`。
- `startupGate` 默认开启：目标节点指纹未就绪时，Helm pre-install/pre-upgrade Hook 写入 `cube.tencent.com/pvm-not-ready=true:NoSchedule`，再逐节点探针 CNI；指纹已匹配则不写该污点。
- 安装/升级前另有 cubevs CIDR Hook（weight `-110`）：`cubeNode.network.cidr`（默认 `172.16.0.0/18`）与集群 Service CIDR / ClusterIP 重叠则 fail-fast。
- init：`pvm-host-bootstrap`；mutate 严格按 ensure taint → 删除本 namespace/本 release/本节点依赖 Pod → invalidate → Lease → mutate/reboot。
- 成功路径按 write ready → verify live fingerprint → clear taint；主容器每 30 秒 reconcile 分裂态。
- 只有 PVM DaemonSet 容忍临时门闩。CNI、kube-proxy 须以 `Exists` 或显式 key 容忍门闩。PVM 保持 Pod 网络。
- 升 PVM 镜像 **只 bump `images.pvmHostBootstrap`**，不 recreate Big Pod。

为何拆成四个：产物安装与可 reboot 的 PVM 引导分离；非 PVM compute 节点不拉 PVM 大镜像；只升 Installer / Bootstrap / PVM 时可不碰 Big Pod template。

### 2.3.1 节点上你会看到的标记

| 标记 | 含义 |
| --- | --- |
| `pvm-host-ready` | 宿主机 PVM 内核已按预期装好；内容带指纹，换核后必须对上当前 `uname` 才算就绪 |
| `effective-pvm` | 本节点 guest 该用 PVM（`1`）还是 bm（`0`）；有 `allow-pvm-bootstrap` 且 host 就绪 → `1`，否则 → `0` |
| `node-prep-ready` | bootstrap 预检通过，Big Pod 可以启动 |
| `/run/wait-node-prep.ready` | 本轮 Pod 内临时标记，重启即没 |
| toolbox 下「组件已就绪」标记 | 该组件 stage 成功，产物可被版本矩阵采集 |
| toolbox 下「组件正在替换」标记 | 正在换目录；矩阵会标未完成；成功后清除，失败会留下直到下次成功 |

Guest 选核：先看 `effective-pvm`；没有则尽量保持节点上一次已在用的内核；再没有才用 Chart 首次安装默认（`cubeNode.pvmGuestKernel.enabled`）。

### 2.4 数据面入口

| 资源 | Chart 模板 | 职责 |
| --- | --- | --- |
| `cube-proxy` | `templates/proxy.yaml` | sandbox HTTP/HTTPS；`placement.controlPlane`；Pod 网络 |
| `cube-lifecycle-manager` | `templates/lifecycle-manager.yaml` | pause/resume；Proxy 经 Redis 发现副本 |
| `cube-proxy-certs` | `proxy.yaml` | TLS：selfSigned / inline / existingSecret / certManager |
| Service / Ingress | `proxy-service.yaml` / `proxy-ingress.yaml` | ClusterIP；Ingress SSL passthrough，TLS 在 Proxy 终结 |
| cluster DNS | `templates/cluster-dns.yaml` | 启用时把 `*.cubeProxy.domain` rewrite 到 Proxy Service |

CubeProxy 经 Redis 中的 owner 元数据转发到目标 compute 节点 sandbox。

## 3. DNS

Chart **不**部署自有 CoreDNS。Proxy 启用且 `configureClusterDNS=true`（默认）时：

- Helm hook 将 `domain` / `*.domain` rewrite 到 `<release>-proxy.<ns>.svc.cluster.local`。
- `cubeNode.dns.sandbox.followNodeDns=true`：guest 跟随节点/集群 DNS。

```mermaid
sequenceDiagram
  participant Guest as sandbox guest
  participant CN as cube-node Pod
  participant KDNS as cluster CoreDNS
  participant PX as cube-proxy Pod

  CN->>KDNS: ClusterFirst
  Guest->>KDNS: followNodeDns
  KDNS-->>Guest: *.cube.app → CubeProxy Service
  Guest->>PX: HTTP/HTTPS
```

- 域名：`cubeProxy.domain`（默认 `cube.app`）。
- 平台禁止改 `kube-system/coredns` 时设 `cubeProxy.configureClusterDNS=false`。
- 外部客户端仍需自配公网/Private DNS 或 LB。

## 4. 安装与启动

### 4.1 Helm 渲染

```mermaid
flowchart TD
  A["helm upgrade/install"] --> B["templates/validate.yaml"]
  B --> C{"values 合法?"}
  C -- 否 --> X["fail render"]
  C -- 是 --> D["Secret / ConfigMap / 持久化"]
  D --> E["MySQL / Redis 或外部"]
  E --> F["控制面 Deployment"]
  F --> G["Proxy / cluster-dns"]
  G --> H["cube-node + installer + bootstrap + pvm"]
```

主要校验：

- 启用控制面 / 计算面 / Proxy 时须配置对应 `placement.*.nodeSelector`。
- `configureClusterDNS=true` 须配置 `cubeProxy.domain`。
- compute-only 须配置 `externalControlPlane.masterEndpoint`。
- `pvmHostKernel.enabled=true` 时 `placement.pvm` 须含 `allow-pvm-bootstrap`，且 **不得** 写在 `placement.compute`。
- 已移除 `security.hostNetwork`；cube-node 固定 Pod 网络。

调度：控制面用 `placement.controlPlane`；`cube-node` / installer / bootstrap 用 `placement.compute`；`cube-node-pvm` 用 `placement.pvm`。Chart 管理的容器经 `global.timezone` 注入 `TZ`（默认 `Asia/Shanghai`）。

### 4.2 控制面启动

```mermaid
sequenceDiagram
  participant H as Helm
  participant DB as MySQL
  participant R as Redis
  participant CM as CubeMaster
  participant API as CubeAPI
  participant OPS as CubeOps
  participant WEB as WebUI
  participant CLI as cubemastercli

  H->>DB: create/use MySQL
  H->>R: create/use Redis
  H->>CM: conf.yaml + storage + CA
  CM->>DB: embedded migration
  CM-->>H: /notify/health
  H->>API: CUBE_MASTER_ENDPOINT + MySQL
  API-->>H: /health
  H->>OPS: CUBE_MASTER_ADDR + MySQL
  OPS-->>H: /health
  H->>WEB: nginx → CubeOps
  H->>CLI: CUBEMASTERCLI_ADDRESS / PORT
```

无独立 `cube-db-migrate` Job；`cubemastercli` 不混入 master/node 镜像。

### 4.3 计算节点启动

```mermaid
sequenceDiagram
  participant PVM as cube-node-pvm
  participant Boot as cube-node-bootstrap
  participant Inst as cube-node-installer
  participant Wait as wait-node-prep
  participant CN as cube-node
  participant CM as CubeMaster

  alt allow-pvm-bootstrap node
    PVM->>PVM: pvm-host-bootstrap may reboot
    Note over PVM: mutate 前删各类 ready
    PVM->>PVM: write pvm-host-ready fingerprint
  end
  Boot->>Boot: wait-pvm-host label + fingerprint gate
  opt nodeInit.enabled
    Boot->>Boot: cube-node-init
  end
  Boot->>Boot: write node-prep-ready
  Inst->>Inst: stage shim/kernel/guest
  Wait->>Wait: poll fingerprint → exit 0
  Wait-->>CN: init 完成；主容器启动
  CN->>CN: self-stage；按节点 PVM 决定选 guest 内核
  CN->>CM: register + heartbeat
```

探针约定：

- cubelet：startup 等 9999；readiness 默认 exec（9999 + network-agent `/readyz` + sock）；liveness 查 9999。
- `cube-egress`：`127.0.0.1:9090/admin/v1/health`。
- `cube-egress-net`：`cube-dev`、ip rule、table 100、mangle `TRANSPROXY`。

### 4.4 注册与验收关注点

- CubeMaster `/notify/health`、CubeOps `/health`、CubeAPI `/health`（若启用）。
- CubeAPI（或经 CubeOps SDK）能查到 healthy node。
- `cube-node` / installer / bootstrap ready 数等于命中 `placement.compute` 的节点数；`cube-node-pvm` ready 数等于命中 `placement.pvm` 的节点数。
- egress 启用时 sidecar Ready。

## 5. 运行期数据流

### 5.1 WebUI / CubeOps / CubeAPI / Master

```mermaid
flowchart LR
  U["Browser / Operator"] --> WEB["cube-webui"]
  WEB -->|"/opsapi/ /cubeapi/v1/"| OPS["cube-ops"]
  Ext["External E2B SDK"] --> API["cube-api"]
  OPS --> CM["cube-master"]
  OPS --> MYSQL[("MySQL")]
  API --> CM
  API --> MYSQL
  CM --> MYSQL
  CM --> REDIS[("Redis")]
```

### 5.2 Sandbox 入口

```mermaid
flowchart LR
  CLIENT["Client"] --> DNS["DNS · cube.app"]
  DNS --> PROXY["cube-proxy"]
  PROXY --> REDIS[("Redis")]
  PROXY --> CM["CubeMaster"]
  PROXY --> NODE["Cube Node / Sandbox"]
```

无 Ingress Controller 时可关 `cubeProxy.ingress.enabled`，自行把外部流量接到 Service。生产应提供正式证书，并把 sandbox 域名指向 Ingress。

### 5.3 出站 egress

```mermaid
flowchart LR
  SB["Sandbox"] --> DEV["cube-dev"]
  DEV --> EGNET["cube-egress-net"]
  EGNET --> EG["cube-egress"]
  EG --> EXT["External"]
  EG --> CA["cube-egress-ca"]
```

Master / API / Node 共享 `cube-egress-ca`，保证模板构建与运行期信任一致。

### 5.4 模板构建

CubeMaster 在进程内通过 go-containerregistry 拉取镜像并导出 rootfs，无需
Docker-in-Docker sidecar；产物写入 Master storage。

## 6. compute-only / 外部控制面

```mermaid
flowchart TB
  subgraph EXT["External Control Plane"]
    ECM["External CubeMaster"]
    EDB[("MySQL / Redis")]
  end
  subgraph NS["Compute Namespace"]
    NODE["cube-node + installer + bootstrap"]
  end
  NODE --> ECM
  ECM --> EDB
```

```yaml
controlPlane:
  enabled: false
externalControlPlane:
  enabled: true
  masterEndpoint: <external-master>:8089
  apiEndpoint: http://<external-api>:3000  # optional, for helm test
```

不安装内置 Master / API / MySQL / Redis / WebUI；默认不装 Proxy（避免与外部数据面不一致）。配置了 `apiEndpoint` 时 helm test 会校验外部 API 与节点注册。

## 7. 关键 values 开关

| values 路径 | 默认 | 影响 |
| --- | --- | --- |
| `global.timezone` | `Asia/Shanghai` | 注入 Chart 管理容器的 `TZ` |
| `storageClass.create` / `name` / `provisioner` | `create=false` | 是否由 chart 创建 StorageClass；默认不创建（PVC 走集群 default SC；TKE 用 `values-tke.yaml`） |
| `persistence.storageClassName` | `""` | 三 PVC 共用此 SC；`""` → 集群 default |
| `*.persistence.storageClassName` (master/mysql/redis) | `""` | 组件级覆盖；非空优先于顶层 |
| `controlPlane.enabled` | `true` | 内置控制面 |
| `externalControlPlane.enabled` | `false` | 外部 CubeMaster |
| `placement.controlPlane.nodeSelector` | `cube-control=true` | 控制面调度 |
| `placement.compute.nodeSelector` | `cube-node=true` | 计算面（不含 allow-pvm） |
| `placement.pvm.nodeSelector` | 另含 `allow-pvm-bootstrap=true` | 仅 PVM 宿主机 DaemonSet |
| `cubeProxy.domain` | `cube.app` | sandbox 域名 |
| `cubeProxy.configureClusterDNS` | `true` | 是否写入集群 CoreDNS |
| `cubeNode.dns.sandbox.followNodeDns` | `true` | guest 跟随节点 DNS |
| `cubeNode.pvmGuestKernel.enabled` | `true` | 首次安装默认是否倾向 PVM guest |
| `bootstrap.pvmHostKernel.enabled` | `true` | host kernel bootstrap（可能重启节点） |
| `bootstrap.pvmHostKernel.startupGate.enabled` | `true` | PVM 未就绪时使用 Node NoSchedule 污点硬门闩 |
| `bootstrap.pvmHostKernel.bootArgs` | `nopti pti=off` | 当前 `kvm_pvm` 不支持 host KPTI |
| `bootstrap.nodeInit.*` | 多项 | 预检、XFS、KVM、CIDR |
| `mysql.host` / `redis.host` | `""` | 非空则用第三方 |
| `cubeProxy.enabled` / `ingress.enabled` | `true` | Proxy / Ingress |
| `lifecycleManager.enabled` | `true` | Proxy 启用时必开 |
| `cubeEgress.enabled` | `true` | Big Pod egress sidecar |
| `cubeOps.enabled` | `true` | CubeOps（JWT 运维 API；WebUI 上游） |
| `webui.enabled` | `true` | WebUI（要求 `cubeOps.enabled=true`） |

## 8. Helm test

| Test Pod | 覆盖 |
| --- | --- |
| `<release>-health-test` | Master / Ops / API / 节点注册 / WebUI / Proxy / 工作负载 Ready / Egress 存在性 |
| `<release>-mysql-test` / `redis-test` | 内置依赖连通性 |
| `<release>-dns-test` | `cube.app` / wildcard → Proxy Service |
| `<release>-node-image-test` | 镜像内 runtime 工具与 asset |
| `<release>-node-runtime-test` | `/dev/kvm`、cubelet / network-agent socket |

```bash
helm test <release> -n <namespace> --timeout 20m --logs
```

## 9. 所有权与卸载边界

Chart 管理并随 release 卸载：控制面与计算面工作负载、内置 MySQL/Redis、Proxy、CA/TLS/config Secret、Helm test RBAC、diagnostics ConfigMap 等。

Chart **不**管理：节点 label/taint、第三方 DB、外部 DNS/LB、hostPath 数据、host kernel / GRUB / udev / fstab / XFS 等节点级持久修改。卸载后按平台 runbook 清理宿主机残留。

---

## 下一步

- [Helm 安装](./install.md)
- [升级](./upgrade.md)
- [常见问题](./faq.md)
- [产品架构概览](../../architecture/overview.md)
