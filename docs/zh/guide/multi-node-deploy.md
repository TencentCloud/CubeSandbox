# 多机集群部署

本指南介绍如何将单机 Cube Sandbox 部署扩展为多机集群，通过添加**计算节点**来实现。计算节点只运行沙箱运行时组件（`Cubelet`、`network-agent`、`CubeShim`），并向第一台机器上的控制面注册。

::: tip 前置条件
添加计算节点前，你必须先通过[本地构建部署指南](./self-build-deploy.md)完成控制节点的部署。
:::

## 架构概览

```
┌─────────────────────────────────────────┐
│              控制节点                     │
│  CubeMaster, cube-api, CubeProxy,      │
│  CoreDNS, MySQL, Redis,                │
│  Cubelet, network-agent                 │
└──────────────────┬──────────────────────┘
                   │  /internal/meta API
       ┌───────────┼───────────┐
       ▼           ▼           ▼
┌────────────┐┌────────────┐┌────────────┐
│ 计算节点 #1 ││ 计算节点 #2 ││ 计算节点 #N │
│ Cubelet    ││ Cubelet    ││ Cubelet    │
│ net-agent  ││ net-agent  ││ net-agent  │
└────────────┘└────────────┘└────────────┘
```

- **控制节点**运行完整技术栈：编排调度（CubeMaster）、API 网关（cube-api）、代理（CubeProxy + CoreDNS）、数据库（MySQL + Redis），同时自身也作为计算节点。
- 每个**计算节点**只运行 `Cubelet` 和 `network-agent`，向控制面 `CubeMaster` 注册并接收沙箱调度请求。

## 前置条件

每台计算节点需满足与控制节点相同的硬件和软件要求：

- **物理机或裸金属服务器**（不支持嵌套虚拟化）
- **x86_64** 架构，**已启用 KVM**（`ls /dev/kvm`）
- **Docker** 已安装并运行
- 到控制节点的**网络连通性**（默认需访问 `CubeMaster` 的 `8089` 端口）

完整要求列表请参阅[本地构建部署 — 前置条件](./self-build-deploy#前置条件)。

## 第一步：准备发布包

使用与控制节点**相同的发布包**。将其拷贝到计算节点并解压：

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
```

## 第二步：配置环境变量

```bash
cp env.example .env
```

编辑 `.env`，设置以下变量：

```bash
ONE_CLICK_DEPLOY_ROLE=compute
CUBE_SANDBOX_NODE_IP=<当前节点IP>
ONE_CLICK_CONTROL_PLANE_IP=<控制节点IP>
```

| 变量 | 说明 |
|------|------|
| `ONE_CLICK_DEPLOY_ROLE` | 计算节点必须设为 `compute` |
| `CUBE_SANDBOX_NODE_IP` | 当前节点主网卡 IP |
| `ONE_CLICK_CONTROL_PLANE_IP` | 控制节点 IP，自动拼接为 `<ip>:8089` 作为 CubeMaster 地址 |

如果 CubeMaster 使用非默认端口，也可以显式指定：

```bash
ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR=<控制节点IP>:8089
```

同时设置时，`ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` 优先级高于 `ONE_CLICK_CONTROL_PLANE_IP`。

## 第三步：安装

```bash
sudo ./install-compute.sh
```

计算节点安装脚本会：

1. 只安装 `Cubelet`、`network-agent`、`cube-shim`、`cube-image`、`cube-kernel-scf` 和运行时脚本
2. 只启动宿主机进程：`network-agent`、`cubelet`
3. 自动把 `Cubelet` 的 `meta_server_endpoint` 指向控制面 `CubeMaster`
4. 通过控制面的 `/internal/meta` 接口注册节点并上报状态

## 验证部署

### 健康检查

```bash
sudo ./smoke.sh
```

计算节点模式下，`quickcheck.sh` 会验证：

- 本机 `network-agent` 健康状态
- 控制面 `CubeMaster` 可达
- 当前节点已出现在控制面的 `/internal/meta/nodes/{node_id}` 中

### 从控制节点验证

在控制节点上确认计算节点已注册：

```bash
curl http://127.0.0.1:8089/internal/meta/nodes
```

返回结果中应包含计算节点的 IP 和健康状态。

## 常用操作

### 停止计算节点服务

```bash
sudo ./down.sh
```

计算节点模式下，该命令只会停止 `cubelet` 和 `network-agent`，不影响控制面或其他计算节点。

### 重新安装

直接再次运行 `install-compute.sh` 即可。安装脚本会自动停止已有部署再进行安装。

### 查看日志

| 组件 | 日志路径 |
|------|----------|
| Cubelet | `/data/log/Cubelet/` |
| CubeShim | `/data/log/CubeShim/` |
| Hypervisor (VMM) | `/data/log/CubeVmm/` |
| 运行时 PID 文件 | `/var/run/cube-sandbox-one-click/` |
| 进程标准输出/错误 | `/var/log/cube-sandbox-one-click/` |

控制节点的日志路径请参阅[本地构建部署 — 查看日志](./self-build-deploy#查看日志)。

## 配置参考

计算节点使用相同的 `.env` 文件格式。以下变量与计算节点部署特别相关：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ONE_CLICK_DEPLOY_ROLE` | `control` | 计算节点必须设为 `compute` |
| `ONE_CLICK_CONTROL_PLANE_IP` | 空 | 控制节点 IP，默认拼接为 `<ip>:8089` |
| `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` | 空 | 显式指定 CubeMaster 地址，优先级高于 `ONE_CLICK_CONTROL_PLANE_IP` |
| `CUBE_SANDBOX_NODE_IP` | `10.0.0.10` | **必须修改。** 当前节点主网卡 IP |
| `ONE_CLICK_INSTALL_PREFIX` | `/usr/local/services/cubetoolbox` | 安装目录 |
| `ONE_CLICK_RUN_QUICKCHECK` | `1` | 安装后是否执行健康检查 |

完整配置参考（构建选项、数据库、代理等）请参阅[本地构建部署 — 配置参考](./self-build-deploy#配置参考)。

## 故障排查

### 计算节点无法连接 CubeMaster

检查网络连通性：

```bash
curl http://<控制节点IP>:8089/internal/meta/nodes
```

如果失败，请检查：
- 控制节点的防火墙规则（`8089` 端口需可访问）
- `.env` 中 `ONE_CLICK_CONTROL_PLANE_IP` 或 `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` 的值

### 节点未出现在控制面

如果 `smoke.sh` 本地通过但控制面上看不到该节点：

1. 检查 Cubelet 日志：`/data/log/Cubelet/`
2. 确认 Cubelet 配置中的 `meta_server_endpoint` 指向正确的 CubeMaster 地址
3. 确保 `CUBE_SANDBOX_NODE_IP` 设为可路由的 IP（不是 `127.0.0.1`）

通用故障排查（Docker、KVM、DNS 等）请参阅[本地构建部署 — 故障排查](./self-build-deploy#故障排查)。
