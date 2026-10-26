# Pi Agent + CubeSandbox 示例

[English](README.md)

在 CubeSandbox MicroVM 内运行 [Pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
（面向终端的 AI 编码 Agent）。Agent 在一个隔离、可复现的沙箱内编辑文件、执行命令并访问 LLM API。

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- 一个 LLM provider 的 API Key（默认 DeepSeek；也支持 Anthropic、OpenAI 等 provider）。
- Python 3.10+（宿主端驱动脚本）。

## 1. 构建基础模板镜像

```bash
docker build --platform linux/amd64 \
  -t localhost:5000/pi-agent-cube:latest \
  examples/pi-agent-integration
docker push localhost:5000/pi-agent-cube:latest
```

## 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image localhost:5000/pi-agent-cube:latest \
  --alias pi-agent \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

任务变为 `READY` 后记下 `template_id`。

## 3. 配置宿主端驱动

```bash
cd examples/pi-agent-integration
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID 以及你的 provider key
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自第 2 步 |
| `CUBE_WARMUP_TEMPLATE_ID` | `run_pi_warmup.py` | 来自下文 warmup 模板任务 |
| `PI_PROVIDER` | Pi CLI | `deepseek`（默认）、`anthropic`、`openai` 等 |
| `PI_MODEL` | Pi CLI | 对应 provider 的模型 id |

## 4. 一次性运行（直连注入密钥）

```bash
python run_pi_agent.py --prompt "创建 hello.py 打印 'Hello from CubeSandbox' 并运行它。"
```

密钥通过 `sandbox.commands.run(..., envs=...)` 逐命令传入，只在该命令执行期间存在，不会写入 VM 内的持久文件。

> **安全：** 直连方式出网是放开的，Agent 被攻破可能外泄注入的密钥。共享集群请用保险柜方式（第 6 步）：默认拒绝出网 + 链路上注入密钥。

三个脚本都以 `--mode json` 运行 Pi，默认输出精简转写（助手文本、工具调用、失败项）。加 `--raw`（或设置 `PI_STREAM_RAW=1`）可改为打印 Pi 的原始 JSONL 事件流。

## 5. pause / resume（会话持久化）

```bash
python resume_pi_agent.py
```

第一轮让 Pi 写 `/workspace/plan.md`，随后 `sandbox.pause()` 对 VM 打快照。脚本用
`Sandbox.connect(sandbox_id)` 恢复，校验 `/workspace/plan.md` 与 Pi 状态目录
（`/root/.pi/agent`）仍在，再执行第二轮续写。沙箱生命周期用 `try/finally` 手动管理（不用
context manager），避免 pause 后被过早 `kill` 掉。

## 6. 制作 Pi SDK warmup 快照

一次性方案会为每个任务启动新的 `pi` 进程。warmup 方案把常驻 Node adapter 作为镜像命令，
启动时创建一个持久化的 Pi SDK `AgentSession`，初始化完成后 `GET /readyz` 才返回 HTTP 200。
Cube 会等待该探针成功后制作模板快照，因此从模板恢复的沙箱中已经包含初始化好的 Node 进程和
session。

```bash
docker build --platform linux/amd64 \
  -f Dockerfile.warmup \
  --build-arg PI_AGENT_IMAGE=localhost:5000/pi-agent-cube:latest \
  -t localhost:5000/pi-agent-warmup-cube:latest .

docker push localhost:5000/pi-agent-warmup-cube:latest
```

使用 adapter 的探针制作 warmup 模板（默认端口为 `8080`，可通过 `PI_WARMUP_PORT` 修改）：

```bash
cubemastercli tpl create-from-image \
  --image localhost:5000/pi-agent-warmup-cube:latest \
  --alias pi-warmup \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe       8080 \
  --probe-path  /readyz
```

将任务生成的模板 ID 写入 `CUBE_WARMUP_TEMPLATE_ID`，然后向恢复出的 session 发送任务：

```bash
python run_pi_warmup.py \
  --prompt "创建 hello.py，打印 'Hello from pre-warmed Pi' 并运行它。"
```

驱动脚本通过 `/prompt` 请求传递 provider key。adapter 只把它放在 Pi 的内存鉴权存储中，
不会写入 session 或模板快照。单个 adapter 只维护一个 session，因此任务会串行执行；并发请求会
收到 HTTP 409。

## 7. 受限出网 + 密钥保险柜（推荐用于共享集群）

```bash
python network_policy.py
```

- 出网默认拒绝，仅放行 LLM host（`PI_LLM_HOST`）。
- CubeEgress 在链路上把 provider 密钥作为 HTTP 头注入（Anthropic 用 `x-api-key`，其他用
  `Authorization: Bearer`），因此沙箱内 `printenv` 看不到真实密钥，只有占位值。
- Pi 基于 Node.js（忽略系统 CA 库），脚本会设置 `NODE_EXTRA_CA_CERTS` 让 Pi 信任 CubeEgress 的
  拦截 CA；否则 vault 路径会以 `Connection error` 失败。若镜像里 CA 路径不同，可用
  `PI_NODE_EXTRA_CA_CERTS` 覆盖。
- 任何其他目的地都会返回 `403 Forbidden - CubeEgress`。

若 Agent 需要访问额外主机（包镜像源、MCP 服务器等），请增加对应的放行规则，或把这些依赖预装进模板。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| preflight 报 `pi: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| provider 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（vault） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host（及所需其他 host）加入规则 |
| vault 路径下 Pi 报 `Connection error` / TLS 失败 | Pi 基于 Node，忽略系统 CA 库，不信任 CubeEgress 拦截 CA | 脚本已把 `NODE_EXTRA_CA_CERTS` 指向系统 CA 包；若 CA 在别处，用 `PI_NODE_EXTRA_CA_CERTS` 覆盖 |
| 就绪探针超时 | 镜像缺少 envd | 确认 `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| warmup `/readyz` 一直不成功 | `PI_PROVIDER` / `PI_MODEL` 不是当前固定 Pi 版本内置的模型 | 查看 adapter 日志，改用支持的 provider/model 组合 |
| warmup 任务返回 HTTP 409 | 常驻 session 正在处理其他任务 | 等当前任务完成；一个 adapter 只维护一个 session |
| `pause()`/`connect()` 报错 | 平台版本过低不支持快照 | 升级 CubeSandbox 平台 |

## 参考

- 集成指南：[`docs/guide/integrations/pi-agent.md`](../../docs/zh/guide/integrations/pi-agent.md)
- 快照 / 克隆 / 回滚：[`docs/guide/snapshot-rollback-clone.md`](../../docs/zh/guide/snapshot-rollback-clone.md)
- 网络 / 出网策略示例：[`examples/network-policy`](../network-policy)
- Pi coding agent：<https://www.npmjs.com/package/@earendil-works/pi-coding-agent>
