---
title: Web 终端
---

# Web 终端

Dashboard 可以为运行中的沙箱打开交互式 TTY，无需暴露容器运行时或节点端口。

这是一条仅用于运维控制台的链路：Dashboard 向 CubeOps 认证，CubeOps 校验目标并将 WebSocket 转发到 CubeMaster；CubeAPI 不参与该功能。

## 使用方法

1. 打开 **Sandboxes**，进入一个运行中沙箱的详情页。
2. 点击 **打开终端**。
3. 如果沙箱包含多个容器，先选择目标容器。
4. 点击 **连接**。终端支持 ANSI 输出、光标控制、回滚、复制粘贴、窗口缩放、全屏、断开和重新连接。

暂停、删除或停止沙箱后不能建立新会话。关闭终端面板会主动断开 WebSocket 并清理 exec 进程。连续 30 分钟没有终端输入、输出或窗口缩放时，会话会自动关闭；keepalive 不会延长空闲期限。

## 部署配置

一键部署会在 `.one-click.env` 中自动生成 `CUBE_TERMINAL_GATEWAY_TOKEN`；Helm Chart 会创建共享 Secret，并注入 CubeOps 和 CubeMaster。手动部署时，请生成至少 32 字节的随机值，并在两个服务中配置相同的值：

```bash
CUBE_TERMINAL_GATEWAY_TOKEN=<随机密钥>
```

如果 Dashboard 与 CubeOps 不同源，需要配置逗号分隔的精确来源白名单：

```bash
CUBE_TERMINAL_ALLOWED_ORIGINS=https://dashboard.example.com
```

生产环境必须启用 TLS，使浏览器使用 `wss://`。不要将 CubeMaster 的内部终端接口暴露到公网；该接口仅供控制平面使用，并要求共享网关凭据。

Cubelet 默认在 `/data/cubelet/fifo` 下创建 containerd 终端 FIFO。如果部署中该目录不可写，请设置 `CUBELET_TERMINAL_FIFO_DIR`。

终端 grant 保存在 CubeOps 内存中。Chart 默认只有一个 CubeOps 副本；如果水平扩容 CubeOps，需要通过粘性路由等方式，确保创建会话请求和随后 WebSocket 升级落在同一副本。

## 安全与运维

创建会话的请求必须已认证。服务端会确认目标容器属于该沙箱，然后签发 60 秒有效、只能使用一次的 grant。WebSocket 握手同时要求 grant、HttpOnly/SameSite 绑定 Cookie、`cube-terminal.v1` 子协议以及允许的 `Origin`。grant 不会出现在 URL 或日志中。

CubeOps 会限制每个用户、每个沙箱和全局的待用 grant 与活动会话数。结构化日志会记录 grant 签发、握手拒绝、会话打开和关闭，并包含会话、用户、沙箱和容器标识。不要记录终端输入输出，其中可能包含敏感信息。

## 故障排查

- **“打开终端”不可点击：** 沙箱不是运行状态。
- **WebSocket 升级返回 401/403：** 检查浏览器来源、登录状态、代理 Cookie 转发和 HTTPS 配置。
- **创建会话返回 503：** 检查 CubeOps 与 CubeMaster 是否使用相同的 `CUBE_TERMINAL_GATEWAY_TOKEN`。
- **连接后立即断开：** 检查 CubeMaster 到 Cubelet 的网络连接，并确认目标容器仍在运行。
