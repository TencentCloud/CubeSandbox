# 多机集群部署

本指南介绍如何将单机 Cube Sandbox 部署扩展为多机集群，通过添加**计算节点**来实现。计算节点只运行沙箱运行时组件（内置 network runtime 的 `Cubelet`、`CubeShim`），并向第一台机器上的控制面注册。

:::: warning 生产环境注意
如果您计划在生产环境中使用 Cube Sandbox，请参阅[网络加固](./network-hardening.md)指南，在将服务暴露到不可信网络之前完成安全加固。
::::

:::: tip 前置条件
添加计算节点前，你必须先通过[本地构建部署指南](./self-build-deploy.md)完成控制节点的部署。
::::

## 架构概览

```
┌─────────────────────────────────────────┐
│              控制节点                    │
│  CubeMaster, CubeOps, cube-api,         │
│  CubeProxy, CoreDNS, MySQL, Redis,      │
│  Cubelet (network runtime)              │
└──────────────────┬──────────────────────┘
                   │  /internal/v1/node-agent API
       ┌───────────┼───────────┐
       ▼           ▼           ▼
┌────────────┐┌────────────┐┌────────────┐
│ 计算节点 #1 ││ 计算节点 #2 ││ 计算节点 #N │
│ Cubelet    ││ Cubelet    ││ Cubelet    │
│ net runtime││ net runtime││ net runtime│
└────────────┘└────────────┘└────────────┘
```

- **控制节点**运行完整技术栈：编排调度（CubeMaster）、节点管理（CubeOps）、API 网关（cube-api）、代理（CubeProxy + CoreDNS）、数据库（MySQL + Redis）、内置 MinIO S3 存储（供 volume 使用），同时自身也作为计算节点。
- 每个**计算节点**只运行内置 network runtime 的 `Cubelet`，向控制面 `CubeOps` 注册并接收来自 `CubeMaster` 的沙箱调度请求。

## 前置条件

每台计算节点需满足与控制节点相同的硬件和软件要求：

- **物理机或裸金属服务器**（不支持嵌套虚拟化）
- **x86_64** 或 **aarch64**（ARM64）架构，**已启用 KVM**（`ls /dev/kvm`）
- **Docker** 已安装并运行
- 到控制节点的**网络连通性**（默认需访问 `CubeOps` 的 `3010` 端口进行节点注册；使用内置 MinIO 时还需访问 `9000` 端口）

