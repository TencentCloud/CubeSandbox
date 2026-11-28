# 离线部署

## 1. 离线部署提前做的工作

在进入目标机执行安装前，先在**有公网的准备机**和**目标节点**上完成以下准备工作：

| 准备项 | 是否必需 | 执行位置 | 对应小节 |
| --- | --- | --- | --- |
| 确认宿主机软硬件条件 | 必需 | 目标节点 | [1.1 确认宿主机条件](#host-requirements) |
| 下载一键部署包（及可选的 PVM 内核包） | 必需 | 公网准备机 → 目标节点 | [1.2 下载离线安装产物](#download-artifacts) |
| 准备并安装宿主机系统软件包 | 无内网源时必需 | 公网准备机 → 目标节点 | [1.3 准备宿主机系统软件包](#preinstall-os-packages) |
| 准备并导入组件容器镜像（**核心**） | 必需 | 公网准备机 → 内网仓库 / 目标节点 | [1.4 准备容器镜像](#prepare-container-images) |
| 推送沙箱模板源镜像到内网仓库 | 离线建模板时必需 | 公网准备机 → 内网仓库 | [1.5 准备沙箱模板源镜像](#prepare-template-images) |

### 1.1 确认宿主机条件 {#host-requirements}

目标节点的软硬件要求与在线部署完全一致（详见[快速开始 — 前置条件](./quickstart.md#前置条件)）：

- **架构与系统**：`x86_64` 或 `aarch64` Linux，内核版本 **≥ 5.4**（`uname -r`），`glibc >= 2.31`，具备 `root` 权限。
- **内存**：至少 **8 GB**（安装脚本会做硬性检查，低于该值将直接中止）。
- **磁盘**：`/data/cubelet` 挂载点须使用 **XFS** 文件系统，可用空间至少 **50 GB**。
- **虚拟化支持（KVM / PVM）**：
  - 物理机 / 裸金属服务器原生提供 `/dev/kvm`。
  - 无 `/dev/kvm` 的 `x86_64` 云服务器需提前准备 PVM 宿主机内核包（见 [1.2](#download-artifacts) 与 [2.1](#install-pvm-kernel)）。
  - `aarch64`（ARM64）节点不支持 PVM，宿主机必须本身已启用 KVM。
- **多节点集群额外要求**：控制节点与所有计算节点之间需配置时间同步（如内网 `chrony` / NTP，避免心跳偏差超过 15 秒导致节点抖动），且内网端口互通（详见[多机集群部署](./multi-node-deploy.md)）。

### 1.2 下载离线安装产物 {#download-artifacts}

在有公网的机器上下载下列产物（若内网已有制品中转站或 Release 镜像站，可直接从内网获取）：

1. **一键部署包**：`cube-sandbox-one-click-<version>-<arch>.tar.gz`
   - `<arch>` 须与目标节点架构一致（`amd64` 或 `arm64`），下载渠道见[下载与 Release 说明](./downloads.md)。
   - 控制节点与计算节点复用**同一份**部署包。
2. **PVM 宿主机内核包（RPM / DEB，按需）**：
   - 仅当目标节点为无 `/dev/kvm` 的 `x86_64` 云服务器时需要。
   - 从最新的 `kernel-release-*` Release 下载与目标发行版匹配的内核包。

下载完成后，通过 `scp`、内网制品库或离线介质拷贝到目标节点。

### 1.3 准备宿主机系统软件包（无内网软件源时） {#preinstall-os-packages}

**若目标节点已配置内网 apt / dnf / yum 软件源，且确认源内包含标准 Docker Engine 与 Compose，可直接跳至 [1.4](#prepare-container-images)。**

::: warning 跳过前请确认 Docker 可用
许多企业内网 OS 软件源仅包含系统基础包，**不包含 Docker Engine 和 Docker Compose**。若跳过本步且内网源缺失 Docker，安装器会尝试联网安装并失败。
:::

当目标节点完全没有可用软件源（或内网源缺少下列依赖）时，请在有公网的同发行版机器上提前下载离线安装包，拷贝至目标节点完成安装：

::: details 系统软件包清单（点击展开）

**每个节点必装：**

- **Docker Engine 和 Docker Compose**：只要 `docker` 和 `docker compose version`（或 `docker-compose`）均能正常执行，`install.sh` 就会自动跳过联网安装。装好后执行以下命令验证：

  ```bash
  docker version && docker compose version   # 或：docker-compose --version
  ```

  > ⚠️ **严禁使用 Snap 版 Docker**（其沙箱限制无法读取 `/usr/local/services`，安装器会自动检测并中止）。请使用系统软件包方式安装 `docker-ce` 或 `docker.io`。

- **基础命令工具**：`tar`、`ss`、`ip`（`iproute2`）、`bash`、`curl`、`grep`、`sed`、`pgrep`（`procps`）、`awk`、`date`、`openssl`
- **存储依赖**：`e2fsprogs`、`util-linux`（默认 `cubecow` 存储后端依赖 `mkfs.ext4`、`mount`、`umount`、`losetup`）

**按需安装：**

- `dnsmasq`：宿主机没有 `systemd-resolved` / `resolvectl` 时**必装**，否则安装器的 DNS 兜底逻辑会尝试联网安装它。
- `s3fs` + `jq`：S3 Volume 挂载依赖。安装器会尝试自动补装但失败时仅告警不中断，为避免后续挂载卷报错，离线环境务必提前装好。
- `python3`、`nvme-cli`、`libaio`、`libnuma`、`libuuid`：仅当开启 CubeS3lvol（`ONE_CLICK_ENABLE_S3LVOL=1`）时需要。
- `logrotate`：若最小化系统镜像未自带，建议提前补装。

:::

### 1.4 准备容器镜像（核心） {#prepare-container-images}

离线部署最关键的准备工作是提供安装器所需的容器镜像。请根据节点规模在以下两种方式中任选其一：

- **方式 A：推送至内网镜像仓库（如 Harbor，多节点集群推荐）**
  - 在有公网的机器上拉取镜像并推送到内网镜像仓库（或先打包导入中转机后再推送）。
  - 在目标节点的 `.env` 中通过覆盖变量指定内网仓库地址（若需认证，在目标节点执行一次 `docker login`）。安装器会直接从内网拉取，无需逐台导入。
- **方式 B：纯离线打包与 `docker load`（无内网仓库或单机 / 少量节点适用）**
  - 在有公网且与目标节点**相同 CPU 架构（`amd64` / `arm64`）**的机器上执行 `docker pull` → `docker save`，拷贝 `.tar` 文件至目标节点后执行 `docker load`。

#### 所需镜像清单（按节点角色）

**1. 控制节点 / All-in-One 单机（`./install.sh`）：**

| 镜像 | 默认引用 | `.env` 覆盖变量 | 说明 |
| --- | --- | --- | --- |
| MySQL | `cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0` | `CUBE_SANDBOX_MYSQL_IMAGE` | 若使用外部 MySQL / PostgreSQL 可跳过 |
| Redis | `cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine` | `CUBE_SANDBOX_REDIS_IMAGE` | 若使用外部 Redis 可跳过 |
| MinIO | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/minio:<release>` | `CUBE_SANDBOX_MINIO_IMAGE` | 默认开启；关闭内置 MinIO 时可跳过 |
| CoreDNS | `cube-sandbox-image.tencentcloudcr.com/opensource/coredns/coredns:1.14.2` | `CUBE_PROXY_COREDNS_IMAGE` | 必需 |
| WebUI（OpenResty） | `cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat` | `WEB_UI_IMAGE` | 必需 |
| cube-proxy | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-proxy:<tag>` | `CUBE_SANDBOX_CUBE_PROXY_IMAGE` | 必需 |
| cube-lifecycle-manager | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:<tag>` | `CUBE_SANDBOX_CUBE_LCM_IMAGE` | 必需 |
| cube-egress | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:<tag>` | `CUBE_SANDBOX_CUBE_EGRESS_IMAGE` | 必需 |

**2. 计算节点（`./install-compute.sh`）：**

| 镜像 | 默认引用 | `.env` 覆盖变量 | 说明 |
| --- | --- | --- | --- |
| cube-egress | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/cube-egress:<tag>` | `CUBE_SANDBOX_CUBE_EGRESS_IMAGE` | **必需**（出口透明代理运行在每个计算节点） |
| JuiceFS mount | `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/juicefs:ce-v1.4.1` | `CUBE_JFS_IMAGE` | 仅当 `ONE_CLICK_ENABLE_JFS=1` 时需要；`MIRROR=cn` 时默认走 `-cn` 域名 |

> **查询精确版本 `<tag>` / `<release>`：** 先在准备机或目标机解压部署包，在解压后的根目录执行：
>
> ```bash
> grep -rhE '_IMAGE.*_DEFAULT="' scripts/
> ```
>
> 即可查出当前版本对应的 `cube-proxy`、`cube-lifecycle-manager`、`cube-egress`、`minio` 和 `juicefs` 精确镜像地址（包含 `-int` 与 `-cn` 两种默认引用）。

#### 方式 B（纯离线打包与导入）操作示例

```bash
# 1. 在有公网且与目标同架构的机器上拉取清单中的每个镜像
docker pull <镜像引用1>
docker pull <镜像引用2>
# ...

# 2. 按节点角色分别打包（计算节点只需打包 cube-egress，可大幅减小传输体积）
docker save -o cube-images-control.tar <镜像1> <镜像2> ...
docker save -o cube-images-compute.tar <cube-egress镜像>

# 3. 拷贝到目标节点并导入本地 Docker
docker load -i cube-images-control.tar

# 4. 在目标节点核对镜像列表（名称和 tag 必须与安装器预期完全一致）
docker images
```

::: tip 镜像引用一致性原则
- 如果拉取并导入了 `-cn` 域名的默认镜像，部署时必须在 `.env` 中设置 `MIRROR=cn`；如果拉取了 `-int` 域名镜像，则保持 `MIRROR` 不设置。
- 如果将镜像推送到了内网仓库或使用了自定义镜像名，必须在 `.env` 中通过上表对应的覆盖变量显式指定。
:::

### 1.5 准备沙箱模板源镜像（可选，离线建模板必需） {#prepare-template-images}

如果需要在离线环境中构建沙箱模板（例如官方代码解释器镜像 `sandbox-code:latest`），建议提前把模板源镜像推送至**内网镜像仓库**：

- **工作原理**：`CubeMaster` / `CubeTemplateCenter` 构建模板时会从指定的容器仓库拉取源镜像，构建完成后通过内网将模板产物分发至各计算节点；计算节点不需要单独拉取源镜像。
- **自定义业务镜像**：若需在离线环境下基于 Dockerfile 制作自定义模板镜像，由于 `docker build` 通常需要访问 `apt` / `pip` 等软件源，请将基础源指向内网镜像站，或在有公网的环境中提前构建好完整镜像后再导入内网仓库（做法见[本地与远程镜像实践 — 常见问题](./tutorials/template-build-practice.md#常见问题)）。

导入内网仓库的命令示例（以官方 `sandbox-code:latest` 为例）：

```bash
# 1. 在有公网的机器上拉取并打包
docker pull cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
docker save -o sandbox-code.tar cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# 2. 拷贝到能访问内网仓库的机器（如控制节点），导入并推送到内网仓库
docker load -i sandbox-code.tar
docker tag cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  <内网仓库>/cube/sandbox-code:latest
docker push <内网仓库>/cube/sandbox-code:latest
```

---

## 2. 如何部署

完成第 1 节的全部准备工作后，即可在目标离线环境中按正常流程执行安装。**以下所有安装命令均须以 `root` 身份运行**：

```bash
sudo su root
```

### 2.1（仅无 `/dev/kvm` 的 x86_64 云服务器）安装 PVM 宿主内核 {#install-pvm-kernel}

若目标机已有 `/dev/kvm`（物理机 / 裸金属 / 已启用 KVM 的 ARM64 机器），直接跳过本步进入 [2.2](#deploy-control-node)。

对于无 `/dev/kvm` 的 `x86_64` 云服务器，先离线安装 [1.2](#download-artifacts) 拷贝过来的 PVM 宿主内核包（RPM / DEB）并重启机器，确认新内核生效后再继续部署（详细命令见 [PVM 部署 — 第一步](./pvm-deploy.md#第一步安装-pvm-宿主机内核)）。

### 2.2 部署控制节点（或 All-in-One 单机） {#deploy-control-node}

1. **解压离线部署包并初始化 `.env`：**

```bash
tar -xzf cube-sandbox-one-click-<version>-<arch>.tar.gz
cd cube-sandbox-one-click-<version>-<arch>
cp env.example .env
```

2. **按离线环境编辑 `.env`：**

按需配置节点 IP、外部数据库/Redis、PVM 开关以及镜像引用。

- **使用 `docker load` 预载官方默认镜像时**：
  - 导入 `-int` 镜像：无需额外设置镜像变量。
  - 导入 `-cn` 镜像：在 `.env` 中取消注释并设置 `MIRROR=cn`。
- **使用内网镜像仓库时**：在 `.env` 中配置对应镜像覆盖变量（示例）：

```bash
# 按需指定当前节点内网 IP（留空则自动探测 eth0）
CUBE_SANDBOX_NODE_IP=10.0.0.10

# 若使用 PVM 虚拟机环境，取消注释设为 1
# CUBE_PVM_ENABLE=1

# 使用内网仓库覆盖各组件镜像（按实际仓库地址与版本 tag 替换）
CUBE_SANDBOX_MYSQL_IMAGE=harbor.internal/cube/mysql:8.0
CUBE_SANDBOX_REDIS_IMAGE=harbor.internal/cube/redis:7-alpine
CUBE_SANDBOX_MINIO_IMAGE=harbor.internal/cube/minio:<release>
CUBE_PROXY_COREDNS_IMAGE=harbor.internal/cube/coredns:1.14.2
WEB_UI_IMAGE=harbor.internal/cube/openresty:1.21.4.1-6-alpine-fat
CUBE_SANDBOX_CUBE_PROXY_IMAGE=harbor.internal/cube/cube-proxy:<tag>
CUBE_SANDBOX_CUBE_LCM_IMAGE=harbor.internal/cube/cube-lifecycle-manager:<tag>
CUBE_SANDBOX_CUBE_EGRESS_IMAGE=harbor.internal/cube/cube-egress:<tag>
```

3. **执行安装：**

```bash
./install.sh
```

按机器形态对照参考文档（仅用上述本地包安装步骤替代在线安装命令，其余流程一致）：
- **无 `/dev/kvm` 的云服务器（PVM）**：参考[快速开始](./quickstart.md)。
- **裸金属 / 已有 KVM 的机器**：参考[裸金属部署](./bare-metal-deploy.md)。

::: tip 离线安装日志说明
当公网 registry 不可达时，`cube-proxy`、`cube-lifecycle-manager`、`cube-egress` 会各输出一行 `WARN: pull failed but local image exists; using cached copy` 并自动使用预载镜像，其余服务也会静默使用本地镜像。**这些均属正常预期行为，并非报错。** 仅当出现 `pull failed ... and no local copy is cached` 时，才表示对应镜像未导入或名称不匹配，请回到 [1.4](#prepare-container-images) 核对。
:::

### 2.3（可选）添加离线计算节点 {#deploy-compute-node}

如需扩展为多机集群，在每台新计算节点上复用同一份部署包（并确保已完成 `cube-egress` 镜像预载或可访问内网镜像仓库）：

```bash
tar -xzf cube-sandbox-one-click-<version>-<arch>.tar.gz
cd cube-sandbox-one-click-<version>-<arch>
cp env.example .env
```

编辑 `.env`，设置计算节点角色、控制面地址、S3 凭据与镜像配置（完整参数与从控制节点获取 `CUBE_S3_*` 的方式见[多机集群部署](./multi-node-deploy.md)）：

```bash
ONE_CLICK_DEPLOY_ROLE=compute
CUBE_SANDBOX_NODE_IP=10.0.0.11
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.10

# 与所准备镜像保持一致（二选一：使用 MIRROR=cn 或显式指定内网仓库镜像）
# MIRROR=cn
# CUBE_SANDBOX_CUBE_EGRESS_IMAGE=harbor.internal/cube/cube-egress:<tag>
```

执行计算节点安装脚本：

```bash
./install-compute.sh
```

### 2.4 安装验收与离线创建模板 {#verify-and-create-template}

1. **运行端到端健康校验：**

在控制节点部署目录下执行：

```bash
./smoke.sh
```

验收通过后即可访问服务：
- **Web 控制台**：`http://<控制节点IP>:12088`
- **E2B 兼容 API**：`http://<控制节点IP>:3000`（使用示例见[快速开始 — 第四步](./quickstart.md#第四步运行第一段-agent-代码)）

2. **使用内网仓库镜像创建首个沙箱模板（可选）：**

将 [1.5](#prepare-template-images) 推入内网仓库的源镜像传入 `cubemastercli` 创建模板（详见[快速开始 — 第三步](./quickstart.md#第三步制作模板)）：

```bash
cubemastercli tpl create-from-image \
  --image <内网仓库>/cube/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

两个常见内网仓库细节：
- **需要登录认证**：追加 `--registry-username <user> --registry-password <pass>`。
- **使用纯 HTTP（无 TLS）**：镜像引用必须显式带上 `http://` 前缀，例如 `http://harbor.internal:5000/ns/app:tag`。

> 若环境没有内网镜像仓库、希望直接基于本地镜像制作模板，可参考 [run-code-env-inheritance 示例](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/run-code-env-inheritance/README.md)。

### 2.5 离线升级与卸载 {#upgrade-and-uninstall}

- **离线升级**：升级本质上是 **新版本部署包 + 新版本容器镜像**（各组件镜像 tag 会随版本更新）。按 [1.2](#download-artifacts) 和 [1.4](#prepare-container-images) 备好新版本产物后，在新部署包目录执行：

  ```bash
  ./install.sh --mode=upgrade
  ```

  > 关于升级时的 `.env` 三方合并规则及开关项注意事项，请参阅 [one-click README — Target Machine Installation](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/one-click/README.md)。

- **卸载**：与在线部署一致，直接执行：

  ```bash
  /usr/local/services/cubetoolbox/scripts/one-click/uninstall.sh
  ```

---

## 3. 常见问题排查

| 现象 | 可能原因 | 处理方式 |
| --- | --- | --- |
| `pull failed for <ref> and no local copy is cached` | 导入的镜像名称/tag 与安装器解析出的引用不一致（如 `-int`/`-cn` 或 tag 不匹配） | 用 `docker images` 对照部署包默认引用（`env.example` 或 [1.4](#prepare-container-images) 的 `grep` 命令）；检查 `.env` 中 `MIRROR` 与覆盖变量是否一致 |
| 找不到 `docker`，安装器执行 `apt-get` / `yum install docker` 失败 | 内网软件源未收录 Docker 且未提前安装 | 将 Docker 加入内网软件源，或按 [1.3](#preinstall-os-packages) 提前离线安装 Docker Engine + Compose |
| 安装器因 Snap Docker 中止 | `snap install docker` 的沙箱无法读取 `/usr/local/services` | 执行 `sudo snap remove docker`，改用软件包方式安装 `docker-ce` / `docker.io` |
| 安装器安装 `dnsmasq` 时中止 | 宿主机缺少 `systemd-resolved`，且内网源没有 `dnsmasq` | 将 `dnsmasq` 加入内网源或提前离线安装；或换用带 `systemd-resolved` 的操作系统 |
| 模板构建长时间停留在 `phase: PULLING` | `--image` 镜像未推入内网仓库，或 `CubeMaster` / `CubeTemplateCenter` 无法访问该仓库 | 确认镜像已成功推送且地址拼写正确；HTTP 仓库需加 `http://` 前缀；需认证时补充 `--registry-username` / `--registry-password` |
