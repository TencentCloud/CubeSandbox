# OpenHands × Cube Sandbox

在硬件隔离的 Cube Sandbox MicroVM 中运行 [OpenHands](https://www.openhands.dev/)
编码智能体：智能体的每一条命令、每一次文件编辑、每一个脚本都在 MicroVM 内执行，
而且模板的启动快照中已经包含一个**正在运行**的 OpenHands Agent Server。

[English README](./README.md)

## 为什么做这个集成

OpenHands 自带的 `DockerWorkspace` 每次会话都要冷启动一个 agent-server 容器。
Cube Sandbox 的模板会在首次启动*之后*拍摄快照，因此本示例把 agent server
预装进模板并连同运行状态一起冻结：

| | DockerWorkspace | CubeSandboxWorkspace（本示例） |
|---|---|---|
| 会话启动 | 容器启动 + 服务冷启动 | 快照热启动，服务已在监听 |
| 隔离性 | 共享宿主机内核 | 拥有独立内核的 KVM MicroVM |
| 长任务会话 | 容器停止即丢失 | 整机 `pause()` / `resume()` —— agent server、shell 会话与执行中的进程逐位冻结、逐位恢复 |
| 网络策略 | Docker 网络 | 平台级出口管控（白名单 / 黑名单 / 断网） |

由于 agent 会话状态活在 VM 内的 server 进程里，`pause()` / `resume()`
冻结与恢复的是**整个会话**而不只是执行环境（演示见 `pause_resume.py`）。

## 工作原理

```
 host                                Cube Sandbox platform
┌──────────────────────────┐        ┌──────────────────────────────────┐
│ OpenHands SDK            │        │ MicroVM (from openhands template)│
│  Conversation ───────────┼─HTTP──▶│  agent-server :8000  (pre-warmed │
│  CubeSandboxWorkspace    │ proxy  │   in the template snapshot)      │
│   └─ e2b SDK (create/    │        │  envd :49983 (SDK ops daemon)    │
│      pause/resume/kill)  │        │  /workspace  (agent working dir) │
└──────────────────────────┘        └──────────────────────────────────┘
```

`CubeSandboxWorkspace` 继承 SDK 的 `RemoteWorkspace`（与官方 `DockerWorkspace`
使用同一扩展点），并把它指向沙箱代理出来的 agent-server 地址。
`Conversation(agent=..., workspace=...)` 会自动成为 `RemoteConversation`：
智能体主循环在 MicroVM 内运行，LLM 流量也从沙箱内发起（见
[安全对齐](#安全对齐)）。

## 前置条件

- 已部署的 Cube Sandbox（单机即可）——参见
  [快速开始](../../docs/zh/guide/quickstart.md) /
  [裸金属部署](../../docs/zh/guide/bare-metal-deploy.md)
- `cubemastercli` 在 `$PATH` 中，构建镜像需要 Docker
- 宿主机脚本需要 [`uv`](https://docs.astral.sh/uv/)（或较新的 pip >= 26）——
  **较旧的 pip（如 Ubuntu 24.04 自带的 24.0）无法解析 OpenHands 的依赖图**
  （上游 `lmnr` / `opentelemetry` 约束冲突）
- 一个 OpenAI 兼容的 LLM 端点和密钥（仅 `main.py` 需要——
  `smoke_test.py` 与 `pause_resume.py` 无需 LLM）

## 1. 构建模板镜像

```bash
docker build -t openhands-sandbox:latest examples/openhands-integration
```

（可选）接入平台前先用纯 Docker 做个体检：

```bash
docker run --rm -d -p 8000:8000 -p 49983:49983 --name oh-sbx openhands-sandbox:latest
curl http://127.0.0.1:8000/ready
curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health   # => 204
docker stop oh-sbx
```

## 2. 注册模板

把镜像推送到部署环境可以拉取的镜像仓库，然后：

```bash
cubemastercli tpl create-from-image \
  --image     <registry>/openhands-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 8000 \
  --expose-port 49983 \
  --probe 8000 \
  --probe-path /ready
```

探针直接指向 **agent server 的就绪端点**（`:8000/ready`——初始化完成前
它不会返回 2xx；不是仅探测 envd，也不是只表示进程存活的 `/health`），
平台只会在服务完全初始化后才拍摄启动快照——热启动的保证由构建流程强制成立，
而不是靠运气。参照
[从 OCI 镜像创建模板](../../docs/zh/guide/tutorials/template-from-image.md)
等待模板进入 `READY`，记下生成的 `tpl-...` id。

## 3. 配置

```bash
cd examples/openhands-integration
uv venv .venv --python 3.12 && source .venv/bin/activate
uv pip install -r requirements.txt
cp .env.example .env   # 填写 E2B_API_URL、E2B_API_KEY、CUBE_TEMPLATE_ID、LLM_*
```

## 4. 运行

```bash
python smoke_test.py       # 无需 LLM 密钥
python pause_resume.py     # 无需 LLM 密钥
python main.py             # 完整智能体演示（需要 .env 中的 LLM_*）
```

- `smoke_test.py` 端到端验证集成：打印 创建→健康 延迟（热启动证据）、调用
  `/server_info`、通过 OpenHands workspace API 往返执行 bash 与文件传输。
- `pause_resume.py` 在 VM 内启动一个每秒计数器，暂停 VM 后**彻底丢弃
  workspace 对象**（`kill_on_exit=False`），8 个墙钟秒后用全新的
  `CubeSandboxWorkspace(sandbox_id=...)` 重连（自动恢复），展示计数序列
  没有任何空洞——会话跨越了原对象的生命周期，从冻结的那一瞬间原样继续。
- `main.py` 运行一次真实的 OpenHands 会话（写程序、执行、修错），最后*从沙箱
  内部*独立核验结果，而不是听智能体自己汇报。

## 在你自己的代码里使用

```python
from openhands.sdk import LLM, Conversation
from openhands.tools.preset.default import get_default_agent
from cubesandbox_workspace import CubeSandboxWorkspace

agent = get_default_agent(llm=LLM(model="openai/...", api_key=...), cli_mode=True)

with CubeSandboxWorkspace(template="tpl-...") as workspace:
    conversation = Conversation(agent=agent, workspace=workspace)
    conversation.send_message("修复 /workspace/repo 中失败的测试")
    conversation.run()
    workspace.pause()      # 冻结整个会话……
    workspace.resume()     # ……稍后原样继续

# 也可以让暂停的会话活得比当前进程更久（kill_on_exit=False），
# 之后——哪怕隔天、哪怕换个进程——重连即自动恢复：
#   workspace = CubeSandboxWorkspace(sandbox_id="...")
```

## 安全对齐

- **出口管控**：智能体主循环在 MicroVM 内运行，因此沙箱需要能访问你的 LLM
  端点。要收紧其余出口，可在创建沙箱时配置只包含该端点的网络白名单（参见
  [网络策略](../../docs/zh/guide/network-policy.md) 与 quickstart 的
  `network_allowlist.py` 写法）——智能体除了"自己的大脑"哪儿也去不了。
- **LLM key 在哪里**：OpenHands 的 agent-server 架构把智能体主循环放在
  workspace 内运行，因此标准用法（包括官方 `DockerWorkspace`）会把
  `LLM(api_key=...)` 随会话一起发进沙箱。若要求真 key 不进 VM，可给智能体
  配置占位符，由 CubeEgress
  [凭证注入](../../docs/zh/guide/security-proxy.md)在线上附加真实的
  `Authorization` 请求头（写法参考
  [`examples/pi-agent-integration/network_policy.py`](../pi-agent-integration/network_policy.py)）；
  本模板的 `SSL_CERT_FILE` 已让预热的服务端信任拦截 CA。
- **入口管控——一个开关即可私有化**：`CubeSandboxWorkspace(...,
  private_traffic=True)` 会以 `allow_public_traffic=False` 创建沙箱，并在
  每个 workspace HTTP 请求中携带沙箱级
  [traffic token](../../docs/zh/guide/restrict-public-access.md)
  （`e2b-traffic-access-token` 请求头）——只有持有令牌者才能穿过平台代理
  访问 agent server，共享部署强烈建议开启。边界说明：OpenHands 1.38.0 的
  Conversation WebSocket 不携带自定义请求头，完整会话需使用公网流量；
  workspace API 调用不受影响。令牌在 pause/resume 后依然有效（平台在服务端
  校验、workspace 在客户端保留）；跨进程重连私有沙箱时，请持久化
  `workspace.traffic_token` 并以 `traffic_access_token=` 传回。
  需要纵深防御时，服务端也支持会话密钥：
  以 `SESSION_API_KEY=<secret>` 启动（模板 CMD 会透传环境变量），并在
  `CubeSandboxWorkspace(api_key=...)` 传入同一值——SDK 会以
  `X-Session-API-Key` 请求头发送。
- **运行账户**：agent server 与 envd 执行 SDK 文件/命令操作使用同一个
  uid-1000 `user` 账户，双方创建的文件天然互相可读写。注意这只是属主约定
  而非权限边界——`cubesandbox-base` 给 `user` 配置了免密 sudo；真正的隔离
  边界是 MicroVM 本身。

## 资源建议

- 模板构建：镜像约 750 MB（独立 CPython + OpenHands 服务端）。
- 沙箱：CLI 工具集（bash + 文件编辑）下 2 vCPU / 2 GB 内存即可流畅运行；
  按任务实际编译/运行的负载增加余量。
- `--writable-layer-size 2G` 足够覆盖常规任务的文件产出与 pip 缓存
  （新启动的沙箱仅占用约 164 KB）；仓库较大的任务请调高。

## 已知限制

- **旧版 pip 无法安装宿主机依赖**：`lmnr` 钉住了一个预发布版 opentelemetry，
  较旧的 pip 解析器（如 Ubuntu 24.04 自带的 24.0）会解析失败——请使用 uv
  或较新的 pip（>= 26）。模板构建内部已使用 uv。
- **浏览器工具集默认关闭**（`cli_mode=True`）：不把 Playwright/Chromium
  装进模板以控制体积。需要浏览智能体时，在 Dockerfile 中安装浏览器栈并
  去掉 `cli_mode`。
- **版本配对**：宿主机 `openhands-sdk`/`openhands-tools` 与模板内
  `openhands-agent-server` 钉在同一版本线（1.38.0），请同步升级；
  `workspace.get_server_info()` 可查看服务端版本排查不匹配。

## 排障

- *`CubeSandboxWorkspace` 健康检查超时*：确认模板镜像带有 agent-server CMD
  （第 1 步的本地 Docker 体检）、注册时带了 `--expose-port 8000`、且
  `E2B_API_URL` 指向你的部署。若部署的 cube-proxy HTTP 端口不是 80，
  在 `.env` 中设置 `CUBE_PROXY_HTTP_PORT` 与之一致。
- *`get_server_info` 正常但会话失败*：通常是 LLM 配置问题——记住 LLM 是
  **从沙箱内部**调用的；检查出口策略与 `LLM_BASE_URL` 在 VM 内的可达性
  （`workspace.execute_command("curl -sI $LLM_BASE_URL")`）。

## 参考

- 集成指南：[`docs/zh/guide/integrations/openhands.md`](../../docs/zh/guide/integrations/openhands.md)
- 从 OCI 镜像创建模板：[`docs/zh/guide/tutorials/template-from-image.md`](../../docs/zh/guide/tutorials/template-from-image.md)
- OpenHands Agent SDK：<https://github.com/OpenHands/software-agent-sdk>
