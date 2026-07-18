---
title: OpenCode 集成指南
author: pei-pei45
date: 2026-07-18
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: zh-CN
---

# OpenCode 集成指南

[English](../../guide/integrations/opencode.md)

在 CubeSandbox MicroVM 内运行 [OpenCode CLI](https://opencode.ai/)
（开源、面向终端的 AI 编码 Agent）。本文涵盖镜像构建、密钥注入、出网管控与
基于快照的会话持久化，并配套可运行的
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
示例项目。

## 集成对象与版本

| 组件 | 版本 |
|---|---|
| OpenCode | `opencode-ai@1.17.20`（通过 `--build-arg OPENCODE_VERSION=x.y.z` 固定；依赖 1.17 加入的 `--dangerously-skip-permissions` 标志） |
| Node.js | 20（通过 NodeSource 安装） |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK（宿主端驱动） | `e2b`（最新版） |
| CubeSandbox 平台 | `>= 0.3.0`（pause/resume）/ `>= 0.4.0`（CubeEgress 凭证保险柜） |

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- OpenCode 兼容的 LLM provider 密钥。OpenCode 内置 Anthropic、OpenAI、
  Google Gemini、Azure、AWS Bedrock、DeepSeek、Groq、Mistral、OpenRouter
  等预设，任何一个都可以。自定义上游可通过 `OPENCODE_BASE_URL` /
  `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` 指向。
- Python 3.10+（宿主端驱动脚本）。

## 为什么把 OpenCode 跑在沙箱里

OpenCode 是一款终端 Agent，会编辑文件、执行命令、安装软件包。直接在工作站上
运行，会把 Agent 的破坏半径和你的开发环境混在一起。把它放到 CubeSandbox 里
能获得：

| 关注点 | CubeSandbox 提供 |
|---|---|
| **隔离** | 每个会话独占一台 KVM MicroVM，独占 guest 内核 |
| **可复现** | 每个会话都从同一份模板快照启动 |
| **快速启动** | 冷启动 < 60ms，并行 N 个 Agent 几乎无成本 |
| **长任务** | `sandbox.pause()` 对 VM + rootfs 打快照，之后再恢复 |
| **密钥卫生** | CubeEgress 在链路上注入鉴权头，VM 内永远看不到真实 Key |
| **出网审计** | 每次访问 LLM API 都会记录在出网审计日志中 |
| **开放生态** | OpenCode 原生支持 MCP，可以与 CubeSandbox 的出网策略对齐 |

## 接入步骤

### 1. 构建模板镜像

镜像在 `cubesandbox-base` 之上叠加 Node.js 20 与 OpenCode CLI；envd 已经监听
`:49983`。

```dockerfile
# examples/opencode-integration/Dockerfile（节选）
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=20
ARG OPENCODE_VERSION=1.17.20

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg jq less procps python3 python3-pip ripgrep \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg \
    && gpg --show-keys /etc/apt/keyrings/nodesource.gpg 2>/dev/null \
        | grep -q "6F71F525282841EEDAF851B42F59B5F99B1BE0B4" \
        || (echo "ERROR: NodeSource GPG fingerprint mismatch" && exit 1) \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g --omit=dev \
        "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && rm -rf /root/.npm

# OpenCode 以非 root 用户运行。基础镜像已经用 uid=1000 作为 ``user`` exec 账号，
# 所以 UID/GID 由系统自动分配；pause/resume 快照按用户名保留身份。MicroVM 提
# 供外层隔离，这层降权是对 prompt injection 场景（LLM agent 被诱导执行
# shell）的纵深防御。
#
# /workspace 设成 world-writable，因为 e2b exec 信道把用户名限制在 ``root``
# 和 ``user``，而 ``user`` 写不进 opencode 拥有的目录。OpenCode 把它的磁盘
# 状态拆成 ``$OPENCODE_CONFIG_DIR``（配置 + agents）与
# ``$OPENCODE_DATA_DIR``（会话、auth.json、storage）；两者都放在
# ``/workspace/.opencode`` 之下，pause/resume 时会跟项目文件一起被快照。
ARG OPENCODE_CONFIG_DIR=/workspace/.opencode/config
ARG OPENCODE_DATA_DIR=/workspace/.opencode/data

RUN groupadd --system opencode \
    && useradd  --system --gid opencode \
                --home-dir /home/opencode --shell /bin/bash \
                --no-create-home opencode \
    && install -d -o opencode -g opencode -m 0700 /home/opencode \
    && install -d -o opencode -g opencode -m 0777 /workspace

ENV OPENCODE_CONFIG_DIR=${OPENCODE_CONFIG_DIR} \
    OPENCODE_DATA_DIR=${OPENCODE_DATA_DIR} \
    XDG_CONFIG_HOME=/workspace \
    XDG_DATA_HOME=/workspace \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    OPENCODE_DISABLE_AUTOUPDATE=1

RUN install -d -o opencode -g opencode -m 0777 "${OPENCODE_CONFIG_DIR}" \
    && install -d -o opencode -g opencode -m 0777 "${OPENCODE_DATA_DIR}/storage" \
    && printf '{}\n' > "${OPENCODE_CONFIG_DIR}/opencode.json"

WORKDIR /workspace
USER opencode
```

构建并推送（在仓库根目录运行，确保相对构建上下文 `examples/opencode-integration`
能正确解析）：

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/opencode-cube:latest \
  examples/opencode-integration
docker push <your-registry>/opencode-cube:latest
```

### 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务变为 `READY` 后记下 `template_id`，之后每次 `Sandbox.create()` 都传给它。
中等任务用 `4G` 可写层就够；如果 Agent 需要安装较重的工具链，建议提升到
`8G+`。

### 3. 配置宿主端驱动

```bash
cd examples/opencode-integration
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID 以及 provider 密钥
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自第 2 步 |
| `OPENCODE_PROVIDER` | `env_utils.provider()` | 可选 —— 未设置时从已配的密钥反推 |
| `OPENCODE_MODEL` / `OPENCODE_BASE_URL` | OpenCode CLI 标志 | 模型 id 与可选的自定义上游端点 |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GOOGLE_API_KEY` / ... | `envs=...`（直连）或 CubeEgress 注入（vault） | provider 密钥 |
| `OPENCODE_LLM_HOST` | `network_policy.py` | 默认拒绝下放行的 LLM host |

### 4. 运行时配置与 API Key 注入

OpenCode 以无交互模式启动：`opencode run "..."` 让它处理完 prompt 即退出（不
进入交互 TUI）。通过 `--dangerously-skip-permissions` 标志（OpenCode 1.17+
加入，别名 `--yolo`）在该次运行期间自动放行工具调用 —— 因为 exec 信道无法
回答权限弹窗。prompt 作为末尾的位置参数。两种密钥注入方式共用同一份模板：

**直连方式** —— 每个命令注入一次 Key。`e2b` 的 `commands.run(envs=...)` 把环
境放进 exec 信封，而不是写入 VM 内的持久文件，所以 Key 只在该命令执行期间存
在。exec 信道只接受 `root` 和 `user` 两个用户名；传 `user="opencode"` 会触发
`invalid username: 'opencode'`。镜像内 Agent 仍以非 root 身份运行（因为
Dockerfile 有 `USER opencode`），SDK 层面的 `user` 参数只约束 exec 信道的身
份，不影响容器内进程身份：

```python
result = sandbox.commands.run(
    "cd /workspace && opencode run --dangerously-skip-permissions -m claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'",
    envs={"ANTHROPIC_API_KEY": key},
    user="user",
    timeout=900,
)
```

**保险柜方式** —— 让 Key 完全不进入 VM（见第 6 步）。

### 5. 会话持久化（pause / resume）

```bash
cd examples/opencode-integration
python resume_opencode.py
```

与 [快照 / 克隆 / 回滚](../snapshot-rollback-clone.md) 引擎在 SDK 层等价：

- `sandbox.pause()` 对运行中的 VM（内存 + rootfs）打快照并释放算力。
- `Sandbox.connect(sandbox_id)` 恢复时，`/workspace`、
  `/workspace/.opencode/config/`、`/workspace/.opencode/data/` 以及其它所有文
  件都保留。第二轮再调用
  `opencode run --dangerously-skip-permissions -c`，让 OpenCode 自动续接
  `$OPENCODE_DATA_DIR/storage/` 下最近一次会话。

> **生命周期注意事项：** 用 `try/finally` 管理沙箱生命周期，不要用
> `with Sandbox.create(...)` context manager。`__exit__` 会 `kill` 沙箱，把
> pause 撤销。示例里显式创建沙箱，只在 `finally` 中调用 `sandbox.kill()`。

```python
sandbox = Sandbox.create(template=template_id, timeout=1800)
try:
    run_turn(sandbox, prompt_1)          # 写入 /workspace/plan.md
    sandbox_id = sandbox.pause() or sandbox.sandbox_id
    sandbox = Sandbox.connect(sandbox_id)
    assert_state_survived(sandbox)       # /workspace + /workspace/.opencode/{config,data}/ 完好
    run_turn(sandbox, prompt_2, continue_session=True)
finally:
    sandbox.kill()
```

### 6. 网络与出网策略（凭证保险柜）

在完成第 3 步配置后，在 `examples/opencode-integration/` 目录里跑：

```bash
cd examples/opencode-integration
python network_policy.py
```

脚本演示了共享集群的推荐模式：默认拒绝出网 + 链路上注入 Key。

```python
# 凭证注入走原生 cubesandbox SDK（见 security-proxy.md）。
from cubesandbox import Sandbox, Rule, Match, Action, Inject

host = "api.anthropic.com"
rules = [
    Rule(
        name="allow_anthropic_llm",
        match=Match(scheme="https", sni=host, host=host),
        action=Action(allow=True, audit="metadata", inject=[
            Inject(header="x-api-key", secret=ANTHROPIC_API_KEY, format="${SECRET}"),
            Inject(header="anthropic-version", secret="2023-06-01", format="${SECRET}"),
        ]),
    ),
]

sandbox = Sandbox.create(
    template=CUBE_TEMPLATE_ID,
    allow_internet_access=False,   # 默认拒绝；规则中的 host 自动放行
    network={"rules": rules},
)
```

效果：

- 沙箱内 `printenv ANTHROPIC_API_KEY` 只能看到一个占位值。
- 每次访问 LLM host 都会在链路上被加上鉴权头。
- 其它目的地址在 L3/L4 由 CubeVS 直接丢弃（`allow_internet_access=False`），
  根本不会离开沙箱。
- 每次放行/拒绝都会落到出网审计日志中。

非 Anthropic provider 用 `Authorization: Bearer` 头注入。如果某个 provider 不接
受在链路上注入头，那就退回直连方式（`envs=...`）—— 但永远不要把 Key 写进沙
箱内的持久文件。

## 使用场景与最佳实践

- **隔离开发。** 把编码 Agent 跑在沙箱里，让它的文件编辑与 shell 命令无法触
  碰宿主机。
- **执行 Agent 生成的代码并回收结果。** 让 Agent 把产出写到 `/workspace`，然
  后用 `sandbox.files` 或 `commands.run` 把产物读回来。
- **长任务的断点续跑。** 用 `pause()` + `connect()` 给一次长重构打快照，之后
  再恢复；也可以从同一份快照 fork 出多个变体。
- **不改镜像就切换 LLM provider。** OpenCode 自身由上游环境变量决定
  （`ANTHROPIC_API_KEY`、`OPENAI_API_KEY`、...）；通过 `OPENCODE_BASE_URL`
  指向任何 Anthropic / OpenAI 兼容端点即可，模板无需重建。
- **重型依赖预装进模板。** 默认拒绝策略下，运行时再去拉依赖会很慢，建议在
  镜像里把常用工具链装好。
- **OpenCode 与 MCP 协同。** OpenCode 原生支持 MCP，可以在同一份模板里跑
  一个 MCP server，让 OpenCode 调用它做文件系统、浏览器或数据库访问——出
  网策略仍然适用。

## 关键代码片段

### 无头调用 OpenCode

```python
cmd = (
    "cd /workspace && opencode run --dangerously-skip-permissions -m claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=opencode_env, timeout=900)
```

### 启动前版本检查

```python
version = sandbox.commands.run("opencode --version", timeout=60)
```

## 注意事项

- **Node.js 版本。** OpenCode 要求 Node 18.20+；基础镜像自带的是较老的 apt
  Node，请走 NodeSource 安装（Dockerfile 已处理）。
- **非交互模式需要 `--dangerously-skip-permissions`。** `opencode run` 默
  认会对每次工具调用询问权限，非交互模式下无法回答。`run_opencode.py`
  在调用前会带上这个 flag；在更高安全等级的场景里，请通过 `opencode.json`
  收紧白名单。（OpenCode < 1.17 没有这个标志，需要用
  `OPENCODE_PERMISSION='{"*":"allow"}'` 环境变量替代。）
- **Agent 状态目录。** OpenCode 把状态拆成两个目录：
  `/workspace/.opencode/config/`（配置）和
  `/workspace/.opencode/data/`（会话、auth.json、storage）。Dockerfile
  会创建这两个空目录，但不会写入任何凭据。
- **直连方式的 Key 残留。** 直连（`envs=`）下 Key 只在该 exec 调用期间有效，
  但 OpenCode 可能把 provider 凭据缓存在数据目录
  （`/workspace/.opencode/data/auth.json`）里，会跨 `pause()` / `resume()`
  存活。严格隔离场景请用保险柜方式（`network_policy.py`），让 Key 完全不进
  入 VM。
- **CubeEgress 拦截 CA（Node）。** 保险柜方式要求沙箱信任 CubeEgress 根 CA，
  基础镜像把它装进了系统 CA 包。OpenCode 是 Node.js 包，忽略系统 CA 库，因
  此 `network_policy.py` 还会设 `NODE_EXTRA_CA_CERTS`（可用
  `OPENCODE_NODE_EXTRA_CA_CERTS` 覆盖）—— 否则 vault 路径会以
  `unable to verify the first certificate` 失败。
- **出网副作用。** `npm install`、拉 MCP 工具等任务需要把这些 host 加进放行
  规则，或预装进模板。
- **交互式 TTY 特性。** OpenCode 的交互 TUI 走不了 E2B 协议。请用
  `opencode run` 走无头模式，多轮对话由宿主端脚本驱动（`-c` / `-s <id>` 续
  接，`--session-id` 钉住会话 id）。
- **Provider 反推规则。** `env_utils.provider()` 通过当前已配的密钥环境变量
  反推 provider。使用自定义网关时，请显式设置 `OPENCODE_PROVIDER` 绕过按子
  串匹配的启发式。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| preflight 报 `opencode: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| 权限弹窗卡住整个 run | 未带 `--dangerously-skip-permissions` 标志，且运行会读写/执行命令 | 加上 flag，或在 `opencode.json` 里收紧 permissions |
| `unknown flag: --dangerously-skip-permissions` | OpenCode 版本早于 1.17 | 用 `--build-arg OPENCODE_VERSION=1.17.20` 重建镜像，或改用 `OPENCODE_PERMISSION='{"*":"allow"}'` 环境变量 |
| OpenCode 报 `model not found` | `OPENCODE_MODEL` 与当前 provider 不匹配 | 显式设置 `OPENCODE_MODEL`，或用 OpenCode 的 `provider/model` 简写通过 `-m anthropic/claude-sonnet-4-6` 传入 |
| provider 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（vault） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host（及所需其他 host）加入规则 |
| vault 路径下 OpenCode 报 `Connection error` / TLS 失败 | OpenCode 是 Node.js 包，忽略系统 CA 库，不信任 CubeEgress 拦截 CA | 脚本已把 `NODE_EXTRA_CA_CERTS` 指向系统 CA 包；若 CA 在别处，用 `OPENCODE_NODE_EXTRA_CA_CERTS` 覆盖 |
| 模板创建卡在 `PULLING` | registry 无法被 Cube 节点访问 | 推送到集群可达的 registry，或传入鉴权参数 |
| 就绪探针超时 | 镜像缺少 envd | 确认 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` 报错 | 平台版本过低不支持快照 | 升级 CubeSandbox 平台 |

## 参考资料

- 可运行示例：[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- 引入自有镜像：[`docs/zh/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- 从镜像创建模板：[`docs/zh/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- 凭证保险柜 + 出网管控：[`docs/zh/guide/security-proxy.md`](../security-proxy.md)
- OpenCode CLI：<https://opencode.ai/>
- OpenCode 文档：<https://opencode.ai/docs/>
- OpenCode 仓库：<https://github.com/anomalyco/opencode>
