# Cube Sandbox One-Click

本目录用于构建并交付 `cube-sandbox` 的单机一键发布包。

## 目录说明

- `build-release-bundle-builder.sh`：推荐入口；先在 builder 镜像中编译 one-click 需要的组件，再在宿主机继续执行发布包打包。
- `build-vm-assets.sh`：构建 `containerd-shim-cube-rs`、`cube-runtime`、`cube-agent`，把 `cube-agent` 注入 guest image 作为 `/sbin/init`，并收集 guest kernel。
- `build-release-bundle.sh`：底层打包入口；消费源码树或 `ONE_CLICK_*_BIN` 预编译产物，组装 `sandbox-package` 并生成最终发布包。
- `config-cube.toml`：one-click 默认 runtime 配置模板。
- `support/`：MySQL/Redis 的 `docker compose` 模板，安装后落到 `/usr/local/services/cubetoolbox/support/`；`support/bin/mkcert` 为内置的 mkcert 二进制。
- `cubeproxy/`：`cube proxy` 的 compose 模板、`global.conf` 模板与 CoreDNS 模板。
- `webui/`：Dashboard 的 Nginx 运行时文件，安装后落到 `/usr/local/services/cubetoolbox/webui/`。
- `install.sh`：目标机控制节点安装与启动入口（默认 all-in-one）。
- `install-compute.sh`：目标机计算节点安装入口。
- `down.sh`：停止 one-click 安装的服务与依赖。
- `smoke.sh`：执行基础健康检查。
- `env.example`：构建机和目标机共用的环境变量模板。
- `lib/common.sh`：公共 shell 函数。
- `scripts/one-click/`：systemd 托管部署安装后使用的校验与维护辅助脚本。
- `terraform/tencentcloud/`：在腾讯云上部署**集群版** CubeSandbox 的 Terraform 部署器（TKE 控制面 + CVM 计算节点）。`create.sh` 为入口，`destroy.sh` 负责整体销毁。这些文件同时位于发布包顶层和 `sandbox-package` 内（见“腾讯云集群部署”）。

## 支持的操作系统

- 构建 / 部署执行机：推荐使用 Linux。腾讯云 Terraform 部署脚本（`terraform/tencentcloud/create.sh` 和 `destroy.sh`）也支持 macOS，包括 macOS 默认的 Bash 3.2 环境。
- Windows：不支持原生 `cmd.exe` / PowerShell 直接执行。Windows 用户请通过 WSL2（Ubuntu 或其他 Linux 发行版）运行这些 shell 脚本。
- 目标机：one-click 运行时要求 Linux，并依赖 systemd 与 Docker/containerd 能力。腾讯云 Terraform 部署器会创建 Linux CVM/TKE 资源，并通过 SSH 完成配置。

## 构建输入

必须准备的固定 kernel 制品是普通 guest kernel `vmlinux`，也可以额外打包 PVM guest kernel `vmlinux-pvm`：

- `vmlinux`
- `vmlinux-pvm`（可选）

默认放在 `assets/kernel-artifacts/`，也可以通过环境变量覆盖：

```bash
export ONE_CLICK_CUBE_KERNEL_VMLINUX=/abs/path/to/vmlinux
export ONE_CLICK_CUBE_KERNEL_PVM_VMLINUX=/abs/path/to/vmlinux-pvm
```

运行时仍然使用 `cube-kernel-scf/vmlinux`。默认情况下该文件是普通 guest kernel；如果目标机安装时设置 `CUBE_PVM_ENABLE=1`，安装脚本会把包内的 `vmlinux-pvm` 覆盖安装为 `cube-kernel-scf/vmlinux`。

guest image 不再依赖本地 zip，而是在构建 one-click 发布包时基于 `deploy/guest-image/Dockerfile` 本地生成。常用覆盖参数如下：

```bash
export ONE_CLICK_GUEST_IMAGE_DOCKERFILE=/abs/path/to/cube-sandbox/deploy/guest-image/Dockerfile
# 可选，默认取 Dockerfile 所在目录
export ONE_CLICK_GUEST_IMAGE_CONTEXT_DIR=/abs/path/to/cube-sandbox/deploy/guest-image
# 可选，默认是 cube-sandbox-guest-image:one-click
export ONE_CLICK_GUEST_IMAGE_REF=cube-sandbox-guest-image:one-click
# 可选，默认跟随当前仓库 revision
export ONE_CLICK_GUEST_IMAGE_VERSION=custom-guest-image-version
```

## 构建发布包

建议先复制环境模板：

```bash
cd deploy/one-click
cp env.example .env
```

推荐在宿主机的仓库根目录执行：

```bash
./deploy/one-click/build-release-bundle-builder.sh
```

这个入口会先：

- 通过根目录 builder 镜像在容器内编译 `cubemaster`、`cubemastercli`、`cubelet`、`cubecli`、`cube-api`、`network-agent`、`cube-agent`、`containerd-shim-cube-rs`、`cube-runtime`
- 在 builder 内对 `CubeMaster`、`Cubelet` 执行 `go mod download`，首次构建会在线拉取 Go modules，后续复用 builder HOME 下的模块缓存
- 将预编译产物落到 `deploy/one-click/.work/prebuilt/`
- 回到宿主机调用 `build-release-bundle.sh`，构建 WebUI 静态资源，继续 guest image 和最终打包

