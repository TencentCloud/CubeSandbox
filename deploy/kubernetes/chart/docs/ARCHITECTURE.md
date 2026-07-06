# CubeSandbox Chart 组件关系与运行流程

本文说明 `deploy/kubernetes/chart` 的整体架构、组件关系、安装流程和运行期关键链路，帮助交付、运维和后续开发快速理解 Chart 如何还原 One Click 包能力。

## 1. 总体分层

CubeSandbox Chart 按职责分为 7 层：

| 层级 | 组件 | Kubernetes 形态 | 主要职责 |
| --- | --- | --- | --- |
| 控制面 | CubeMaster | Deployment + Service + Secret + PVC/hostPath | 节点注册、模板/rootfs artifact 管理、内置 DB migration、核心调度/元数据能力 |
| 控制面 API | CubeAPI | Deployment + Service | 对外 HTTP API，读写 MySQL，访问 CubeMaster |
| 管理入口 | WebUI | Deployment + Service + ConfigMap | 静态控制台，反向代理 `/cubeapi/` 到 CubeAPI |
| 运维入口 | cubemastercli | Deployment | 面向 `kubectl exec` 的 CLI Pod，交付真实 `cubemastercli` 并注入本 Release 的 CubeMaster endpoint |
| 依赖存储 | MySQL / Redis | 内置 StatefulSet + Headless Service + volumeClaimTemplates/hostPath，或第三方服务 | MySQL 存储业务数据；Redis 存储 CubeProxy/Sidecar 状态 |
| 计算面 | Cube Node Big Pod | DaemonSet | 节点初始化、运行 cubelet/network-agent、透明 egress sidecar |
| 数据面入口 | CubeProxy / CubeDNS | CubeProxy Deployment；CubeDNS DaemonSet 或 Deployment | HTTP/HTTPS sandbox 入口；sandbox 域名解析 |

默认完整部署形态：

```mermaid
flowchart TB
  subgraph CP["Control Plane selected by placement.controlPlane"]
    CM["cube-master Deployment\ncubemaster:8089"]
    API["cube-api Deployment\nHTTP API:3000"]
    WEB["cube-webui Deployment\nWebUI:12088"]
    CLI["cubemastercli Deployment\nreal cubemastercli binary"]
    MYSQL[("cube-mysql\nMySQL 8.0 + PVC")]
    REDIS[("cube-redis\nRedis 7-alpine + PVC")]
    PROXY["cube-proxy-node Deployment\nHTTP/HTTPS 80/443"]
    CMS["cube-master-config Secret\nconf.yaml"]
    CA["cube-egress-ca Secret"]
    CERT["cube-proxy-certs Secret"]
  end

  subgraph COMPUTE["Compute Nodes selected by placement.compute"]
    DNS["cube-dns DaemonSet\nCoreDNS 127.0.0.54:53"]
    subgraph NODE["cube-node DaemonSet Big Pod"]
      WAITDNS["init: wait-cube-dns"]
      PVM["init: pvm-host-bootstrap\noptional"]
      INIT["init: cube-node-init"]
      CUBELET["container: cube-node\ncubelet + network-agent"]
      EG["sidecar: cube-egress\nOpenResty transparent proxy"]
      EGNET["sidecar: cube-egress-net\nTPROXY/ip-rule/sysctl"]
    end
  end

  WEB --> API
  CLI --> CM
  API --> CM
  API --> MYSQL
  CM --> MYSQL
  CM --> REDIS
  CM --> CMS
  CM --> CA
  API --> CA
  PROXY --> REDIS
  PROXY --> CM
  PROXY --> CERT
  DNS --> PROXY
  WAITDNS --> DNS
  INIT --> CM
  CUBELET --> CM
  EG --> CA
  EG --> CUBELET
  EGNET --> EG
```

## 2. 资源与镜像职责

### 2.1 控制面

