---
title: WebUI 控制台
---

# WebUI 控制台

Cube Sandbox **Dashboard（控制台）** 是一个内置的网页界面，让你在浏览器里就能看清集群里跑着什么、管理沙箱、构建模板、检查节点健康——不用敲一行 CLI。

> ⏱ 大约 3 分钟读完。读完之后，你就能在笔记本上把控一个集群。

## 1. 在哪里打开？

Dashboard 是一个静态前端，由 **控制节点** 上的 nginx 容器托管。

| 部署方式 | 访问地址 | 说明 |
| --- | --- | --- |
| 一键部署 / 多机集群 | `http://<控制节点IP>:12088` | 默认端口，可通过 `WEB_UI_HOST_PORT` 修改 |
| 裸金属 / 物理机部署 | `http://<服务器IP>:12088` | 同样使用 12088 |
| 本地开发 | `http://localhost:5173` | Vite 开发服务器，自动代理 `/cubeapi` 到 `127.0.0.1:3000` |

::: tip 记住 12088，不是 3000
`3000` 端口是 E2B 兼容的 REST API（CubeAPI），`12088` 是给人用的 Dashboard。Dashboard 在内部会通过同源前缀 `/cubeapi/v1` 调用 CubeAPI，所以你只需要打开 `12088` 这一个端口。
:::

如果你不知道控制节点的 IP，可以在服务器上跑 `ip -4 addr`，或者在同网段下直接访问 `http://<主机名>:12088`。

## 2. 一眼看完侧边栏

所有功能都在左侧栏的 11 个图标后面。鼠标悬停会显示名字。

| # | 图标 | 页面 | 用途 |
| --- | --- | --- | --- |
| 1 | 📊 | **Overview（概览）** | 集群关键指标：运行中沙箱数、CPU/内存使用率、健康节点数 |
| 2 | 📦 | **Sandboxes（沙箱）** | 所有 micro-VM 的实时列表，支持暂停 / 恢复 / 销毁 |
| 3 | 🧩 | **Templates（模板）** | 可复用的沙箱快照目录，支持从 OCI 镜像创建新模板 |
| 4 | 🖥️ | **Nodes（节点）** | 集群健康：每台宿主机的 CPU、内存、可用槽位 |
| 5 | 🧬 | **Versions（版本矩阵）** | 跨节点的组件版本分布（内核、agent、guest 镜像） |
| 6 | 🌐 | **Network（网络）** | API 网关配置与每节点速率限制 |
| 7 | 📈 | **Observability（可观测性）** | 运行时状态、沙箱健康、模板构建总览 |
| 8 | 🔑 | **API Keys（API 密钥）** | 存储 Dashboard 请求使用的 `X-API-Key` |
| 9 | 🏪 | **Template Store（模板商店）** | 安装官方预置镜像，一键生成模板 |
| 10 | 🤖 | **AgentHub（智能体中心）** | 在 Cube Sandbox 上招募并管理 AI 智能体实例 |
| 11 | ⚙️ | **Settings（设置）** | 主题、语言、集群信息、键盘快捷键 |

::: tip 新用户？从 **Overview** 开始。
它把最重要的信息聚在同一屏上，并且会自动刷新。
:::

## 3. 头三件你一定会做的事

### 3.1 检查集群是否健康

打开 **Overview**（`/`）。你应该看到四张偏绿色的 KPI 卡片：

- **Running Sandboxes** — 当前活跃的 micro-VM 数量
- **CPU / Memory Utilization** — 整集群的压力
- **Healthy Nodes** — `N/M` 个节点处于 `Ready` 状态

如果哪项数字飘红，点进 **Nodes** 看是哪个宿主出了问题。

### 3.2 创建一个沙箱

1. 点左侧栏的 **Sandboxes**，再点右上角 **+ New sandbox**。
2. 在网格里挑一个模板。标记为 `STALE` 的不可用——选 `READY` 的。
3. （可选）填几对 `meta` 键值对作为标签。
4. 点 **Create**。几秒内你就会被跳转到该沙箱的详情页，能看到日志在实时滚动。

