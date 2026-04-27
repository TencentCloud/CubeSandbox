# Cube Sandbox One-Click

本目录用于构建并交付 `cube-sandbox` 的单机一键发布包。

## 目录说明

- `build-release-bundle-builder.sh`：推荐入口；先在 builder 镜像中编译 one-click 需要的组件，再在宿主机继续执行发布包打包。
- `build-vm-assets.sh`：构建 `containerd-shim-cube-rs`、`cube-runtime`、`cube-agent`，把 `cube-agent` 注入 guest image 作为 `/sbin/init`，并收集 guest kernel。
- `build-release-bundle.sh`：底层打包入口；消费源码树或 `ONE_CLICK_*_BIN` 预编译产物，组装 `sandbox-package` 并生成最终发布包。
- `config-cube.toml`：one-click 默认 runtime 配置模板。
- `support/`：MySQL/Redis 的 `docker compose` 模板，安装后落到 `/usr/local/services/cubetoolbox/support/`；`support/bin/mkcert` 为内置的 mkcert 二进制。
- `cubeproxy/`：`cube proxy` 的 compose 模板、`global.conf` 模板、CoreDNS 模板与包装 Dockerfile。
- `webui/`：Dashboard 的 Nginx 运行时文件，安装后落到 `/usr/local/services/cubetoolbox/webui/`。
- `install.sh`：目标机控制节点安装与启动入口（默认 all-in-one）。
- `install-compute.sh`：目标机计算节点安装入口。
- `down.sh`：停止 one-click 安装的服务与依赖。
- `smoke.sh`：执行基础健康检查。
- `env.example`：构建机和目标机共用的环境变量模板。
- `lib/common.sh`：公共 shell 函数。
- `scripts/one-click/`：安装后实际执行的启停与校验脚本。
- `sql/`：MySQL 初始化 schema 和 seed 数据。

## 构建输入

需要准备的固定文件只有 guest kernel 的 `vmlinux`：

- `vmlinux`

默认放在 `assets/kernel-artifacts/`，也可以通过环境变量覆盖：

```bash
export ONE_CLICK_CUBE_KERNEL_VMLINUX=/abs/path/to/vmlinux
```

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

其中 `Cubelet` 直接使用仓库内现成的 `dynamicconf/conf.yaml`；`network-agent` 实际启动时优先通过 `--cubelet-config` 读取 `Cubelet/config/config.toml` 中的网络插件配置，以保证和 `Cubelet` 的网络参数保持一致；`cube-api` 则直接读取 `.one-click.env` 中的环境变量启动，默认监听 `0.0.0.0:3000` 并转发到本机 `cubemaster`。MySQL/Redis 固定部署到 `/usr/local/services/cubetoolbox/support`，由目标机本地 `docker compose` 管理；`cube proxy` 固定部署到 `/usr/local/services/cubetoolbox/cubeproxy`，在目标机本地 `docker compose build && up`。WebUI 固定部署到 `/usr/local/services/cubetoolbox/webui`，默认监听 `12088`，通过标准 nginx 容器托管发布包里的 `webui/dist`，并通过 Docker `host-gateway` 把 `/cubeapi` 反代到宿主机 CubeAPI。

## 目标机安装