| 资源 | 模板 | 镜像 / 数据 | 说明 |
| --- | --- | --- | --- |
| `cube-master` | `templates/master.yaml` | `images.master` | 复用 `CubeMaster/docker/Dockerfile`；启动时挂载 Chart 渲染的 `conf.yaml`；数据库迁移由 CubeMaster 内置逻辑完成 |
| `cube-master-config` | `templates/master-config-secret.yaml` | `deploy/kubernetes/chart/files/cube-master/conf.yaml` 渲染结果 | 注入 MySQL / Redis / CA 等运行配置 |
| `cube-master-storage` | `templates/master.yaml` / `templates/master-pvc.yaml` | PVC / existingClaim / hostPath / emptyDir | 对应 One Click `/data/CubeMaster/storage` artifact 目录；默认使用 PVC，避免多 control 节点场景下 hostPath 数据绑定单节点 |
| `cube-api` | `templates/api.yaml` | `images.api` | 暴露 HTTP API，连接 CubeMaster 和 MySQL |
| `cubemastercli` | `templates/cubemastercli.yaml` | `images.cubemastercli` | 运维 CLI Pod；只内置真实 `CubeMaster/bin/cubemastercli`，Chart 注入本 Release 的 CubeMaster endpoint |
| `cube-webui` | `templates/webui.yaml` | `images.webui` + nginx ConfigMap | 提供控制台入口，`/cubeapi/` 代理到 CubeAPI |
| `cube-secret` | `templates/secret.yaml` | MySQL / Redis / Proxy 密码 | Chart 管理内置依赖和组件间连接密码 |

### 2.2 内置或第三方 MySQL / Redis

| 模式 | 行为 |
| --- | --- |
| 内置 MySQL | `mysql.host=""` 时安装 `cube-mysql` StatefulSet / Headless Service / volumeClaimTemplates，显式配置 `mysql.persistence.hostPath` 时才使用 hostPath |
| 第三方 MySQL | `mysql.host` 非空时不安装内置 MySQL，CubeMaster / CubeAPI 使用外部地址 |
| 内置 Redis | `redis.host=""` 且控制面或 CubeProxy 需要 Redis 时安装 `cube-redis` StatefulSet / Headless Service / volumeClaimTemplates |
| 第三方 Redis | `redis.host` 非空时不安装内置 Redis，CubeProxy / CubeMaster 使用外部地址 |

### 2.3 计算面 Big Pod

`cube-node` DaemonSet 是计算节点上的 Big Pod。它只调度到
`placement.compute.nodeSelector` 命中的节点，Chart 不负责给节点打 label。

| 容器 | 类型 | 镜像 | 职责 |
| --- | --- | --- | --- |
| `wait-cube-dns` | Init Container | `cubeNode.dns.checkImage` | 等待 node-local `cube-dns` 可用，并验证 `cube.app`、wildcard、Kubernetes Service 域名解析 |
| `pvm-host-bootstrap` | Init Container，可选 | `images.pvmHostBootstrap` | 安装/配置 PVM host kernel，必要时协调节点重启 |
| `cube-node-init` | Init Container | `images.nodeInit` | 节点预检和准备：KVM、XFS、内存、glibc、cgroup、cubecow 依赖、CIDR 冲突、CubeMaster 连通性 |
| `cube-node` | 主容器 | `images.node` | 运行 cubelet 和 network-agent；选择 guest kernel；向 CubeMaster 注册节点 |
| `cube-egress` | Sidecar | `images.cubeEgress` | 透明出站代理，提供 loopback admin health |
| `cube-egress-net` | Sidecar | `images.cubeEgressNet` | 管理 host network namespace 中的 TPROXY、ip rule、sysctl 规则 |

关键 hostPath：

| hostPath | 用途 |
| --- | --- |
| `/data/cubelet` | cubelet 数据和 `cubelet.sock` |
| `/tmp/cube` | network-agent gRPC socket |
| `/data/cube-shim` | cube runtime/shim 运行数据 |
| `/data/snapshot_pack` | snapshot pack 数据 |
| `/data/log` | Cube Node / shim / egress 日志 |
| `/dev`、`/sys`、`/lib/modules` | KVM、内核模块、网络和系统能力 |

### 2.4 数据面入口

