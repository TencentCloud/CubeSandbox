# 离线部署

> **适用场景：** 目标机器**没有公网**（内网机房、隔离网段、受限环境），需要离线部署 CubeSandbox。这类机器通常接入内网，并配有内网软件源。

**离线部署完全可行。** 只需提前准备以下产物，然后按正常流程安装。安装步骤与正常部署完全相同。

| 提前准备 | 怎么准备 | 详见 |
| --- | --- | --- |
| 安装包（含可选的 PVM 内核包） | 在有公网的机器上从 Releases 下载，拷贝到目标节点 | 步骤 1 |
| 系统软件包（Docker、Compose 等） | 有**内网软件源**即可跳过；无源才需手动安装 | 步骤 2 |
| 容器镜像（**核心**） | 在有公网的机器上拉取打包，`docker load` 到目标节点；或推送到**内网镜像仓库** | 步骤 3 |
| 模板源镜像（可选） | 推送到**内网镜像仓库**，创建模板时直接引用 | 步骤 5 |

备好前 3 类，即可按步骤 4 安装；模板源镜像在安装完成后再推入内网仓库（步骤 5）。

::: tip 适用范围
本指南覆盖**一键部署**（单机、裸金属、多机集群）。以下两种部署方式不适用：

- **腾讯云 Terraform 部署器**：需要访问腾讯云 API 创建云资源。
- **Kubernetes（Helm）部署**：集群必须能访问存放组件镜像的仓库。若已有内网仓库（如 Harbor），按 [Kubernetes 部署指南](./kubernetes/)操作，把 chart 指向该仓库即可。
:::

::: details 为什么离线可行（原理）
一键安装脚本原生支持离线：

- 预载的容器镜像直接生效，无需改动脚本。
- 部署包自带全部版本锁定产物（组件二进制、`vmlinux`、guest 镜像、`mkcert`）。
- 真正需要**公网**的只有三件事：下载安装包、安装缺失的软件包、拉取容器镜像。它们对应步骤 1–3：内网源能覆盖的直接走内网，其余提前准备好。
:::

## 开始之前：确认宿主机条件

