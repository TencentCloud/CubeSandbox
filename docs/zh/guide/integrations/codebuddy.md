---
title: CodeBuddy Code 集成指南
author: pei-pei45
date: 2026-07-16
tags:
  - integration
  - codebuddy
  - coding-agent
  - agent
lang: zh-CN
---

# CodeBuddy Code 集成指南

[English](../../guide/integrations/codebuddy.md)

在 CubeSandbox MicroVM 内运行 [腾讯云 CodeBuddy Code CLI](https://www.codebuddy.ai/docs/cli/README)
（面向终端的 AI 编码 Agent）。本文涵盖镜像构建、密钥注入、出网管控与基于快照的会话持久化，并配套可运行的
[`examples/codebuddy-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/codebuddy-integration)
示例项目。

## 集成对象与版本

| 组件 | 版本 |
|---|---|
| CodeBuddy Code | `@tencent-ai/codebuddy-code`（通过 `--build-arg CODEBUDDY_VERSION=x.y.z` 固定） |
| Node.js | 20（通过 NodeSource 安装） |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK（宿主端驱动） | `e2b`（最新版） |
| CubeSandbox 平台 | `>= 0.3.0`（pause/resume）/ `>= 0.4.0`（CubeEgress 凭证保险柜） |

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 集群拉取。
- 一个 CodeBuddy Code 账号，或自定义上游 API Key。CodeBuddy Code 可对接：
  国际版平台（`CODEBUDDY_INTERNET_ENVIRONMENT=io`，默认）、国内版平台（`internal`）、
  iOA 企业版平台（`ioa`），或通过 `CODEBUDDY_BASE_URL` / `ANTHROPIC_BASE_URL` 指向任何
  Anthropic / OpenAI 兼容端点。
- Python 3.10+（宿主端驱动脚本）。

## 为什么把 CodeBuddy 跑在沙箱里

CodeBuddy Code 是一款终端 Agent，会编辑文件、执行命令、安装软件包。直接在工作站上运行，会把
Agent 的破坏半径和你的开发环境混在一起。把它放到 CubeSandbox 里能获得：

| 关注点 | CubeSandbox 提供 |
|---|---|
| **隔离** | 每个会话独占一台 KVM MicroVM，独占 guest 内核 |
| **可复现** | 每个会话都从同一份模板快照启动 |
| **快速启动** | 冷启动 < 60ms，并行 N 个 Agent 几乎无成本 |
| **长任务** | `sandbox.pause()` 对 VM + rootfs 打快照，之后再恢复 |
| **密钥卫生** | CubeEgress 在链路上注入鉴权头，VM 内永远看不到真实 Key |
| **出网审计** | 每次访问 LLM API 都会记录在出网审计日志中 |

## 接入步骤

### 1. 构建模板镜像

镜像在 `cubesandbox-base` 之上叠加 Node.js 20 与 CodeBuddy CLI；envd 已经监听 `:49983`。

```dockerfile
# examples/codebuddy-integration/Dockerfile（节选）
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=20
ARG CODEBUDDY_VERSION=2.117.1

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

RUN npm install -g --omit=dev --ignore-scripts \
        "@tencent-ai/codebuddy-code@${CODEBUDDY_VERSION}" \
    && codebuddy --version \
    && rm -rf /root/.npm

# CodeBuddy 以非 root 用户运行。基础镜像已经用 uid=1000 作为 ``user`` exec 账号，
# 所以 UID/GID 由系统自动分配；pause/resume 快照按用户名保留身份。MicroVM 提供外层隔离，
# 这层降权是对 prompt injection 场景（LLM agent 被诱导执行 shell）的纵深防御。
#
# /workspace 设成 world-writable，因为 e2b exec 信道把用户名限制在 ``root`` 和 ``user``，
# 而 ``user`` 写不进 codebuddy 拥有的目录。CodeBuddy 状态目录放在
# /workspace/.codebuddy，pause/resume 时会跟项目文件一起被快照。
RUN groupadd --system codebuddy \
    && useradd  --system --gid codebuddy \
                --home-dir /home/codebuddy --shell /bin/bash \
                --no-create-home codebuddy \
    && install -d -o codebuddy -g codebuddy -m 0700 /home/codebuddy \
    && install -d -o codebuddy -g codebuddy -m 0777 /workspace

ENV CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    DISABLE_AUTOUPDATER=1 \
    DISABLE_FEEDBACK_COMMAND=1 \
    CODEBUDDY_INTERNET_ENVIRONMENT=io

WORKDIR /workspace
USER codebuddy
```

构建并推送（在仓库根目录运行，确保相对构建上下文 `examples/codebuddy-integration`
能正确解析）：

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/codebuddy-cube:latest \
  examples/codebuddy-integration
docker push <your-registry>/codebuddy-cube:latest
```

### 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/codebuddy-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务变为 `READY` 后记下 `template_id`，之后每次 `Sandbox.create()` 都传给它。中等任务用 `4G`
可写层就够；如果 Agent 需要安装较重的工具链，建议提升到 `8G+`。

### 3. 配置宿主端驱动

```bash
cd examples/codebuddy-integration
cp .env.example .env
# 填写 E2B_API_URL、CUBE_TEMPLATE_ID、CODEBUDDY_INTERNET_ENVIRONMENT 以及 provider key
pip install -r requirements.txt
```

| 变量 | 作用位置 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址（`http://<node>:3000`） |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空字符串 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自第 2 步 |
| `CODEBUDDY_INTERNET_ENVIRONMENT` | CodeBuddy CLI | `io`（默认，国际版）、`internal`（国内）、`ioa`（腾讯企业版） |
| `CODEBUDDY_MODEL` / `CODEBUDDY_BASE_URL` | CodeBuddy CLI | 模型 id 与可选的自定义上游端点 |
| `CODEBUDDY_API_KEY` / `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / ... | `envs=...`（直连）或 CubeEgress 注入（vault） | provider 密钥 |
| `CODEBUDDY_LLM_HOST` | `network_policy.py` | 默认拒绝下放行的 LLM host |

### 4. 运行时配置与 API Key 注入

CodeBuddy 以无交互模式启动：`-p` 让它处理完 prompt 即退出（不进入交互 TUI），配合 `-y` /
`--dangerously-skip-permissions`（任何会读写文件或执行命令的非交互运行都必须带，否则 CLI 会
卡在无法在 exec 信道回答的权限弹窗上）。prompt 作为末尾的位置参数。两种密钥注入方式共用同一
份模板：

**直连方式** —— 每个命令注入一次 Key。`e2b` 的 `commands.run(envs=...)` 把环境放进 exec 信
封，而不是写入 VM 内的持久文件，所以 Key 只在该命令执行期间存在。exec 信道只接受
`root` 和 `user` 两个用户名；传 `user="codebuddy"` 会触发 `invalid username: 'codebuddy'`。
镜像内 Agent 仍以非 root 身份运行（因为 Dockerfile 有 `USER codebuddy`），SDK 层面的
`user` 参数只约束 exec 信道的身份，不影响容器内进程身份：

```python
result = sandbox.commands.run(
    "cd /workspace && codebuddy -p -y --model claude-sonnet-4-6 'do something'",
    envs={"ANTHROPIC_API_KEY": key},
    user="user",
    timeout=900,
)
```

**保险柜方式** —— 让 Key 完全不进入 VM（见第 6 步）。

### 5. 会话持久化（pause / resume）

```bash
cd examples/codebuddy-integration
python resume_codebuddy.py
```

与 [快照 / 克隆 / 回滚](../snapshot-rollback-clone.md) 引擎在 SDK 层等价：

- `sandbox.pause()` 对运行中的 VM（内存 + rootfs）打快照并释放算力。
- `Sandbox.connect(sandbox_id)` 恢复时，`/workspace`、`/workspace/.codebuddy` 以及其它所有
  文件都保留。第二轮再调用 `codebuddy -p -y -c`，让 CodeBuddy 自动续接
  `$CODEBUDDY_CONFIG_DIR/projects/` 下最近一次会话。

> **生命周期注意事项：** 用 `try/finally` 管理沙箱生命周期，不要用 `with Sandbox.create(...)`
> context manager。`__exit__` 会 `kill` 沙箱，把 pause 撤销。示例里显式创建沙箱，只在
> `finally` 中调用 `sandbox.kill()`。

```python
sandbox = Sandbox.create(template=template_id, timeout=1800)
try:
    run_turn(sandbox, prompt_1)          # 写入 /workspace/plan.md
    sandbox_id = sandbox.pause() or sandbox.sandbox_id
    sandbox = Sandbox.connect(sandbox_id)
    assert_state_survived(sandbox)       # /workspace + /workspace/.codebuddy 完好
    run_turn(sandbox, prompt_2, continue_session=True)
finally:
    sandbox.kill()
```

### 6. 网络与出网策略（凭证保险柜）

在完成第 3 步配置后，在 `examples/codebuddy-integration/` 目录里跑：

```bash
cd examples/codebuddy-integration
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
- 其它目的地址在 L3/L4 由 CubeVS 直接丢弃（`allow_internet_access=False`），根本不会离开沙箱。
- 每次放行/拒绝都会落到出网审计日志中。

非 Anthropic provider 用 `Authorization: Bearer` 头注入。如果某个 provider 不接受在链路上注入
头，那就退回直连方式（`envs=...`）—— 但永远不要把 Key 写进沙箱内的持久文件。

## 使用场景与最佳实践

- **隔离开发。** 把编码 Agent 跑在沙箱里，让它的文件编辑与 shell 命令无法触碰宿主机。
- **执行 Agent 生成的代码并回收结果。** 让 Agent 把产出写到 `/workspace`，然后用
  `sandbox.files` 或 `commands.run` 把产物读回来。
- **长任务的断点续跑。** 用 `pause()` + `connect()` 给一次长重构打快照，之后再恢复；也
  可以从同一份快照 fork 出多个变体。
- **不改镜像就切换 LLM provider。** CodeBuddy 自身由 `CODEBUDDY_INTERNET_ENVIRONMENT` +
  `CODEBUDDY_API_KEY` 决定上游；通过 `CODEBUDDY_BASE_URL` 指向 Anthropic / OpenAI /
  DeepSeek / Gemini 等即可，模板无需重建。
- **重型依赖预装进模板。** 默认拒绝策略下，运行时再去拉依赖会很慢，建议在镜像里把常用工具链
  装好。

## 关键代码片段

### 无头调用 CodeBuddy

```python
cmd = (
    "cd /workspace && codebuddy -p -y --model claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=codebuddy_env, timeout=900)
```

### 启动前版本检查

```python
version = sandbox.commands.run("codebuddy --version", timeout=60)
```

## 注意事项

- **Node.js 版本。** CodeBuddy 要求 Node 18.20+；基础镜像自带的是较老的 apt Node，请走
  NodeSource 安装（Dockerfile 已处理）。
- **非交互模式需要预置 Key。** `codebuddy -p` 不会回落到浏览器登录流程；忘了设
  `CODEBUDDY_API_KEY`（或对应的 provider 环境变量）就会卡在认证弹窗上。`run_codebuddy.py`
  在启动沙箱前就会因为找不到 Key 而直接报错。
- **权限模式。** `-y` 会跳过所有工具调用弹窗，这是非交互执行所必需的，因为 exec 信道无
  法回答弹窗。在更高安全等级的场景里，请通过 `settings.json`（`permissions.defaultMode`、
  `permissions.allow` 等）收紧白名单，而不是去掉 `-y`。
- **Agent 状态目录。** `/workspace/.codebuddy` 存放 CodeBuddy 的会话缓存（配置、历史、会话、
  计划、文件历史）。在镜像里请保持空目录，避免租户之间的会话泄露：Dockerfile 会创建该目录，
  但不会写入任何凭据。
- **直连方式的 Key 残留。** 直连（`envs=`）下 Key 只在该 exec 调用期间有效，但 CodeBuddy
  可能把 provider 凭据缓存在状态目录（`/workspace/.codebuddy/`）里，会跨 `pause()` /
  `resume()` 存活。严格隔离场景请用保险柜方式（`network_policy.py`），让 Key 完全不进入 VM。
- **CubeEgress 拦截 CA（Node）。** 保险柜方式要求沙箱信任 CubeEgress 根 CA，基础镜像把它装
  进了系统 CA 包。CodeBuddy 是 Node.js 包，忽略系统 CA 库，因此 `network_policy.py` 还会
  设 `NODE_EXTRA_CA_CERTS`（可用 `CODEBUDDY_NODE_EXTRA_CA_CERTS` 覆盖）—— 否则 vault
  路径会以 `unable to verify the first certificate` 失败。
- **出网副作用。** `npm install`、拉 MCP 工具等任务需要把这些 host 加进放行规则，或预装
  进模板。
- **交互式 TTY 特性。** CodeBuddy 的交互 TUI 走不了 E2B 协议。请用 `-p -y` 走无头模式，
  多轮对话由宿主端脚本驱动（`-c` / `--resume` 续接，`--session-id` 钉住会话 id）。

## 排错

| 现象 | 可能原因 | 处理 |
|---|---|---|
| preflight 报 `codebuddy: command not found` | CLI 变更后未重建模板 | 重建镜像并重新注册模板 |
| 启动时弹出登录浏览器界面卡住 | `-p` 模式要求预置 API Key，不会回落到交互登录 | 启动前设置 `CODEBUDDY_API_KEY`（或对应的 provider 环境变量） |
| 权限弹窗卡住整个 run | 忘了在会读写/执行命令的运行上加 `-y` / `--dangerously-skip-permissions` | 加 `-y`，或在 `settings.json` 里收紧 permissions |
| provider 鉴权失败 | 密钥未传入（直连）或缺少 inject 规则（vault） | 传 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝且无匹配放行规则 | 把 LLM host（及所需其他 host）加入规则 |
| vault 路径下 CodeBuddy 报 `Connection error` / TLS 失败 | CodeBuddy 是 Node.js 包，忽略系统 CA 库，不信任 CubeEgress 拦截 CA | 脚本已把 `NODE_EXTRA_CA_CERTS` 指向系统 CA 包；若 CA 在别处，用 `CODEBUDDY_NODE_EXTRA_CA_CERTS` 覆盖 |
| 模板创建卡在 `PULLING` | registry 无法被 Cube 节点访问 | 推送到集群可达的 registry，或传入鉴权参数 |
| 就绪探针超时 | 镜像缺少 envd | 确认 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` 报错 | 平台版本过低不支持快照 | 升级 CubeSandbox 平台 |

## 参考资料

- 可运行示例：[`examples/codebuddy-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/codebuddy-integration)
- 引入自有镜像：[`docs/zh/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- 从镜像创建模板：[`docs/zh/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- 快照 / 克隆 / 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- 凭证保险柜 + 出网管控：[`docs/zh/guide/security-proxy.md`](../security-proxy.md)
- CodeBuddy Code CLI：<https://www.codebuddy.ai/docs/cli/README>
- CodeBuddy Code 安装：<https://www.codebuddy.ai/docs/cli/installation>
- CodeBuddy Code 环境变量：<https://www.codebuddy.ai/docs/cli/env-vars>
- CodeBuddy Code 目录结构（`~/.codebuddy`）：<https://www.codebuddy.ai/docs/cli/codebuddy-dir>