| 资源 | 模板 | 职责 |
| --- | --- | --- |
| `cube-proxy-node` Deployment | `templates/proxy-node.yaml` | 提供 sandbox HTTP/HTTPS 数据面入口，使用 `placement.controlPlane` 与 one-click control 节点语义对齐，并使用 `hostNetwork` 监听节点 `80/443` |
| `cube-proxy-certs` Secret / Certificate | `templates/proxy-node.yaml` | TLS 证书，支持 selfSigned、inline、existingSecret、certManager |
| `cube-dns` | `templates/dns.yaml` | 提供 sandbox 域名解析；默认 node-local |

CubeProxy 默认保持 One Click 的 host-network 语义：

- `cubeProxy.hostNetwork=true`，Pod IP 等于所在节点 HostIP。
- `cube-proxy-node` 复用 `placement.controlPlane`，与 one-click control 节点上的 CubeProxy 对齐。
- `cube-dns` 复用 `placement.compute`，与 `cube-node` 保持同一组计算节点。
- nginx 监听节点 `80/443`，Chart 启动脚本会把镜像默认 `8081/8080` patch 为 values 中配置的端口。
- node-local `cube-dns` 应通过 `cubeProxy.advertiseIP` 或 `cubeDns.answerIP` 返回 control 节点 CubeProxy 入口。
- CubeProxy 通过 Redis 中的 owner `HostIP:hostPort` 元数据转发到目标 compute 节点 sandbox。
- nginx `global.conf` 中写入 `resolver`，Lua Redis 客户端可以解析内置或第三方 Redis DNS 名称。
- Chart 不修改 CubeProxy Lua 后端解析语义；跨节点访问仍遵循社区 CubeProxy 使用 Redis `HostIP:hostPort` 的原始路径。

## 3. 默认 DNS 架构

默认 `cubeDns.mode=nodeLocal`：

```mermaid
sequenceDiagram
  participant CN as cube-node Pod
  participant DNS as cube-dns on same node
  participant KDNS as Kubernetes DNS
  participant PX as cube-proxy-node on control node

  CN->>DNS: resolve cube.app / *.cube.app via 127.0.0.54
  DNS-->>CN: A record = cubeProxy.advertiseIP / cubeDns.answerIP
  CN->>DNS: resolve kubernetes.default.svc.cluster.local
  DNS->>KDNS: forward cluster/service DNS query
  KDNS-->>DNS: ClusterIP answer
  DNS-->>CN: ClusterIP answer
  CN->>PX: sandbox HTTP/HTTPS traffic
```

关键点：

- `cube-dns` 以 DaemonSet + `hostNetwork` 运行在计算节点。
- `cube-dns` 监听 `127.0.0.54:53`；启用 `cubeDns.sandboxGateway.enabled`
  时也监听当前 compute 节点 HostIP，供 sandbox guest 作为 nameserver。
- `cube-proxy-node` 以 Deployment + `hostNetwork` 运行在 control 节点，监听节点 `80/443`。
- `cube-node` 使用 `dnsPolicy: None`，显式配置 `nameserver 127.0.0.54`。
- `cube.app` / `*.cube.app` 优先解析为 `cubeDns.answerIP`，其次为 `cubeProxy.advertiseIP`；两者为空时才回退到当前 compute HostIP。
- 其他域名转发到 `cubeDns.forward.upstreams`，为空时使用 `/etc/resolv.conf`。
- Chart 不修改宿主机全局 DNS，不影响非 Cube Pod。
- Cube sandbox guest DNS 由 `cubeNode.dns.sandbox` 单独写入 Cubelet dynamicconf。
- 默认写入当前 compute 节点 HostIP，让 guest 使用 node-local `cube-dns`。
- sandbox 访问 Kubernetes Service ClusterIP 可能绕过宿主机 kube-proxy DNAT；Chart 不再渲染 sandbox service proxy 资源或对应 DNS override。需要暴露给 sandbox 的 in-cluster Service 应由平台网络层、AgentWay provider 或 operator 管理的外部代理处理。
- 外部客户端、浏览器、SDK 或未显式使用该 `dnsConfig` 的 Pod，需要由使用方配置 DNS / 负载均衡 / Ingress，把 `cubeProxy.domain` 与 wildcard 子域名指向 CubeProxy 入口。