要停掉一个沙箱，去 **Sandboxes** 列表，找到对应行，点最右边的暂停 / 销毁按钮。

### 3.3 配置 API Key（仅在开启鉴权时需要）

如果你的部署开启了鉴权，Dashboard 必须在请求里带上 API Key，否则所有请求都会失败。

1. 点左侧栏的 **API Keys**。
2. 把 Key（形如 `sk-cube-…`）粘进输入框。
3. 点 **Save**。Key 会保存在浏览器的 `localStorage.cube.apiKey`，Dashboard 之后每次请求都会自动带上 `X-API-Key` 请求头。

::: details 这个 Key 从哪来？
开启鉴权的人会生成它。完整流程见 [鉴权](./authentication.md)。
:::

## 4. Web 终端

Dashboard 内置了 **交互式 Web 终端**，你可以直接从浏览器进入运行中沙箱的 Shell——不需要 SDK，也不需要 SSH。

### 4.1 打开终端

有两个入口，只有在沙箱处于 **运行中（running）** 状态时可用（鼠标悬停按钮会提示原因）：

- **沙箱详情页** — 页头的 **Open Terminal** 按钮。
- **Sandboxes 列表** — 行操作里的终端图标。

<!-- TODO: screenshot: terminal dialog -->
![Web 终端弹窗](../assets/webui-terminal.png)

### 4.2 终端能力

