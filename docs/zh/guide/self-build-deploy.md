# 本地构建部署

> 如果你希望跳过本地构建，直接开始体验，请参阅[快速开始](./quickstart)。

本指南介绍如何从源码构建 Cube Sandbox 发布包并在单台裸金属服务器上完成部署。本地构建部署适用于**评估体验、开发测试**场景，也是需要自定义组件或扩展计算节点时的起点。

部署完成后，你将获得一个完整可用的 Cube Sandbox 实例：

- E2B 兼容 REST API 监听在 `3000` 端口
- CubeMaster、Cubelet、network-agent、CubeShim 作为宿主机进程运行
- MySQL 和 Redis 通过 Docker Compose 管理
- CubeProxy 提供 TLS（mkcert）和 CoreDNS 域名路由（`cube.app`）

## 前置条件

### 硬件要求

- **物理机或裸金属服务器**（不支持嵌套虚拟化）
- **x86_64** 架构
- **已启用 KVM** — 通过 `ls /dev/kvm` 验证
- 推荐配置：8 核以上 CPU、16 GB 以上内存

### 软件要求（目标机）

| 要求 | 说明 |
|------|------|
| Linux | 推荐 Ubuntu 22.04+ |
| Docker | 已安装并正常运行 |
| root 权限 | `install.sh` 需要 root 执行 |
| DNS 路由 | `systemd-resolved`（推荐）或 `NetworkManager + dnsmasq` |
| `tar`、`rg`、`ss` | 安装脚本依赖 |

### 软件要求（构建机）

| 要求 | 说明 |
|------|------|
| Docker | 用于运行 builder 容器 |
| `make` | 用于构建 builder 镜像 |
| `tar`、`python3`、`truncate`、`ldd`、`mkfs.ext4` | Guest 镜像生成和打包所需 |

> 构建机和目标机可以是同一台物理机。

### 网络要求

- 需要联网拉取 `mysql:8.0` 和 `redis:7-alpine` Docker 镜像。
- `mkcert` 二进制文件已内置在发布包中，安装时若系统尚未安装 `mkcert`，会自动从包内复制到 `/usr/local/bin/mkcert`，无需联网下载。
- CubeProxy 镜像构建使用 Alpine 和 PyPI 软件源（可配置）。

## 第一步：构建部署包

以下步骤在**构建机**上执行。

### 1.1 准备内核文件

获取编译好的 `vmlinux` 内核文件（自行编译或使用预编译版本），放置到指定目录：

```bash
cp /path/to/vmlinux deploy/one-click/assets/kernel-artifacts/
```

默认文件名为 `vmlinux`。可通过环境变量 `ONE_CLICK_CUBE_KERNEL_VMLINUX` 覆盖路径。

### 1.2 执行构建

在仓库根目录执行：

```bash
cd cube-sandbox
./deploy/one-click/build-release-bundle-builder.sh
```

该脚本会：

1. 构建或复用 `cube-sandbox-builder` Docker 镜像
2. 在 builder 容器内编译所有组件（CubeMaster、Cubelet、cube-api、network-agent、cube-agent、CubeShim、cube-runtime）
3. 在宿主机上构建 Guest VM 镜像
4. 将所有产物打包为发布包

### 1.3 获取构建产物

构建成功后，发布包位于：

```
deploy/one-click/dist/cube-sandbox-one-click-<version>.tar.gz
```

其中 `<version>` 取自当前 Git commit ID。

发布包包含：

- 所有编译后的二进制文件（cubemaster、cubelet、cube-api、network-agent、containerd-shim-cube-rs、cube-runtime）
- Guest VM 镜像（`cube-guest-image-cpu.img`）
- 内核包（`cube-kernel-scf.zip`）
- CubeProxy 和 CoreDNS 的 Docker Compose 模板
- MySQL/Redis 的 Docker Compose 模板
- 安装脚本（`install.sh`、`install-compute.sh`、`down.sh`、`smoke.sh`）
- 环境变量模板（`env.example`）

## 第二步：部署到目标机

### 2.1 上传并解压