如果构建机已经具备完整工具链，或者你想手动指定 `ONE_CLICK_*_BIN`，也可以继续直接执行底层入口：

```bash
./deploy/one-click/build-release-bundle.sh
```

无论走推荐入口还是直接执行底层入口，`CubeMaster` / `Cubelet` 都不再依赖仓库内 `vendor/`，而是在构建时通过 Go modules 实时解析依赖。

WebUI 会在最终打包阶段于构建机执行构建，因此构建机需要具备 `npm`。目标机不会构建 WebUI 镜像，而是把发布包里的 `webui/dist` 挂载到标准 nginx 容器中。如果要复用已经构建好的 Dashboard，可设置：

```bash
export ONE_CLICK_WEB_DIST_DIR=/abs/path/to/web/dist
```

### Go Modules 依赖下载

- 首次构建 `CubeMaster`、`Cubelet` 时会执行 `go mod download`
- 构建机需要能访问对应的模块源；如处于内网环境，请提前配置 `GOPROXY`、`GOPRIVATE` 和私有仓库凭据
- 推荐入口会把 builder HOME 持久化到宿主机缓存目录，因此同一台机器上的后续构建通常不会重复全量下载
- `cubelog` 仍然通过仓库内本地模块 `../cubelog` 引用，不走远端下载

成功后会生成：

```bash
deploy/one-click/dist/cube-sandbox-one-click-<version>.tar.gz
```

发布包中会包含：

- `sandbox-package.tar.gz`
- `CubeAPI/bin/cube-api`
- `containerd-shim-cube-rs`、`cube-runtime`
- 本地构建得到的 `cube-image/cube-guest-image-cpu.img`
- `cubeproxy/` 目录及其 `build-context`
- `support/` 目录及其 compose 模板
- `webui/` 目录、compose 模板、nginx 配置和已构建的 `web/dist` 静态资源
- 基于 `vmlinux` 现场打包得到的 `cube-kernel-scf.zip`
- 目标机可直接执行的 `install.sh` / `install-compute.sh` / `down.sh` / `smoke.sh`

## 配置映射

one-click 不会在目标机额外创建一层全局 `configs/`，而是直接落到各组件原生配置入口：

- `configs/single-node/cubemaster.yaml` -> `CubeMaster/conf.yaml`
- `Cubelet/config/` -> `Cubelet/config/`
- `Cubelet/dynamicconf/` -> `Cubelet/dynamicconf/`
- `configs/single-node/network-agent.yaml` -> `network-agent/network-agent.yaml`
- `CubeAPI/bin/cube-api` -> `/usr/local/services/cubetoolbox/CubeAPI/bin/cube-api`
- `support/` -> `/usr/local/services/cubetoolbox/support/`
- `cubeproxy/` -> `/usr/local/services/cubetoolbox/cubeproxy/`
- `webui/` -> `/usr/local/services/cubetoolbox/webui/`

其中 `Cubelet` 直接使用仓库内现成的 `dynamicconf/conf.yaml`；`network-agent` 实际启动时优先通过 `--cubelet-config` 读取 `Cubelet/config/config.toml` 中的网络插件配置，以保证和 `Cubelet` 的网络参数保持一致；`cube-api` 则直接读取 `.one-click.env` 中的环境变量启动，默认监听 `0.0.0.0:3000` 并转发到本机 `cubemaster`。MySQL/Redis 固定部署到 `/usr/local/services/cubetoolbox/support`，以 Docker 容器运行并由专用 systemd service 管理；`cube proxy` 固定部署到 `/usr/local/services/cubetoolbox/cubeproxy`，从发布包内 build context 本地构建镜像，并由 systemd 管理。WebUI 固定部署到 `/usr/local/services/cubetoolbox/webui`，默认监听 `12088`，通过标准 nginx 容器托管发布包里的 `webui/dist`，并通过 Docker `host-gateway` 把 `/cubeapi` 反代到宿主机 CubeAPI；其生命周期同样由 systemd 托管。

## 目标机安装

