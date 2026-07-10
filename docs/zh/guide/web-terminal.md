# WebUI 终端登录

CubeSandbox WebUI 可在运行中的沙箱详情页打开交互式终端。终端复用 Cubelet 的 containerd `Task.Exec` TTY 能力，支持 ANSI 输出、光标控制、滚动回溯、窗口尺寸同步、多终端会话和容器选择。

## 启用

终端默认关闭。为避免浏览器直接访问节点，CubeAPI 通过受控 WebSocket 转发流量到 Cubelet；两端必须配置相同的随机密钥。

```bash
# CubeAPI
export CUBE_TERMINAL_PROXY_TOKEN="$(openssl rand -hex 32)"
export CUBE_API_TERMINAL_PORT=10555

# 每台 Cubelet 节点（使用与 CubeAPI 相同的 CUBE_TERMINAL_PROXY_TOKEN）
export CUBE_TERMINAL_PROXY_TOKEN="<same value>"
export CUBELET_TERMINAL_LISTEN_ADDR="0.0.0.0:10555"
# 可选：无用户输入多久后断开，默认 1800 秒
export CUBELET_TERMINAL_IDLE_TIMEOUT_SECS=1800
```

完成配置后重启 CubeAPI 和所有 Cubelet。控制平面必须能访问每个节点的 `10555/TCP`；不要将该端口暴露到公网。生产环境应使用现有 HTTPS/WSS 入口发布 WebUI，浏览器到 CubeAPI 的传输会自动使用 WSS。

## 使用方式

1. 登录 WebUI，进入正在运行的沙箱详情页。
2. 点击“打开终端”，在面板中选择目标容器与 `/bin/sh` 或 `/bin/bash`。
3. 使用终端执行命令；面板会随窗口变化同步行列数。`Ctrl/⌘ + Shift + C/V` 可复制/粘贴，工具栏可调整字号、全屏或重新连接。

暂停、退出或创建中的沙箱会禁用入口。每次连接由一个有效期 60 秒、仅可使用一次的会话票据建立；异常断线后点击“重新连接”即可创建新会话。

## 权限与审计

- 普通 REST 请求仍经过 CubeAPI 的 API key / 外部鉴权回调。
- 配置 WebUI 数据库时，创建终端会话还必须携带有效的 WebUI session token。
- Cubelet 只接受携带共享代理密钥的控制平面连接，且不会接受来自浏览器的直连。
- CubeAPI 记录 `terminal.session.requested` 和 `terminal.session.closed` 审计事件，包含操作人（如已登录）、时间、沙箱和容器 ID。
- 终端进程在目标容器内以该容器已有的权限和网络策略运行，不会获得宿主机权限或绕过 CubeEgress 策略。

## 已知限制

- 当前允许 `/bin/sh` 和 `/bin/bash`；镜像必须包含所选 shell。
- 会话无用户输入达到空闲超时时将断开；关闭面板也会终止其对应 shell。
- 多容器沙箱会显示所有运行中的容器。多个用户和同一用户的多个终端可并行使用，彼此拥有独立的 TTY 进程。