弹窗是一个完整的 [xterm.js](https://xtermjs.org/) 终端，在沙箱内以 **root** 身份运行 `/bin/bash` 登录 Shell：

- ANSI 颜色与光标控制（vim、htop 等都能正常用）
- 复制 / 粘贴——`Ctrl+Shift+V` 或右键粘贴；选中文字即原生复制
- 滚动回退、窗口大小同步、全屏切换、字体大小 `+`/`-` 调节

### 4.3 会话管理、重连与空闲超时

- **多会话** — 每次打开终端都会启动独立 Shell；不同沙箱的会话可以并存。
- **单沙箱会话上限** — 每个沙箱最多同时容纳 8 个终端会话，超出的连接会被拒绝。可通过 CubeAPI 环境变量 `TERMINAL_MAX_SESSIONS_PER_SANDBOX` 调整（默认 `8`）。此外还有跨所有沙箱的全局上限——`TERMINAL_MAX_SESSIONS_GLOBAL`（默认 `128`）。
- **重连** — 异常断开、Shell 退出或出错时，弹窗会显示明确状态并提供 **Reconnect（重连）** 按钮。
- **空闲超时** — 30 分钟既没有任何输入也没有任何 Shell 输出的会话会被服务端终止；任何活动都会重置计时器，所以 `tail -f`、长时间构建等持续输出会话不会因没敲键盘而被断开。可通过 CubeAPI 环境变量 `TERMINAL_IDLE_TIMEOUT_SECS` 调整（单位秒，默认 `1800`）。
- **传输限制** — 客户端 WebSocket 消息上限为 64 KiB；服务端写入带有 10 秒超时。

### 4.4 多容器沙箱

当一个沙箱运行多个容器时，弹窗标题栏会显示**容器选择器**，列出该沙箱上报的全部容器（默认选中主容器）。切换容器会将会话重连到该容器自己的环境中——独立的文件系统、进程空间与环境变量。

工作原理：每个基于 cube-base 镜像创建的容器都会启动自己的 `envd`，监听端口 `49983 + <容器索引>`（主容器保持 `49983`）。Cubelet 把端口记录在容器 label `cube.envd-port` 中，CubeMaster 在沙箱信息 API 上以 `envd_port` 透出，CubeAPI 据此把终端 WebSocket 路由到所选容器的端口。镜像显式设置了 `ENVD_PORT` 的容器按其设置值生效。非浏览器客户端可在终端 WebSocket URL 上加 `container` 查询参数（容器 ID 或名称）获得同样行为。

注意事项：

- 只有运行 `envd` 的容器（基于 `cube-base` 的镜像）可以通过终端登录；选择其他容器会以连接错误失败。
- 在多容器终端支持上线之前创建的沙箱只暴露主容器——选择其他容器会被拒绝并提示"无终端端点"。
- 多节点部署中，**跨宿主机**访问非主容器的 envd 端口需要在创建沙箱时暴露该端口（与暴露 `49983` 同一机制）；同宿主机路由开箱即用。

### 4.5 工作原理

浏览器 xterm.js ⇄ WSS ⇄ CubeAPI（`GET /cubeapi/v1/sandboxes/{sandboxID}/terminal/ws`）⇄ CubeProxy ⇄ `envd`（主容器端口 `49983`，其余容器 `49983 + 索引`），由 envd 在沙箱内托管 PTY。部署启用 TLS 时，传输链路复用同一套 HTTPS/WSS 加密。Shell 只是 **所选容器内的 root**——与 SDK `exec` 的权限边界一致——并不能借此访问宿主机。

### 4.6 鉴权与审计

- **鉴权模式** — CubeAPI 支持两种鉴权模式（外加完全开放），其统一鉴权中间件保护所有路由，终端也不例外。由于浏览器无法在 WebSocket 握手上设置请求头，终端端点会自行校验凭证，逻辑与中间件一致。详见 [鉴权](./authentication.md)。
  - **callback 模式**（设置了 `AUTH_CALLBACK_URL`）：CubeAPI 会把凭证连同 `X-Request-Path`、`X-Request-Method` 一起转发给回调地址，回调返回 HTTP 200 即放行。能够校验 WebUI 登录 JWT 的回调对终端同样生效。**随附部署以可选方式提供这条链路：** 把 `AUTH_CALLBACK_URL` 指向 CubeOps 内置的验证端点即可（`POST /api/v1/auth/verify`——one-click 中为 `http://127.0.0.1:3010/api/v1/auth/verify`；Helm 中设置 `controlPlane.api.authCallbackUrl: "auto"`），它校验 WebUI 登录 JWT 并通过 `X-Auth-User` 返回验签后的用户名——见[鉴权](./authentication.md#回调响应操作人身份可选)。注意终端挂载在 `/cubeapi/v1` 前缀下（`GET /cubeapi/v1/sandboxes/{sandboxID}/terminal/ws`）——按 path 白名单放行的回调需要放行该前缀。
  - **simple-key 模式**（设置了 `CUBE_API_KEY` 且未设置回调）：凭证必须与 `CUBE_API_KEY` 字符串相等。**已知限制：** 浏览器端持有的是 CubeOps JWT，与 `CUBE_API_KEY` 并不相等，因此该模式下 Web 终端无法通过鉴权、不可用——如需在鉴权下使用终端，请改用 callback 模式（或不开启鉴权）。
  - **未开启鉴权（开放模式）**：终端**默认拒绝（fail closed）**——未配置任何鉴权后端时，终端握手默认以 `403` 拒绝，因为无鉴权的终端等于所有沙箱的 root shell。**这正是 one-click/Helm 的开箱状态：** 它们默认不给 CubeAPI 配置鉴权，因此终端不可用，直到你接好上面的 CubeOps 验证端点，或为开发场景显式设置 `TERMINAL_ALLOW_UNAUTHENTICATED=true` 放行无鉴权终端——此后任何能访问 Dashboard 的人都能对任意运行中的沙箱打开终端，请据此控制 Dashboard 的访问范围。
  - 在所有模式下，会话凭证都通过 `Sec-WebSocket-Protocol` 子协议头（`cube-terminal.<token>`）传输，而不是 URL——令牌不会出现在 URL、服务端访问日志或浏览器历史记录中。非浏览器客户端应改用标准的 `Authorization: Bearer <token>` 请求头。`token` 查询参数仍为 CLI/curl 保留但**默认禁用**（前置代理可能记录完整 URL）；确需使用时显式设置 `TERMINAL_TOKEN_QUERY_PARAM=true`。CubeAPI 自身的请求日志会对终端路由剔除查询字符串。
- **Origin 校验** — CubeAPI 会拒绝 `Origin` 与请求主机不匹配的 WebSocket 升级请求，跨源的浏览器连接将收到 `403`。scheme、主机名与**有效端口**必须一致：`Origin` 不带显式端口时按 scheme 默认端口（80/443）处理，因此 `Origin: http://example.com` 不再匹配 `Host: example.com:3000`；`Host` 不带端口时只匹配不带端口的 `Origin`。如果在**非默认端口**上用反向代理前置 CubeAPI，请转发完整的 authority 以保留端口（nginx 用 `proxy_set_header Host $http_host;`）；`proxy_set_header Host $host;` 会丢掉端口，导致非默认端口的 Origin 被拒绝。多源部署可设置 `TERMINAL_ALLOWED_ORIGINS`（逗号分隔的精确 Origin 列表，如 `https://cube.example.com,https://admin.example.com:8443`）——设置后 `Origin` 必须等于其中一项，不再走 Host 匹配规则。不发送 `Origin` 头的客户端（curl、CLI）不受影响。
- **反向代理要求** — 终端是长连接 WebSocket，前置代理必须使用 HTTP/1.1 并转发 `Upgrade`/`Connection` 头（`proxy_http_version 1.1; proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection $connection_upgrade;`），且 `proxy_read_timeout` 必须大于服务端空闲超时——随附的 nginx 配置（one-click 与 Helm）按默认 1800 秒空闲超时设置为 `7206s`。两套随附配置都会把 `/cubeapi/v1/sandboxes/*/terminal/ws` 直接代理到 CubeAPI；其余 `/cubeapi/v1/*` SDK 请求仍走 CubeOps（CubeOps 没有终端路由）。
- **可调参数（CubeAPI 环境变量）** — `SANDBOX_PROXY_URL`：CubeAPI 经 CubeProxy 连接沙箱内 envd 时使用的 CubeProxy 基础地址（默认 `http://127.0.0.1`，在 CubeProxy 共享宿主机网络的 one-click 部署中无需修改；Helm chart 会自动将其设置为 CubeProxy Service 地址）。`TERMINAL_IDLE_TIMEOUT_SECS` 与 `TERMINAL_MAX_SESSIONS_PER_SANDBOX` 见 §4.3；`TERMINAL_MAX_SESSIONS_GLOBAL` 限制所有沙箱的并发终端会话总数（默认 `128`，超出后以 `429` 拒绝）。§4.6 的安全开关：`TERMINAL_ALLOW_UNAUTHENTICATED`（默认 `false`）、`TERMINAL_TOKEN_QUERY_PARAM`（默认 `false`）、`TERMINAL_ALLOWED_ORIGINS`（默认空）。
- **审计** — CubeAPI 会记录会话打开 / 关闭 / 超时事件，包含时间戳、操作人身份、客户端 IP、沙箱 ID、目标容器（选择了非默认容器时）和 Shell PID。被拒绝的尝试（令牌错误、Origin 不匹配、沙箱不存在或未运行、超出会话上限）同样会被审计，并记录原因和客户端 IP。身份按可信级别拆分：权威的 `user` 只来自鉴权回调的 `X-Auth-User` 响应头（`identity_source=auth_callback`）；缺失时，已授权 Bearer JWT 的 `username` / `sub` claim 单独记入 `claimed_user`（`identity_source=unverified_jwt_claim`）——这是调用者自称的参考值，不是已验证身份。simple-key 与未开启鉴权的部署中两个字段均为空——见[鉴权](./authentication.md#回调响应操作人身份可选)。随附 nginx 配置在终端路由上用 `$remote_addr` 覆盖 `X-Forwarded-For`，审计的客户端 IP 无法通过自带转发头伪造。

### 4.7 已知限制

- simple-key 鉴权模式下（设置了 `CUBE_API_KEY` 而未设置 `AUTH_CALLBACK_URL`）Web 终端不可用——浏览器端的 CubeOps JWT 永远不会等于 `CUBE_API_KEY`（见 §4.6）。
- 创建时限制了公网访问的沙箱（`allowPublicTraffic=false` / 流量访问令牌）无法使用 Web 终端——流量令牌在创建后不可恢复，弹窗会显示连接错误。
- 终端访问只做用户身份认证，**不做**按沙箱的授权——任何已认证用户都能对任意沙箱打开终端。这与当前沙箱 API 的权限模型一致（尚无按用户的沙箱归属），已纳入未来的多租户工作。
- 沙箱镜像必须包含 `envd`（所有标准模板都已包含）。
- 多容器沙箱：只有基于 `envd` 的容器可被选择，且存量沙箱只暴露主容器——见 §4.4。

## 5. 键盘快捷键

Dashboard 对键盘很友好。最常用的三个：

| 按键 | 作用 |
| --- | --- |
| `⌘ K` / `Ctrl K` | 打开 **Command Palette**——输入页面名直接跳转 |
| `?` | 打开 **Settings → Shortcuts**（应用内查看完整快捷键列表） |
| `R` | 刷新所有可见数据面板 |
| `Esc` | 关闭弹窗或 Command Palette |

## 6. 个性化

打开左侧栏的 **Settings**：

- **Appearance → Theme** — 浅色 / 深色 / 跟随系统
- **Appearance → Language** — English 或 简体中文
- **Cluster** — 只读展示 CubeAPI 端点、沙箱域名、默认实例类型、速率限制、鉴权开关

顶栏右上角和 ⌘K 输入框里也有同样的快捷开关。

## 7. 常见问题

**为什么还要单独做个 Dashboard，不能直接用 curl 吗？**
绝大多数操作（从镜像创建模板、看版本矩阵、排查节点）在 UI 里更容易发现和理解。Dashboard 本质上只是 CubeAPI 的一个轻量客户端——每个页面背后都是一次 `/cubeapi/v1/*` 请求，这跟 E2B SDK、`curl` 调的是同一个 E2B 兼容 REST API。

**Dashboard 会保存我的数据吗？**
只会在浏览器里保存一样东西：`localStorage.cube.apiKey` 里的 API Key。其他所有状态（模板、沙箱、日志）都在集群上。

**能改端口吗？**
可以——在 `.env` 里设 `WEB_UI_HOST_PORT`，然后重跑 `install.sh`。改动会在下次启动 `cube-sandbox-webui.service` 时生效。

**能关掉 Dashboard 吗？**
可以——把 `.env` 里的 `WEB_UI_ENABLE` 设成 `0`（或不设）。集群照常运行，只是不再提供 WebUI；`3000` 端口的 E2B 兼容 API 不受影响。

**Dashboard 是开源的吗？我能自己构建吗？**
可以——它在仓库的 `web/` 目录里，用 Vite + React + TypeScript + Tailwind 构建。详见 [本地构建部署](./self-build-deploy.md) 和 [`web/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/web/README.md)。

## 8. 下一步

- [快速开始](./quickstart.md) — 如果你还没安装，几分钟到能跑的 Dashboard
- [服务管理与日志](./service-management.md) — 如何启停 / 重启 `cube-sandbox-webui.service` 容器
- [鉴权](./authentication.md) — 还没开启 API Key？这里有完整步骤
- [HTTPS 证书与域名解析](./https-and-domain.md) — 给 Dashboard 加 TLS
- [架构概览](../architecture/overview.md) — 了解 Dashboard 背后的 CubeAPI / CubeMaster / Cubelet 怎么协作