把 `cube-sandbox-one-click-<version>.tar.gz` 拷到目标机后：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
sudo ./install.sh
```

默认会安装到 `/usr/local/services/cubetoolbox`。

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

如果显式设置了 `CUBE_SANDBOX_NODE_IP`，安装脚本会优先使用该值；否则会把自动探测到的节点 IP 写入 MySQL 的 `t_cube_host_info.ip` 和 `t_cube_sub_host_info.host_ip`，并用于 `cube proxy` / DNS 的地址渲染。

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

- 只安装 `Cubelet`、`network-agent`、`cube-shim`、`cube-image`、`cube-kernel-scf` 和运行所需脚本
- 只启动 `network-agent`、`cubelet`
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

安装时会在这个目录下渲染并使用 `docker-compose.yaml`，统一管理：

- `mysql:8.0`
- `redis:7-alpine`

`cube proxy` 和它的 DNS 解析在 one-click 里是必选能力，`.env` 中这两个值必须保持为 `1`：

```bash
CUBE_PROXY_ENABLE=1
CUBE_PROXY_DNS_ENABLE=1
```

其它常用参数如下：

```bash
CUBE_PROXY_HOST_PORT=443
CUBE_PROXY_CERT_DIR="${ONE_CLICK_INSTALL_PREFIX}/cubeproxy/certs"
CUBE_PROXY_DNS_ANSWER_IP="${CUBE_SANDBOX_NODE_IP}"
WEB_UI_ENABLE=1
WEB_UI_IMAGE=cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat
WEB_UI_HOST_PORT=12088
WEB_UI_UPSTREAM=http://host.docker.internal:3000
CUBE_API_BIND=0.0.0.0:3000
CUBE_API_HEALTH_ADDR=127.0.0.1:3000
CUBE_API_SANDBOX_DOMAIN=cube.app
ALPINE_MIRROR_URL=https://mirrors.tuna.tsinghua.edu.cn/alpine
PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
```

安装过程中会做这些事：

- 若系统尚未安装 `mkcert`，从安装包内置的 `support/bin/mkcert` 复制到 `/usr/local/bin/mkcert`，再在宿主机 `CUBE_PROXY_CERT_DIR`（默认 `/usr/local/services/cubetoolbox/cubeproxy/certs/`）下执行 `mkcert -install` 并生成 `cube.app+3.pem`、`cube.app+3-key.pem`
- 在 `/usr/local/services/cubetoolbox/support/` 下生成 `docker-compose.yaml` 并启动 MySQL/Redis
- 用 `CUBE_SANDBOX_NODE_IP` 渲染 `cubeproxy/global.conf`
- 在 `/usr/local/services/cubetoolbox/cubeproxy/` 下生成 `docker-compose.yaml`，并把宿主机 `CUBE_PROXY_CERT_DIR` 只读挂载到容器内 `/usr/local/openresty/nginx/certs/`，同时把 `ALPINE_MIRROR_URL` / `PIP_INDEX_URL` 作为 build args 传给 `Dockerfile.oneclick` 本地构建 `cube proxy` 镜像
- 启动 `CoreDNS` 容器；若目标机有 `resolvectl`，则创建专用 dummy link（默认 `cube-dns0`）并分配本地地址，`CoreDNS` 默认绑定到该链路地址 `169.254.254.53`，再把 `cube.app` 域名通过该链路路由到本地 DNS，避免污染宿主机默认公网 DNS；若目标机没有 `resolvectl`，则回退到 `NetworkManager + dnsmasq`，默认继续使用 `127.0.0.54`
- 启动宿主机进程 `network-agent`、`cubemaster`、`cube-api`、`cubelet`，并在 `quickcheck.sh` 中校验 `cube-api /health`
- 在 `/usr/local/services/cubetoolbox/webui/` 下启动标准 WebUI nginx 容器。该容器只读挂载 `webui/dist` 静态资源，发布 `WEB_UI_HOST_PORT`（默认 `12088`），把 `host.docker.internal` 映射到 Docker `host-gateway`，并通过 nginx 反代校验 `/cubeapi/v1/health`

停止 one-click 时会同时停止 `/usr/local/services/cubetoolbox/support` 下的 MySQL/Redis、WebUI、`cube proxy` / `CoreDNS`、宿主机进程 `network-agent` / `cubemaster` / `cube-api` / `cubelet`，并回滚 `cube.app` 的宿主机 DNS 路由配置。

部署完成后，如需让 E2B 官方 SDK 指向 one-click 节点，可以在客户端侧设置：

```bash
export E2B_API_URL=http://<target-host>:3000
export E2B_API_KEY=dummy
```

## 安装脚本启动前预检清单

`install.sh` / `install-compute.sh` 会在启动早期执行一次性 preflight 检查，确保依赖尽早失败，不会跑到中途才报错。

### compute 角色（`install-compute.sh`）

必需命令：

- `tar`
- `rg`
- `ss`
- `bash`
- `curl`
- `sed`
- `pgrep`
- `date`

条件命令：

- 若启用 `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1` 且 `/etc/docker/daemon.json` 已存在，需要 `python3`

### control 角色（`install.sh`，默认）

必需命令：

- `docker`
- `tar`
- `rg`
- `ss`
- `bash`
- `curl`
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

## 前置条件

- 目标机需要 `root` 权限。
- 目标机优先使用 `systemd-resolved` / `resolvectl` 做 `cube.app` 的 split DNS；当前实现会创建专用 dummy link（默认 `cube-dns0`）并为其添加本地 `/32` 地址，`CoreDNS` 默认绑定到 `169.254.254.53`，再把该地址和 `~cube.app` 绑定到该链路。若该能力不可用，则安装脚本会尝试回退到 `NetworkManager + dnsmasq`，默认使用 `127.0.0.54`。
- 目标机默认联网拉取 `mysql:8.0` 和 `redis:7-alpine`。
- `mkcert` 二进制已内置在发布包中（`support/bin/mkcert`），安装时若系统未预装 `mkcert`，会自动从包内复制到 `/usr/local/bin/mkcert`，无需联网下载。
- `cube proxy` 的 TLS 证书和私钥保存在宿主机 `CUBE_PROXY_CERT_DIR`，并通过 `docker compose` 以只读方式挂载进容器；更新证书后无需重建镜像，只需重启 `cube-proxy` 或在容器内 reload nginx。
- `cube proxy` 镜像构建默认使用国内源：`ALPINE_MIRROR_URL=https://mirrors.tuna.tsinghua.edu.cn/alpine`、`PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple`；若目标机网络策略不同，可以在 `.env` 中覆盖。
- 推荐入口 `build-release-bundle-builder.sh` 需要宿主机具备 `docker` / `make` / `tar` / `python3` / `truncate` / `ldd` / `mkfs.ext4` 等工具。
- 推荐入口只把组件编译放进 builder；guest image 与最终打包仍在宿主机执行。
- 若直接执行底层入口 `build-release-bundle.sh`，构建机还需要根据 build mode 自行准备 `go` / `cargo` / `make` 等本地工具链。
- 若直接执行底层入口或首次使用推荐入口，构建机还需要能联网下载 Go modules；受限网络环境建议预先配置可用的 `GOPROXY`。
- 若启用 VM 路径，目标机仍需满足 `network-agent`、tap、路由等运行权限要求。