可选 `cubeDns.mode=service`：

- `cube-dns` 以 Deployment + ClusterIP Service 运行。
- `cube.app` / wildcard 必须通过 `cubeDns.answerIP` 或 `cubeProxy.advertiseIP` 返回明确的 CubeProxy 入口。
- 使用方需要自行把客户端 DNS 或上游 DNS 指向该 Service。

## 4. 安装与启动流程

### 4.1 Helm 渲染与校验

```mermaid
flowchart TD
  A["helm upgrade/install"] --> B["templates/validate.yaml"]
  B --> C{"values 组合是否合法?"}
  C -- 否 --> X["fail render"]
  C -- 是 --> D["渲染 Secret / ConfigMap / 持久化卷"]
  D --> E["渲染 MySQL / Redis 或使用第三方服务"]
  E --> F["渲染控制面 Deployment"]
  F --> G["渲染 cube-dns / cube-proxy-node"]
  G --> H["渲染 cube-node DaemonSet"]
  H --> I["等待 --wait / rollout / helm test"]
```

主要 validate 规则：

- `controlPlane.enabled=true` 时必须配置 `placement.controlPlane.nodeSelector`。
- `cubeNode.enabled=true` 时必须配置 `placement.compute.nodeSelector`。
- `cubeProxy.enabled=true` 时必须配置 `placement.controlPlane.nodeSelector`。
- `cubeDns.enabled=true` 时必须配置 `placement.compute.nodeSelector`。
- compute-only 模式必须显式配置 `externalControlPlane.masterEndpoint`。
- `cubeNode.dns.useCubeDns=true` 时要求 `cubeDns.enabled=true`、`cubeDns.mode=nodeLocal`、`security.hostNetwork=true`。
- `cubeDns.mode` 只能为 `nodeLocal` 或 `service`。
- PVM host kernel bootstrap 只能在明确命中 selector 的节点上执行。

### 4.1.1 调度与时区

- CubeMaster、CubeAPI、WebUI、cubemastercli、内置 MySQL、内置 Redis、CubeProxy 使用 `placement.controlPlane`。
- `cube-node`、`cube-dns` 使用 `placement.compute`。
- 所有 Chart 管理的 Cube 容器、sidecar 和 initContainer 都通过 `global.timezone` 注入 `TZ`，默认 `Asia/Shanghai`。

### 4.2 控制面启动

```mermaid
sequenceDiagram
  participant H as Helm
  participant DB as MySQL
  participant R as Redis
  participant CM as CubeMaster
  participant API as CubeAPI
  participant WEB as WebUI
  participant CLI as cubemastercli

  H->>DB: create/use MySQL
  H->>R: create/use Redis
  H->>CM: mount conf.yaml + storage + CA
  CM->>DB: run embedded schema migration
  CM-->>H: /notify/health ready
  H->>API: start with CUBE_MASTER_ENDPOINT + MySQL config
  API->>CM: call CubeMaster
  API->>DB: read/write business data
  API-->>H: /health ready
  H->>WEB: render nginx upstream to CubeAPI
  WEB->>API: proxy /cubeapi/
  H->>CLI: inject CUBEMASTERCLI_ADDRESS / CUBEMASTERCLI_PORT
  CLI->>CM: cubemastercli --address ... --port ... node list
```

说明：

- Chart 不交付独立 `cube-db-migrate` Job。
- `cubemastercli` 只通过独立 `cubemastercli` 运维镜像交付；不混入 `cube-master` 或 `cube-node` 运行镜像，不提供 `ctl` wrapper。
- CubeMaster 内置 migration SQL，启动时自行迁移。
- CubeMaster artifact storage 默认使用 PVC，可切换到 existingClaim；hostPath 仅适合单节点临时环境。

### 4.3 计算节点启动

