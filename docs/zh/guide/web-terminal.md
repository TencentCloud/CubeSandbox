---
title: Web 终端
---

# Web 终端

Web 终端可以从 CubeSandbox 控制台为运行中的沙箱容器打开交互式 Shell。浏览器通过已部署的 WebUI nginx 和 CubeOps 建立连接，不会直接连接 CubeMaster、Cubelet 或 containerd。

## 使用条件

- 使用当前角色为 `admin` 的账号登录控制台。
- 沙箱和所选容器必须处于运行状态。
- 访问已部署的控制台入口。Vite 开发服务器可用于前端开发，但不能替代生产 Web 终端入口。
- 生产环境应在控制台或受信任的反向代理处终止 HTTPS，使终端使用 WSS。

暂停或停止的沙箱会禁用终端按钮。这只是前端的提前提示；CubeOps 和 Cubelet 在启动 Shell 前仍会再次校验目标状态。

## 打开并使用终端

1. 打开 **Sandboxes（沙箱）** 页面。
2. 点击运行中沙箱所在行的终端图标，或者进入沙箱详情页后点击 **打开终端**。
3. 等待状态从 **正在连接** 变为 **已连接**。
4. 如果沙箱包含多个容器，使用容器选择器为需要的运行中容器新建独立终端标签页。未运行的容器保持禁用。
5. 使用 **新建会话** 打开另一个独立 Shell。关闭一个标签页不会关闭其他标签页。

工具栏支持 12 到 20 的终端字号和浏览器全屏模式。所选字号只会保存在非敏感的本地键 `cube.terminal.fontSize` 中。

### 输入、复制和粘贴

- 可以直接输入；终端会把 UTF-8 输入发送给所选 Shell。
- `Ctrl+C` 会发送给 TTY，用于中断 `ping`、`top` 等前台命令，不会关闭 Shell。
- 复制前先选中终端文本。`Ctrl+Shift+C` / `Ctrl+Shift+V` 是常见的终端复制和粘贴快捷键；macOS 使用对应的 Command 快捷键。浏览器剪贴板权限和安全策略仍然生效。
- 不要把凭据粘贴进 Shell。CubeSandbox 不会把终端 payload 写入审计表或应用日志，但 Shell、历史记录或执行的命令本身仍可能记录这些内容。

## 连接和会话生命周期

浏览器从 CubeOps 获取短期、一次性的 grant，并且只通过 WebSocket 子协议提交。grant 不会进入 URL、DOM、浏览器存储或查询缓存。用户 JWT 和 Cookie 也不会复制到 WebSocket URL 或子协议中。

传输意外中断时，Cubelet 默认保留同一个 Shell 30 秒。UI 大约在 1、2、4 秒后重试，并继续使用相同 session ID 和最后收到的字节偏移。恢复成功后仍是同一个 Shell 进程，并会回放缓冲输出。超过宽限期后，UI 会显示 `SESSION_LOST` 并提供新建会话操作。

主动关闭、Shell 退出、沙箱暂停/停止、空闲超时、最长生命周期和服务 drain 都会结束旧 Shell。这些终止状态不会静默重连到一个新 Shell。

## 配置参考

一键部署在 `deploy/one-click/env.example` 中提供以下变量。Helm 部署在 `deploy/kubernetes/chart/values.yaml` 的 `terminal` 下提供等价配置。

