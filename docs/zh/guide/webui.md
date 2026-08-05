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

### 3.3 打开 Web 终端

运行中的沙箱会在 **Sandboxes** 表格中显示终端图标，详情页也会显示 **终端** 按钮。点击后即可打开交互式登录 Shell，不需要在沙箱内安装 SSH 服务，也不需要额外暴露端口。

- 会话以 root 用户在 `/root` 目录启动 `/bin/bash -i -l`，并支持动态调整 PTY 大小。
- 输入和输出使用 WebSocket 二进制帧传输，UTF-8 文本和全屏终端程序都能正常工作。
- 关闭弹窗会终止 Shell；点击 **重新连接** 会创建一个新 Shell，不会重新挂接旧会话。
- 沙箱暂停时不会显示终端入口，请先恢复沙箱。

::: details 鉴权与安全边界
浏览器只使用访问 JWT 请求一个 30 秒有效的终端票据。票据绑定单个用户和单个沙箱、只能使用一次，并通过 `Sec-WebSocket-Protocol` 传递，不会出现在 URL 中。CubeOps 还会校验 WebSocket 同源请求，将客户端帧限制为 256 KiB，限制每个用户和沙箱最多 4 个会话，打开阶段最多等待 15 秒，并在空闲 30 分钟后自动关闭会话。
:::

自定义部署时，CubeOps 必须能够通过 HTTP 访问 CubeProxy：

```bash
CUBE_API_SANDBOX_DOMAIN=cube.app
CUBE_SANDBOX_PROXY_URL=http://127.0.0.1
```

Dashboard 的反向代理必须在 `/opsapi/` 上转发 `Upgrade` 和 `Connection` 请求头。一键部署 nginx 和 Kubernetes Helm Chart 已包含这项配置。

连接失败时可以按下面的顺序排查：

| 现象 | 检查项 |
| --- | --- |
| 返回 `409 sandbox must be running` | 恢复沙箱后重试。 |
| 返回 `502` 或“无法打开沙箱终端” | 检查 `CUBE_SANDBOX_PROXY_URL`、CubeProxy 健康状态、沙箱泛域名解析，以及 envd 的 `49983` 端口。 |
| 票据创建成功，但 WebSocket 立即断开 | 确认 `/opsapi/` 转发了 WebSocket 升级请求头，且读取超时大于会话时长。 |

### 3.4 登录（JWT 鉴权）

Dashboard 从 v0.6.0 起使用 JWT 鉴权。首次访问时会自动跳转到登录页。

1. 输入账号和密码。一键部署的默认账号是 `admin` / `admin`，生产环境必须立即在设置页修改密码。
2. 登录成功后会获得短期访问令牌和 7 天有效的刷新令牌，后续请求使用 `Authorization: Bearer <jwt>`。
3. `/opsapi/*` 管理接口由 Dashboard 同源代理转发；不要将 CubeOps 的 `3010` 端口直接暴露到公网。

::: details 令牌生命周期
- **访问令牌**：默认 15 分钟，类型为 `access`，audience 为 `cubeops:access`。
- **刷新令牌**：默认 7 天，类型为 `refresh`，audience 为 `cubeops:refresh`，不能当作访问令牌使用。
- 登录接口会按 IP 限制失败尝试次数。
:::

## 4. 键盘快捷键

Dashboard 对键盘很友好。最常用的三个：

| 按键 | 作用 |
| --- | --- |
| `⌘ K` / `Ctrl K` | 打开 **Command Palette**——输入页面名直接跳转 |
| `?` | 打开 **Settings → Shortcuts**（应用内查看完整快捷键列表） |
| `R` | 刷新所有可见数据面板 |
| `Esc` | 关闭弹窗或 Command Palette |

## 5. 个性化

打开左侧栏的 **Settings**：

- **Appearance → Theme** — 浅色 / 深色 / 跟随系统
- **Appearance → Language** — English 或 简体中文
- **Cluster** — 只读展示 CubeAPI 端点、沙箱域名、默认实例类型、速率限制、鉴权开关

顶栏右上角和 ⌘K 输入框里也有同样的快捷开关。

## 6. 常见问题

**为什么还要单独做个 Dashboard，不能直接用 curl 吗？**
绝大多数操作（从镜像创建模板、看版本矩阵、排查节点）在 UI 里更容易发现和理解。Dashboard 本质上只是 CubeAPI 的一个轻量客户端——每个页面背后都是一次 `/cubeapi/v1/*` 请求，这跟 E2B SDK、`curl` 调的是同一个 E2B 兼容 REST API。

**Dashboard 会保存我的数据吗？**
浏览器的 `localStorage` 中只保存 JWT 访问令牌和刷新令牌；终端票据寿命很短，只保存在内存中。模板、沙箱和日志等集群状态仍保存在服务端。

**能改端口吗？**
可以——在 `.env` 里设 `WEB_UI_HOST_PORT`，然后重跑 `install.sh`。改动会在下次启动 `cube-sandbox-webui.service` 时生效。

**能关掉 Dashboard 吗？**
可以——把 `.env` 里的 `WEB_UI_ENABLE` 设成 `0`（或不设）。集群照常运行，只是不再提供 WebUI；`3000` 端口的 E2B 兼容 API 不受影响。

**Dashboard 是开源的吗？我能自己构建吗？**
可以——它在仓库的 `web/` 目录里，用 Vite + React + TypeScript + Tailwind 构建。详见 [本地构建部署](./self-build-deploy.md) 和 [`web/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/web/README.md)。

## 7. 下一步

- [快速开始](./quickstart.md) — 如果你还没安装，几分钟到能跑的 Dashboard
- [服务管理与日志](./service-management.md) — 如何启停 / 重启 `cube-sandbox-webui.service` 容器
- [鉴权](./authentication.md) — 了解 JWT 登录、令牌生命周期与密码管理
- [HTTPS 证书与域名解析](./https-and-domain.md) — 给 Dashboard 加 TLS
- [架构概览](../architecture/overview.md) — 了解 Dashboard 背后的 CubeAPI / CubeMaster / Cubelet 怎么协作
