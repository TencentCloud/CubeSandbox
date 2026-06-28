# 网络加固

Cube Sandbox 的控制面与管理类服务为了便于本地快速体验，部分服务默认绑定在
`0.0.0.0` 上。当机器可被不可信网络访问时，这会暴露出攻击面：大多数管理端点
既无鉴权也无 TLS。本文说明默认的监听面，并介绍如何通过**绑定地址配置**与
**防火墙规则**进行收紧。

::: warning
一键部署 / 本地构建部署面向开发与评估场景。在将部署放到可被公网访问的机器上
之前，请通读本文，并至少应用下文中的一种加固方案。
:::

## 默认监听面

| 进程 | 默认绑定 | 端口 | 配置方式 | 说明 |
|------|---------|------|----------|------|
| CubeMaster | `0.0.0.0` | 8089 | `.env` 中的 `CUBEMASTER_HTTP_BIND` | 集群管理 HTTP API，**无鉴权** |
| CubeAPI | `0.0.0.0` | 3000 | `.env` 中的 `CUBE_API_BIND` | 沙箱生命周期 API |
| Cubelet gRPC | `0.0.0.0` | 9999 | `Cubelet/config/config.toml` 的 `tcp_address` | 节点管理 RPC，**无 TLS** |
| Cubelet HTTP | `0.0.0.0` | 9998 | `Cubelet/config/config.toml` 的 `[http] address` | 调试 / metrics |
| cube-proxy | `0.0.0.0` | 80 / 443 | `CUBE_PROXY_HTTP_PORT` / `CUBE_PROXY_HTTPS_PORT` | 设计上即面向公网 |
| WebUI | `0.0.0.0` | 12088 | `.env` 中的 `WEB_UI_HOST_PORT`（仅端口） | 控制台 |
| MySQL | `127.0.0.1` | 3306 | compose 模板中硬编码 | 已仅绑回环 |
| Redis | `127.0.0.1` | 6379 | compose 模板中硬编码 | 已仅绑回环 |

MySQL 与 Redis 已由内置 compose 模板绑定到回环地址，网络上不可达。需要重点关注
的是表中默认为 `0.0.0.0` 的其余服务。

## 各服务绑定地址配置

### CubeMaster

在运行 `install.sh` 之前，于 `.env` 中设置：

```bash
# 绑定到内网网卡（多机部署安全）：
CUBEMASTER_HTTP_BIND=10.0.0.11

# 或仅绑回环（单机一体化、无 compute 节点）：
CUBEMASTER_HTTP_BIND=127.0.0.1
```

::: warning
如果你有 compute 节点或使用 host 网络模式的 cube-proxy，**请勿**将
`CUBEMASTER_HTTP_BIND` 设为 `127.0.0.1`——它们需要通过节点的外网/内网 IP 访问
CubeMaster，绑回环会导致它们断联。
:::

### CubeAPI

在 `.env` 中设置，取值为 `<地址>:<端口>`：

```bash
# 绑定到内网网卡：
CUBE_API_BIND=10.0.0.11:3000
```

修改 `CUBE_API_BIND` 时，需同步更新 `CUBE_API_HEALTH_ADDR`：

```bash
CUBE_API_HEALTH_ADDR=10.0.0.11:3000
```

::: warning
WebUI 容器通过 `host.docker.internal` 访问 CubeAPI。将 CubeAPI 绑定到
`127.0.0.1` 会导致桥接模式 Docker 下的 WebUI 不可用。请改用内网 IP 绑定或
防火墙规则。
:::

### Cubelet（gRPC + HTTP）

Cubelet 从 `config/config.toml` 读取监听地址。安装完成后该文件位于
`<PKG_ROOT>/Cubelet/config/config.toml`。编辑：

```toml
[http]
  # 默认 ":9998" 即 0.0.0.0:9998
  address = "10.0.0.11:9998"

[grpc]
  # 默认 ":9999" 即 0.0.0.0:9999
  tcp_address = "10.0.0.11:9999"
```

::: warning
多机部署中，控制节点通过 `tcp_address` 连接每个 compute 节点的 Cubelet。如果
绑定到内网 IP，请确保控制节点能够路由到该地址。绑定到 `127.0.0.1`
**会破坏**多机通信。
:::

### WebUI