把 `cube-sandbox-one-click-<version>.tar.gz` 拷到目标机后：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
sudo ./install.sh
```

one-click 固定安装到 `/usr/local/services/cubetoolbox`。

新的 one-click 安装统一只使用 systemd 托管：

- 控制节点：`cube-sandbox-control.target`
- 计算节点：`cube-sandbox-compute.target`

安装脚本会自动把单元文件安装到 `/etc/systemd/system/`，并按角色执行 `enable --now`。旧的 shell 启停脚本只作为 pre-systemd 历史版本升级时的短期过渡能力保留，不属于新安装的运行接口。

常用命令：

```bash
sudo ./smoke.sh
sudo ./down.sh
```

控制节点安装完成后，可以打开 Dashboard：

```bash
http://<target-host>:12088
```

安装前可以在 `.env` 里显式设置当前节点内网 IP；如果不设置，`install.sh` 会尝试自动探测 `eth0` 的 IPv4：

```bash
# CUBE_SANDBOX_NODE_IP=10.0.0.10
```

如果显式设置了 `CUBE_SANDBOX_NODE_IP`，安装脚本会优先使用该值；否则会把自动探测到的节点 IP 写入运行时环境，并用于 `cube proxy` / DNS 的地址渲染。

### 数字助手环境变量

数字助手（AgentHub）需要 CubeAPI 连接 MySQL 保存助手实例、存档、模板和操作流水。one-click 默认会根据 `CUBE_SANDBOX_MYSQL_HOST`、`CUBE_SANDBOX_MYSQL_PORT`、`CUBE_SANDBOX_MYSQL_USER`、`CUBE_SANDBOX_MYSQL_PASSWORD`、`CUBE_SANDBOX_MYSQL_DB` 拼出 `DATABASE_URL`，指向随 one-click 启动的 MySQL：

```bash
# 可选；未设置时由 one-click 自动拼接。
DATABASE_URL=mysql://cube:cube_pass@127.0.0.1:3306/cube_mvp
```

创建或重新配置 OpenClaw 数字助手前，请在 WebUI 的 **AgentHub 设置** 中填写 LLM API Key（以及 provider、Base URL、模型）。

### 计算节点安装

如果第一台机器已经按默认方式部署为控制+计算节点，第二台机器可复用同一个发布包作为计算节点：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
```

在 `.env` 里至少设置：

```bash
ONE_CLICK_DEPLOY_ROLE=compute
ONE_CLICK_CONTROL_PLANE_IP=10.0.0.11
```

如需显式指定计算节点 IP，或目标机默认网卡不是 `eth0`，再额外设置：

```bash
CUBE_SANDBOX_NODE_IP=10.0.0.12
```

然后执行：

```bash
sudo ./install-compute.sh
```

计算节点模式会：

- 安装 `Cubelet`、`network-agent`、`cube-shim`、`cube-image`、`cube-kernel-scf`、`cube-egress` 和运行所需脚本，并安装 `docker`
- 启动 `network-agent`、`cubelet`，并通过 `cube-sandbox-compute.target` 拉起 `cube-egress`（透明出网 MITM 代理，以 docker 容器运行，用于强制执行沙箱出网策略）
- `cube-egress` 启动前会通过主节点的 `/cube/ca/<file>` 接口拉取与模板一致的 MITM 根 CA（含私钥），保证模板信任 compute 节点上 `cube-egress` 签发的叶子证书
- 将 `Cubelet` 的 `meta_server_endpoint` 指向 `ONE_CLICK_CONTROL_PLANE_IP:8089`
- 通过主节点的 `/internal/meta` 接口自动注册节点

注意事项：

- 所有计算节点都需要让 `Cubelet` 监听和主节点配置一致的 gRPC 端口，默认是 `9999`
- `CUBE_SANDBOX_NODE_IP` 会同时作为 one-click 配置值和 `Cubelet` 节点注册 IP
- 主节点必须能访问计算节点的 `9999/tcp`，计算节点必须能访问主节点的 `8089/tcp`

MySQL/Redis 依赖默认会部署到：

```bash
/usr/local/services/cubetoolbox/support
```

安装时会在这个目录下准备运行期文件，并由 systemd 分别管理：

- `mysql:8.0`
- `redis:7-alpine`

### 使用外部 MySQL / Redis

如果希望使用已有的 MySQL/Redis 服务器，而不是内置的本地容器，可在执行
`install.sh` 之前在 `.env` 中设置以下变量（参见 `env.example`）：

```bash
# 外部 MySQL（凭据字段可按需覆盖）
CUBE_EXTERNAL_MYSQL_HOST=10.0.0.20
CUBE_EXTERNAL_MYSQL_PORT=3306
CUBE_EXTERNAL_MYSQL_USER=cube
CUBE_EXTERNAL_MYSQL_PASSWORD=cube_pass
CUBE_EXTERNAL_MYSQL_DB=cube_mvp

# 外部 Redis
CUBE_EXTERNAL_REDIS_HOST=10.0.0.21
CUBE_EXTERNAL_REDIS_PORT=6379
CUBE_EXTERNAL_REDIS_PASSWORD=ceuhvu123
```

当设置了 `CUBE_EXTERNAL_MYSQL_HOST`（和/或 `CUBE_EXTERNAL_REDIS_HOST`）时，`install.sh` 会：

- 用外部 MySQL/Redis 地址改写 `CubeMaster/conf.yaml`；
- 将 `DATABASE_URL`（CubeAPI）和 `CUBE_PROXY_REDIS_*`（cube proxy）写入 `.one-click.env`，让各服务都连接外部地址；
- mask 对应的 `cube-sandbox-mysql.service` / `cube-sandbox-redis.service`，本地容器不会再被启动；
- 让 `quickcheck.sh` 和 `up-support.sh` 跳过对已外置依赖的本地生命周期管理（`down-support.sh` 未感知外部依赖，仍会执行 `docker compose down`，但由于本地容器从未被启动，这是无害的空操作）。

外部 MySQL 需要预先授予所配置用户对目标库的访问权限；CubeMaster 首次启动会自行执行内置 schema 迁移。

`cube proxy` 和它的 DNS 解析在 one-click 里是必选能力，`.env` 中这两个值必须保持为 `1`：

```bash
CUBE_PROXY_ENABLE=1
CUBE_PROXY_DNS_ENABLE=1
```