完整要求列表请参阅[本地构建部署 — 前置条件](./self-build-deploy.md#前置条件)。

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

# CUBE_S3_*：可选但强烈建议。缺失时仅告警并继续安装，S3 卷插件不可用。
# 取值方式见下方提示。内置 MinIO 时形如：
CUBE_S3_ENDPOINT=http://<控制节点IP>:9000
CUBE_S3_ACCESS_KEY_ID=<取自控制节点>
CUBE_S3_SECRET_ACCESS_KEY=<取自控制节点>
CUBE_S3_BUCKET=cube-volumes
CUBE_S3_S3FS_EXTRA_OPTS=-ouse_path_request_style
```

| 变量 | 说明 |
|------|------|
| `ONE_CLICK_DEPLOY_ROLE` | 计算节点必须设为 `compute` |
| `CUBE_SANDBOX_NODE_IP` | 当前节点主网卡 IP |
| `ONE_CLICK_CONTROL_PLANE_IP` | 控制节点 IP，自动拼接为 `<ip>:3010` 作为 CubeOps 节点注册地址 |
| `CUBE_S3_*` | 可选但强烈建议。Volume 插件依赖 S3；缺失时仅告警并继续安装，但 S3 卷插件不可用。取值见下方提示。 |

::: tip 缺失时仅告警
`install-compute.sh` 检查 `CUBE_S3_ENDPOINT`，缺失时打印醒目的黄色警告并继续安装。节点可正常部署，但 **S3 卷插件不可用**，补齐后重装即可。

从控制节点取 `CUBE_S3_*` 回填值：

1. 在控制节点执行：
   ```bash
   grep '^CUBE_S3_' /usr/local/services/cubetoolbox/.one-click.env
   ```
   输出为空说明控制节点自身未配 S3。
2. 把输出逐行拷贝到计算节点 `.env`。
3. 重新执行 `sudo ./install-compute.sh`。

使用内置 MinIO 时，还需放行计算节点到控制面的 TCP 9000。
:::

如果 CubeOps 使用非默认端口，也可以显式指定：

```bash
ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR=<控制节点IP>:3010
```

同时设置时，`ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` 优先级高于 `ONE_CLICK_CONTROL_PLANE_IP`。

## 第三步：安装

```bash
sudo ./install-compute.sh
```

计算节点安装脚本会：

1. 只安装内置 network runtime 的 `Cubelet`、`cube-shim`、`cube-image`、`cube-kernel-scf` 和运行时脚本
2. 只启动宿主机进程：`cubelet`
3. 自动把 `Cubelet` 的 `meta_server_endpoint` 指向控制面 `CubeOps`
4. 通过控制面的 `/internal/v1/node-agent` 接口向 CubeOps 注册节点并上报状态

## 验证部署

### 健康检查

```bash
sudo ./smoke.sh
```

计算节点模式下，`quickcheck.sh` 会验证：

- 本机 `Cubelet` 及其内置 network runtime 健康状态
- 控制面 `CubeOps` 可达
- 当前节点已出现在控制面的 `/internal/v1/nodes/{node_id}` 中

### 从控制节点验证

在控制节点上确认计算节点已注册：

```bash
curl http://127.0.0.1:3010/internal/v1/nodes
```

返回结果中应包含计算节点的 IP 和健康状态。

## 配置 CubeMaster 调度评分

多机部署时，应在控制节点的 CubeMaster 配置中设置 `scheduler.score`。如果未配置评分，CubeMaster 会先过滤可用节点，再按照过滤后的节点顺序进行选择，新的沙箱可能集中到第一个可用节点，直到资源过滤器把流量推到其他节点。

可以将下面这些调度字段合并到 `cubemaster.yaml` 中已有的 `scheduler` 段。请保留当前部署已有的 `filter`、超时和其他 scheduler 配置。

```yaml
scheduler:
  # 保留当前部署已有的 filter、超时和其他 scheduler 配置。
  priority_select_num: 3
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 2
      local_create_num: 3
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1.0
        enable_weight_factors:
          - mvm_num
          - local_create_num
          - quota_cpu_usage
          - quota_mem_usage
```

对于多机集群，建议将 `scheduler.priority_select_num` 设置为大于 `1` 的值，让 CubeMaster 从评分最高的一组节点中随机选择。随项目提供的默认配置使用 `priority_select_num: 1`，这意味着评分只会决定下一个沙箱落到哪一个节点，而不会在多个高分节点之间分散放置。小规模集群可以从 `3` 开始，并根据节点数量继续调整。`scheduler.least_select_name` 默认值为 `random`，通常不需要显式设置。

完整的 CubeMaster 调度配置、Cubelet 节点上报、quota / label / 并发对调度的影响，以及新增计算节点后的 template redo 操作，请参阅[CubeMaster 调度器配置参考](./cubemaster-scheduler-config.md)。

更新 `cubemaster.yaml` 后，请按当前部署方式重启 CubeMaster，让调度器加载新的评分配置。

## 从客户端连接集群

客户端应用需要 CubeAPI 控制面地址，以及一条通过 CubeProxy 访问沙箱服务的数据面链路。根据客户端类型选择最简单的方式：

| 方式 | 适用场景 | 泛域名 DNS | 额外组件 |
| --- | --- | :---: | :---: |
| CubeSandbox SDK + `CUBE_PROXY_NODE_IP` | Python、Go 和 Node.js SDK | 不需要 | 不需要 |
| CubeProxy 路径模式 | curl、后端服务、通用 HTTP 客户端 | 不需要 | 不需要 |
| 泛域名 DNS | 生产环境、浏览器、SPA、官方 E2B SDK | 需要 | 不需要 |
| E2B 开发 sidecar | 本地没有 DNS，但必须使用官方 E2B SDK | 不需要 | 需要 |

### CubeSandbox SDK：直连 CubeProxy

CubeSandbox SDK 可以直接连接指定的 CubeProxy IP，同时保留用于沙箱路由的虚拟 `Host`，因此不需要配置泛域名 DNS：

```bash
export CUBE_API_URL="http://<控制面IP>:3000"
export CUBE_PROXY_NODE_IP="<CubeProxy节点IP>"
export CUBE_PROXY_PORT_HTTP=80
export CUBE_TEMPLATE_ID="<模板ID或别名>"
```

设置后即可正常使用 SDK。控制面请求访问 CubeAPI，数据面请求直接连接 CubeProxy。

### 通用 HTTP 客户端：路径模式

任意 HTTP 客户端都可以通过 CubeProxy 路径前缀访问沙箱服务：

```text
http://<CubeProxy地址>:<HTTP端口>/sandbox/<sandbox-id>/<容器端口>/<路径>
```

例如：

```bash
curl http://10.0.0.5/sandbox/abc123/49999/health
```

路径模式不需要 DNS 或证书配置，并支持 WebSocket 升级。但它不适合使用 `/static/app.js` 等根绝对路径加载资源的 SPA，此类应用应使用泛域名 DNS。

### 生产环境和浏览器访问：泛域名 DNS

Host 模式使用 `<端口>-<sandbox-id>.<域名>` 格式的沙箱域名。需要配置指向 CubeProxy 的泛域名 A 记录：

```text
*.cube.example.com  →  <CubeProxy公网或内网IP>
```

CubeAPI 必须使用相同的基础域名：

```bash
export CUBE_API_SANDBOX_DOMAIN=cube.example.com
```

一键部署内置 CoreDNS，可供本机解析 `*.cube.app`。它主要用于本地体验；生产环境和多机共享环境应使用托管 DNS 或内网 DNS 服务。`/etc/hosts` 不支持泛域名记录。

TLS 和 DNS 的完整配置请参阅 [HTTPS 证书与域名解析](./https-and-domain.md)。

### 官方 E2B SDK 无泛域名 DNS：开发 sidecar

官方 E2B SDK 没有 CubeSandbox SDK 的 IP 直连选项。本地开发环境无法配置泛域名 DNS 时，可以使用 [E2B 开发 sidecar 示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/e2b-dev-sidecar)：

```bash
cd examples/e2b-dev-sidecar
pip install -r requirements.txt
cp env.example .env
```

连接远程集群时配置：

```bash
E2B_API_URL="http://<控制面IP>:3000"
CUBE_REMOTE_PROXY_BASE="https://<CubeProxy节点IP>:443"
E2B_API_KEY="<API密钥>"
CUBE_TEMPLATE_ID="<模板ID或别名>"
```

然后运行：

```bash
python demo.py
```

`CUBE_REMOTE_PROXY_BASE` 必须指向 CubeProxy，不能填写 sidecar 自己的监听地址。集群启用鉴权时，需要使用有效的 API Key。

## 常用操作

### 停止计算节点服务

```bash
sudo ./down.sh
```

计算节点模式下，该命令只会停止 `cubelet`，不影响控制面或其他计算节点。

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

控制节点的日志路径请参阅[本地构建部署 — 查看日志](./self-build-deploy.md#查看日志)。

## 配置参考

计算节点使用相同的 `.env` 文件格式。以下变量与计算节点部署特别相关：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ONE_CLICK_DEPLOY_ROLE` | `control` | 计算节点必须设为 `compute` |
| `ONE_CLICK_CONTROL_PLANE_IP` | 空 | 控制节点 IP，默认拼接为 `<ip>:3010` |
| `ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` | 空 | 显式指定 CubeOps 地址，优先级高于 `ONE_CLICK_CONTROL_PLANE_IP` |
| `CUBE_SANDBOX_NODE_IP` | `10.0.0.10` | **必须修改。** 当前节点主网卡 IP |
| `CUBE_SANDBOX_NETWORK_CIDR` | `192.168.0.0/18`（取自 `config.toml`） | cubevs 本地网络 CIDR。需与控制节点一致。格式为 IPv4 CIDR（如 `10.100.0.0/18`），掩码范围 /16~/24。安装时自动检测宿主机冲突。 |
| `CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK` | `0` | 设为 `1` 跳过冲突检测（不推荐）。 |
| `ONE_CLICK_RUN_QUICKCHECK` | `1` | 安装后是否执行健康检查 |
| `CUBE_S3_*` | 空 / 由控制面 MinIO 填入 | 可选但强烈建议。Volume 插件依赖 S3，缺失时仅告警、S3 卷插件不可用。从控制节点 `/usr/local/services/cubetoolbox/.one-click.env` 拷贝（取值方法见上文第二步）；`ENDPOINT` / `ACCESS_KEY_ID` / `SECRET_ACCESS_KEY` / `BUCKET` 无可用默认值。 |

完整配置参考（构建选项、数据库、代理等）请参阅[本地构建部署 — 配置参考](./self-build-deploy.md#配置参考)。

## 故障排查

### 计算节点无法连接 CubeOps

检查网络连通性：

```bash
curl http://<控制节点IP>:3010/internal/v1/nodes
```

如果失败，请检查：
- 控制节点的防火墙规则（`3010` 端口需可访问）
- `.env` 中 `ONE_CLICK_CONTROL_PLANE_IP` 或 `ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` 的值

### 节点未出现在控制面

如果 `smoke.sh` 本地通过但控制面上看不到该节点：

1. 检查 Cubelet 日志：`/data/log/Cubelet/`
2. 确认 Cubelet 配置中的 `meta_server_endpoint` 指向正确的 CubeOps 地址
3. 确保 `CUBE_SANDBOX_NODE_IP` 设为可路由的 IP（不是 `127.0.0.1`）

通用故障排查（Docker、KVM、DNS 等）请参阅[本地构建部署 — 故障排查](./self-build-deploy.md#故障排查)。
