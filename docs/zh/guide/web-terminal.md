---
title: Web 终端
---

# Web 终端

Web 终端可以从 CubeSandbox 控制台为运行中的沙箱容器打开交互式 Shell。浏览器通过已部署的 WebUI nginx 和 CubeOps 建立连接，不会直接连接 CubeMaster、Cubelet 或 containerd。

## 使用条件

- 使用具备终端访问权限的账号登录控制台。
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

Web Terminal 运行时默认值由 `CubeOps/internal/config/config.go` 统一维护，部署层不再重复这些数值。部署环境没有显式覆盖时，CubeOps 使用程序内置默认值；管理员确有需要时，可以在 `.one-click.env` 中加入受支持的 `CUBE_TERMINAL_*` 环境变量，单独覆盖相应设置。

例如，`CUBE_TERMINAL_ALLOWED_ORIGINS` 可以填写用逗号分隔的额外受信任精确 `http://` 或 `https://` Origin。同源控制台不需要配置。其他参数应保持缺省，除非运维人员有明确的覆盖理由。

Cubelet 还会执行内置限额：每节点 100 个会话、每沙箱 10 个会话、每容器 5 个会话。浏览器无法提高这些限额。

## 部署并验证 Web 终端

下面的流程使用 one-click 发布包启动 CubeSandbox，并验证从浏览器到容器的
完整终端链路。请使用具备 KVM、systemd 和 Docker 的 Linux 主机，并为该主机
单独准备部署配置和凭据；不要把生产环境的 `.env` 复制到新的安装环境。

### 准备发布包

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
chmod 600 .env
```

只设置目标主机必需的部署参数，例如节点地址和 WebUI 端口。
密码和管理员凭据不得进入 shell history 或命令行参数。

### 安装并检查服务

```bash
sudo ./install.sh
systemctl is-active cube-sandbox-cubeops.service
systemctl is-active cube-sandbox-cubemaster.service
systemctl is-active cube-sandbox-cubelet.service
systemctl is-active cube-sandbox-webui.service
curl -fsS http://127.0.0.1:12088/health
```

预期结果是四个服务都返回 `active`，且 `/health` 返回 HTTP 200。
使用浏览器打开已部署的 WebUI，不得使用 Vite 开发服务器代替。

### 创建沙箱并打开终端

1. 使用具备终端访问权限的账号登录控制台。如当前部署仍使用文档记载的
   初始凭据，应立即通过正常的修改密码流程更换。
2. 如当前部署中没有 `READY` 模板和运行中沙箱，按
   [快速开始：制作模板](./quickstart.md#第三步制作模板)操作，
   然后在控制台中从该模板创建一个沙箱。
3. 打开 **Sandboxes（沙箱）**，找到运行中的沙箱，在列表或详情页点击
   **打开终端**。
4. 等待状态变为 **已连接**，并确认终端表面存在真实输出。

暂停或停止的目标会保持禁用。如模板准备或沙箱创建失败，应先解决对应的
生命周期问题，再继续验证终端，而不是改用另一个沙箱。

### 验证终端交互

在终端中执行：

```text
printf 'WEB_TERMINAL_HOST=%s\n' "$(hostname)"
ls --color=auto
stty size
top
```

按 `Ctrl+C` 停止 `top`，确认仍可继续使用同一个 Shell。执行
`ping -c 3 127.0.0.1`，检查普通命令的输入和输出。调整终端窗口大小后再次
执行 `stty size`，返回的行列数应随之变化。完成后正常关闭终端。

不再需要沙箱时，可以通过控制台将其删除。如需停止并移除 one-click 安装，
执行：

```bash
sudo ./down.sh
```

排查问题或共享截图时，不得包含凭据、grant、Cookie、完整认证头、internal
token、终端 payload 或数据库密码。

### Shared Internal Token

CubeOps 和 CubeMaster 必须收到同一个 `CUBE_TERMINAL_INTERNAL_TOKEN`。

- 一键部署在 `CUBE_TERMINAL_INTERNAL_TOKEN` 为空时生成 token，将其移动到 `/usr/local/services/cubetoolbox/.terminal-internal-token`，要求 root 所有且权限为 `0400` 或 `0600`，并从共享 runtime env 中删除。
- 本次变更不提供 Helm/Kubernetes 或 Terraform/TKE 的 terminal token wiring。将来增加这些部署模式时，应通过其既有 secret 管理流程向 CubeOps 和 CubeMaster 提供同一个 token。

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

### 终端请求返回 401 或 403

- `401`：重新登录；当前登录会话可能不存在或已经过期。
- Grant 请求返回 `403`：确认该账号具备目标沙箱的终端访问权限。
- WebSocket upgrade 返回 `403`：确认浏览器 Origin 与控制台的 scheme、host、port 完全一致，或者已加入额外 allowed-origin 列表。

### WebSocket 没有返回 101

一键部署 nginx 源 `deploy/one-click/webui/nginx.conf` 必须包含精确终端 location、Upgrade/Connection 头、`Host $http_host`、关闭 buffering，以及 7200 秒 read timeout。线上生成文件为 `/usr/local/services/cubetoolbox/webui/nginx.generated.conf`。

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