其它常用参数如下：

```bash
CUBE_PROXY_HTTPS_PORT=443
CUBE_PROXY_HTTP_PORT=80
# 已废弃：CUBE_PROXY_HOST_PORT 会被忽略；如需调整启动后检查端口，请配置 CUBE_PROXY_HTTP_PORT。
CUBE_PROXY_CERT_DIR=/usr/local/services/cubetoolbox/cubeproxy/certs
CUBE_PROXY_DNS_ANSWER_IP="${CUBE_SANDBOX_NODE_IP}"
WEB_UI_ENABLE=1
WEB_UI_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
WEB_UI_HOST_PORT=12088
WEB_UI_UPSTREAM=http://host.docker.internal:3000
CUBE_API_BIND=0.0.0.0:3000
CUBE_API_HEALTH_ADDR=127.0.0.1:3000
CUBE_API_SANDBOX_DOMAIN=cube.app
```

安装过程中会做这些事：

- 若系统尚未安装 `mkcert`，从安装包内置的 `support/bin/mkcert` 复制到 `/usr/local/bin/mkcert`，再在宿主机 `CUBE_PROXY_CERT_DIR`（默认 `/usr/local/services/cubetoolbox/cubeproxy/certs/`）下执行 `mkcert -install` 并生成 `cube.app+3.pem`、`cube.app+3-key.pem`
- 在 `/usr/local/services/cubetoolbox/support/`、`cubeproxy/`、`coredns/`、`webui/` 下生成运行期配置与渲染文件
- 用 `CUBE_SANDBOX_NODE_IP` 渲染 `cubeproxy/global.conf`
- 安装 `/etc/systemd/system/cube-sandbox-*.service|target|timer`，并把宿主机进程与容器统一交给 systemd 管理
- MySQL、Redis、cube proxy、WebUI、CoreDNS 仍使用 Docker 运行，但生命周期改由各自的 systemd service 直接管理，而不是运行期依赖 `docker compose up -d`
- 若目标机有 `resolvectl`，则创建专用 dummy link（默认 `cube-dns0`）并分配本地地址，`CoreDNS` 默认绑定到该链路地址 `169.254.254.53`，再把 `cube.app` 域名通过该链路路由到本地 DNS；若目标机没有 `resolvectl`，则回退到 `NetworkManager + dnsmasq`：同样会创建该 dummy link，并让 `dnsmasq` 在 `169.254.254.53` 上额外监听，安装器同时把 `/etc/resolv.conf` 从 NetworkManager 手里接管（`rc-manager=unmanaged`）并改写为指向该非 loopback IP。这样宿主与 `systemd-resolved` 路径保持对称，避免 Docker 在 `/etc/resolv.conf` 只剩 loopback nameserver 时默默回退到内置公网 DNS（`8.8.8.8`）——一旦回退，宿主上所有依赖域名解析的容器（典型如 `docker build` 跑 `apk update`）都会因为公网 DNS 在内网不可达而失败。
- 启动宿主机进程 `network-agent`、`cubemaster`、`cube-api`、`cubelet`，并在 `quickcheck.sh` 中校验 systemd 状态与业务健康检查
- 在 `/usr/local/services/cubetoolbox/webui/` 下运行标准 WebUI nginx 容器。该容器只读挂载 `webui/dist` 静态资源，发布 `WEB_UI_HOST_PORT`（默认 `12088`），把 `host.docker.internal` 映射到 Docker `host-gateway`，并通过 nginx 反代校验 `/cubeapi/v1/health`

停止 one-click 时会同时停止 `/usr/local/services/cubetoolbox/support` 下的 MySQL/Redis、WebUI、`cube proxy` / `CoreDNS`、宿主机进程 `network-agent` / `cubemaster` / `cube-api` / `cubelet`，并回滚 `cube.app` 的宿主机 DNS 路由配置。

部署完成后，如需让 E2B 官方 SDK 指向 one-click 节点，可以在客户端侧设置：

```bash
export E2B_API_URL=http://<target-host>:3000
export E2B_API_KEY=e2b_000000
```

## 安装脚本启动前预检清单

`install.sh` / `install-compute.sh` 会在启动早期执行一次性 preflight 检查，确保依赖尽早失败，不会跑到中途才报错。

### compute 角色（`install-compute.sh`）

必需命令：

- `docker`（cube-egress 以 docker 容器运行，安装器会自动安装；docker 是硬性前置依赖，离线/无法自动安装的环境请提前装好 Docker）
- `tar`
- `ss`
- `bash`
- `curl`
- `grep`
- `sed`
- `pgrep`
- `date`

条件命令：

- 若启用 `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1` 且 `/etc/docker/daemon.json` 已存在，需要 `python3`
- 若打包内 `Cubelet/config/config.toml` 启用了 `storage_backend = "cubecow"`，还会额外检查：
  `mkfs.ext4`、`mount`、`umount`、`losetup`

推荐安装包（覆盖上述 `cubecow` 依赖）：

- Debian / Ubuntu：`e2fsprogs`、`util-linux`
- OpenCloudOS / RHEL / CentOS：`e2fsprogs`、`util-linux`
可直接执行的安装示例：