| 环境变量 | Helm value | 默认值 | 含义 |
| --- | --- | --- | --- |
| `CUBE_TERMINAL_ENABLED` | `terminal.enabled` | `true` | shared internal token 存在时启用终端 grant 和 WebSocket 网关。 |
| `CUBE_TERMINAL_ALLOWED_ORIGINS` | `terminal.allowedOrigins` | 空 | 额外受信任的精确 `http://` 或 `https://` Origin。同源控制台无需加入列表。 |
| `CUBE_TERMINAL_GRANT_TTL_SECONDS` | `terminal.grantTTLSeconds` | `60` | 未消费的一次性 grant 有效期；超过 60 的值会被拒绝。 |
| `CUBE_TERMINAL_HANDSHAKE_TIMEOUT_SECONDS` | `terminal.handshakeTimeoutSeconds` | `10` | 建立终端 relay 的最长时间。 |
| `CUBE_TERMINAL_PING_INTERVAL_SECONDS` | `terminal.pingIntervalSeconds` | `20` | WebSocket ping 间隔。 |
| `CUBE_TERMINAL_PONG_TIMEOUT_SECONDS` | `terminal.pongTimeoutSeconds` | `10` | ping 后等待传输存活响应的时间。 |
| `CUBE_TERMINAL_WRITE_DEADLINE_SECONDS` | `terminal.writeDeadlineSeconds` | `10` | 慢终端消费者的单次写入 deadline。 |
| `CUBE_TERMINAL_IDLE_TIMEOUT_MINUTES` | `terminal.idleTimeoutMinutes` | `30` | 用户空闲超时。只有 stdin 会重置；输出、resize、ping 和 pong 都不会重置。 |
| `CUBE_TERMINAL_MAX_LIFETIME_HOURS` | `terminal.maxLifetimeHours` | `8` | Shell 的绝对最长生命周期，包括持续活跃的 Shell。 |
| `CUBE_TERMINAL_RECONNECT_GRACE_SECONDS` | `terminal.reconnectGraceSeconds` | `30` | detached 恢复窗口；设为 `0` 可关闭恢复。 |
| `CUBE_TERMINAL_REPLAY_BUFFER_BYTES` | `terminal.replayBufferBytes` | `262144` | 为恢复保留在内存中的最大输出（256 KiB）。 |
| `CUBE_TERMINAL_MAX_FRAME_BYTES` | `terminal.maxFrameBytes` | `65536` | WebSocket 入站帧上限（64 KiB）。 |
| `CUBE_TERMINAL_STDIN_QUEUE_FRAMES` | `terminal.stdinQueueFrames` | `8` | 有界 stdin 队列深度。 |
| `CUBE_TERMINAL_STDOUT_PENDING_BYTES` | `terminal.stdoutPendingBytes` | `262144` | 触发慢消费者处理前允许等待的 stdout 上限（256 KiB）。 |
| `CUBE_TERMINAL_MAX_SESSIONS_PER_USER` | `terminal.maxSessionsPerUser` | `5` | 单个用户允许的活跃会话数。 |
| `CUBE_TERMINAL_MAX_SESSIONS_PER_REPLICA` | `terminal.maxSessionsPerReplica` | `200` | 单个 CubeOps 副本允许的活跃连接数。 |
| `CUBE_TERMINAL_DRAIN_TIMEOUT_SECONDS` | `terminal.drainTimeoutSeconds` | `30` | CubeOps 优雅停止窗口。 |

Cubelet 还会执行内置限额：每节点 100 个会话、每沙箱 10 个会话、每容器 5 个会话。浏览器无法提高这些限额。

## 30 分钟：从一键部署到终端

以下步骤用于一次性的 Linux one-click 测试部署。请使用本次任务专用的
发布包和部署主机，不要复用生产 `.env`、凭据或沙箱 ID。