WebUI 的 docker-compose 模板以 `<WEB_UI_HOST_PORT>:80` 形式发布端口，该变量目前
只接受端口号。如需限制访问，请使用防火墙规则：

```bash
sudo iptables -A INPUT -p tcp --dport 12088 -s 10.0.0.0/24 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 12088 -j DROP
```

### cube-proxy（HTTP/HTTPS）

cube-proxy 使用 host 网络模式，通常是沙箱流量的公网入口。除非你在其前面再放一层
专用反向代理，否则保持 `0.0.0.0`：

```bash
CUBE_PROXY_HTTP_PORT=80
CUBE_PROXY_HTTPS_PORT=443
```

若需限制来源 IP，请使用防火墙规则。

### MySQL / Redis

内置容器已通过 compose 模板绑定到 `127.0.0.1`，无需额外配置。若使用外部
MySQL/Redis（`CUBE_EXTERNAL_MYSQL_HOST`），请在网络层做好访问控制。

## 加固方案

根据你的环境，选择以下一种或两种方案。

### 方案 A：绑定到内网网卡

若机器同时拥有公网与内网网卡，将管理类服务绑定到**内网 IP**，使其仅内网可达，
公网接口不暴露：

```bash
# .env
CUBEMASTER_HTTP_BIND=10.0.0.11
CUBE_API_BIND=10.0.0.11:3000
CUBE_API_HEALTH_ADDR=10.0.0.11:3000
```

```toml
# Cubelet/config/config.toml
[http]
  address = "10.0.0.11:9998"
[grpc]
  tcp_address = "10.0.0.11:9999"
```

这样既保持多机通信正常（compute 节点经内网连接），又避免公网访问。

对于**单机一体化、无 compute 节点**的场景，可以绑定到 `127.0.0.1` 实现最大限度
收紧（注意上文各服务的相关限制）。

### 方案 B：防火墙源 IP 白名单

保持默认的 `0.0.0.0` 绑定，但通过防火墙规则只放通**可信来源 IP**（你的 compute
节点和管理机）。

使用 `iptables`：

```bash
# CubeMaster 只放通 compute 节点 + 管理机
sudo iptables -A INPUT -p tcp --dport 8089 -s 10.0.0.12 -j ACCEPT       # compute-1
sudo iptables -A INPUT -p tcp --dport 8089 -s 10.0.0.13 -j ACCEPT       # compute-2
sudo iptables -A INPUT -p tcp --dport 8089 -s 192.168.1.100 -j ACCEPT   # 管理机
sudo iptables -A INPUT -p tcp --dport 8089 -j DROP                      # 拒绝其余

# 对 9999、3000、12088 按需重复同样的规则
```

使用 `ufw`：

```bash
sudo ufw default deny incoming
sudo ufw allow from 10.0.0.0/24 to any port 8089 proto tcp   # CubeMaster
sudo ufw allow from 10.0.0.0/24 to any port 9999 proto tcp   # Cubelet
sudo ufw allow from 10.0.0.0/24 to any port 3000 proto tcp   # CubeAPI
sudo ufw allow from 10.0.0.0/24 to any port 12088 proto tcp  # WebUI
sudo ufw allow 22/tcp     # SSH
sudo ufw allow 80/tcp     # cube-proxy（如需公网）
sudo ufw allow 443/tcp    # cube-proxy TLS
sudo ufw enable
```

### 两者结合（推荐）

为实现纵深防御，**同时绑定内网 IP 并设置防火墙规则**。即使某一层配置失误，另一层
仍能提供保护。

## 多机端口可达性要求

部署 compute 节点时，以下连通性必须保持开放。请确保你的绑定地址与防火墙规则
**不会**阻断这些路径：

| 方向 | 端口 | 服务 | 用途 |
|------|------|------|------|
| Compute → 控制节点 | 8089/tcp | CubeMaster | 沙箱元数据、节点注册 |
| 控制节点 → Compute | 9999/tcp | Cubelet gRPC | 沙箱生命周期操作 |

## 默认凭据

`env.example` 中的以下取值均为**示例值**——任何超出本地评估范围的部署都必须修改：