```bash
# Debian / Ubuntu
sudo apt-get update
sudo apt-get install -y e2fsprogs util-linux

# OpenCloudOS / RHEL / CentOS
sudo dnf install -y e2fsprogs util-linux || \
sudo yum install -y e2fsprogs util-linux
```

### control 角色（`install.sh`，默认）

必需命令：

- `docker`
- `tar`
- `ss`
- `bash`
- `curl`
- `grep`
- `sed`
- `pgrep`
- `date`
- `ip`
- `awk`

二选一命令：

- 证书准备阶段：`mkcert`（已内置在安装包中，若系统无此命令会自动从包内安装）
- DNS 分流阶段：`resolvectl`，或 `systemctl + NetworkManager`
- 若缺少 `dnsmasq` 且走 `NetworkManager` 回退路径，还需包管理器之一：`dnf` / `yum` / `apt-get`

条件命令：

- 若启用 `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1` 且 `/etc/docker/daemon.json` 已存在，需要 `python3`
- 若打包内 `Cubelet/config/config.toml` 启用了 `storage_backend = "cubecow"`，还会额外检查：
  `mkfs.ext4`、`mount`、`umount`、`losetup`

推荐安装包（覆盖上述 `cubecow` 依赖）：

- Debian / Ubuntu：`e2fsprogs`、`util-linux`
- OpenCloudOS / RHEL / CentOS：`e2fsprogs`、`util-linux`
可直接执行的安装示例：

```bash
# Debian / Ubuntu
sudo apt-get update
sudo apt-get install -y e2fsprogs util-linux

# OpenCloudOS / RHEL / CentOS
sudo dnf install -y e2fsprogs util-linux || \
sudo yum install -y e2fsprogs util-linux
```

## 前置条件

> **安全提示**：所有核心服务默认绑定 `0.0.0.0`。在将部署放到可被不可信网络访问的
> 机器上之前，请参阅[网络加固指南](../../docs/zh/guide/network-hardening.md)，了解绑定地址
> 配置、防火墙规则与凭据轮换。

- 目标机需要 `root` 权限。
- 目标机优先使用 `systemd-resolved` / `resolvectl` 做 `cube.app` 的 split DNS；当前实现会创建专用 dummy link（默认 `cube-dns0`）并为其添加本地 `/32` 地址，`CoreDNS` 默认绑定到 `169.254.254.53`，再把该地址和 `~cube.app` 绑定到该链路。若该能力不可用，则安装脚本会回退到 `NetworkManager + dnsmasq`：同样创建该 dummy link，并通过 `listen-address` / `bind-interfaces` 让 `dnsmasq` 同时绑定 `127.0.0.1` 和 `169.254.254.53`；随后安装器自己写 `/etc/resolv.conf`（NetworkManager 切到 `rc-manager=unmanaged`），把 nameserver 指向 `169.254.254.53`，让宿主应用和 Docker 容器看到同一个非 loopback 解析器。
- 目标机默认联网拉取 `mysql:8.0` 和 `redis:7-alpine`。
- `mkcert` 二进制已内置在发布包中（`support/bin/mkcert`），安装时若系统未预装 `mkcert`，会自动从包内复制到 `/usr/local/bin/mkcert`，无需联网下载。
- `cube proxy` 的 TLS 证书和私钥保存在宿主机 `CUBE_PROXY_CERT_DIR`，并通过 `docker compose` 以只读方式挂载进容器；更新证书后无需重建镜像，只需重启 `cube-proxy` 或在容器内 reload nginx。
- 推荐入口 `build-release-bundle-builder.sh` 需要宿主机具备 `docker` / `make` / `tar` / `python3` / `truncate` / `ldd` / `mkfs.ext4` 等工具。
- 推荐入口只把组件编译放进 builder；guest image 与最终打包仍在宿主机执行。
- 若直接执行底层入口 `build-release-bundle.sh`，构建机还需要根据 build mode 自行准备 `go` / `cargo` / `make` 等本地工具链。
- 若直接执行底层入口或首次使用推荐入口，构建机还需要能联网下载 Go modules；受限网络环境建议预先配置可用的 `GOPROXY`。
- 若启用 VM 路径，目标机仍需满足 `network-agent`、tap、路由等运行权限要求。

## 已知限制

- 如果 `assets/kernel-artifacts/` 下缺少 `vmlinux`，`build-vm-assets.sh` 和 `build-release-bundle.sh` 会立即失败；`vmlinux-pvm` 在构建时是可选制品，但安装时若设置 `CUBE_PVM_ENABLE=1`，发布包内必须包含它；发布包里的 `cube-kernel-scf.zip` 会在打包阶段自动生成。
- 如果 `deploy/guest-image/Dockerfile` 构建失败，或构建机的 `mkfs.ext4` 不支持 `-d`，guest image 生成会立即失败。
- `cube-snapshot/spec.json` 在当前 one-click 首版中不是强制产物；缺失时相关插件会退化为告警，而不是阻塞基础启动。
- 如果目标机既没有 `systemd-resolved` / `resolvectl`，也没有可重启的 `NetworkManager`，当前 one-click 仍会报错，因为这类环境下暂未接入第三套宿主机 DNS 方案。

## DNS 排障