## 已知限制

- 如果 `assets/kernel-artifacts/` 下缺少 `vmlinux`，`build-vm-assets.sh` 和 `build-release-bundle.sh` 会立即失败；发布包里的 `cube-kernel-scf.zip` 会在打包阶段自动生成。
- 如果 `deploy/guest-image/Dockerfile` 构建失败，或构建机的 `mkfs.ext4` 不支持 `-d`，guest image 生成会立即失败。
- `cube-snapshot/spec.json` 在当前 one-click 首版中不是强制产物；缺失时相关插件会退化为告警，而不是阻塞基础启动。
- 如果目标机既没有 `systemd-resolved` / `resolvectl`，也没有可重启的 `NetworkManager`，当前 one-click 仍会报错，因为这类环境下暂未接入第三套宿主机 DNS 方案。

## DNS 排障

- 查看当前 split DNS 状态：`resolvectl status`
- 验证宿主机 stub 是否正常：`dig +tcp +timeout=3 docker.cnb.cool @127.0.0.53`
- 验证本地 CoreDNS 是否正常：若使用 `systemd-resolved` 路径，默认执行 `dig +tcp +timeout=3 foo.cube.app @169.254.254.53`；若使用 `NetworkManager` 回退路径，则执行 `dig +tcp +timeout=3 foo.cube.app @127.0.0.54`
- 若使用 `systemd-resolved` 路径，正常情况下默认网卡不应承载本地 CoreDNS 地址；该地址应只出现在专用 dummy link 上。
