# Web 终端

Cube Sandbox WebUI 为运行中的沙箱容器提供交互式终端，您可以直接在浏览器中打开 Shell 会话。

## 快速开始

1. 在侧边栏导航到 **沙箱** 页面
2. 找到正在运行的沙箱，点击 **终端** 图标（或从沙箱详情页点击 **打开终端**）
3. 终端面板将打开，并在默认容器中启动 `/bin/bash -l` Shell
4. 正常输入命令 — 终端支持 ANSI 颜色、滚动、复制粘贴和窗口自适应

## 功能特性

- **完整交互式 Shell**：基于 PTY 的 `/bin/bash -l`，支持 `top`、`vim`、`htop` 等终端工具
- **多容器支持**：如果沙箱包含多个容器，可通过工具栏下拉菜单选择要连接的容器
- **ANSI 颜色与光标控制**：基于 xterm.js 的完整终端模拟，支持 256 色
- **窗口自适应**：拖拽终端面板或切换全屏 — PTY 尺寸自动同步
- **滚动回溯**：5000 行滚动缓冲区；支持鼠标滚轮和触控板滚动
- **复制粘贴**：选中文本后使用 `Ctrl+Shift+C` 复制，`Ctrl+Shift+V` 粘贴
- **字号调节**：通过工具栏 `+`/`-` 按钮调整（10px–24px）
- **自动重连**：网络瞬断时自动重试（最多 3 次，指数退避）

## 快捷键

| 快捷键 | 功能 |
|--------|------|
| `Ctrl+Shift+C` | 复制选中文本 |
| `Ctrl+Shift+V` | 粘贴剪贴板内容 |
| `Ctrl+L` | 清屏 |

## 权限要求

当平台配置了认证回调时，终端访问需要认证。终端使用与 WebUI 相同的认证模型 — 登录后的会话令牌通过 WebSocket 升级请求的查询参数传递。

终端访问仅对 **运行中** 的沙箱可用。已暂停或已删除的沙箱无法连接。

## 空闲超时

终端会话在 30 分钟无活动（无键盘输入）后自动关闭。可通过 CubeAPI 的 `TERMINAL_IDLE_TIMEOUT_SECS` 环境变量配置。

```bash
# 设置 1 小时空闲超时
TERMINAL_IDLE_TIMEOUT_SECS=3600
```

## 已知限制

- **无会话持久化**：关闭浏览器标签页会终止终端会话，容器内的 Shell 进程也会终止。
- **暂停不可恢复**：暂停沙箱会关闭所有活跃终端会话。恢复沙箱后需重新打开终端。
- **每终端单 Shell**：每个终端面板启动一个 Shell 进程。如需同一沙箱内多个并发 Shell，请打开多个终端面板。
- **envd 依赖**：终端功能依赖沙箱内运行的 envd 守护进程（端口 49983）。所有 Cube Sandbox 模板默认包含 envd。

## 架构

```
浏览器 (xterm.js)
  │  wss://<host>/cubeapi/v1/sandboxes/{id}/terminal?token=...
  ▼
CubeAPI (WebSocket 升级 + 代理)
  │  HTTP POST → CubeProxy → envd process.Process/Connect
  ▼
envd (沙箱 VM 内)
  │  PTY → /bin/bash -l
  ▼
容器 Shell
```

## 审计日志

每次终端会话都会生成结构化审计日志：

- **会话开始**：记录 `event_type=terminal_session_start`，包含沙箱 ID、容器名称、用户和会话 ID
- **会话结束**：记录 `event_type=terminal_session_end`，包含关闭原因（`client_disconnect`、`idle_timeout`、`sandbox_destroyed` 等）和会话时长

终端 I/O 内容（stdin、stdout、stderr）**不会被记录**。仅记录会话元数据。