将发布包上传到目标机并解压：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
```

### 2.2 配置环境变量

```bash
cp env.example .env
```

大多数变量均有适用于单机部署的默认值。安装脚本会自动从 `eth0` 探测节点 IP。如果你的主网卡名称不同，或需要指定特定 IP，可在 `.env` 中显式设置：

```bash
CUBE_SANDBOX_NODE_IP=<你的节点IP>
```

完整参数说明请参阅下方[配置参考](#配置参考)。

### 2.3 执行安装

#### 控制节点

```bash
sudo ./install.sh
```

安装脚本会依次执行：

1. 可选配置 Docker 镜像加速（如 `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR=1`）
2. 解压沙箱包到 `/usr/local/services/cubetoolbox`（可配置）
3. 创建日志和数据目录
4. 将 CubeShim 二进制文件软链接到 `/usr/local/bin/`
5. 安装内置的 `mkcert`（若系统尚无此命令），为 `cube.app` 域名生成 TLS 证书
6. 通过 Docker Compose 启动 MySQL 和 Redis
7. 构建并启动 CubeProxy 容器
8. 启动 CoreDNS 容器，配置宿主机 DNS 路由（`cube.app`）
9. 启动宿主机进程：network-agent、cubemaster、cube-api、cubelet
10. 执行健康检查（如 `ONE_CLICK_RUN_QUICKCHECK=1`）

安装完成后，安装器会把 `cubemastercli` 和 `cubecli` 软链接到 `/usr/local/bin`。

#### 添加计算节点（多机集群）

如果需要扩展到多台机器，可以添加计算节点并注册到本控制节点。完整操作请参阅[多机集群部署](./multi-node-deploy)指南。

## 验证部署

### 健康检查

```bash
sudo ./smoke.sh
```

该命令运行 `quickcheck.sh`，验证 `cube-api /health` 端点是否正常响应。

计算节点的健康检查请参阅[多机集群部署 — 验证部署](./multi-node-deploy#验证部署)。

### 使用 E2B SDK 测试

在客户端机器上设置以下环境变量：

```bash
export CUBE_TEMPLATE_ID=<你的模板ID>
export E2B_API_URL=http://<目标机IP>:3000
export E2B_API_KEY=dummy
export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

| 变量 | 说明 |
|------|------|
| `CUBE_TEMPLATE_ID` | 沙箱模板 ID，所有示例均需要 |
| `E2B_API_URL` | Cube API 地址，不设置会请求 E2B 官方云服务 |
| `E2B_API_KEY` | SDK 强制非空校验，本地部署填任意字符串即可 |
| `SSL_CERT_FILE` | mkcert 签发的 CA 根证书路径，HTTPS 连接需要 |

**执行代码**

```python
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox!')")
    print(result)
```

**执行 Shell 命令**

```python
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)
```

**读取沙箱内文件**

```python
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    content = sandbox.files.read("/etc/hosts")
    print(content)
```

更多示例可查看仓库中的 `CubeAPI/examples/` 目录。

## 常用操作

### 停止所有服务

```bash
sudo ./down.sh
```

该命令会停止所有宿主机进程（cubelet、cubemaster、cube-api、network-agent）、Docker 容器（CubeProxy、CoreDNS、MySQL、Redis），并回滚 `cube.app` 的 DNS 路由配置。

### 重新安装

直接再次运行 `install.sh` 即可。安装脚本会自动停止已有部署再进行安装。

### 查看日志

| 组件 | 日志路径 |
|------|----------|
| cube-api | `/data/log/CubeAPI/` |
| CubeMaster | `/data/log/CubeMaster/` |
| Cubelet | `/data/log/Cubelet/` |
| CubeShim | `/data/log/CubeShim/` |
| Hypervisor (VMM) | `/data/log/CubeVmm/` |
| CubeProxy | `/data/log/cube-proxy/` |
| 运行时 PID 文件 | `/var/run/cube-sandbox-one-click/` |
| 进程标准输出/错误 | `/var/log/cube-sandbox-one-click/` |

## 配置参考

所有配置通过 `.env` 文件管理。以下为完整参数说明。

