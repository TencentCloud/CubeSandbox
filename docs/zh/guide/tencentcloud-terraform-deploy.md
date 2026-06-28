# 腾讯云集群部署（Terraform）

本指南介绍如何使用发布包中自带的 Terraform 部署器，在腾讯云上一键拉起一个**集群版** Cube Sandbox：托管的 TKE 控制面运行 `cube-master` / `cube-api` / `cube-proxy` / `cube-webui`，后端使用云数据库 MySQL + Redis，并配备一个或多个 CVM PVM 计算节点。一台跳板机（SSH 端口为 `443`）作为构建主机和私有 VPC 的堡垒机。

::: tip 网络加固
集群版的网络加固由**腾讯云安全组**完成：部署器按角色创建 **4 个独立安全组**（跳板机 / 计算节点 / TKE Pod / CLB），各自按最小权限放行（如对公网仅放行必要的入口，计算节点与 TKE 节点无任何公网入站），计算节点不分配公网 IP。如需进一步收紧，可在[腾讯云安全组控制台](https://console.cloud.tencent.com/vpc/securitygroup)按需对上述各个安全组（`cubesandbox-sg-jumpserver` / `cubesandbox-sg-compute` / `cubesandbox-sg-tke-pod` / `cubesandbox-sg-clb`）分别调整入站 / 出站规则。对于 WebUI / cube-api / cube-proxy 这三个对公网开放的服务，请额外参阅[公网服务加固建议](#公网服务加固建议)。
:::

::: tip 适用场景
本部署利用云上资源**快速搭建一套高可用的 CubeSandbox 沙箱服务**：所有云资源默认按量计费（详见下文[计费模式](#计费模式)），用完即可通过 `destroy.sh` 一键释放。如果想长期使用，推荐改用**包年包月**资源以获得更优的成本节省（见[计费模式](#计费模式)）。如果只需要单机部署验证，请参阅之前的部署文档：[PVM 部署](./pvm-deploy.md)或[裸金属部署](./bare-metal-deploy.md)。
:::

## 架构概览

```
                        公网
                          │
                 ┌────────┴────────┐
                 │   跳板机 CVM    │  SSH:443，构建主机 / 堡垒机
                 │  (公网 IP)      │  docker build & push → TCR
                 └────────┬────────┘
                          │ VPC 内网
   ┌──────────────────────┼─────────────────────────────┐
   │                      │                              │
┌──┴───────────────┐  ┌───┴───────────┐         ┌────────┴────────┐
│  TKE 托管控制面  │  │  CVM 计算节点  │  …      │  云数据库        │
│  cube-master     │  │  Cubelet      │         │  MySQL + Redis  │
│  cube-api        │  │  network-agent│         └─────────────────┘
│  cube-proxy      │  └───────────────┘
│  cube-webui      │         ▲
└────────┬─────────┘         │ CFS (NFS, RWX) 共享存储
         │                   │
         └───────────────────┘  cube-master 多副本共享 /data/CubeMaster/storage
```

| 组件 | 形态 | 说明 |
|------|------|------|
| 跳板机 | CVM（公网 IP，SSH 443） | 构建镜像、推送 TCR、作为私有 VPC 的堡垒机 |
| 控制面 | TKE 托管集群 | 运行 `cube-master` / `cube-api` / `cube-proxy` / `cube-webui` |
| 计算节点 | CVM PVM | 运行 `Cubelet` / `network-agent`，承载沙箱 |
| 数据库 | 云数据库 MySQL 8.0 + Redis 7.0 | 仅 VPC 内网访问，不开公网 |
| 共享存储 | CFS（通用标准型 NFS） | `cube-master` 多副本以 ReadWriteMany 共享模板/快照/运行时状态 |
| 镜像仓库 | TCR（基础版） | 由本次部署创建，跳板机推送四个组件镜像 |
| 网络出口 | NAT 网关 + EIP | 整个 VPC 通过 NAT 访问公网 |

## 默认配置创建的资源明细

下表列出**默认配置**（地域 `ap-guangzhou`、主可用区自动探测首个可用区、计算节点数 1、TKE 节点数 2）下创建的全部云资源。所有资源均为**按量计费**（详见[计费模式](#计费模式)）。

| 资源类型 | 数量 | 规格 / 配置 |
|---------|------|------------|
| VPC | 1 | CIDR `10.0.0.0/16` |
| 子网 | 1 | `10.0.1.0/24`（主可用区；仅当某角色落在其他可用区时才额外创建 /24 子网） |
| NAT 网关 + EIP | 1 + 1 | 带宽 200 Mbps，按流量计费 |
| 路由表条目 | 1 | `0.0.0.0/0` → NAT 网关 |
| 安全组 | 4 | 按角色拆分（跳板机 / 计算节点 / TKE Pod / CLB），最小权限，见下表 |
| SSH 密钥对 | 1 | 自动生成于 `terraform/tencentcloud/.ssh/` |
| 跳板机 CVM | 1 | `SA9.MEDIUM4`（2C4G），系统盘 50GB 高性能云硬盘，公网带宽 200 Mbps，SSH 端口 443 |
| 计算节点 CVM | 1 | `SA9.2XLARGE16`（8C16G），系统盘 50GB，**额外挂载 1 块 200GB CBS 数据盘**（XFS，挂载于 `/data/cubelet`），**无公网 IP** |
| TKE 托管集群 | 1 | 托管集群 L20，Kubernetes `1.34.1`，Pod CIDR `10.200.0.0/16`，Service CIDR `192.168.0.0/20`，仅 VPC 内网 API |
| TKE worker 节点 | 1 初始 + 节点池期望 2（min 1 / max 4） | `SA9.LARGE8`（4C8G），系统盘 50GB 高性能云硬盘 |
| 云数据库 MySQL | 1 | 8.0 InnoDB 通用型，4GB 内存 / 200GB 存储，跨可用区双机（地域有 ≥2 可用区时）/ 半同步，仅内网 3306 |
| 云数据库 Redis | 1 | 7.0 标准架构（主从），1GB 内存，端口 6379，仅内网 |
| CFS 文件系统 | 1 | 通用标准型 NFS（弹性按量），ReadWriteMany，供 cube-master 多副本共享 |
| TCR 镜像仓库 | 1 | 基础版 + 命名空间 + VPC 接入 + 长期访问令牌 |
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

| 端口 / 范围 | 来源 | 用途 |
|------------|------|------|
| TCP 80 | `0.0.0.0/0` | cube-proxy + cube-webui 的 CLB（HTTP） |
| TCP 443 | `0.0.0.0/0` | cube-proxy 的 CLB（HTTPS） |
| TCP 3000 | `0.0.0.0/0` | cube-api 的 CLB（公网访问） |
| TCP 8089 | `10.0.0.0/16`（仅 VPC 内网） | cube-master 的内网 CLB |

四个安全组出站均默认放行全部（`0.0.0.0/0` ALL）。数据库、TKE API Server 等均**不开公网**，仅限 VPC 内网访问。

### 公网服务加固建议

`cubesandbox-sg-clb` 默认对 `0.0.0.0/0` 放行 WebUI（80）、cube-proxy（80 / 443）与 cube-api（3000）三个公网入口。三者的安全模型不同，建议按服务分别加固：

- **WebUI（CLB 80）**：WebUI 控制台**目前不带任何鉴权 / 权限控制**，任何能访问到它的人都能操作沙箱。强烈建议为 WebUI 的 CLB **单独创建一个安全组**，并在其中配置**严格的源 IP 白名单**（仅放行你的办公网 / 管理机出口 IP），而不是沿用对公网放行的 `cubesandbox-sg-clb`。可在[腾讯云安全组控制台](https://console.cloud.tencent.com/vpc/securitygroup)新建安全组后，绑定到 WebUI 对应的 CLB 实例。
- **cube-api（CLB 3000）**：cube-api 默认**对所有请求放行、不做凭证校验**。对外暴露前请务必启用 **Auth Callback** 鉴权，把鉴权决策委托给你自己的鉴权服务，详见[鉴权配置](./authentication.md)。
- **cube-proxy（CLB 80 / 443）**：cube-proxy 是沙箱流量的公网入口，设计上即面向公网。若希望限制对沙箱的公开访问，请参阅[限制公开访问](./restrict-public-access.md)启用每沙箱独立的入站 token 等机制。

## 前置条件

运行 `create.sh` 的机器只需满足以下条件，**无需预装 Terraform**：

- **腾讯云 API 凭证**：在[访问管理控制台](https://console.cloud.tencent.com/cam/capi)创建密钥对，导出 `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`。
- **本地工具**：`ssh`、`scp`、`nc`，以及访问腾讯云 API 的网络。`terraform` 与 `jq` 在缺失时会自动安装——可写时装到 `/usr/local/bin`（如以 root 运行），否则装到本地 `.bin/`。`terraform` 从 HashiCorp 官方下载（需 `curl`/`wget` + `unzip`），`jq` 来自系统包管理器或 GitHub 静态二进制。
- `mkcert` / `openssl` **无需**本地安装——cube-proxy 证书在跳板机上生成。

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
4. 构建并推送四个组件镜像到本次创建的 TCR，随后部署 TKE 插件与 CVM 计算节点。`create.sh` 默认至少创建 1 个计算节点，可用 `TENCENTCLOUD_COMPUTE_NODE_COUNT` 调整。

## 配置

常用环境变量（与 `create.sh` / `variables.tf` 默认值一致）列在 `terraform/tencentcloud/env.example` 中，可复制为 `.env` 后填写：

```bash
export TENCENTCLOUD_REGION=ap-guangzhou
export TENCENTCLOUD_AVAILABILITY_ZONE=               # 留空则自动探测首个可用区
export TENCENTCLOUD_COMPUTE_NODE_COUNT=2              # CVM PVM 计算节点数（默认 1）
export TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE=200        # 每个计算节点的 CBS 数据盘大小（GB，默认 200）
export TENCENTCLOUD_TKE_NODE_COUNT=2                 # TKE worker 节点数（默认 2）
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.2XLARGE16
export TENCENTCLOUD_CUBE_IMAGE_TAG=latest            # 四个组件镜像共用的 tag
```

### 常用变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` | 无 | **必填。** 腾讯云 API 凭证 |
| `TENCENTCLOUD_REGION` | `ap-guangzhou` | 地域 |
| `TENCENTCLOUD_AVAILABILITY_ZONE` | 自动探测（首个可用区） | 主可用区（子网 / MySQL / Redis / TKE 控制面），留空则自动探测地域内首个可用区 |
| `TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE` | `SA9.MEDIUM4` | 跳板机机型 |
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | `SA9.2XLARGE16` | 计算节点首选机型 |
| `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | `SA9.LARGE8` | TKE worker 节点机型（4C8G） |
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | `1` | CVM PVM 计算节点数 |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | `200` | 每个计算节点的 CBS 数据盘大小（GB，XFS，挂载于 `/data/cubelet`）。计算节点的沙箱镜像模板、快照及运行时数据均存放于此目录，请按实际需求调整 |
| `TENCENTCLOUD_TKE_NODE_COUNT` | `2` | TKE worker 节点数 |
| `TENCENTCLOUD_TKE_CLUSTER_VERSION` | `1.34.1` | TKE Kubernetes 版本 |
| `TENCENTCLOUD_MYSQL_PASSWORD` | 不安全的演示值 | MySQL root 密码（生产必改） |
| `TENCENTCLOUD_REDIS_PASSWORD` | 不安全的演示值 | Redis 密码（生产必改） |
| `TENCENTCLOUD_CUBE_DB` / `TENCENTCLOUD_CUBE_USER` / `TENCENTCLOUD_CUBE_PASSWORD` | `cube_mvp` / `cube` / 演示值 | 应用库名 / 账号 / 密码 |
| `TENCENTCLOUD_CUBEMASTER_REPLICAS` 等 | `2` | 各组件副本数（默认 2 = 高可用） |

### 非交互 / CI 运行

无 TTY 时交互菜单会回退到默认值，建议显式设置以保持可控。密码变量是例外：非交互运行**拒绝**使用内置公开演示密码启动，必须显式设置——或设 `TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1` 主动接受不安全默认值（仅限用完即弃的沙箱）。

```bash
export TENCENTCLOUD_AVAILABILITY_ZONE=               # 留空则自动探测首个可用区
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.2XLARGE16
export TENCENTCLOUD_LOCAL_BUNDLE=/path/to/cube-sandbox-one-click-<version>.tar.gz  # 在解压包内运行时自动探测
export TENCENTCLOUD_PVM_KERNEL_VMLINUX=/path/to/vmlinux-pvm  # 仅当发布包不含 vmlinux-pvm 时需要
export TENCENTCLOUD_MYSQL_PASSWORD=...    # 非交互运行必填（无不安全回退）
export TENCENTCLOUD_REDIS_PASSWORD=...    # 非交互运行必填
export TENCENTCLOUD_CUBE_PASSWORD=...     # 非交互运行必填
export TENCENTCLOUD_BUILD_IMAGES=0        # 复用已推送的镜像
```

更多高级开关（`TENCENTCLOUD_VERBOSE`、`TENCENTCLOUD_REINSTALL`、`TENCENTCLOUD_RESET_DB`、SSH 端口/密钥路径等）见 `create.sh` 头部注释。

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
| CLB（cube-proxy） | 注解 | `TRAFFIC_POSTPAID_BY_HOUR` | 按流量按量 |

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

> 网络（VPC / 子网 / NAT）→ TCR → CVM（跳板机 + 计算节点）→ 跳板机上构建并推送镜像 → MySQL / Redis → CFS 共享存储 → TKE 集群 + Kubernetes 插件 → 健康检查 → 计算节点安装。

- Kubernetes provider 只在 TKE API Server 就绪后才接入。
- 拆除时，CFS 共享存储会先于其子网销毁（其 NFS 挂载点是该子网内的一个 ENI）。
- Terraform 状态保存在本地 `terraform/tencentcloud/`（`*.tfstate`，已被 gitignore，无远端后端）。请妥善保留该目录与生成的 `.env`，以便后续 `destroy.sh` 或重跑能找到并管理同一批资源。
- 解析后的选择会保存到 `terraform/tencentcloud/.env` 并在下次运行时自动加载；显式设置的环境变量始终优先。

## 部分失败后的重试

若某阶段中途失败（如所选地域/可用区机型售罄、账号配额限制或瞬时 API 错误），**无需**全部销毁重来：

- 修复原因——多数情况下是**调整配置**：换一个 `TENCENTCLOUD_AVAILABILITY_ZONE` / `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` / `TENCENTCLOUD_REGION`、提升配额、设置密码等，然后直接**重跑 `./terraform/tencentcloud/create.sh`**。
- 重跑时 `create.sh` 会重新加载 `.env` 中的选择，将状态与云上已存在的资源对账（刷新并导入有状态资源，而非重建），并**从中断处继续**。已有计算节点会保留（绝不缩容）。
- 可用性确实因地域**和**可用区而异——某可用区提供的机型在另一可用区可能不可用。交互式的可用区 / 机型菜单按你的地域实时查询，最终选择在 apply 时校验。
- 仅在确实要拆除部署时才运行 `destroy.sh`；普通重试之间无需运行它。

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

当某个拆除步骤失败或回收站清理未确认时，`destroy.sh` 也会打印这些链接。
:::

## 高级：自带 cube-proxy TLS 证书

`cube-proxy` 为 `cube.app` / `*.cube.app` 终止 TLS，其内置 nginx 配置硬编码了证书路径 `…/certs/cube.app+3.pem` 与 `…/certs/cube.app+3-key.pem`：

- **默认**：`create.sh` 在跳板机上用内置 `mkcert` 生成**自签名**证书对（SAN：`cube.app`、`*.cube.app`、`localhost`、`127.0.0.1`），下载到 `terraform/tencentcloud/cubeproxy-certs/`，Terraform 将该目录下所有文件打包进 `cubeproxy-certs` Secret（因含 TLS 私钥，用 Secret 而非 ConfigMap），以只读方式挂载到 cube-proxy pod 的 `/usr/local/openresty/nginx/certs/`。
- **自带证书**：运行 `create.sh` 前，把你的 PEM 证书 + 私钥放入 `terraform/tencentcloud/cubeproxy-certs/`，文件名必须正好是 `cube.app+3.pem` 与 `cube.app+3-key.pem`（nginx 期望的名字），并覆盖 `cube.app` 与 `*.cube.app` 这两个 SAN。`create.sh` 会复用已有文件而不重新生成，因此 CA 签发的证书会被原样使用，不再有自签名告警。
- **轮换证书**：替换这两个文件并重跑 `create.sh`；部署阶段会刷新 `cubeproxy-certs` Secret 并重启 cube-proxy 以加载新证书。自签名默认证书会在浏览器/客户端触发"不受信任 CA"告警，任何非用完即弃的场景都应替换它。

## 故障排查

部署相关的常见问题（Docker、KVM、DNS、配额等）请参阅[故障排障 — 部署相关](./troubleshooting/deployment.md)。
