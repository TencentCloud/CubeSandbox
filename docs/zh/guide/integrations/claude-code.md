---
title: Claude Code 集成指南
author: dcdc4747
date: 2026-07-29
tags:
  - integration
  - claude-code
  - coding-agent
  - agent
lang: zh-CN
---

# Claude Code 集成指南

[English](../../../guide/integrations/claude-code.md)

在 CubeSandbox MicroVM 内运行 [Claude Code](https://docs.anthropic.com/en/docs/claude-code)
（Anthropic 的 AI 编程 CLI 工具）。本文覆盖镜像构建、密钥注入、出网管控，以及
基于快照的会话持久化，配套的可运行示例位于
[`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)。

## 集成对象与版本

| 组件 | 版本 |
|---|---|
| Claude Code CLI | `@anthropic-ai/claude-code`（通过 `--build-arg CLAUDE_CODE_VERSION=x.y.z` 固定） |
| Node.js | 24（通过 NodeSource 安装） |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK（宿主端驱动） | `e2b`（最新） |
| CubeSandbox 平台 | `>= 0.3.0`（pause/resume）/ `>= 0.4.0`（CubeEgress 密钥保险柜） |

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可通过 `http://<node>:3000` 访问。
- `cubemastercli` 已加入 `$PATH`，已连接集群。
- 构建工作站安装 Docker，且有 Cube 节点可拉取的镜像仓库。
- LLM 供应商 API 密钥。默认使用 Anthropic；任何兼容 Anthropic 协议的
  端点（如 DeepSeek）都可以通过 `ANTHROPIC_BASE_URL` 和 `ANTHROPIC_AUTH_TOKEN` 接入。
- Python 3.10+ 用于宿主端驱动脚本。

## 为什么要在沙箱中运行 Claude Code

Claude Code 会编辑文件、执行 Shell 命令、安装软件包，并可自主串联工具调用。
直接在开发工作站上运行会将 Agent 的爆炸半径与本地开发环境混在一起。在
CubeSandbox 中运行带来以下好处：

| 关注点 | CubeSandbox 提供的解决方案 |
|---|---|
| **隔离性** | KVM MicroVM 每会话独立，专属客户内核 |
| **可复现性** | 每个会话从相同的模板快照启动 |
| **快速启动** | 冷启动低于 60 毫秒，并行 Agent 成本低 |
| **长任务支持** | `sandbox.pause()` 对 VM + rootfs 做快照，稍后恢复 |
| **密钥安全** | CubeEgress 在线注入认证请求头——VM 永不见真实密钥 |
| **出口审计** | 每次对 Anthropic API 的请求均记录在出口审计日志中 |

## 接入步骤

### 1. 构建模板镜像

镜像在 `cubesandbox-base` 之上叠加 Node.js 24 和 Claude Code CLI，envd 已在
`:49983` 上监听。

```dockerfile
# examples/claude-code-integration/Dockerfile（节选）
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=24
ARG CLAUDE_CODE_VERSION=latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg jq less procps python3 python3-pip ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version \
    && npm cache clean --force \
    && rm -rf /root/.npm /var/lib/apt/lists/*

WORKDIR /workspace
EXPOSE 49983
```

构建并推送：

```bash
# 国内用户可使用南京大学镜像：
#   --build-arg CUBE_BASE_IMAGE=ghcr.nju.edu.cn/tencentcloud/cubesandbox-base:2026.16
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
docker push <your-registry>/claude-code-cube:latest
```

### 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

任务状态变为 `READY` 后，记录 `template_id`——后续每次 `Sandbox.create()` 调用
都需要它。`4G` 可写层适合中等规模任务；如需安装大型工具链（如 Claude Code 插件、
MCP 服务器），可上调至 `8G+`。

### 3. 配置宿主端驱动

```bash
cd examples/claude-code-integration
cp .env.example .env
# 填入 E2B_API_URL、CUBE_TEMPLATE_ID 和你的 API 密钥
pip install -r requirements.txt
```

| 变量 | 流向 | 说明 |
|---|---|---|
| `E2B_API_URL` | 本地进程 | CubeAPI 地址 (`http://<node>:3000`) |
| `E2B_API_KEY` | 本地进程 | 本地开发填任意非空值即可 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 从步骤 2 获取 |
| `ANTHROPIC_API_KEY` | `envs=...`（直接模式）或 CubeEgress 注入（保险库模式） | API 密钥（标准方式） |
| `ANTHROPIC_AUTH_TOKEN` | `envs=...`（直接模式） | API 密钥（第三方兼容端点，如 DeepSeek） |
| `ANTHROPIC_BASE_URL` | 传入 exec 环境 | API 网关/兼容端点 |
| `CC_MODEL` | `--model` 参数 | 默认：`claude-sonnet-4-6` |
| `CC_EFFORT` | `--effort` 参数 | 努力级别：low, medium, high, xhigh, max |
| `CC_LLM_HOST` | `network_policy.py` | 默认拒绝出口下允许的 API 主机 |

### 4. 运行时配置与 API 密钥注入

Claude Code 以 headless 模式运行，使用 `-p`（处理提示词后退出，无交互式 TUI）
和 `--output-format json`（机器可读输出）。同一模板上支持两种密钥注入模式：

**直接模式** — 按命令转发密钥。`e2b` 的 `commands.run(envs=...)` 将环境变量
放入 exec 信包而非 VM 中的持久化文件，密钥仅在命令生命周期内有效：

```python
result = sandbox.commands.run(
    "cd /workspace && claude -p '重构 auth 模块' "
    "--output-format json --model claude-sonnet-4-6 "
    "--dangerously-skip-permissions",
    envs={"ANTHROPIC_API_KEY": key},
    user="user",
    timeout=900,
)
```

**保险库模式** — 密钥完全不出现在 VM 中（见步骤 6）。

示例脚本默认解析 JSON 流并打印简洁的文本摘要（助手文本、工具调用及错误）；
传入 `--raw`（或设置 `CC_STREAM_RAW=1`）可查看原始 JSON 事件流。

### 5. 会话持久化（暂停 / 恢复）

```bash
python resume_claude_code.py
```

这在 SDK 层面映射了 [快照/克隆/回滚](../snapshot-rollback-clone.md) 引擎：

- `sandbox.pause()` 对运行中的 VM（内存 + rootfs）做快照并释放计算资源。
- `Sandbox.connect(sandbox_id)` 恢复沙箱，`/workspace`、Claude Code 状态目录
  等所有文件完整保留（以非 root 用户运行时状态目录为 `/home/user/.claude`）。

> **生命周期提醒**：使用 `try/finally` 管理沙箱生命周期，而非
> `with Sandbox.create(...)` 上下文管理器。上下文管理器在 `__exit__` 时会
> 销毁沙箱，导致 pause 操作无效。示例采用显式创建，仅在 `finally` 中调用
> `sandbox.kill()`。

```python
sandbox = Sandbox.create(template=template_id, timeout=1800)
try:
    run_turn(sandbox, prompt_1)          # 写入 /workspace/plan.md
    sandbox_id = sandbox.pause() or sandbox.sandbox_id
    sandbox = Sandbox.connect(sandbox_id)
    assert_state_survived(sandbox)       # /workspace + 状态目录完好
    run_turn(sandbox, prompt_2)          # 继续工作
finally:
    sandbox.kill()
```

### 6. 网络与出口策略（凭证保险库）

`network_policy.py` 展示了共享集群的推荐模式：默认拒绝出口 + 在线密钥注入。

```python
# 凭证注入使用原生 cubesandbox SDK（详见 security-proxy.md）
from cubesandbox import Sandbox, Rule, Match, Action, Inject

host = "api.anthropic.com"
rules = [
    Rule(
        name="allow_anthropic_api",
        match=Match(scheme="https", sni=host, host=host),
        action=Action(allow=True, audit="metadata", inject=[
            Inject(header="x-api-key", secret=ANTHROPIC_API_KEY, format="${SECRET}"),
            Inject(header="anthropic-version", secret="2023-06-01", format="${SECRET}"),
        ]),
    ),
]

sandbox = Sandbox.create(
    template=CUBE_TEMPLATE_ID,
    allow_internet_access=False,   # 默认拒绝；规则中的主机自动放行
    network={"rules": rules},
)
```

效果：

- 沙箱内执行 `printenv ANTHROPIC_API_KEY` 只显示占位符。
- 每次对 `api.anthropic.com` 的请求，认证请求头被在线附加。
- 其他所有流量被 CubeVS 在 L3/L4 层丢弃（`allow_internet_access=False`），
  永不离开沙箱。
- 每条允许/拒绝决策均记入出口审计日志。

对于兼容 Anthropic 协议的网关（如 DeepSeek），调整规则中的 host 并设置
`ANTHROPIC_BASE_URL` 环境变量。

## 使用场景与最佳实践

- **隔离开发。** 将 Claude Code 运行在沙箱内，其文件编辑和 Shell 命令无法接触宿主机。
- **执行 Agent 生成的代码并收集结果。** 让 Claude Code 输出到 `/workspace`，
  再通过 `sandbox.files` 或 `commands.run` 回读产物。
- **长任务检查点/恢复。** 使用 `pause()` + `connect()` 对长时间重构任务做快照，
  稍后恢复，或从一个快照分叉出多个并行变体。
- **将重量级依赖预安装进模板**，尤其在默认拒绝出口策略下，避免运行时拉取。
- **批量处理。** 在并行沙箱中运行多个 Claude Code 实例，用于代码审查、迁移
  或分析流水线。

## 关键代码片段

### Headless Claude Code 调用

```python
cmd = (
    "cd /workspace && claude -p "
    "'检查项目，运行 app.py，并将摘要写入 result.md' "
    "--output-format json --model claude-sonnet-4-6 "
    "--dangerously-skip-permissions"
)
result = sandbox.commands.run(cmd, envs=cc_env, user="user", timeout=900)
```

### 预检版本检查

```python
version = sandbox.commands.run("claude --version", timeout=60)
```

### 使用特定的努力级别

```python
# 控制推理深度：low, medium, high, xhigh, max
cmd = (
    "claude -p '审计此代码的安全问题。' "
    "--output-format json --effort high"
)
```

### 设置自动化权限模式

```python
# plan: 编辑前询问（默认）；acceptEdits: 自动批准编辑；
# bypassPermissions: 完全自动（需非 root 用户）
cmd = (
    "claude -p '修复 src/ 中所有的 lint 错误' "
    "--output-format json --permission-mode acceptEdits"
)
```

### 跳过所有权限检查（仅限沙箱）

```python
# 推荐用于隔离沙箱。必须以非 root 用户运行；
# Claude Code 会拒绝 root/sudo 用户使用此选项。
cmd = (
    "claude -p '执行任务' "
    "--output-format json --dangerously-skip-permissions"
)
result = sandbox.commands.run(cmd, envs=cc_env, user="user", timeout=900)
```

## 注意事项

- **Node.js 版本。** Claude Code 需要较新的 Node 运行时；基础镜像自带的 apt 版
  Node 较旧，务必通过 NodeSource 安装（Dockerfile 已处理）。
- **Agent 状态目录。** 以 `user` 身份运行时（headless 自动化推荐方式），Claude
  Code 的会话缓存位于 `/home/user/.claude`。以 `root` 身份运行时则位于
  `/root/.claude`。镜像中保持为空以避免跨租户泄露会话；构建时创建但不填入任何凭据。
- **直接模式密钥持久化。** 直接模式 (`envs=`) 下的密钥作用域仅限于 exec 调用，
  但沙箱快照可能捕获 VM 内环境变量。如需严格隔离，请使用保险库模式
  （`network_policy.py`），密钥永不进入 VM。
- **CubeEgress CA（Node.js）。** 保险库模式下沙箱须信任 CubeEgress 根证书，
  基础镜像已安装至系统证书包。Claude Code 运行在 Node.js 上，Node.js 忽略
  系统证书库，因此 `network_policy.py` 同时设置了 `NODE_EXTRA_CA_CERTS`
  （可通过 `CC_NODE_EXTRA_CA_CERTS` 覆写）——缺少此项将导致保险库路径 TLS 失败。
- **仅支持 Headless 模式。** Claude Code 的交互式 TUI 无法在 E2B 协议上使用。
  请使用 `-p` / `--print` 配合 `--output-format json` 获取机器可读输出，并
  在宿主脚本中驱动多轮对话。
- **权限模式。** 在自动化沙箱环境中，推荐使用 `--dangerously-skip-permissions`
  （需非 root 用户）或 `--permission-mode acceptEdits`（root 可用）实现自主执行。
  默认的 `plan` 模式每次工具调用需要人工批准，不适用于 headless 沙箱场景。
  注意：`--dangerously-skip-permissions` 在 root 用户下会被 Claude Code 拒绝。
- **出口副作用。** 执行 `npm install` 或获取 MCP 工具的任务，需将相关主机加入
  白名单或预先安装到模板中。
- **API 速率限制。** Claude Code 与 Anthropic API 交互，受标准速率限制和 Token
  配额约束。高吞吐批量处理场景建议分布到多个 API Key 和沙箱中。

## 故障排除

| 症状 | 可能原因 | 解决方法 |
|---|---|---|
| 预检报 `claude: command not found` | CLI 变更后模板未重建 | 重建镜像，重新注册模板 |
| API 认证失败 | 密钥未转发（直接模式）或缺少注入规则（保险库模式） | 传入 `envs={...}` 或修正规则的 `sni`/`host` |
| `403 Forbidden - CubeEgress` | 默认拒绝出口且无匹配的允许规则 | 在规则中添加 `api.anthropic.com`（及其他所需主机） |
| Claude Code TLS / 连接错误（保险库模式） | Node.js 忽略系统 CA 证书库 | 按 `network_policy.py` 中所示设置 `NODE_EXTRA_CA_CERTS` |
| 模板创建卡在 `PULLING` | Cube 节点无法访问仓库 | 推送到集群可访问的仓库，必要时提供认证信息 |
| 就绪探针超时 | 基础镜像不包含 envd | 确保 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` 报错 | 平台版本过旧不支持快照 | 升级 CubeSandbox 平台 |
| Claude Code 挂起无输出 | 在非 TTY 通道上启动了 TUI 模式 | 始终在 headless 模式使用 `-p` / `--print` |
| Token 超限 | 任务规模超出配置的努力级别 | 降低 `--effort` 级别或拆分任务 |

## 参考资料

- 可运行示例：[`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
- 自定义镜像：[`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- 从镜像创建模板：[`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- 快照 / 克隆 / 回滚：[`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- 凭证保险库 + 出口管控：[`docs/guide/security-proxy.md`](../security-proxy.md)
- Claude Code：<https://docs.anthropic.com/en/docs/claude-code>
- E2B Claude Code 集成：<https://e2b.dev/docs/agents/claude-code>