### 0-5 分钟：准备发布包

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
chmod 600 .env
```

只设置目标主机需要的部署参数，例如节点地址和 WebUI 端口。数据库密码和
管理员凭据不要写入 shell history，也不要放在命令行参数中；请通过部署流程
提供的安全输入方式使用运维方提供的管理员凭据。

### 5-20 分钟：安装并检查服务

```bash
sudo ./install.sh
systemctl is-active cube-sandbox-cubeops.service
systemctl is-active cube-sandbox-cubemaster.service
systemctl is-active cube-sandbox-cubelet.service
systemctl is-active cube-sandbox-webui.service
curl -fsS http://127.0.0.1:12088/health
```

预期结果是四个服务都返回 `active`，并且 `/health` 返回 HTTP 200。用非
localhost 浏览器打开配置好的 WebUI 入口；不要用 Vite 开发服务器完成此检查。

### 20-25 分钟：登录并打开终端

1. 使用当前角色为 `admin` 的管理员账号登录。
2. 打开 **Sandboxes（沙箱）**，选择一个已经运行的沙箱。
3. 从列表或详情页点击 **打开终端**。
4. 等待状态变为 **已连接**，并确认终端表面有真实输出。

预期结果是真实 xterm 表面和 Shell 提示符。暂停或停止的目标必须保持禁用。

### 25-30 分钟：执行 `top` 并清理

在终端中执行：

```text
printf 'WEB_TERMINAL_HOST=%s\n' "$(hostname)"
top
```

按 `Ctrl+C` 返回 Shell，然后正常关闭终端。预期结果是能看到实时 `top`，
回到同一个 Shell，并且正常关闭后不会自动重连。

对于一次性的 one-click 主机，最后执行：

```bash
sudo ./down.sh
```

对于共享主机，只能通过正常 Dashboard/API 流程删除本次任务专用的精确沙箱，
不要停止或删除主机上的共享服务。

### Shared Internal Token

CubeOps 和 CubeMaster 必须收到同一个 `CUBE_TERMINAL_INTERNAL_TOKEN`。

- 一键部署在 `CUBE_TERMINAL_INTERNAL_TOKEN` 为空时生成 token，将其移动到 `/usr/local/services/cubetoolbox/.terminal-internal-token`，要求 root 所有且权限为 `0400` 或 `0600`，并从共享 runtime env 中删除。
- Helm 默认创建并在升级时复用 Secret。使用外部控制面等场景，应通过 `terminal.existingSecret` 和 `terminal.secretKey` 引用运维方管理的 Secret。
- Terraform 接受 `TENCENTCLOUD_TERMINAL_INTERNAL_TOKEN`，留空则自动生成。生成的 `.env`、resolved variables 和 Terraform state 必须使用权限控制、加密存储和受限 backend 访问保护。

不得把该 token 放进命令参数、URL、日志、截图或提交到仓库的 values 文件。

## 安全模型

- 浏览器入口是控制台同源的 `/opsapi/v1/terminal/ws`。
- 生产部署应使用 HTTPS/WSS。只有在 WebUI 使用另一个受信任 Origin 时才配置 `CUBE_TERMINAL_ALLOWED_ORIGINS`。
- nginx 必须通过 `Host $http_host` 保留浏览器 Host 和非默认端口，否则严格 Origin 校验会拒绝 upgrade。
- grant 默认 60 秒过期，只能原子消费一次，并绑定用户、沙箱、容器和会话操作。
- Shell 继承容器已有的用户、capabilities、挂载、namespace、seccomp 和网络边界。Web 终端不会提供类似 SSH 的宿主机访问，也不会增加容器权限。
- 审计数据只包含用户、目标、时间戳、关闭原因、退出码、字节计数和恢复次数，不包含终端输入/输出或原始 grant。

## 故障排查

### 终端按钮被禁用

确认沙箱和目标容器都处于运行状态。暂停目标会在 UI 中禁用，并被后端以 `TARGET_NOT_RUNNING` 拒绝。

### Grant 请求返回 401 或 403

- `401`：重新登录，并确认账号当前角色为 `admin`。
- WebSocket upgrade 返回 `403`：确认浏览器 Origin 与控制台的 scheme、host、port 完全一致，或者已加入额外 allowed-origin 列表。

### WebSocket 没有返回 101

三份 nginx 源都必须包含精确终端 location、Upgrade/Connection 头、`Host $http_host`、关闭 buffering，以及 7200 秒 read timeout：

1. 一键部署：`deploy/one-click/webui/nginx.conf`。线上生成文件为 `/usr/local/services/cubetoolbox/webui/nginx.generated.conf`。
2. Helm：`deploy/kubernetes/chart/templates/_helpers.tpl`，渲染到 WebUI ConfigMap。
3. Terraform：`deploy/one-click/terraform/tencentcloud/tke-addons.tf`；`create.sh` 通常先把一键部署 canonical 模板复制为 `webui-nginx.conf`，再渲染 `cube-webui-nginx-conf` ConfigMap。

普通 `/opsapi/` REST location 应继续使用 300 秒 timeout，并保留现有 `/sandbox/` WebSocket 路由。health 或 REST 请求成功，并不能证明终端 upgrade 头和长连接 timeout 已生效。

### 终端在接近五分钟时断开

检查实际生效的 nginx 配置，而不只是源模板。精确 `/opsapi/v1/terminal/ws` location 必须使用 `proxy_read_timeout 7200s`；普通 `/opsapi/` block 仍为 `300s`。

### Shell 无法恢复

恢复只适用于配置宽限期内的意外传输中断。主动关闭、Shell 退出、沙箱状态切换、服务 drain、缓冲溢出或 Cubelet 重启都会结束旧会话。看到 `SESSION_LOST` 后应启动新会话。

### 服务检查

一键部署应优先检查有界的当前状态，除非确实需要读取日志：

```bash
systemctl show cube-sandbox-cubeops.service cube-sandbox-cubemaster.service \
  -p Id -p ActiveState -p SubState --no-pager
curl -fsS http://127.0.0.1:3010/health
```

排障时不要打印受保护的环境文件或 terminal Secret。

## 已知限制

- Cubelet 重启后不能恢复已有终端会话。
- TTY 模式会把 stderr 合并到 stdout，没有独立 stderr 流。
- detached 恢复默认最多 30 秒，输出回放上限为 256 KiB。
- Web 终端不提供 SSH、SFTP、文件上传/下载、终端录制或协作输入。
- 剪贴板行为由浏览器和操作系统控制；CubeSandbox 不会绕过剪贴板权限策略。