```mermaid
sequenceDiagram
  participant DS as cube-node DaemonSet
  participant DNS as wait-cube-dns
  participant PVM as pvm-host-bootstrap
  participant INIT as cube-node-init
  participant CN as cube-node
  participant EG as cube-egress
  participant EN as cube-egress-net
  participant CM as CubeMaster

  DS->>DNS: wait 127.0.0.54 and validate DNS records
  DNS-->>DS: success
  opt bootstrap.pvmHostKernel.enabled=true
    DS->>PVM: install/check PVM host kernel
    PVM-->>DS: success or reboot-required failure signal
  end
  DS->>INIT: host preflight and preparation
  INIT->>INIT: check kvm_pvm / CUBE_PVM_ENABLE consistency
  INIT->>CM: /notify/health connectivity check
  INIT-->>DS: success
  DS->>CN: start cubelet + network-agent
  CN->>CN: select guest kernel vmlinux-bm/vmlinux-pvm
  CN->>CM: register node and heartbeat
  DS->>EG: start egress worker
  DS->>EN: wait cube-dev and apply TPROXY rules
```

`cube-node` 使用 startupProbe、readinessProbe、livenessProbe：

- startupProbe：等待 cubelet 9999 启动，避免慢启动期间被 liveness 提前杀死。
- readiness/liveness：检查 cubelet 9999。
- `cube-egress`：检查 `127.0.0.1:9090/admin/v1/health`。
- `cube-egress-net`：检查 `cube-dev`、ip rule、table 100 local route、mangle `TRANSPROXY` 80/443 规则。

### 4.4 节点注册与健康链路

```mermaid
flowchart LR
  CN["cube-node\ncubelet"] -->|register / heartbeat| CM["CubeMaster"]
  API["CubeAPI"] -->|list nodes / sandbox API| CM
  WEB["WebUI"] -->|/cubeapi/*| API
  HT["helm test cube-health-test"] --> API
  HT --> CM
  HT --> K8S["Kubernetes API\nDaemonSet ready / Pod containers"]
```

验收关注点：

- CubeMaster `/notify/health` 成功。
- CubeAPI `/health` 成功。
- CubeAPI 能查询到 healthy node。
- `cube-node` DaemonSet ready 数等于命中 selector 的节点数。
- `cube-egress` / `cube-egress-net` sidecar 存在且 Ready。

## 5. 运行期关键数据流

### 5.1 WebUI / API / Master / DB

```mermaid
flowchart LR
  U["Browser / Operator"] --> WEB["cube-webui Service"]
  WEB -->|/cubeapi/*| API["cube-api Service"]
  API --> CM["cube-master Service"]
  API --> MYSQL[("MySQL")]
  CM --> MYSQL
  CM --> REDIS[("Redis")]
```

用途：

- 控制台操作、模板管理、sandbox 查询等通过 CubeAPI。
- CubeAPI 读写业务数据到 MySQL。
- CubeMaster 维护节点和任务元数据，使用内置迁移保证 schema。

### 5.2 Sandbox 入口流量

```mermaid
flowchart LR
  CLIENT["Client / Sandbox domain"] --> DNS["DNS: cube.app / wildcard"]
  DNS --> PROXY["cube-proxy-node"]
  PROXY --> REDIS[("Redis state")]
  PROXY --> CM["CubeMaster"]
  PROXY --> NODE["Target Cube Node / Sandbox"]
```

说明：

- `cube-proxy-node` 默认启用，随 Chart 一起安装和卸载。
- `cube-proxy-node` 默认使用 `hostNetwork`，确保 control 节点能够接入 `80/443` 流量。
- Chart 不创建 `cube-proxy-node` ClusterIP Service，避免 Kubernetes 随机分发到非目标节点后偏离 One Click 的显式 CubeProxy host 入口模型。
- 需要多节点统一入口时，应由外部 DNS/LB 明确指向预期 control 节点 CubeProxy 或保证社区 CubeProxy 的 `HostIP:hostPort` 跨节点路径可用，而不是依赖默认 ClusterIP 随机分流。
- TLS 支持 selfSigned、existingSecret、inline、certManager。
- 生产环境应提供正式证书，并把 sandbox domain / wildcard DNS 指向明确的 CubeProxy 入口。
- Chart 自带 `cube-dns` 默认只服务 `cube-node` Big Pod；面向用户访问的外部 DNS、LB 或 Ingress 需要由使用方显式配置。

