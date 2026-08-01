---
title: Web 终端
---

# Web 终端

**Web 终端**让你直接在控制台里打开运行中沙箱的交互式 Shell——不需要 SSH，不需要 SDK，也不需要命令行。沙箱出问题时，这是最快的排查入口。

> ⏱ 阅读约 4 分钟。读完你就能在浏览器里调试任何运行中的沙箱。

## 1. 从哪里打开

有两个入口，都只在沙箱状态为 `running` 时可用：

| 位置 | 点哪里 |
| --- | --- |
| **沙箱**列表（`/sandboxes`） | 行操作列里的终端图标 |
| **沙箱详情**（`/sandboxes/<id>`） | 页头的 **终端** 按钮 |

弹出的对话框里是一个完整的 `xterm.js` 终端，在沙箱内以 `root` 身份运行 `/bin/bash -i -l`。颜色、方向键、全屏程序（`top`、`vim`、`htop`）都能正常工作，拖动对话框大小会同步调整 Shell 的窗口尺寸。

::: tip 沙箱必须处于运行中
已暂停、暂停中或已终止的沙箱里没有可用的 envd，因此入口按钮会置灰，后端也会以 `409 Conflict` 拒绝请求。请先恢复沙箱。
:::

## 2. 底层原理

终端复用了**已有的 envd PTY 数据面**——和 Python/Node/Go SDK 用的是同一条链路——而不是在集群里新增任何 RPC：

```
浏览器（xterm.js）
   │  wss://<控制台>/opsapi/v1/terminal/ws?ticket=<一次性票据>
   ▼
CubeOps :3010            ← JWT 鉴权、票据签发、会话注册表、审计日志
   │  Connect-JSON over HTTP（流式 Start/Connect + unary SendInput/Update/SendSignal）
   ▼
CubeProxy ──► 沙箱内 envd :49983（process.Process PTY API）
```

一共涉及两个请求：

1. `POST /opsapi/v1/sdk/sandboxes/<id>/terminal`——用你的登录 JWT 鉴权。CubeOps 校验沙箱存在且处于运行中，然后返回一个有效期 30 秒的**一次性票据**。
2. `GET /opsapi/v1/terminal/ws?ticket=…`——WebSocket 升级请求，票据在首次使用时即作废。

之所以要用票据，是因为浏览器无法给 WebSocket 握手附加 `Authorization` 头。把登录 JWT 放进 query string 会让一个长期有效的凭证泄漏到访问日志和浏览器历史里；而 30 秒、单次使用、独立作用域的票据既无法重放，也对其他任何接口无效。

## 3. 会话、空闲超时与断线重连

CubeOps 维护一张活跃终端会话表：

- **同一沙箱可开多个终端**。每个终端有独立的 PTY 和 PID，互不共享回滚缓冲和历史记录。
- **关闭对话框会杀掉 Shell**。前端发送显式关闭帧，CubeOps 随即向 PTY 发送 `SIGKILL`。
- **断线则不会**。网络抖动或合上笔记本时，Shell 仍在运行，对话框会给出**重新连接**按钮，通过 envd 的 `Connect` RPC 重新挂载到同一个 PID 上——包括前台正在跑的程序，会话原样还在。
- **空闲会话会被回收**。30 分钟无键盘输入后，CubeOps 会关闭 WebSocket 并杀掉 PTY。可通过 `ops.yaml` 的 `terminal_idle_timeout` 或环境变量 `CUBE_OPS_TERMINAL_IDLE_TIMEOUT` 配置（Go duration 语法，如 `15m`）。

::: warning 开着终端并不能阻止沙箱被暂停
键盘输入会经 CubeProxy 刷新沙箱的 `last_active` 时间戳；但仅仅**开着**终端并不产生这类流量，`cube-lifecycle-manager` 仍可能在下面把沙箱自动暂停。发生时终端会报告流已中断。
:::

## 4. 审计

每次会话的开始与结束都会以结构化日志写入 CubeOps 日志（默认 `/data/log/CubeOps/cubeops-req.log`）：

```
terminal_session_start sessionID=<uuid> sandboxID=<id> username=<user> clientIP=<ip> pid=<pid> reconnect=false
terminal_session_end   sessionID=<uuid> sandboxID=<id> username=<user> clientIP=<ip> pid=<pid> reason=client_close durationMs=48213
```

`reason` 取值：`client_close`（用户关闭终端）、`pty_exit`（Shell 自行退出）、`detached`（连接丢失，PTY 保留待重连）、`idle_timeout`（被空闲清扫回收）、`stream_error`、`reconnect_failed`。

grep `terminal_session_` 即可还原谁、从哪个 IP、在哪个沙箱里开了多久的 Shell。

## 5. 部署要求

WebSocket 需要一条转发 `Upgrade` 头且不会在 5 分钟后超时的 nginx location。一键部署已默认带上：

```nginx
location = /opsapi/v1/terminal/ws {
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 7206s;
    proxy_send_timeout 7206s;
    proxy_buffering off;

    rewrite ^/opsapi/(.*)$ /api/$1 break;
    proxy_pass http://cube-ops:3010;
}
```

如果你在控制台前面自建了反向代理，需要照搬这条 location。缺少它会导致握手返回 `400 Bad Request`，或者终端在 `proxy_read_timeout` 之后无声断开。

本地开发时 `npm run dev` 已经代理了升级请求（`vite.config.ts` 中 `/opsapi` 条目的 `ws: true`）。

## 6. 已知限制

- **一个 PTY 一个 Shell，一次请求一个 PTY**。一个沙箱就是一台运行单个 envd 的微虚拟机，因此没有"选择容器"的下拉框——Cube Sandbox 里不存在这个概念。
- **鉴权粒度较粗**。任何已登录的控制台用户都能对任意沙箱打开终端，且在沙箱内是 `root`。目前没有按沙箱划分归属的模型。请把控制台登录视同为对集群内所有沙箱的 root 权限，并据此收紧访问（参见[鉴权](./authentication.md)）。
- **回滚缓冲只在浏览器里**。上限 5000 行，关闭对话框即丢失。需要留存的内容请用[沙箱日志](./sandbox-logs.md)。
- **不支持文件上传下载**。请使用 SDK 的文件系统 API 进出文件。

## 7. 故障排查

| 现象 | 可能原因 |
| --- | --- |
| 终端按钮置灰 | 沙箱不在 `running` 状态——请先恢复 |
| 打开时返回 `409 Conflict` | 同上，只是状态在刷新页面到点击之间发生了变化 |
| 对话框打开后提示"连接失败" | nginx 缺少 WebSocket location（见 §5），或 CubeProxy 连不上沙箱 |
| 终端空闲约 5 分钟后断开 | 反向代理的 `proxy_read_timeout` 仍是默认的 300s |
| 重连时提示"Shell 已不可用" | PTY 已被杀掉——沙箱被暂停，或被空闲清扫回收 |
| 全屏程序显示错乱 | 断线期间调整过对话框大小；重连即可重新同步尺寸 |
