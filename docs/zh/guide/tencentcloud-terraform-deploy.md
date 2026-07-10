# 腾讯云集群部署（Terraform）

本指南介绍如何使用发布包中自带的 Terraform 部署器，在腾讯云上一键拉起一个**集群版** Cube Sandbox：托管的 TKE 控制面运行 `cube-master` / `cube-api` / `cube-proxy` / `cube-lifecycle-manager` / `cube-webui`，后端使用云数据库 MySQL + Redis，并配备一个或多个 CVM PVM 计算节点。一台跳板机（SSH 端口为 `443`）作为构建主机和私有 VPC 的堡垒机。

::: tip 网络加固
集群版的网络加固由**腾讯云安全组**完成：部署器按角色创建 **4 个独立安全组**（跳板机 / 计算节点 / TKE Pod / CLB），各自按最小权限放行（如对公网仅放行必要的入口，计算节点与 TKE 节点无任何公网入站），计算节点不分配公网 IP。**默认采用内网模式**（`TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='false'`）：WebUI / cube-api / cube-proxy 三个用户侧服务关联**内网 CLB**，仅 VPC 内部（经跳板机 / VPN）可访问，不对公网暴露；如需公网访问，须显式设置 `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='true'` 切换为公网 CLB。如需进一步收紧，可在[腾讯云安全组控制台](https://console.cloud.tencent.com/vpc/securitygroup)按需对上述各个安全组（`cubesandbox-sg-jumpserver` / `cubesandbox-sg-compute` / `cubesandbox-sg-tke-pod` / `cubesandbox-sg-clb`）分别调整入站 / 出站规则。**当开启公网模式时**，请额外参阅[公网服务加固建议](#公网服务加固建议)。
:::

::: tip 适用场景
本部署利用云上资源**快速搭建一套高可用的 CubeSandbox 沙箱服务**：所有云资源默认按量计费（详见下文[计费模式](#计费模式)），用完即可通过 `destroy.sh` 一键释放。如果想长期使用，推荐改用**包年包月**资源以获得更优的成本节省（见[计费模式](#计费模式)）。如果只需要单机部署验证，请参阅之前的部署文档：[PVM 部署](./pvm-deploy.md)或[裸金属部署](./bare-metal-deploy.md)。

**注意**：默认配置为 **POC / 功能验证**（2 台 `SA9.MEDIUM8` 计算节点、控制面单副本、无 CFS）。承载沙箱数量有限。生产或压测请调整计算节点与 TKE worker 的规格和数量，详见[节点规格与容量规划](#节点规格与容量规划)与[默认部署模式](#默认部署模式)。
:::

## 架构概览

```
                          公网 / Internet
                            │
               ┌────────────┴────────────────────┐
               │                                 │
      ┌────────┴────────┐             ┌──────────┴─────────┐
      │   跳板机 CVM    │             │  CLB (内网/公网)    │
      │  (公网 IP)      │             │  cube-api :3000    │
      │  SSH:443        │             │  cube-proxy :80/443 │
      │  build & push   │             │  cube-webui :80    │
      └────────┬────────┘             └──────────┬─────────┘
               │                                 │
  ┌────────────┼─────────────────────────────────┼──────────┐
  │            │            VPC 内网              │           │
  │  ┌─────────┴────────┐       ┌───────────────┴─────┐    │
  │  │  CVM 计算节点 ×N │       │    TKE 托管集群          │
  │  │  Cubelet         │       │  cube-master (×1)       │
  │  │  network-agent   │       │  cube-api (×1)          │
  │  │  CubeEgress      │       │  cube-proxy (×1)        │
  │  └──────────────────┘       │  cube-lifecycle-manager │
  │                             │  cube-webui (×1)        │
  │                             └───┬─────────────┬────────┘
  │                                 │             │         │
  │                    ┌────────────┴───┐ (可选)  │         │
  │                    │  CFS (NFS)     │         │         │
  │                    │  共享存储       │         │         │
  │                    └────────────────┘         │         │
  │                                               │         │
  │                         ┌─────────────────────┴───┐     │
  │                         │  云数据库 MySQL + Redis  │     │
  │                         └─────────────────────────┘     │
  │                                                         │
  │  ┌──────────────┐       ┌──────────────┐                │
  │  │ TCR (可选)   │       │  NAT + EIP   │→ 公网出口     │
  │  └──────────────┘       └──────────────┘                │
  └─────────────────────────────────────────────────────────┘
```

| 组件 | 形态 | 说明 |
|------|------|------|
| 跳板机 | CVM（公网 IP，SSH 443） | 构建镜像（TCR 模式）、作为私有 VPC 的堡垒机 |
| 负载均衡 | CLB（内网/公网） | 前置 `cube-api` / `cube-proxy` / `cube-webui`，默认内网模式，用户流量入口 |
| 控制面 | TKE 托管集群 | 运行 `cube-master` / `cube-api` / `cube-proxy` / `cube-lifecycle-manager` / `cube-webui` |
| 计算节点 | CVM PVM | 运行 `Cubelet` / `network-agent` / `CubeEgress`，**实际承载沙箱** |
| 数据库 | 云数据库 MySQL 8.0 + Redis 7.0 | 仅 VPC 内网访问，不开公网 |
| 共享存储 | CFS（通用标准型 NFS，**可选**） | `USE_CFS=true` 且 cubemaster 多副本时，ReadWriteMany 共享 `/data/CubeMaster/storage` |
| 镜像仓库 | TCR（基础版，**可选**） | `USE_TCR=true` 时创建；跳板机构建并推送组件镜像 |
| 网络出口 | NAT 网关 + EIP | 整个 VPC 通过 NAT 访问公网 |

::: info TKE worker 与 PVM 计算节点是两套资源
- **`TENCENTCLOUD_TKE_NODE_COUNT`**：TKE **worker** 数量，运行控制面 Pod（cubemaster / cube-api 等）。
- **`TENCENTCLOUD_COMPUTE_NODE_COUNT`**：**PVM 计算节点**数量，运行 Cubelet，**执行 sandbox**。
两者独立配置，默认均为 `2`，但职责完全不同。
:::

## 默认部署模式

与 `env.example` / `variables.tf` 一致，**默认是公网镜像 + 单副本控制面 + 无 CFS 的 POC 配置**：

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `TENCENTCLOUD_USE_TCR` | `false` | 不创建 TCR；使用公网预置镜像，不在跳板机构建 |
| `TENCENTCLOUD_USE_CFS` | `false` | 不创建 CFS；cubemaster 使用 Pod 本地存储 |
| `TENCENTCLOUD_CUBEMASTER_REPLICAS` 等 | `1` | 控制面组件默认单副本 |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | `2` | PVM 计算节点 |
| `TENCENTCLOUD_TKE_NODE_COUNT` | `2` | TKE worker（`worker_config.count`） |
| `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK` | `false` | cube-api / cube-proxy / cube-webui 使用 VPC 内网 CLB |

**高级模式：**

- `TENCENTCLOUD_USE_TCR=true`：创建 TCR，在跳板机构建并推送五个组件镜像。
- `TENCENTCLOUD_USE_CFS=true` 且 `TENCENTCLOUD_CUBEMASTER_REPLICAS>1`：创建 CFS，cubemaster 多副本共享存储。
- `TENCENTCLOUD_CUBE_PROXY_REPLICAS>1`：让 cube-proxy 以多副本方式运行，并复用同一个 CLB 对外服务。

`cube-proxy` 支持**多副本**。默认仍为单副本（`TENCENTCLOUD_CUBE_PROXY_REPLICAS=1`），适合小规模部署；当代理流量增长时，可以按需调高。每个 cube-proxy 副本都会把自己的 admin endpoint 注册到 Redis，独立的 `cube-lifecycle-manager` 从 Redis 注册表发现副本并协调自动暂停 / 自动恢复，因此无需静态配置副本列表。

## 默认配置创建的资源明细

下表列出**默认配置**（地域 `ap-guangzhou`、可用区 `ap-guangzhou-6`、计算节点 2、TKE worker 2、无 CFS/TCR）下创建的云资源。所有资源均为**按量计费**（详见[计费模式](#计费模式)）。

| 资源类型 | 数量 | 规格 / 配置 |
|---------|------|------------|
| VPC | 1 | CIDR `10.0.0.0/16` |
| 子网 | 1 | `10.0.1.0/24`（主可用区；仅当某角色落在其他可用区时才额外创建 /24 子网） |
| NAT 网关 + EIP | 1 + 1 | 带宽 200 Mbps，按流量计费 |
| 路由表条目 | 1 | `0.0.0.0/0` → NAT 网关 |
| 安全组 | 4 | 按角色拆分（跳板机 / 计算节点 / TKE Pod / CLB），最小权限，见下表 |
| SSH 密钥对 | 1 | 自动生成于 `terraform/tencentcloud/.ssh/` |
| 跳板机 CVM | 1 | `SA9.MEDIUM4`（2C4G），系统盘 50GB 通用型 SSD 云硬盘，公网带宽 200 Mbps，SSH 端口 443 |
| 计算节点 CVM | 2 | `SA9.MEDIUM8`（4C8G），系统盘 50GB + **200GB CBS 数据盘**（XFS，`/data/cubelet`），**无公网 IP** |
| TKE 托管集群 | 1 | 托管集群 **L5**，Kubernetes `1.34.1`，Pod CIDR `10.200.0.0/16`，Service CIDR `192.168.0.0/20`，仅 VPC 内网 API |
| TKE worker 节点 | 2 | `SA9.LARGE8`（4C8G），由 `worker_config.count` 创建（**无独立节点池**） |
| 云数据库 MySQL | 1 | 8.0 InnoDB 通用型，4GB 内存 / 200GB 存储，跨可用区双机（地域有 ≥2 可用区时）/ 半同步，仅内网 3306 |
| 云数据库 Redis | 1 | 7.0 标准架构（主从），1GB 内存，端口 6379，仅内网 |
| CFS 文件系统 | 0（默认） | `USE_CFS=true` 时创建 1 个通用标准型 NFS |
| TCR 镜像仓库 | 0（默认） | `USE_TCR=true` 时创建基础版 + 命名空间 + VPC 接入 |
| 操作系统镜像 | — | OpenCloudOS Server 9（公共镜像，CVM 复用） |

::: tip OS 镜像
所有 CVM（跳板机 / 计算节点 / TKE worker）默认使用 **OpenCloudOS Server 9** 公共镜像，可用 `TENCENTCLOUD_IMAGE_NAME` 覆盖。
:::

### 安全组放行端口

部署器按**最小权限**原则创建 **4 个按角色拆分的安全组**，各自只放行该角色实际需要的入站端口；攻破任一角色都不会继承其他角色的入站面。

**1. `cubesandbox-sg-jumpserver`（跳板机）**

| 端口 / 范围 | 来源 | 用途 |
|------------|------|------|
| TCP 443 | `0.0.0.0/0` | 跳板机 SSH（cloud-init 已将 sshd 改到 443） |
| ALL | `10.0.0.0/16` | VPC 内网互通 |

**2. `cubesandbox-sg-compute`（计算节点）** — 无任何公网入站

| 端口 / 范围 | 来源 | 用途 |
|------------|------|------|
| ALL | TKE Pod CIDR | cube-proxy（Pod）访问计算节点的全部端口（沙箱动态端口 20000-29999） |
| ALL | `10.0.0.0/16` | VPC 内网互通（跳板机管理、cube-master 调度） |

**3. `cubesandbox-sg-tke-pod`（TKE worker 节点）** — 无任何公网入站

| 端口 / 范围 | 来源 | 用途 |
|------------|------|------|
| ALL | TKE Pod CIDR | Pod 间通信 |
| ALL | `10.0.0.0/16` | VPC 内网（CLB 健康检查、跳板机管理、CFS NFS 挂载） |

**4. `cubesandbox-sg-clb`（负载均衡 CLB）**

下表中 80 / 443 / 3000 三个入口的来源取决于 `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK`：**默认内网模式（`false`）**下来源为 VPC CIDR `10.0.0.0/16`，仅 VPC 内网可达；**开启公网模式（`true`）**时来源为 `0.0.0.0/0`，对公网开放。

| 端口 / 范围 | 来源（内网模式 / 公网模式） | 用途 |
|------------|------|------|
| TCP 80 | `10.0.0.0/16` / `0.0.0.0/0` | cube-proxy + cube-webui 的 CLB（HTTP） |
| TCP 443 | `10.0.0.0/16` / `0.0.0.0/0` | cube-proxy 的 CLB（HTTPS） |
| TCP 3000 | `10.0.0.0/16` / `0.0.0.0/0` | cube-api 的 CLB |
| TCP 8089 | `10.0.0.0/16`（始终仅 VPC 内网） | cube-master 的内网 CLB（不受公网开关影响） |

四个安全组出站均默认放行全部（`0.0.0.0/0` ALL）。数据库、TKE API Server 等均**不开公网**，仅限 VPC 内网访问。

### 公网服务加固建议

> 本节仅在**开启公网模式**（`TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='true'`）时适用。默认内网模式下三个服务均不对公网暴露，可跳过本节。

开启公网模式后，`cubesandbox-sg-clb` 会对 `0.0.0.0/0` 放行 WebUI（80）、cube-proxy（80 / 443）与 cube-api（3000）三个公网入口。三者的安全模型不同，建议按服务分别加固：

- **WebUI（CLB 80）**：WebUI 控制台**目前不带任何鉴权 / 权限控制**，任何能访问到它的人都能操作沙箱。强烈建议为 WebUI 的 CLB **单独创建一个安全组**，并在其中配置**严格的源 IP 白名单**（仅放行你的办公网 / 管理机出口 IP），而不是沿用对公网放行的 `cubesandbox-sg-clb`。可在[腾讯云安全组控制台](https://console.cloud.tencent.com/vpc/securitygroup)新建安全组后，绑定到 WebUI 对应的 CLB 实例。
- **cube-api（CLB 3000）**：cube-api 默认**对所有请求放行、不做凭证校验**。对外暴露前请务必启用 **Auth Callback** 鉴权，把鉴权决策委托给你自己的鉴权服务，详见[鉴权配置](./authentication.md)。
- **cube-proxy（CLB 80 / 443）**：cube-proxy 是沙箱流量的公网入口，设计上即面向公网。若希望限制对沙箱的公开访问，请参阅[限制公开访问](./restrict-public-access.md)启用每沙箱独立的入站 token 等机制。

## 前置条件

运行 `create.sh` 的机器只需满足以下条件，**无需预装 Terraform**：

- **腾讯云 API 凭证**：在[访问管理控制台](https://console.cloud.tencent.com/cam/capi)创建密钥对，导出 `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`。
- **本地工具**：`ssh`、`scp`、`nc`，以及访问腾讯云 API 的网络。`terraform` 与 `jq` 在缺失时会自动安装——可写时装到 `/usr/local/bin`（如以 root 运行），否则装到本地 `.bin/`。`terraform` 从 HashiCorp 官方下载（需 `curl`/`wget` + `unzip`），`jq` 来自系统包管理器或 GitHub 静态二进制。
- `mkcert` / `openssl` **无需**本地安装——cube-proxy 证书在跳板机上生成。
- **构建 / 部署执行机**：推荐 Linux；`create.sh` / `destroy.sh` 也支持 macOS（含 Bash 3.2）。Windows 请使用 WSL2。

### Terraform Provider Init 加速（推荐）

`terraform init` 默认会从 `registry.terraform.io` 和 GitHub 下载 Provider。国内网络环境下，`tencentcloudstack/tencentcloud` Provider 可能在下载 checksum 或安装包时超时，典型报错为 `failed to retrieve authentication checksums`、`Client.Timeout exceeded while awaiting headers`。腾讯云官方提供了 Terraform Provider 镜像源，可通过 Terraform CLI 配置文件启用（参考：[Init 加速](https://cloud.tencent.com/document/product/1653/82912)）。

在 Linux / macOS 上，创建或更新当前用户的 `~/.terraformrc`：

```hcl
provider_installation {
  network_mirror {
    url = "https://mirrors.tencent.com/terraform/"
    include = ["registry.terraform.io/tencentcloudstack/*"]
  }

  direct {
    exclude = ["registry.terraform.io/tencentcloudstack/*"]
  }
}
```

### 部署前准备（云服务开通与授权）

建议在 `create.sh` **首次 apply 前**完成，避免中途失败留下半创建资源。

#### 1. 账号基础

- [注册腾讯云账号](https://cloud.tencent.com/register) 并完成**实名认证**。
- 子账号部署时，需授予创建 VPC / CVM / TKE / CDB / Redis / CLB 等资源的权限；可参考[使用 TKE 预设策略授权](https://cloud.tencent.com/document/product/457/46033) 关联 `QcloudTKEFullAccess` 等策略。

#### 2. TKE 容器服务角色授权（必须）

首次使用 TKE 前须完成**服务角色**授权，否则创建集群、CLB 型 Service 等会失败。

1. 登录 [容器服务 TKE 控制台](https://console.cloud.tencent.com/tke2)，按提示完成服务授权。
2. 确认 `TKE_QCSRole` / `IPAMDofTKE_QCSRole` 等角色已授权。

官方文档：[服务授权相关角色权限说明](https://cloud.tencent.com/document/product/457/43416)、[容器服务快速入门](https://cloud.tencent.com/document/product/457/6759)。

#### 3. 私有域解析 Private DNS（按需）

| 场景 | 是否需要 |
|------|----------|
| 默认 `USE_TCR=false` | 通常不需要 |
| `USE_TCR=true` | 建议开通 |
| E2B SDK 访问 `*.cube.app` | **需要**（见 [E2B 与 cube.app 域名](#e2b-与-cubeapp-域名)） |

未开通时 API 可能返回 `ResourceNotFound.ServiceNotSubscribed`（私有域解析服务未开通）。

开通：[DNSPod 内网解析](https://console.dnspod.cn/privateDNS) → 勾选协议 → 立即开通。文档：[Private DNS 产品介绍](https://cloud.tencent.com/document/product/1338/50527)、[开通 Private DNS](https://cloud.tencent.com/document/product/1338/50533)。

#### 4. 文件存储 CFS（按需）

仅当 `TENCENTCLOUD_USE_CFS=true` 且 cubemaster 多副本时需要。

1. 登录 [文件存储 CFS 控制台](https://console.cloud.tencent.com/cfs)，按提示**开通 CFS 服务**。
2. Terraform 会在 apply 阶段自动创建文件系统（无需手动创建）。

文档：[CFS 快速入门](https://cloud.tencent.com/document/product/582/9132)。若账号未开通，控制台可能看不到实例列表；销毁时优先运行 `destroy.sh` 的 CFS 阶段。

#### 5. 其他服务 Preflight

| 服务 | 控制台 |
|------|--------|
| VPC / NAT | [VPC](https://console.cloud.tencent.com/vpc) |
| CVM | [CVM](https://console.cloud.tencent.com/cvm) |
| TKE | [TKE](https://console.cloud.tencent.com/tke2) |
| MySQL / Redis | [MySQL](https://console.cloud.tencent.com/cdb) / [Redis](https://console.cloud.tencent.com/redis) |
| CLB | [CLB](https://console.cloud.tencent.com/clb) |
| TCR（`USE_TCR=true`） | [TCR](https://console.cloud.tencent.com/tcr) |

## 快速开始

部署器位于解压后发布包的**顶层目录**，解压后即可直接运行：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>

# 复制环境变量模板，填入凭证（TENCENTCLOUD_SECRET_ID / TENCENTCLOUD_SECRET_KEY）并按需编辑其余项
cp terraform/tencentcloud/env.example terraform/tencentcloud/.env
# $EDITOR terraform/tencentcloud/.env

./terraform/tencentcloud/create.sh
```

`create.sh` 会自动加载同目录下的 `.env`（仅填充未在当前 shell 中显式设置的变量），因此凭证既可直接写入 `.env`，也可改用 `export` 注入——后者优先级更高，会覆盖 `.env` 中的同名值：

```bash
export TENCENTCLOUD_SECRET_ID="your-secret-id"
export TENCENTCLOUD_SECRET_KEY="your-secret-key"
```

`.env` 中各项配置的含义与默认值详见下文[配置](#配置)；未填写的项全部使用默认值。

`create.sh` 完全在解压后的发布包内运行，会自动完成：

1. 自动探测本地发布包（外层 `cube-sandbox-one-click-<version>.tar.gz`，若已删除则重新打包解压目录），作为组件镜像和计算节点安装的**离线源**。若设置了 `TENCENTCLOUD_LOCAL_BUNDLE=/path/to.tar.gz` 或探测到本地包，则无需公网下载；否则跳板机回退到**在线安装**（需公网）。
2. 如不存在则在 `terraform/tencentcloud/.ssh/` 下生成 SSH 密钥对。
3. 在跳板机上用内置 `mkcert` 生成 cube-proxy 的 TLS 证书（`cube.app` / `*.cube.app`），下载到 `terraform/tencentcloud/cubeproxy-certs/` 供 Secret 挂载。
4. **默认模式**（`USE_TCR=false`）：拉取公网预置镜像，部署 TKE 插件与 CVM 计算节点（默认 2 台）。
5. **TCR 模式**（`USE_TCR=true`）：创建 TCR，在跳板机构建并推送五个组件镜像，再部署 TKE 与计算节点。

## 配置

常用环境变量（与 `create.sh` / `variables.tf` 默认值一致）列在 `terraform/tencentcloud/env.example` 中，可复制为 `.env` 后填写：

```bash
export TENCENTCLOUD_REGION=ap-guangzhou
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_NODE_COUNT=2              # CVM PVM 计算节点数
export TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE=200        # 每个计算节点的 CBS 数据盘大小（GB）
export TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY=1s
export TENCENTCLOUD_TKE_NODE_COUNT=2                 # TKE worker 节点数（运行控制面 Pod）
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_USE_TCR=false                    # 默认：公网预置镜像
export TENCENTCLOUD_USE_CFS=false                    # 默认：无 CFS
export TENCENTCLOUD_CUBE_IMAGE_TAG=v0.5.1-rc5
export TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS=1
```

### 常用变量

> 交互式运行 `create.sh` 时，"Deployment configuration" 阶段会逐项引导确认下列大部分配置（地域 / VPC / 机型 / 密码 / 镜像 tag 等），其中也包含**是否开启公网模式**的 `[y/N]` 询问（默认 N = 内网）。下表的变量可在运行前显式设置以跳过对应交互；无 TTY 的非交互运行则一律采用默认值。

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` | 无 | **必填。** 腾讯云 API 凭证 |
| `TENCENTCLOUD_REGION` | `ap-guangzhou` | 地域 |
| `TENCENTCLOUD_AVAILABILITY_ZONE` | `ap-guangzhou-6` | 主可用区（子网 / MySQL / Redis / TKE 控制面） |
| `TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE` | `SA9.MEDIUM4` | 跳板机机型 |
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | `SA9.MEDIUM8` | 计算节点首选机型 |
| `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | `SA9.LARGE8` | TKE worker 节点机型（4C8G） |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | `2` | **PVM 计算节点**数（运行 Cubelet / sandbox） |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | `200` | 每个计算节点的 CBS 数据盘大小（GB，XFS，挂载于 `/data/cubelet`）。计算节点的沙箱镜像模板、快照及运行时数据均存放于此目录，请按实际需求调整 |
| `TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY` | `1s` | Cubelet 向 CubeMaster 上报节点状态/资源的间隔。`create.sh` 会在每台计算节点上写入 `Cubelet/config/config.toml` |
| `TENCENTCLOUD_TKE_NODE_COUNT` | `2` | **TKE worker** 数（运行控制面 Pod；对应 `worker_config.count`） |
| `TENCENTCLOUD_USE_TCR` | `false` | `true` 时创建 TCR 并在跳板机构建/推送镜像；`false` 时使用公网预置镜像 |
| `TENCENTCLOUD_USE_CFS` | `false` | `true` 且 cubemaster 多副本时创建 CFS NFS 共享存储 |
| `TENCENTCLOUD_TKE_CLUSTER_VERSION` | `1.34.1` | TKE Kubernetes 版本 |
| `TENCENTCLOUD_MYSQL_PASSWORD` | 不安全的演示值 | MySQL root 密码（生产必改） |
| `TENCENTCLOUD_REDIS_PASSWORD` | 不安全的演示值 | Redis 密码（生产必改） |
| `TENCENTCLOUD_CUBE_DB` / `TENCENTCLOUD_CUBE_USER` / `TENCENTCLOUD_CUBE_PASSWORD` | `cube_mvp` / `cube` / 演示值 | 应用库名 / 账号 / 密码 |
| `TENCENTCLOUD_CUBEMASTER_REPLICAS` | `1` | cube-master 副本数 |
| `TENCENTCLOUD_CUBE_API_REPLICAS` | `1` | cube-api 副本数 |
| `TENCENTCLOUD_CUBE_PROXY_REPLICAS` | `1` | cube-proxy 副本数。支持设置为大于 `1`；每个副本都会注册到 Redis，供 cube-lifecycle-manager 发现 |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS` | `1` | cube-lifecycle-manager 副本数。除非已验证 CLM 高可用行为，否则建议保持 `1` |
| `TENCENTCLOUD_CUBE_WEBUI_REPLICAS` | `1` | cube-webui 副本数 |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DEFAULT_IDLE_TIMEOUT` | `5m` | lifecycle metadata 未指定 `TimeoutSeconds` 时使用的默认空闲超时 |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_HEARTBEAT_TTL` | `15s` | cube-proxy Redis 注册心跳的过期时间 |
| `TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DISCOVERY_REFRESH` | `3s` | cube-lifecycle-manager 扫描 Redis 注册表的间隔 |
| `TENCENTCLOUD_CUBE_PROXY_HEARTBEAT_INTERVAL_MS` | `5000` | cube-proxy 注册心跳间隔（毫秒） |
| `TENCENTCLOUD_CUBE_ADMIN_TOKEN` | 空 | cube-lifecycle-manager 调用 cube-proxy `/admin/*` 接口时使用的共享 token。留空则自动生成；自定义值至少 16 个字符 |
| `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK` | `false` | cube-api / cube-proxy / cube-webui 的网络暴露模式。**默认 `false`**：关联内网 CLB，仅 VPC 内网（经跳板机 / VPN）可访问；设为 `true` 则关联公网 CLB，对公网开放，安全组同步放行 `0.0.0.0/0`。cube-master 始终为内网 CLB，不受此开关影响。开启公网前请阅读[公网服务加固建议](#公网服务加固建议) |

### 非交互 / CI 运行

无 TTY 时交互菜单会回退到默认值，建议显式设置以保持可控。密码变量是例外：非交互运行**拒绝**使用内置公开演示密码启动，必须显式设置——或设 `TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1` 主动接受不安全默认值（仅限用完即弃的沙箱）。

```bash
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_LOCAL_BUNDLE=/path/to/cube-sandbox-one-click-<version>.tar.gz  # 在解压包内运行时自动探测
export TENCENTCLOUD_PVM_KERNEL_VMLINUX=/path/to/vmlinux-pvm  # 仅当发布包不含 vmlinux-pvm 时需要
export TENCENTCLOUD_MYSQL_PASSWORD=...    # 非交互运行必填（无不安全回退）
export TENCENTCLOUD_REDIS_PASSWORD=...    # 非交互运行必填
export TENCENTCLOUD_CUBE_PASSWORD=...     # 非交互运行必填
export TENCENTCLOUD_BUILD_IMAGES=0        # TCR 模式下复用已推送的镜像
```

更多高级开关（`TENCENTCLOUD_VERBOSE`、`TENCENTCLOUD_REINSTALL`、`TENCENTCLOUD_RESET_DB`、SSH 端口/密钥路径等）见 `create.sh` 头部注释。

## 节点规格与容量规划

### 默认配置

默认部署配置了 **2 台 4C8G 计算节点**（`SA9.MEDIUM8`，各 200GB 数据盘）与 **2 个 TKE worker**，适合 POC / 功能验证，承载沙箱数量有限。各角色默认规格如下：

| 角色 | 默认机型 | 规格 | 数量 |
|------|---------|------|------|
| 计算节点（PVM） | `SA9.MEDIUM8` | 4C8G + 200GB 数据盘 | 2 台 |
| TKE Worker | `SA9.LARGE8` | 4C8G | 2 台（`worker_config.count`） |
| 跳板机 | `SA9.MEDIUM4` | 2C4G | 1 台 |
| 控制面 Pod | — | cubemaster / cube-api / cube-proxy / cube-lifecycle-manager / cube-webui 各 1 副本 | 运行在 TKE worker 上 |

::: warning
默认配置仅适合功能验证和小规模评估。大规模生产环境使用时，**必须调整计算节点和 TKE 节点的规格与数量**，否则会遇到 CPU/内存不足、Pod 调度失败、沙箱创建超时等问题。
:::

### 调整计算节点

计算节点是实际运行沙箱容器的机器，可通过以下环境变量调整其规格、数量和磁盘大小：

| 环境变量 | 说明 |
|---------|------|
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | 计算节点机型 |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | 计算节点数量 |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | 数据盘大小（GB） |

**示例：提升单台规格并增加数量**（`.env` 配置）：

```bash
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='SA9.4XLARGE32'
TENCENTCLOUD_COMPUTE_NODE_COUNT='4'
TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE='500'
```

**示例：使用更高规格**（`.env` 配置）：

```bash
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='SA9.16XLARGE128'
TENCENTCLOUD_COMPUTE_NODE_COUNT='8'
TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE='1000'
```

::: tip 裸金属服务器
如需更强的计算隔离性和性能（避免虚拟化开销），可选配[腾讯云裸金属服务器](https://cloud.tencent.com/product/cbm)作为计算节点。将 `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` 设为裸金属机型（如 `BMS5.12XLARGE192`）即可，其余部署流程不变。
:::

**示例：异构混部**（直接使用 Terraform 变量，逐台指定不同机型）：

```bash
export TF_VAR_compute_node_count=5
export TF_VAR_compute_instance_types='["SA9.8XLARGE64","SA9.8XLARGE64","SA9.4XLARGE32","SA9.4XLARGE32","SA9.4XLARGE32"]'
export TF_VAR_compute_availability_zones='["ap-guangzhou-6","ap-guangzhou-7","ap-guangzhou-6","ap-guangzhou-7","ap-guangzhou-3"]'
export TF_VAR_compute_data_disk_size=1000
```

### 调整 TKE Worker 节点

TKE Worker 运行 cube-master、cube-api、cube-proxy、cube-lifecycle-manager、cube-webui 等控制面 Pod。大规模场景下控制面的请求量和调度压力会增大，需要更多资源。

| 环境变量 | 说明 |
|---------|------|
| `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | TKE Worker 节点机型 |
| `TENCENTCLOUD_TKE_NODE_COUNT` | TKE Worker 节点数量 |

**示例：升级 TKE Worker 规格**：

```bash
TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE='SA9.2XLARGE16'
TENCENTCLOUD_TKE_NODE_COUNT='4'
```

同时可增加控制面副本数以提高吞吐和可用性（多副本 cubemaster 须配合 `TENCENTCLOUD_USE_CFS=true`）：

```bash
TENCENTCLOUD_USE_CFS=true
TENCENTCLOUD_CUBEMASTER_REPLICAS='3'
TENCENTCLOUD_CUBE_API_REPLICAS='3'
TENCENTCLOUD_CUBE_WEBUI_REPLICAS='2'
```

### 综合示例：生产环境配置

```bash
# --- 计算节点
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='SA9.8XLARGE64'
TENCENTCLOUD_COMPUTE_NODE_COUNT='6'
TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE='1000'

# --- TKE 控制面
TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE='SA9.2XLARGE16'
TENCENTCLOUD_TKE_NODE_COUNT='4'

# --- 控制面副本数（需 CFS）
TENCENTCLOUD_USE_CFS=true
TENCENTCLOUD_CUBEMASTER_REPLICAS='3'
TENCENTCLOUD_CUBE_API_REPLICAS='3'
TENCENTCLOUD_CUBE_WEBUI_REPLICAS='2'

# --- 跳板机可保持默认（仅构建用，不承载运行时负载）
TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE='SA9.MEDIUM4'
```


## 计费模式

::: warning 当前统一为按量计费（POSTPAID）
本部署器创建的所有云资源都**硬编码为按量计费**，目前**不能**通过环境变量或 Terraform 变量切换为包年包月（PREPAID）。
:::

各资源的计费类型固定如下：

| 资源 | 计费字段 | 取值 | 含义 |
|------|---------|------|------|
| 跳板机 CVM | `instance_charge_type` | `POSTPAID_BY_HOUR` | 按小时按量 |
| 计算节点 CVM | `instance_charge_type` | `POSTPAID_BY_HOUR` | 按小时按量 |
| 云数据库 MySQL | `charge_type` | `POSTPAID` | 按量计费 |
| 云数据库 Redis | `charge_type` | `POSTPAID` | 按量计费 |
| NAT 网关 EIP | `internet_charge_type` | `TRAFFIC_POSTPAID_BY_HOUR` | 按流量按量 |
| CLB（cube-proxy） | 注解 | `TRAFFIC_POSTPAID_BY_HOUR` | 按流量按量（仅公网模式下生效；内网模式无此注解） |

之所以默认使用按量计费，是因为本部署面向**快速评估**场景：用完即可通过 `destroy.sh`（即 `terraform destroy`）完全释放，避免持续计费。

### 如何改为包年包月

如果确定长期使用，可手动修改 `terraform/tencentcloud/main.tf` 中对应资源的计费字段，以获得更优价格。示例：

```hcl
# CVM 改为包年包月
resource "tencentcloud_instance" "compute" {
  instance_charge_type                    = "PREPAID"
  instance_charge_type_prepaid_period     = 1                       # 1 个月
  instance_charge_type_prepaid_renew_flag = "NOTIFY_AND_AUTO_RENEW" # 到期自动续费
  # ...
}

# MySQL 改为包年包月
resource "tencentcloud_mysql_instance" "mysql" {
  charge_type     = "PREPAID"
  prepaid_period  = 1   # 1 个月
  auto_renew_flag = 1   # 自动续费
  # ...
}

# Redis 改为包年包月
resource "tencentcloud_redis_instance" "redis" {
  charge_type     = "PREPAID"
  prepaid_period  = 1
  auto_renew_flag = 1
  # ...
}
```

::: danger 包年包月注意事项
- 包年包月资源**不会**被 `destroy.sh`（`terraform destroy`）自动退费/释放，需要手动到期释放或在控制台退费。
- 切换后执行 `destroy.sh` 可能报错或留下残留资源，需在[腾讯云控制台](https://console.cloud.tencent.com/)手动处理。
- 建议仅在确认长期使用时才切换；评估 / 验证阶段请保持默认的按量计费。
:::

## 成本估算

刊例价会随地域、规格档位、活动和时间变动，本文不内置具体价格。请使用腾讯云官方价格计算器，按[默认配置创建的资源明细](#默认配置创建的资源明细)中的规格自行测算：

- [腾讯云价格计算器（总入口）](https://buy.cloud.tencent.com/price)
- [云服务器 CVM 价格](https://buy.cloud.tencent.com/price/cvm) — 跳板机、计算节点、TKE worker
- [容器服务 TKE 价格说明](https://cloud.tencent.com/document/product/457/45157) — 托管集群管理费
- [云数据库 MySQL 价格](https://buy.cloud.tencent.com/price/cdb) ／ [云数据库 Redis 价格](https://buy.cloud.tencent.com/price/redis)
- [NAT 网关](https://cloud.tencent.com/document/product/552/31978) ／ [负载均衡 CLB](https://cloud.tencent.com/document/product/214/8848) ／ [弹性公网 IP](https://cloud.tencent.com/document/product/1199/41648) 价格
- [文件存储 CFS 价格](https://buy.cloud.tencent.com/price/cfs) ／ [容器镜像服务 TCR 价格](https://cloud.tencent.com/document/product/1141/41109)

::: tip 控制成本
所有资源默认按量计费（按秒计费、按小时结算），评估完成后立即 `destroy.sh` 释放即可避免空转计费；长期使用可参照[计费模式](#计费模式)改为包年包月。
:::

## 部署流程（分阶段、快速失败）

资源按以下顺序创建，任一阶段失败即停止：

> 网络（VPC / 子网 / NAT）→ **（`USE_TCR=true` 时）** TCR → CVM（跳板机 + 计算节点）→ **（TCR 模式）** 跳板机上构建并推送镜像 → MySQL / Redis → **（`USE_CFS=true` 时）** CFS 共享存储 → TKE 集群 + Kubernetes 插件 → 健康检查 → 计算节点安装。

- Kubernetes provider 只在 TKE API Server 就绪后才接入。
- 拆除时，**若创建了 CFS**，共享存储会先于其子网销毁（其 NFS 挂载点是该子网内的一个 ENI）。
- Terraform 状态保存在本地 `terraform/tencentcloud/`（`*.tfstate`，已被 gitignore，无远端后端）。请妥善保留该目录与生成的 `.env`，以便后续 `destroy.sh` 或重跑能找到并管理同一批资源。
- 解析后的选择会保存到 `terraform/tencentcloud/.env` 并在下次运行时自动加载；显式设置的环境变量始终优先。

## 部分失败后的重试

若某阶段中途失败（如所选地域/可用区机型售罄、账号配额限制或瞬时 API 错误），**无需**全部销毁重来：

- 修复原因——多数情况下是**调整配置**：换一个 `TENCENTCLOUD_AVAILABILITY_ZONE` / `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` / `TENCENTCLOUD_REGION`、提升配额、设置密码等，然后直接**重跑 `./terraform/tencentcloud/create.sh`**。
- 重跑时 `create.sh` 会重新加载 `.env` 中的选择，将状态与云上已存在的资源对账（刷新并导入有状态资源，而非重建），并**从中断处继续**。已有计算节点会保留（绝不缩容）。
- 可用性确实因地域**和**可用区而异——某可用区提供的机型在另一可用区可能不可用。交互式的可用区 / 机型菜单按你的地域实时查询，最终选择在 apply 时校验。
- 仅在确实要拆除部署时才运行 `destroy.sh`；普通重试之间无需运行它。

## E2B 与 cube.app 域名

单机 one-click 安装会通过 CoreDNS + split DNS 在宿主机上解析 `*.cube.app`。**Terraform 集群版默认不包含这套 DNS 配置**；若要通过 E2B 官方 SDK 创建/访问 sandbox，需要额外配置域名解析（通常需开通 [Private DNS](#3-私有域解析-private-dns按需)）。

### 背景

- cube-proxy 为 `cube.app` / `*.cube.app` 终结 TLS（证书由 `create.sh` 在跳板机用 mkcert 生成，或通过 BYO 证书替换）。
- E2B SDK 访问 sandbox 时使用形如 `https://<sandbox-id>.cube.app` 的 URL。
- 默认 `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK=false` 时，cube-proxy CLB 是 **VPC 内网 VIP**，只有 VPC 内（或经跳板机 / VPN）才能访问。

### 获取 CLB 地址

部署完成后，在 `terraform/tencentcloud/` 目录执行：

```bash
cd terraform/tencentcloud
terraform output tke_cube_proxy_clb_ip    # cube-proxy
terraform output tke_cube_api_clb_ip      # cube-api（E2B_API_URL）
terraform output tke_cube_webui_clb_ip    # WebUI
terraform output tke_cubemaster_clb_ip    # cube-master（VPC 内网）
terraform output jumpserver_ssh_command   # 跳板机 SSH 命令
```

`create.sh` 完成时也会打印上述入口。

### 方案一：Private DNS（推荐，VPC 内访问）

适用于客户端在 VPC 内、或通过跳板机 / VPN 接入 VPC 的场景。

1. 登录 [DNSPod → 内网解析](https://console.dnspod.cn/privateDNS)，创建私有域 `cube.app`。
2. 添加 **A 记录**：主机记录 `*`，记录值为 `terraform output tke_cube_proxy_clb_ip` 输出的内网 VIP；可选再添加主机记录 `@` 指向同一 VIP。
3. 将该私有域**关联到 CubeSandbox 部署所在的 VPC**（可在 [VPC 控制台](https://console.cloud.tencent.com/vpc) 确认 VPC ID）。
4. 在 VPC 内的跳板机或计算节点上验证：

```bash
dig +short test.cube.app
curl -k https://test.cube.app/   # 自签名证书需 -k 或信任 mkcert CA
```

### 方案二：/etc/hosts（仅测试）

在运行 E2B SDK 的机器上（需能路由到 cube-proxy CLB 内网 VIP）手动绑定若干 sandbox 域名。泛域名无法完整覆盖所有 sandbox ID，**不适合多 sandbox 并发测试**。

### 方案三：公网 CLB + 公网 DNS

1. 设置 `TENCENTCLOUD_ENABLE_PUBLIC_NETWORK=true` 重新部署（会重建 CLB，VIP 变化）。
2. 在公网 DNS 中将 `cube.app` / `*.cube.app` 解析到公网 CLB VIP。
3. 替换 `terraform/tencentcloud/cubeproxy-certs/` 中的证书为公网 CA 签发的正式证书（见[高级：自带 cube-proxy TLS 证书](#高级自带-cube-proxy-tls-证书)）。
4. 启用前请阅读[公网服务加固建议](#公网服务加固建议)。

### E2B SDK 环境变量

```bash
export E2B_API_URL=http://<cube-api-clb-vip>:3000
export E2B_API_KEY=e2b_000000
```

**注意：** E2B SDK 访问 sandbox 本身走 `*.cube.app` → cube-proxy，因此 **cube.app 域名解析与 TLS 证书** 是必须项，仅配置 `E2B_API_URL` 不够。更多接入方式见[连接已有集群](./connect-existing-cluster.md)。

| 能力 | 单机 one-click | Terraform 集群版 |
|------|----------------|------------------|
| `*.cube.app` 解析 | 自动（CoreDNS + split DNS） | **需手动配置** PrivateDNS / hosts / 公网 DNS |
| TLS 证书 | 宿主机 mkcert | 跳板机 mkcert → K8s Secret 挂载 |
| cube-api 地址 | `http://<host>:3000` | cube-api CLB 内网 / 公网 VIP |

## 验证

部署完成后，`create.sh` 会执行健康检查并打印访问入口（cube-api、cube-webui 等的 CLB 地址）。你也可以通过跳板机进入 VPC 内网进行排查：

```bash
# create.sh 会输出形如下面的命令
ssh -i terraform/tencentcloud/.ssh/id_rsa -p 443 -o StrictHostKeyChecking=no root@<jumpserver_public_ip>
```

## 拆除部署

```bash
./terraform/tencentcloud/destroy.sh
```

`destroy.sh` 同样需要 `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`，并复用 `create.sh` 保存到 `terraform/tencentcloud/.env` 的选择。它无需额外确认——运行 `destroy.sh` 本身即表示确认拆除。

::: danger 避免意外计费
如果 `destroy.sh` 无法删除全部资源（例如 MySQL/Redis 卡在回收站/隔离状态，或 Terraform 已无法感知的残留），请登录腾讯云控制台手动删除剩余资源，以免被持续计费：

- [VPC / 网络](https://console.cloud.tencent.com/vpc)
- [MySQL 回收站](https://console.cloud.tencent.com/cdb/recycle)
- [Redis 回收站](https://console.cloud.tencent.com/redis/recycle)
- [CFS 文件系统](https://console.cloud.tencent.com/cfs)（若曾启用 `USE_CFS=true`）

当某个拆除步骤失败或回收站清理未确认时，`destroy.sh` 也会打印这些链接。
:::

## 高级：自带 cube-proxy TLS 证书

`cube-proxy` 为 `cube.app` / `*.cube.app` 终止 TLS，其内置 nginx 配置硬编码了证书路径 `…/certs/cube.app+3.pem` 与 `…/certs/cube.app+3-key.pem`：

- **默认**：`create.sh` 在跳板机上用内置 `mkcert` 生成**自签名**证书对（SAN：`cube.app`、`*.cube.app`、`localhost`、`127.0.0.1`），下载到 `terraform/tencentcloud/cubeproxy-certs/`，Terraform 将该目录下所有文件打包进 `cubeproxy-certs` Secret（因含 TLS 私钥，用 Secret 而非 ConfigMap），以只读方式挂载到 cube-proxy pod 的 `/usr/local/openresty/nginx/certs/`。
- **自带证书**：运行 `create.sh` 前，把你的 PEM 证书 + 私钥放入 `terraform/tencentcloud/cubeproxy-certs/`，文件名必须正好是 `cube.app+3.pem` 与 `cube.app+3-key.pem`（nginx 期望的名字），并覆盖 `cube.app` 与 `*.cube.app` 这两个 SAN。`create.sh` 会复用已有文件而不重新生成，因此 CA 签发的证书会被原样使用，不再有自签名告警。
- **轮换证书**：替换这两个文件并重跑 `create.sh`；部署阶段会刷新 `cubeproxy-certs` Secret 并重启 cube-proxy 以加载新证书。自签名默认证书会在浏览器/客户端触发"不受信任 CA"告警，任何非用完即弃的场景都应替换它。

## 故障排查

部署相关的常见问题（Docker、KVM、DNS、配额等）请参阅[故障排障 — 部署相关](./troubleshooting/deployment.md)。