### 5.3 Sandbox 出站 egress

```mermaid
flowchart LR
  SB["Sandbox traffic"] --> DEV["cube-dev interface"]
  DEV --> EGNET["cube-egress-net\nTPROXY/ip-rule"]
  EGNET --> EG["cube-egress\nOpenResty"]
  EG --> EXT["External endpoint"]
  EG --> CA["cube-egress-ca"]
```

说明：

- `cube-egress-net` 只负责 host network 规则。
- `cube-egress` 负责透明代理和证书能力。
- CubeMaster / CubeAPI / Cube Node 共享 `cube-egress-ca` Secret，保证模板构建、AgentHub/OpenClaw 注入和运行期信任一致。

### 5.4 模板构建

```mermaid
flowchart LR
  API["CubeAPI"] --> CM["CubeMaster"]
  CM --> TB["template-builder sidecar\noptional docker:dind"]
  CM --> ST[("CubeMaster storage hostPath/PVC")]
  TB --> ST
```

说明：

- `controlPlane.templateBuilder.enabled=true` 时，CubeMaster Pod 内增加 `template-builder` sidecar。
- sidecar 默认使用 `docker:27-dind`，给模板构建提供 Docker/BuildKit 能力。
- 构建产物写入 CubeMaster artifact storage。

## 6. external control plane / compute-only 模式

compute-only 模式对齐 One Click 的计算节点单独交付场景。

```mermaid
flowchart TB
  subgraph EXT["External Control Plane"]
    ECM["External CubeMaster"]
    EAPI["External CubeAPI optional"]
    EDB[("External MySQL / Redis")]
  end

  subgraph NS["Compute Namespace"]
    DNS["cube-dns"]
    NODE["cube-node DaemonSet"]
  end

  NODE --> ECM
  DNS --> NODE
  EAPI --> ECM
  ECM --> EDB
```

关键 values：

```yaml
controlPlane:
  enabled: false
externalControlPlane:
  enabled: true
  masterEndpoint: <external-master>:8089
  apiEndpoint: http://<external-api>:3000 # optional, for helm test
```

行为：

- 不安装 Chart 内置 Master / API / MySQL / Redis / WebUI。
- `cube-node` 使用 `externalControlPlane.masterEndpoint` 注册外部 CubeMaster。
- 如果配置 `externalControlPlane.apiEndpoint`，Helm test 会校验外部 API 和节点注册。
- 默认不安装 `cube-proxy-node`，避免 compute-only release 留下与外部控制面不一致的数据面资源。

## 7. 关键 values 开关