- 查看当前 split DNS 状态：`resolvectl status`
- 验证宿主机 stub 是否正常：`dig +tcp +timeout=3 docker.cnb.cool @127.0.0.53`
- 验证本地 DNS 入口是否正常：两条路径下客户端入口都是同一个 dummy link IP，统一执行 `dig +tcp +timeout=3 foo.cube.app @169.254.254.53`。CoreDNS 内部仍然绑在 `127.0.0.54`，但只有 `systemd-resolved` 路径直连 CoreDNS，`NetworkManager` 回退路径先到 `dnsmasq` 再转发到 CoreDNS。
- 验证宿主 `/etc/resolv.conf` 是否走该入口：`cat /etc/resolv.conf` 应能看到 `nameserver 169.254.254.53`（两条路径均如此）。
- 验证容器视角：`docker run --rm alpine cat /etc/resolv.conf` 也应是 `nameserver 169.254.254.53`。如果看到 `nameserver 8.8.8.8`，说明宿主 `/etc/resolv.conf` 退化到了 loopback nameserver，导致 Docker 回退到内置公网 DNS。
- 若使用 `systemd-resolved` 路径，正常情况下默认网卡不应承载本地 CoreDNS 地址；该地址应只出现在专用 dummy link 上。

## 腾讯云集群部署 (Terraform)

> 完整指南（架构图、资源清单、TKE / PrivateDNS / CFS 前置条件、E2B 与 `*.cube.app` 域名、容量规划、加固与排障）请参阅文档站：[腾讯云集群部署（Terraform）](../../docs/zh/guide/tencentcloud-terraform-deploy.md)。

除了单机的 `install.sh` 之外，发布包还附带一个基于 Terraform 的部署器，可在腾讯云上拉起**集群版** CubeSandbox：由托管的 TKE 控制面运行 `cubemaster` / `cube-api` / `cube-proxy` / `cube-webui`，后端使用云上 MySQL + Redis，并带一个或多个 CVM PVM 计算节点。跳板机（SSH 端口 `443`）既是构建主机，也是这个原本私有 VPC 的堡垒机。

默认部署模式（与 `env.example` / `variables.tf` 一致）使用**公网预置镜像**（`TENCENTCLOUD_USE_TCR=false`），不在跳板机构建镜像；`cubemaster` 默认**单副本**且**不创建 CFS**（`TENCENTCLOUD_USE_CFS=false`，使用 Pod 本地存储）。启用 `TENCENTCLOUD_USE_CFS=true` 且提高 `TENCENTCLOUD_CUBEMASTER_REPLICAS` 时，才会创建 CFS 共享盘供多副本共用 `/data/CubeMaster/storage`。

`cube-proxy` 默认运行**单副本**（`TENCENTCLOUD_CUBE_PROXY_REPLICAS=1`）。自动暂停 / 自动恢复只有在单副本下才正确，因为每个 sidecar sweeper 只能看到打到自身 Pod 的流量。若要扩展到多副本，前端 LB 必须按 SandboxID 做 hash（会话保持），否则自动暂停 / 自动恢复会误判。

### 部署前准备（摘要）

首次 `create.sh` apply 前建议完成：