| 变量 | 风险 |
|------|------|
| `CUBE_SANDBOX_MYSQL_ROOT_PASSWORD` | 数据库完全访问权限 |
| `CUBE_SANDBOX_MYSQL_PASSWORD` | 应用数据库访问权限 |
| `CUBE_SANDBOX_REDIS_PASSWORD` | 缓存 / 会话访问权限 |
| `E2B_API_KEY`（默认 `e2b_000000`） | API 鉴权 |
| `DATABASE_URL` | 内含 MySQL 用户 / 密码 |

## 自定义鉴权（Auth Callback）

CubeAPI 默认**对所有请求放行、不做任何凭证校验**。对于任何超出本地评估范围的部署，
都应通过 **Auth Callback** 机制将鉴权决策委托给你自己的鉴权服务。启用后，除
`GET /health` 外的所有 API 请求都必须携带凭证，CubeAPI 将其转发到你的回调服务
进行放行/拒绝判定。

### 启用方式

在 `.env` 中设置回调地址，或通过 CLI 参数传入（CLI 参数优先级高于环境变量）：

```bash
# .env
AUTH_CALLBACK_URL=https://your-auth-service/verify

# 或 CLI 参数
./cube-api --auth-callback-url https://your-auth-service/verify
```

未设置 `AUTH_CALLBACK_URL` 时（默认），所有请求无需凭证即可通过。

### 工作原理

```
客户端 ──→ CubeAPI
               │
               ├─ 提取凭证（Authorization: Bearer / X-API-Key）
               ├─ 记录请求路径 + HTTP 方法
               │
               └─ POST → AUTH_CALLBACK_URL
                              │
                     200 ─────┤──→ 放行请求
                  非 200 ─────┘──→ 401 Unauthorized
```

CubeAPI 向回调地址转发的 header：

| Header | 说明 |
|--------|------|
| `Authorization` | `Bearer <token>` —— 客户端使用 Bearer 鉴权时透传 |
| `X-API-Key` | `<key>` —— 客户端使用 API Key 鉴权时透传 |
| `X-Request-Path` | 原始请求路径，如 `/templates/my-tmpl` |
| `X-Request-Method` | HTTP 方法，如 `GET`、`DELETE`、`PATCH` |

`Authorization` 与 `X-API-Key` 互斥——回调会收到客户端实际发送的那一个（Bearer
优先）。若两者都未携带，CubeAPI 直接返回 `401`，不会调用你的服务。

::: warning 必须同时校验路径和方法
同一路径上挂载了多个 HTTP 方法——如 `/templates/:id` 同时处理 GET（读取）、
POST（重建）、DELETE（删除）和 PATCH（更新）。仅按路径白名单授权无法阻止只读
凭证发起删除操作。

请在回调中**同时校验** `X-Request-Path` 和 `X-Request-Method`。
:::

::: warning 回调服务可用性
回调地址不可达时，CubeAPI 采取"失败即拒绝"策略（请求被拒绝）。请保证鉴权服务
具备足够的可用性并保持低延迟，因为每个 API 请求都会等待它返回。
:::

### SDK 侧配置

E2B SDK 会自动将 `E2B_API_KEY` 以 `Authorization: Bearer <key>` 的形式附加到请求中：

```bash
export E2B_API_KEY=your-actual-api-key
```

非 SDK 客户端直接发送 `X-API-Key: <key>` 即可。

::: tip 完整指南
如需回调服务的完整实现示例（Python/FastAPI）和错误响应参考，请查阅
[鉴权配置](/zh/guide/authentication)。
:::

## TLS

- CubeMaster 与 CubeAPI 不做 TLS 终止。如需对外 TLS，请将其置于 cube-proxy 或
  独立反向代理之后。
- Cubelet gRPC 为明文传输；请依靠网络层隔离（绑定地址或防火墙）加以保护。
- cube-proxy 默认通过 `mkcert` 生成的证书做 TLS 终止；面向公网部署时请替换为
  生产环境证书。

## 相关文档

- [网络策略](/zh/guide/network-policy) —— 沙箱出站 CIDR 放通/拒绝。
- [安全代理](/zh/guide/security-proxy) —— 对沙箱出站 HTTP/HTTPS 的 L7 规则。
- [限制公开访问](/zh/guide/restrict-public-access) —— 单沙箱入站令牌。
- [多机集群部署](/zh/guide/multi-node-deploy) —— 添加 compute 节点。