### 构建时选项

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BUILDER_IMAGE` | `cube-sandbox-builder:latest` | 编译使用的 Docker 镜像 |
| `ONE_CLICK_CUBEMASTER_BUILD_MODE` | `local` | CubeMaster 构建模式（`local` = 从源码编译） |
| `ONE_CLICK_CUBELET_BUILD_MODE` | `local` | Cubelet 构建模式 |
| `ONE_CLICK_CUBE_API_BUILD_MODE` | `local` | cube-api 构建模式 |
| `ONE_CLICK_NETWORK_AGENT_BUILD_MODE` | `local` | network-agent 构建模式 |
| `ONE_CLICK_CUBE_AGENT_BUILD_MODE` | `local` | cube-agent 构建模式 |
| `ONE_CLICK_CUBE_SHIM_BUILD_MODE` | `local` | CubeShim 构建模式 |
| `ONE_CLICK_CUBE_KERNEL_VMLINUX` | `assets/kernel-artifacts/vmlinux` | vmlinux 内核文件路径 |

也可以指定预编译二进制文件来跳过编译：

| 变量 | 说明 |
|------|------|
| `ONE_CLICK_CUBEMASTER_BIN` | 预编译 cubemaster 路径 |
| `ONE_CLICK_CUBEMASTERCLI_BIN` | 预编译 cubemastercli 路径 |
| `ONE_CLICK_CUBELET_BIN` | 预编译 cubelet 路径 |
| `ONE_CLICK_CUBECLI_BIN` | 预编译 cubecli 路径 |
| `ONE_CLICK_CUBE_API_BIN` | 预编译 cube-api 路径 |
| `ONE_CLICK_NETWORK_AGENT_BIN` | 预编译 network-agent 路径 |
| `ONE_CLICK_CUBE_AGENT_BIN` | 预编译 cube-agent 路径 |
| `ONE_CLICK_CUBESHIM_BIN` | 预编译 containerd-shim-cube-rs 路径 |
| `ONE_CLICK_CUBE_RUNTIME_BIN` | 预编译 cube-runtime 路径 |

### 目标机选项

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ONE_CLICK_DEPLOY_ROLE` | `control` | 部署角色：`control` 为单机部署（默认）。计算节点请参阅[多机集群部署](./multi-node-deploy) |
| `ONE_CLICK_CONTROL_PLANE_IP` | 空 | 仅计算节点模式使用。详见[多机集群部署 — 配置环境变量](./multi-node-deploy#第二步配置环境变量) |
| `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` | 空 | 仅计算节点模式使用。详见[多机集群部署 — 配置环境变量](./multi-node-deploy#第二步配置环境变量) |
| `CUBE_SANDBOX_NODE_IP` | 自动从 `eth0` 探测 | 节点主网卡 IP 地址。未设置时自动探测；若网卡名称不同请显式指定。 |
| `ONE_CLICK_INSTALL_PREFIX` | `/usr/local/services/cubetoolbox` | 安装目录 |
| `ONE_CLICK_RUN_QUICKCHECK` | `1` | 安装后是否执行健康检查 |
| `ONE_CLICK_RUNTIME_DIR` | `/var/run/cube-sandbox-one-click` | PID 和运行时文件目录 |
| `ONE_CLICK_LOG_DIR` | `/var/log/cube-sandbox-one-click` | 进程标准输出/错误日志目录 |

### 数据库与缓存

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CUBE_SANDBOX_MYSQL_CONTAINER` | `cube-sandbox-mysql` | MySQL 容器名 |
| `CUBE_SANDBOX_REDIS_CONTAINER` | `cube-sandbox-redis` | Redis 容器名 |
| `CUBE_SANDBOX_MYSQL_PORT` | `3306` | MySQL 端口 |
| `CUBE_SANDBOX_REDIS_PORT` | `6379` | Redis 端口 |
| `CUBE_SANDBOX_MYSQL_ROOT_PASSWORD` | `cube_root` | MySQL root 密码 |
| `CUBE_SANDBOX_MYSQL_DB` | `cube_mvp` | MySQL 数据库名 |
| `CUBE_SANDBOX_MYSQL_USER` | `cube` | MySQL 用户名 |
| `CUBE_SANDBOX_MYSQL_PASSWORD` | `cube_pass` | MySQL 用户密码 |
| `CUBE_SANDBOX_REDIS_PASSWORD` | `ceuhvu123` | Redis 密码 |

### CubeProxy 与 DNS

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CUBE_PROXY_ENABLE` | `1` | 启用 CubeProxy（一键部署必须为 `1`） |
| `CUBE_PROXY_HOST_PORT` | `443` | CubeProxy 监听端口 |
| `CUBE_PROXY_DNS_ENABLE` | `1` | 启用 CoreDNS（一键部署必须为 `1`） |
| `CUBE_PROXY_DNS_ANSWER_IP` | `${CUBE_SANDBOX_NODE_IP}` | CoreDNS 对 `cube.app` 返回的 IP |
| `CUBE_PROXY_COREDNS_BIND_ADDR` | `127.0.0.54` | CoreDNS 绑定地址 |
| `ONE_CLICK_MKCERT_BIN` | `assets/bin/mkcert`（内置） | 构建时自定义 mkcert 二进制路径 |
| `ALPINE_MIRROR_URL` | 清华镜像 | CubeProxy 构建使用的 Alpine 软件源 |
| `PIP_INDEX_URL` | 清华镜像 | CubeProxy 构建使用的 PyPI 源 |

### 进程监听地址

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CUBEMASTER_ADDR` | `127.0.0.1:8089` | CubeMaster 监听地址 |
| `NETWORK_AGENT_HEALTH_ADDR` | `127.0.0.1:19090` | network-agent 健康检查端点 |
| `CUBE_API_BIND` | `0.0.0.0:3000` | cube-api 监听地址 |
| `CUBE_API_HEALTH_ADDR` | `127.0.0.1:3000` | cube-api 健康检查地址 |
| `CUBE_API_SANDBOX_DOMAIN` | `cube.app` | 沙箱域名，用于 CubeProxy 路由 |

### Docker 镜像加速（可选）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ONE_CLICK_ENABLE_TENCENT_DOCKER_MIRROR` | `1` | 启用腾讯云 Docker 镜像加速 |
| `ONE_CLICK_TENCENT_DOCKER_MIRROR_URL` | `https://mirror.ccs.tencentyun.com` | 镜像加速地址 |

## 安装后目录结构

安装完成后，部署目录位于 `/usr/local/services/cubetoolbox/`（默认）：

```
/usr/local/services/cubetoolbox/
├── CubeAPI/bin/cube-api                  # E2B 兼容 API 服务
├── CubeMaster/
│   ├── bin/cubemaster                # 调度编排服务
│   ├── bin/cubemastercli             # 命令行工具
│   └── conf.yaml                     # CubeMaster 配置
├── Cubelet/
│   ├── bin/cubelet                   # 节点管理 Agent
│   ├── bin/cubecli                   # 命令行工具
│   ├── config/                       # Cubelet 配置
│   └── dynamicconf/                  # 动态配置
├── network-agent/
│   ├── bin/network-agent             # 网络编排服务
│   └── network-agent.yaml            # 配置文件
├── cube-shim/bin/
│   ├── containerd-shim-cube-rs       # containerd shim
│   └── cube-runtime                  # 运行时
├── cube-image/
│   └── cube-guest-image-cpu.img      # Guest VM 镜像
├── cube-kernel-scf/                  # 内核制品
├── cubeproxy/                        # CubeProxy Docker Compose 与配置
├── coredns/                          # CoreDNS Docker Compose
├── support/                          # MySQL/Redis Docker Compose + 内置 mkcert
├── sql/                              # 数据库初始化 SQL
├── scripts/one-click/                # 运行时管理脚本
└── .one-click.env                    # 当前生效的环境配置
```

## 故障排查

### Docker 未安装或未运行

```
Error: required command not found: docker
```

请参照 [Docker 官方文档](https://docs.docker.com/engine/install/) 安装 Docker，并确保守护进程正在运行：

```bash
sudo systemctl start docker
sudo systemctl enable docker
```

### KVM 不可用

如果 `/dev/kvm` 不存在，说明 KVM 未启用。请确认：

- 运行环境为物理机（非虚拟机）
- 内核支持 KVM

```bash
# 检查 KVM 支持
lsmod | grep kvm
```

Intel CPU 需要 `kvm_intel` 模块，AMD CPU 需要 `kvm_amd` 模块。

### DNS 路由失败

安装脚本通过 `systemd-resolved` 或 `NetworkManager + dnsmasq` 配置 `cube.app` 域名的 split DNS。如果两者都不可用，安装将失败。

**解决方法**：在 `/etc/hosts` 或你的 DNS 服务器中手动添加 `*.cube.app` 指向 `CUBE_SANDBOX_NODE_IP` 的记录。

### mkcert 异常

`mkcert` 二进制已内置在发布包中，部署时会自动安装到 `/usr/local/bin/mkcert`。如需使用其他版本，可在执行 `install.sh` 前手动将 `mkcert` 安装到系统 PATH，或在构建时通过 `ONE_CLICK_MKCERT_BIN` 环境变量指定自定义二进制路径。

### MySQL/Redis 容器启动失败

查看 Docker 日志：

```bash
docker logs cube-sandbox-mysql
docker logs cube-sandbox-redis
```

常见原因：
- 端口冲突（3306 或 6379 已被占用）
- 磁盘空间不足
- Docker 守护进程异常

### ripgrep (`rg`) 未安装

```
Error: required command not found: rg
```

安装 ripgrep：

```bash
# Ubuntu/Debian
sudo apt-get install ripgrep

# CentOS/RHEL
sudo yum install ripgrep
```

## 已知限制

- 如果 `assets/kernel-artifacts/` 下缺少 `vmlinux`，构建会立即失败。
- 如果构建机的 `mkfs.ext4` 不支持 `-d` 参数，Guest 镜像生成会失败。
- `cube-snapshot/spec.json` 在当前一键发布中不是强制产物，缺失时相关功能会退化为告警而不阻塞启动。
- 目标机如果同时缺少 `systemd-resolved` 和 `NetworkManager`，自动 DNS 配置将无法完成。