| values 路径 | 默认 | 影响 |
| --- | --- | --- |
| `global.timezone` | `Asia/Shanghai` | 注入所有 Chart 管理的 Cube 容器、sidecar 和 initContainer 的 `TZ` |
| `storageClass.create` | `true` | 是否创建 Chart 默认的状态组件 StorageClass |
| `storageClass.name` | `cube-cbs-wffc` | CubeMaster storage、内置 MySQL、内置 Redis 默认使用的 StorageClass |
| `storageClass.volumeBindingMode` | `WaitForFirstConsumer` | 多可用区 TKE 集群中等待 Pod 选中 control 节点后再创建 CBS 盘，避免 PV zone 与 control 节点不匹配 |
| `controlPlane.enabled` | `true` | 是否部署内置控制面 |
| `externalControlPlane.enabled` | `false` | 是否使用外部 CubeMaster |
| `placement.controlPlane.nodeSelector` | `cube.tencent.com/role=control` | 控制 CubeMaster、CubeAPI、WebUI、cubemastercli、内置 MySQL、内置 Redis、CubeProxy 调度范围 |
| `placement.compute.nodeSelector` | 含 `allow-pvm-bootstrap=true` | 控制 `cube-node`、`cube-dns` 调度范围，并要求节点显式允许 PVM bootstrap |
| `cubeDns.enabled` | `true` | 是否交付 CubeDNS |
| `cubeDns.mode` | `nodeLocal` | node-local DNS 或 ClusterIP DNS |
| `cubeDns.sandboxGateway.enabled` | `true` | node-local DNS 是否同时监听 compute HostIP，供 sandbox guest 使用 |
| `cubeNode.dns.useCubeDns` | `true` | `cube-node` 是否显式使用 `127.0.0.54` |
| `cubeNode.dns.sandbox.useCubeDns` | `true` | 是否把 sandbox guest `/etc/resolv.conf` 指到 cube-dns；默认写当前 compute HostIP，让 guest 使用 node-local `cube-dns` |
| `cubeNode.dns.sandbox.nameservers` | `[]` | 覆盖写入 sandbox guest `/etc/resolv.conf` 的 DNS server |
| `cubeNode.pvmGuestKernel.enabled` | `true` | 是否选择 PVM guest kernel；`cube-node-init` 校验该值与 `kvm_pvm` 状态一致 |
| `bootstrap.pvmHostKernel.enabled` | `true` | 是否执行 host kernel bootstrap；默认可能安装 host kernel 并按租约重启计算节点 |
| `bootstrap.pvmHostKernel.bootArgs` | `nopti pti=off` | PVM host kernel 启动参数；当前 `kvm_pvm` 不支持 host KPTI，默认关闭 PTI |
| `bootstrap.nodeInit.*` | 多项 | 控制节点预检、XFS、KVM、CIDR 检测 |
| `mysql.host` | `""` | 非空时使用第三方 MySQL |
| `redis.host` | `""` | 非空时使用第三方 Redis |
| `cubeProxy.enabled` | `true` | 是否部署 control 节点 CubeProxy 数据面入口 |
| `cubeProxy.advertiseIP` | `""` | `cubeDns.answerIP` 为空时返回给 `cube.app` / wildcard 的 control 节点 CubeProxy 入口 IP |
| `cubeEgress.enabled` | `true` | 是否在 Big Pod 中启用 egress sidecar |
| `webui.enabled` | `true` | 是否部署 WebUI |
| `controlPlane.templateBuilder.enabled` | `false` | 是否启用模板构建 sidecar |

## 8. Helm test 覆盖

`templates/tests/` 提供 Chart 内置验收：

| Test Pod | 覆盖内容 |
| --- | --- |
| `<release>-health-test` | CubeMaster、CubeAPI、节点注册、WebUI、CubeProxy、DaemonSet/Deployment/StatefulSet ready、Egress sidecar 存在性 |
| `<release>-mysql-test` | 内置 MySQL `mysqladmin ping` |
| `<release>-redis-test` | 内置 Redis `PING` |
| `<release>-dns-test` | `cube.app`、wildcard、Kubernetes Service 域名解析 |
| `<release>-node-image-test` | `cube-node` 镜像内 runtime 工具和必需 asset |
| `<release>-node-runtime-test` | 计算节点 host runtime：`/dev/kvm`、cubelet socket、network-agent socket、node-local DNS |

执行：

```bash
helm test <release> -n <namespace> --timeout 20m --logs
```

## 9. 资源所有权与卸载边界

Chart 管理并随 release 卸载：

- 控制面 Deployment / Service；
- 内置 MySQL / Redis；
- CubeDNS；
- CubeProxy；
- CubeNode DaemonSet；
- CA / TLS / config Secret；
- Helm test RBAC；
- diagnostics ConfigMap。

Chart 不管理：

- 使用方给节点打的 label / taint；
- 第三方 MySQL / Redis；
- 外部 DNS / 负载均衡；
- hostPath 数据目录；
- host kernel、GRUB、udev、fstab 或 XFS 等节点级持久修改。
- One Click 单节点 seed SQL / demo 数据；Chart 依赖真实 `cube-node` Pod 注册节点。

因此卸载后，节点级数据和外部接入应按平台 runbook 清理。
