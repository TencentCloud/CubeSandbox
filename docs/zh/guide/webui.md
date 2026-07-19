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

### 3.3 登录进运行中的沙箱终端

有时你需要进到实例里开一个真正的 shell——调试、装依赖，或者跑个临时命令。
Dashboard 内置了 **Web 终端**，不用再自己拼 CLI 或 SDK 调用。

1. 打开某个沙箱的详情页（**Sandboxes → 点击某一行**）。
2. 点右上角操作栏的 **Terminal** 按钮。该按钮仅在沙箱**运行中**时可用；
   实例暂停 / 停止时按钮禁用并带有提示。
3. 弹出交互式终端面板，里面是一个实时 shell。它是完整的 xterm.js 终端：
   颜色、光标控制、`top`/`vim`、复制粘贴、滚动回溯都能用，视图会随面板
   大小自适应（拖动窗口或点全屏按钮）。

说明：

- **会话生命周期。** 面板打开期间 shell 持续运行。空闲（无输入）会话在服务端
  超时后自动关闭（`CUBE_API_TERMINAL_IDLE_TIMEOUT_SECS`，默认 15 分钟）；关闭
  面板或浏览器标签页会结束会话并回收 shell。连接断开时，面板会显示状态并提供
  **Reconnect（重新连接）** 按钮。
- **多开会话。** 可以同时对同一个或不同沙箱开多个终端，互不干扰。
- **权限。** 终端登录走的是和 Dashboard 其余部分一样的鉴权（WebUI 会话和/或
  API Key）。终端里的命令拥有的权限就是沙箱本身的权限——它不是绕过实例网络 /
  出网策略的通道。每次登录和断开都会写入 CubeAPI 审计日志
  （`terminal.session.started` / `terminal.session.closed`，包含操作人、目标实例
  和时间）。
- **实现原理。** 面板通过 WebSocket 连到
  `/cubeapi/v1/sandboxes/<id>/terminal`，后端复用 SDK 同款加密代理桥接到沙箱既
  有的 envd PTY——不引入新的执行链路。

### 3.4 配置 API Key（仅在开启鉴权时需要）

如果你的部署开启了鉴权，Dashboard 必须在请求里带上 API Key，否则所有请求都会失败。

1. 点左侧栏的 **API Keys**。
2. 把 Key（形如 `sk-cube-…`）粘进输入框。
3. 点 **Save**。Key 会保存在浏览器的 `localStorage.cube.apiKey`，Dashboard 之后每次请求都会自动带上 `X-API-Key` 请求头。

::: details 这个 Key 从哪来？
开启鉴权的人会生成它。完整流程见 [鉴权](./authentication.md)。
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
只会在浏览器里保存一样东西：`localStorage.cube.apiKey` 里的 API Key。其他所有状态（模板、沙箱、日志）都在集群上。

**能改端口吗？**
可以——在 `.env` 里设 `WEB_UI_HOST_PORT`，然后重跑 `install.sh`。改动会在下次启动 `cube-sandbox-webui.service` 时生效。

**能关掉 Dashboard 吗？**
可以——把 `.env` 里的 `WEB_UI_ENABLE` 设成 `0`（或不设）。集群照常运行，只是不再提供 WebUI；`3000` 端口的 E2B 兼容 API 不受影响。

**Dashboard 是开源的吗？我能自己构建吗？**
可以——它在仓库的 `web/` 目录里，用 Vite + React + TypeScript + Tailwind 构建。详见 [本地构建部署](./self-build-deploy.md) 和 [`web/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/web/README.md)。

## 7. 下一步

- [快速开始](./quickstart.md) — 如果你还没安装，几分钟到能跑的 Dashboard
- [服务管理与日志](./service-management.md) — 如何启停 / 重启 `cube-sandbox-webui.service` 容器
- [鉴权](./authentication.md) — 还没开启 API Key？这里有完整步骤
- [HTTPS 证书与域名解析](./https-and-domain.md) — 给 Dashboard 加 TLS
- [架构概览](../architecture/overview.md) — 了解 Dashboard 背后的 CubeAPI / CubeMaster / Cubelet 怎么协作