宿主机要求与正常部署完全相同（详见[快速开始 — 前置条件](./quickstart.md#前置条件)）：

- x86_64 或 aarch64 Linux，glibc ≥ 2.31，root 权限
- 内存至少 8 GB（安装器硬性检查，不足会直接终止安装）
- `/data/cubelet` 使用 XFS，至少 50 GB
- KVM 可用：裸金属原生支持；x86_64 云服务器可先安装 PVM 内核（见步骤 1，不支持 aarch64——ARM 机器必须本身已有 KVM）

## 步骤 1：下载安装产物

**做什么：** 在有公网的机器上下载下列产物，拷贝到目标节点。内网若已有制品中转站或 Release 镜像，直接走内网获取即可。

1. **一键部署包**：`cube-sandbox-one-click-<version>-<arch>.tar.gz`。`<arch>` 与目标节点的架构一致（`amd64` / `arm64`），下载渠道见[下载与 Release 说明](./downloads.md)。
2. **PVM 宿主内核包**（rpm/deb）：仅当目标是无 `/dev/kvm` 的云服务器时需要。从最新的 `kernel-release-*` release 下载，按 [PVM 部署 — 第一步](./pvm-deploy.md#第一步安装-pvm-宿主机内核) 安装并重启，再继续本指南。

用 scp、U 盘等方式把文件传到目标节点。

## 步骤 2：提前安装系统软件包（无内网源时）

**已配置内网软件源？直接跳到步骤 3。**

没有内网源的机器才需要本步：在有公网的机器上下载下列软件包，拷贝到目标节点安装。

::: details 软件包清单（点击展开）

跳过本步前，确认内网源里收录了 Docker 和 Compose——不少内网源不带它们。

**每个节点必装：**

- **Docker Engine 和 Compose**。只要 `docker` 和 `docker compose version`（或 `docker-compose`）都能运行，`install.sh` 就跳过联网安装。装好后验证：

  ```bash
  docker version && docker compose version   # 或：docker-compose --version
  ```

  > ⚠️ 不支持 snap Docker（读不到 `/usr/local/services`）。请用软件包方式安装 docker-ce / docker.io。

- 基础命令：`tar`、`ss`、`ip`（`iproute2`）、`bash`、`curl`、`grep`、`sed`、`pgrep`（`procps`）、`awk`、`date`、`openssl`
- `e2fsprogs` + `util-linux`：默认存储后端 `cubecow` 依赖 `mkfs.ext4`、`mount`、`umount`、`losetup`

**按需安装：**

- `dnsmasq`：宿主机没有 `systemd-resolved`/`resolvectl` 时必装，否则 DNS 兜底逻辑会自动安装它
- `s3fs` + `jq`：S3 卷挂载依赖。安装器会尝试自动补装，失败当时不报错——务必提前装好
- `python3`、`nvme-cli`、`libaio`、`libnuma`、`libuuid`：仅当启用 CubeS3lvol（`ONE_CLICK_ENABLE_S3LVOL=1`）
- `logrotate`：最小化系统镜像若未自带，请补装

:::

## 步骤 3：预载容器镜像

**做什么：** 把步骤 3.1 清单中的镜像导入目标节点的本地 Docker，两种方式任选其一：

- **逐台搬运（下文以此为例）**：在有公网、与目标同架构的机器上 `docker pull` → `docker save` 打包 → 目标节点 `docker load`。
- **走内网镜像仓库（多节点推荐）**：把镜像推到内网仓库（如 Harbor），用覆盖变量把引用指向它。安装器直接从内网拉取，不用逐台 load。

逐台搬运方式的命令：

```bash
# 有公网的机器：拉取清单中的每个镜像
docker pull <镜像引用>

# 按节点角色分别打包（传输量更小）
docker save -o cube-images-control.tar <镜像1> <镜像2> ...

# 目标节点
docker load -i cube-images-control.tar
```

### 3.1 每个节点需要哪些镜像

**控制节点**（默认 `install.sh` 的 all-in-one 单机部署，或集群中的每个控制节点）：

| 镜像 | 默认引用 | 覆盖变量 |
| --- | --- | --- |
| MySQL | `cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0` | `CUBE_SANDBOX_MYSQL_IMAGE` |
| Redis | `cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine` | `CUBE_SANDBOX_REDIS_IMAGE` |
| MinIO（S3 后端，默认开启） | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/minio:<release>` | `CUBE_SANDBOX_MINIO_IMAGE` |
| CoreDNS | `cube-sandbox-image.tencentcloudcr.com/opensource/coredns/coredns:1.14.2` | `CUBE_PROXY_COREDNS_IMAGE` |
| WebUI（nginx） | `cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat` | `WEB_UI_IMAGE` |
| cube-proxy | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-proxy:<tag>` | `CUBE_SANDBOX_CUBE_PROXY_IMAGE` |
| cube-lifecycle-manager | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:<tag>` | `CUBE_SANDBOX_CUBE_LCM_IMAGE` |
| cube-egress | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:<tag>` | `CUBE_SANDBOX_CUBE_EGRESS_IMAGE` |

**计算节点**（`install-compute.sh`）：

| 镜像 | 默认引用 | 说明 |
| --- | --- | --- |
| cube-egress | 同上 | 必需——出口 MITM 代理跑在每个节点上 |
| JuiceFS mount | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/juicefs:ce-v1.4.1` | 仅当 `ONE_CLICK_ENABLE_JFS=1`；`MIRROR=cn` 时走 `-cn` 域名 |

cube-proxy / cube-lifecycle-manager / cube-egress / MinIO 四个镜像的 `<tag>` / `<release>` 占位符，在解压后的部署包根目录执行以下命令查询：

```bash
grep -rhE '_IMAGE.*_DEFAULT="' scripts/
```

### 3.2 镜像引用必须完全一致

- 拉了 `-cn` 引用，就在 `.env` 里设 `MIRROR=cn`；拉了 `-int` 引用则不设。
- 使用内网仓库或自定义镜像名时，通过上表的覆盖变量在 `.env` 中指定。

### 3.3 在目标节点上验证

```bash
docker images   # 清单里的每个引用都必须出现，名字和 tag 完全一致
```

## 步骤 4：安装——与正常部署相同

**做什么：** 按正常流程安装即可，唯一区别是用搬运过来的部署包代替在线安装命令。

**所有命令必须以 root 身份执行**，先切换到 root：

```bash
sudo su root
```

解压部署包后安装：

```bash
tar -xzf cube-sandbox-one-click-<version>-<arch>.tar.gz
cd cube-sandbox-one-click-<version>-<arch>
cp env.example .env
# 按需编辑 .env（节点 IP、外部 MySQL/Redis、MIRROR、镜像覆盖变量……）
./install.sh
```

按你的场景对号入座：

- **无 `/dev/kvm` 的云服务器（PVM）**：跟[快速开始](./quickstart.md)走，只把它的步骤 2（在线安装）换成上面的部署包安装，其余照旧。
- **裸金属 / 已有 KVM 的机器**：见[裸金属部署](./bare-metal-deploy.md)。
- **添加计算节点**：见[多机集群部署](./multi-node-deploy.md)。部署包原样复用，计算节点只需提前 load 好 cube-egress 镜像（步骤 3.1）。

安装完成后，按正常流程验收：

```bash
./smoke.sh
```

控制台地址是 `http://<目标主机>:12088`，E2B 兼容 API 在 3000 端口（[快速开始 — 第四步](./quickstart.md#第四步运行第一段-agent-代码)）。

::: tip 离线安装的日志长什么样？
registry 不可达时，三个组件镜像各打一行 `WARN: pull failed but local image exists; using cached copy` 后改用预载镜像，其余服务静默使用预载镜像。**都是预期行为，不是错误。** 只有出现 `pull failed … and no local copy is cached` 才说明某个镜像没 load，回步骤 3 检查。
:::

## 步骤 5（安装后，可选）：把模板源镜像推入内网仓库

**做什么：** 想在离线环境创建模板，就把模板源镜像推送到**内网镜像仓库**，创建模板时直接引用内网仓库地址。

原理：CubeMaster 构建模板时从 registry 拉取源镜像——内网仓库可达即可，不需要公网。多机集群无需额外处理：CubeMaster 通过内网分发构建产物，计算节点不拉源镜像。

操作分两步：

1. 把源镜像推入内网仓库。在有公网的机器上拉取打包，传到能访问内网仓库的机器（如控制节点）上 load、tag、push（以官方代码解释器镜像为例）：

   ```bash
   # 有公网的机器
   docker pull cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
   docker save -o sandbox-code.tar cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

   # 能访问内网仓库的机器
   docker load -i sandbox-code.tar
   docker tag cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
     <内网仓库>/cube/sandbox-code:latest
   docker push <内网仓库>/cube/sandbox-code:latest
   ```

2. 用内网仓库引用创建模板（详见[快速开始 — 第三步](./quickstart.md#第三步制作模板)）：

   ```bash
   cubemastercli tpl create-from-image \
     --image <内网仓库>/cube/sandbox-code:latest \
     --writable-layer-size 1G \
     --expose-port 49999 \
     --expose-port 49983 \
     --probe 49999
   ```

两个细节：

- 内网仓库需要认证时，加 `--registry-username` / `--registry-password`。
- 内网仓库走 HTTP（无 TLS）时，引用必须带 `http://` 前缀，例如 `http://harbor.internal:5000/ns/app:tag`。

> 也可跳过内网仓库、直接用本地镜像，完整做法见 [run-code-env-inheritance 示例](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/run-code-env-inheritance/README.md)。

## 升级、模板构建与卸载

- **升级 = 新部署包 + 新镜像**（tag 会随版本变化）。升级新版本时，重新执行步骤 1 和步骤 3，然后运行 `./install.sh --mode=upgrade`（详见 [one-click README — Target Machine Installation](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/one-click/README.md)）。
- **离线构建自定义模板镜像**：Dockerfile 构建通常要访问 `apt`/`pip`。把 `apt`/`pip` 软件源指向内网镜像，或在有公网的环境中提前构建好完整镜像，再按步骤 5 推入内网仓库（做法见[本地与远程镜像实践 — 常见问题](./tutorials/template-build-practice.md#常见问题)）。
- **卸载**流程不变：`/usr/local/services/cubetoolbox/scripts/one-click/uninstall.sh`。

## 故障排查

| 现象 | 原因 | 解决 |
| --- | --- | --- |
| `pull failed for <ref> and no local copy is cached` | load 的镜像引用与安装器解析出的不一致（tag 或仓库选错） | 把 `docker images` 的输出与部署包的默认引用对照（`env.example`，或用 3.1 的 grep 查）；检查 `MIRROR` 是否一致（步骤 3.2） |
| 找不到 `docker`，安装器执行 `apt-get/yum install docker` 并失败 | 内网软件源中没有 Docker 软件包，且未提前安装 | 把 Docker 加入内网源，或提前装好 Docker Engine + Compose（步骤 2） |
| 安装器因 snap Docker 中止 | `snap install docker` 的沙箱读不到 `/usr/local/services` | `sudo snap remove docker`，用软件包方式安装 docker-ce |
| 安装器安装 `dnsmasq` 时中止 | 宿主机没有 `systemd-resolved`，内网源中也没有 `dnsmasq` | 将 `dnsmasq` 加入内网源或提前装好；或使用带 `systemd-resolved` 的操作系统 |
| 模板构建卡在 `phase: PULLING` | `--image` 引用不在内网仓库中，或 CubeMaster 访问不了该仓库 | 确认镜像已推入内网仓库、引用正确；HTTP 仓库检查 `http://` 前缀，需要认证时加 `--registry-username` / `--registry-password` |