1. **TKE 服务角色授权**（必须）：登录 [TKE 控制台](https://console.cloud.tencent.com/tke2) 完成服务授权。文档：[服务授权相关角色权限说明](https://cloud.tencent.com/document/product/457/43416)。子账号还需 [TKE 预设策略授权](https://cloud.tencent.com/document/product/457/46033)。
2. **Private DNS**（按需）：`USE_TCR=true` 或 E2B SDK 访问 `*.cube.app` 时需开通。控制台：[DNSPod 内网解析](https://console.dnspod.cn/privateDNS)。文档：[Private DNS 产品介绍](https://cloud.tencent.com/document/product/1338/50527)。
3. **CFS**（按需）：仅 `TENCENTCLOUD_USE_CFS=true` 且 cubemaster 多副本时需要。控制台：[CFS](https://console.cloud.tencent.com/cfs)。文档：[CFS 快速入门](https://cloud.tencent.com/document/product/582/9132)。

> **TKE worker 与 PVM 计算节点是两套资源：** `TENCENTCLOUD_TKE_NODE_COUNT` 控制 TKE worker（运行控制面 Pod）；`TENCENTCLOUD_COMPUTE_NODE_COUNT` 控制 PVM 计算节点（运行 Cubelet / sandbox）。默认均为 `2`，职责不同。

> **E2B SDK：** 集群版不含单机 one-click 的 CoreDNS split DNS。除配置 `E2B_API_URL` 外，还须为 `*.cube.app` 配置 Private DNS 或等价解析，详见[完整指南 — E2B 与 cube.app 域名](../../docs/zh/guide/tencentcloud-terraform-deploy.md#e2b-与-cubeapp-域名)。

该部署器被放在解压后发布包的**顶层**，因此解压后即可直接运行：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>

export TENCENTCLOUD_SECRET_ID="your-secret-id"
export TENCENTCLOUD_SECRET_KEY="your-secret-key"

./terraform/tencentcloud/create.sh
```

`create.sh` 完全在解压后的发布包内运行：

- 它会自动探测本地 bundle（外层的 `cube-sandbox-one-click-<version>.tar.gz`，若该 tar 包已不存在则重新打包解压目录），并将其作为组件镜像和计算节点安装的离线源。当探测到本地 bundle 或通过 `TENCENTCLOUD_LOCAL_BUNDLE=/path/to.tar.gz` 指定时，无需任何公网下载；否则跳板机会回退到**在线安装**（下载 `online-install.sh` 与安装包），此时需要公网访问。
- 如果不存在 SSH 密钥对，它会在 `terraform/tencentcloud/.ssh/` 下自动生成。
- 它会在跳板机上使用内置的 `mkcert`（随 `assets/package/sandbox-package.tar.gz` 发布，即解压内层包后的 `sandbox-package/support/bin/mkcert`，与 `scripts/one-click/up-cube-proxy.sh` 流程一致）生成 cube-proxy CLB 的 TLS 证书（`cube.app` / `*.cube.app`），在跳板机的 `/root/cubeproxy-certs` 保留一份副本，并下载到本地 `terraform/tencentcloud/cubeproxy-certs/` 供 Secret 挂载。
- **默认模式**（`TENCENTCLOUD_USE_TCR=false`）：直接拉取公网预置镜像，部署 TKE addons 和 CVM 计算节点。
- **TCR 模式**（`TENCENTCLOUD_USE_TCR=true`）：创建 TCR 并在跳板机构建/推送四个组件镜像，再部署 TKE addons 和计算节点。默认创建 2 个计算节点；用 `TENCENTCLOUD_COMPUTE_NODE_COUNT` 调整数量。

cube-webui 的 nginx 配置（`webui-nginx.conf`）不单独维护：它派生自规范文件 `deploy/one-click/webui/nginx.conf`（由发布包构建时放入，或在源码树中运行 `create.sh` 时复制）。

运行 `create.sh` 的机器要求：`ssh`、`scp`、`nc`，以及对腾讯云 API 的网络访问。`terraform` 和 `jq` 缺失时会自动安装——`terraform` 从 HashiCorp 发布站点下载（需要 `curl`/`wget` + `unzip`），`jq` 优先用系统包管理器安装，失败时回退到从 GitHub 下载静态二进制。本地无需 `mkcert`/`openssl` —— 证书在跳板机上生成。

常用环境变量覆盖（下方默认值与 `create.sh`、`variables.tf` 中的默认值一致）：

```bash
export TENCENTCLOUD_REGION=ap-guangzhou
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_NODE_COUNT=2          # CVM PVM 计算节点数（默认 2）
export TENCENTCLOUD_TKE_NODE_COUNT=2              # TKE worker 节点数（默认 2）
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_USE_TCR=false                 # 默认使用公网预置镜像
export TENCENTCLOUD_USE_CFS=false                 # 默认无 CFS，cubemaster 单副本
export TENCENTCLOUD_CUBE_IMAGE_TAG=cubesandbox002-20260630
```

非交互 / CI 运行时建议显式设置以下变量（没有 TTY 时交互菜单会回退到默认值，显式设置可避免意外）。密码变量是例外：非交互运行会拒绝使用仓库中公开可见的内置演示密码并要求显式设置；如需在临时沙箱中使用不安全的默认密码，可设置 `TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1`。

```bash
export TENCENTCLOUD_AVAILABILITY_ZONE=ap-guangzhou-6
export TENCENTCLOUD_COMPUTE_INSTANCE_TYPE=SA9.MEDIUM8
export TENCENTCLOUD_LOCAL_BUNDLE=/path/to/cube-sandbox-one-click-<version>.tar.gz  # 在已解压的发布包内运行时会自动探测
export TENCENTCLOUD_PVM_KERNEL_VMLINUX=/path/to/vmlinux-pvm  # 仅当发布包不含 vmlinux-pvm 时需要
export TENCENTCLOUD_MYSQL_PASSWORD=...      # 非交互运行必填（无不安全回退）
export TENCENTCLOUD_REDIS_PASSWORD=...      # 非交互运行必填
export TENCENTCLOUD_CUBE_PASSWORD=...       # 非交互运行必填
export TENCENTCLOUD_BUILD_IMAGES=0          # 复用已推送的镜像
```

整体销毁：

```bash
./terraform/tencentcloud/destroy.sh
```

`destroy.sh` 同样需要 `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`，并复用 `create.sh` 保存在 `terraform/tencentcloud/.env` 中的选择。不再询问、直接销毁——运行 `destroy.sh` 本身即视为确认。

> **⚠ 避免不合理计费：** 当 `destroy.sh` 无法正常删除全部资源时（例如 MySQL/Redis 处于回收站/隔离状态，或 Terraform 已无法感知的残留资源），请登录腾讯云控制台手动删除残留资源，以免被继续计费：
> [VPC / 网络资源](https://console.cloud.tencent.com/vpc)、
> [MySQL 回收站](https://console.cloud.tencent.com/cdb/recycle)、
> [Redis 回收站](https://console.cloud.tencent.com/redis/recycle)、
> [CFS 文件系统](https://console.cloud.tencent.com/cfs)（若曾启用 `USE_CFS=true`）。
> 当某个销毁步骤失败或回收站清理未确认成功时，`destroy.sh` 也会打印这些链接进行提醒。

上述文件也内嵌在 `assets/package/sandbox-package.tar.gz` 中（供跳板机侧的 `build_images.sh` 使用）；顶层副本只是让部署器无需先解压内层包即可访问。

### 运行环境要求与 Terraform 说明

`create.sh` 在你的本地机器上驱动 Terraform，无需事先手动安装 Terraform：

- **凭证：** 必须导出 `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY`（在
  <https://console.cloud.tencent.com/cam/capi> 创建 API 密钥对）。常用的
  `TENCENTCLOUD_*` 变量见 `terraform/tencentcloud/env.example`；更高级的开关在
  `create.sh` 头部注释中说明。
- **本地工具：** `ssh`、`scp`、`nc`，以及对腾讯云 API 的网络访问。`terraform` 和
  `jq` 缺失时会自动安装——当 `/usr/local/bin` 可写时（例如以 root 运行）装到该目录，
  否则装到本地 `.bin/`。`terraform` 从 HashiCorp 发布站点下载（需要 `curl`/`wget` +
  `unzip`）；`jq` 优先用系统包管理器安装，失败时回退到从 GitHub 下载静态二进制。
  本地**无需** `mkcert` / `openssl`——cube-proxy 证书在跳板机上生成。
- **Terraform 状态保存在本地** `terraform/tencentcloud/` 下（`*.tfstate`，已
  gitignore——没有远端 backend）。请保留该目录与生成的 `.env`，以便后续 `destroy.sh`
  或重新运行能找到并管理同一批资源。不要在临时副本里运行 `create.sh` 后又指望另一个
  副本来清理。
- **分阶段、fail-fast 的 apply：** 资源按顺序创建——网络（VPC / 子网 / NAT）→ **（`USE_TCR=true` 时）** TCR →
  CVM（跳板机 + 计算节点）→ **（TCR 模式）** 在跳板机上构建并推送镜像 → MySQL / Redis → **（`USE_CFS=true` 时）** CFS 共享存储 →
  TKE 集群 + Kubernetes addons → 健康检查 → 计算节点初始化。Kubernetes provider 只有在
  TKE API Server 就绪后才会启用。销毁时，若创建了 CFS，会在删除子网之前先删除 CFS（其 NFS 挂载点是该子网
  内的一块弹性网卡）。
- 解析后的选择会保存到 `terraform/tencentcloud/.env` 并在下次运行时自动加载；显式设置
  的环境变量始终优先。

### 部分资源创建失败后的重试

如果某个阶段中途失败（例如所选地域/可用区下机型或可用区售罄、账号配额限制、或临时的
API 错误），**无需**销毁全部资源重头再来：

- 先修复原因——最常见的是**调整配置**：换一个 `TENCENTCLOUD_AVAILABILITY_ZONE` /
  `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` / `TENCENTCLOUD_REGION`、提升配额、设置密码等
  ——然后直接**重新运行 `./terraform/tencentcloud/create.sh`**。
- 重新运行时，`create.sh` 会从 `.env` 重新加载已保存的选择，与云上已存在的资源做状态
  对账（刷新并导入有状态资源，而不是重建），并**从上次中断处继续**。已存在的计算节点
  会被保留（绝不缩容）。
- 资源可用情况确实因**地域**与**可用区**而异：某个机型在一个可用区可用，在另一个可能
  不可用。交互式的可用区 / 机型菜单会针对你的地域在线查询，最终选择会在 apply 阶段
  校验。
- 只有当你确实想拆除整个部署时才需要 `destroy.sh`；普通重试之间不需要它。

### 高级用法：cube-proxy TLS 证书（使用你自己的证书）

`cube-proxy` 负责为 `cube.app` / `*.cube.app` 终结 TLS，其内置 nginx 配置硬编码了
证书路径 `…/certs/cube.app+3.pem` 和 `…/certs/cube.app+3-key.pem`：

- 默认情况下，`create.sh`（`prepare_cubeproxy_certs`）会在跳板机上用内置 `mkcert`
  生成一对**自签名**证书（SAN：`cube.app`、`*.cube.app`、`localhost`、`127.0.0.1`），
  下载到 `terraform/tencentcloud/cubeproxy-certs/`；Terraform 会把该目录下的所有文件
  打进 `cubeproxy-certs` Secret（因其包含 TLS 私钥，使用 Secret 而非 ConfigMap），
  以只读方式挂载到 cube-proxy Pod 的 `/usr/local/openresty/nginx/certs/`。
- **使用你自己的证书：** 在运行 `create.sh` 前，把你的 PEM 证书 + 私钥放进
  `terraform/tencentcloud/cubeproxy-certs/`，文件名必须正好是 `cube.app+3.pem` 和
  `cube.app+3-key.pem`（nginx 期望的名字），并覆盖 `cube.app` 与 `*.cube.app` 这两个
  SAN。`create.sh` 会复用已存在的文件而不再生成，因此 CA 签发的证书（例如映射到
  `cube.app` 的真实域名）会被原样使用，不再有自签名告警。
- **轮换证书：** 替换这两个文件并重新运行 `create.sh`；部署阶段会刷新 `cubeproxy-certs`
  Secret 并重启 cube-proxy 以加载新证书。自签名默认证书会让浏览器/客户端报“不受信任
  的 CA”告警，任何非一次性用途都应替换它。
