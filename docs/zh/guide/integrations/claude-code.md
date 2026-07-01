---
title: Claude Code 集成指南
author: LangQi99
date: 2026-07-01
tags:
  - integration
  - claude-code
  - anthropic
  - agent
lang: zh-CN
---

# Claude Code 集成指南

[English](../../../guide/integrations/claude-code.md)

在 CubeSandbox MicroVM 内运行 [Anthropic Claude Code](https://docs.anthropic.com/en/docs/claude-code)（面向终端的 AI 编码 Agent）。本文覆盖从镜像构建到生产级出口管控的完整链路，配套的可运行示例位于
[`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)。

## 集成对象与版本

| 组件 | 版本 |
|---|---|
| Claude Code CLI | `@anthropic-ai/claude-code`（构建时最新，可通过 `--build-arg CLAUDE_CODE_VERSION=x.y.z` 固定） |
| Node.js | 20 LTS |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK（宿主端驱动） | `e2b >= 2.4.1` |
| CubeSandbox 平台 | `>= 0.3.0`（pause/resume）/ `>= 0.4.0`（CubeEgress 密钥保险柜） |

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可访问（`http://<node>:3000`）
- `cubemastercli` 已安装且已连通集群
- 有 Docker 的构建机，且 registry 能被 Cube 集群拉取
- Anthropic API Key（`sk-ant-...`）；或兼容网关（Bedrock、Vertex、自建 Anthropic 代理）
- Python 3.10+（宿主端驱动脚本）

## 为什么要把 Claude Code 放进沙箱

Claude Code 是一个会编辑文件、执行命令、安装依赖包的终端 Agent。直接跑在开发机上，Agent 的
"爆炸半径" 就等于你的开发环境。放进 CubeSandbox 你能拿到：

| 关注点 | CubeSandbox 提供 |
|---|---|
| **隔离** | 每个会话一个 KVM MicroVM，独立 guest kernel |
| **可复现** | 每次会话都从同一个 template 快照启动 |
| **秒起** | 冷启动 <60ms，N 路并行代价极小 |
| **长任务** | `sandbox.pause()` 快照 VM+rootfs，稍后可恢复 |
| **密钥卫生** | CubeEgress 出口注入 `x-api-key`，VM 永远看不到明文 |
| **出口审计** | 每一次访问 `api.anthropic.com` 都写入 JSONL 审计日志 |

## 架构

```
┌────────────────────────┐        ┌───────────────────────┐
│  宿主端驱动脚本         │  E2B  │  CubeSandbox MicroVM  │
│  (run_claude.py)        │       │                       │
│                         │──────►│  envd (:49983)        │
│  ANTHROPIC_API_KEY      │       │  claude CLI (Node 20) │
│  可选                   │       │  git / python / rg    │
│                         │       │  /workspace           │
└──────────┬──────────────┘       └───────────┬───────────┘
           │                                  │ HTTPS
           │                                  ▼
           │                           ┌───────────────┐
           └── 注入规则 ─────────────► │  CubeEgress   │───► api.anthropic.com
                                       └───────────────┘
                                              │
                                              ▼
                                  /data/log/cube-egress/access.jsonl
```

有两种可互换的集成模式：

| 模式 | Key 走向 | 适用场景 |
|---|---|---|
| **直连** | 每条命令通过 `envs={"ANTHROPIC_API_KEY": key}` 下发 | 单人开发机、快速验证 |
| **保险柜** | 通过 `Sandbox.create(network={"rules": [inject 规则]})` 在出口挂载 | 多租集群、托管服务、审计要求高的场景 |

两种模式使用相同的 template；差别只是 Key 是进 VM，还是由 CubeEgress 在出口挂载。

## 接入步骤

### 1. 构建 template 镜像

镜像在 `cubesandbox-base` 之上加装 Node.js 20 与 Anthropic 官方 CLI，envd 已经在
`:49983` 监听。

```dockerfile
# examples/claude-code-integration/Dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg git jq ripgrep less \
      python3 python3-pip build-essential \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code \
    && claude --version

ENV CLAUDE_CODE_HOME=/root/.claude
WORKDIR /workspace
EXPOSE 49983
```

构建并推送：

```bash
docker build -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
docker push <your-registry>/claude-code-cube:latest
```

### 2. 注册为 Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

`READY` 之后拿到 `template_id`，后续每次 `Sandbox.create()` 都会用到它。中等任务
`4G` 可写层足够；如果 Agent 会安装大型工具链，请拉到 `8G` 或更大。

### 3. 配置宿主端驱动

```bash
cd examples/claude-code-integration
cp env.example .env
# 填 E2B_API_URL、CUBE_TEMPLATE_ID、ANTHROPIC_API_KEY
pip install -r requirements.txt
```

直连模式冒烟测试：

```bash
python run_claude.py --prompt "Create hello.py that prints 'Hello from CubeSandbox!' and run it."
```

### 4. 使用 pause / resume 支撑长任务

```bash
python resume_claude.py
```

这在 SDK 层直接映射到 [快照 · 克隆 · 回滚](../snapshot-rollback-clone.md) 引擎：

- `sandbox.pause()` 把运行中 VM 的内存 + rootfs 快照落盘并释放算力；
- `Sandbox.connect(sandbox_id)` 从快照恢复，`/workspace`、`/root/.claude/` 等所有文件保留；
- 从同一个快照多次 `connect` 可派生多个变体分支，无需重跑初始化。

### 5. 保险柜模式：Key 永远不进 VM

`network_policy.py` 展示多租集群推荐做法：

```python
rules = [
    {
        "name": "allow_anthropic_api",
        "match": {
            "scheme": "https",
            "sni": "api.anthropic.com",
            "host": "api.anthropic.com",
        },
        "action": {
            "allow": True,
            "audit": "metadata",
            "inject": [
                {"header": "x-api-key",         "format": "${SECRET}",
                 "secret": ANTHROPIC_API_KEY},
                {"header": "anthropic-version", "format": "2023-06-01"},
            ],
        },
    },
]

with Sandbox.create(
    template=CUBE_TEMPLATE_ID,
    allow_internet_access=True,
    network={"rules": rules},
) as sandbox:
    ...
```

效果：

- 沙箱内 `printenv | grep ANTHROPIC_API_KEY` 什么都看不到；
- 每次访问 `api.anthropic.com` 都会在出口挂上 `x-api-key`；
- 其他一切默认拒绝，直接返回 `403 Forbidden - CubeEgress`，请求根本不出网；
- 每一次 allow / deny 都会写入 `/data/log/cube-egress/access.jsonl`。

## 关键代码片段

### 无头 `claude` 调用

用 `--print`（关闭交互式 TUI）配 `--allowedTools`（跳过白名单命令的授权提示）：

```python
cmd = (
    "cd /workspace && claude --print "
    "--allowedTools 'Bash(npm:*),Bash(node:*),Bash(python3:*),Edit,Write,Read' "
    f"-- {shlex.quote(prompt)}"
)
result = sandbox.commands.run(
    cmd, envs={"ANTHROPIC_API_KEY": key}, user="root", timeout=300,
    on_stdout=lambda m: sys.stdout.write(m),
    on_stderr=lambda m: sys.stderr.write(m),
)
```

需要机读事件流时加 `--verbose --output-format stream-json`（每一步作为一个 JSON 对象输出）。

### 通过 `envs` 传递密钥（直连模式）

`e2b` 的 `commands.run(envs=...)` 把环境变量放进 exec 信封，不会写入沙箱里的持久 env 文件；
密钥只在这条命令的生命周期里存在：

```python
sandbox.commands.run(
    "claude --print -- 'do something'",
    envs={"ANTHROPIC_API_KEY": key, "ANTHROPIC_MODEL": "claude-sonnet-4-5"},
    user="root",
)
```

### 上传 seed 项目

```python
sandbox.files.write(
    f"{workspace}/{Path(seed).name}",
    Path(seed).read_bytes(),
    user="root",
)
```

### 在任务前后 pause / resume

```python
sandbox = Sandbox.create(template=template_id, timeout=1800)
run_claude(sandbox, prompt_1)
sandbox.pause()

# ... 数小时后 ...

sandbox = Sandbox.connect(sandbox_id)
run_claude(sandbox, prompt_2)  # /workspace 与 /root/.claude 都保留
```

## 注意事项

- **Node.js 版本**：Claude Code 需要 Node ≥ 18。基础镜像是 Ubuntu 22.04，apt 自带的 Node
  过旧，务必走 NodeSource 安装。
- **Agent 状态目录**：`/root/.claude/` 存的是 Claude Code 的本地会话缓存。把它烧进镜像可能
  让上一租户的会话泄漏到下一租户；示例 Dockerfile 特意留空。
- **`--dangerously-skip-permissions`**：仅在你完全接受沙箱风险时使用，优先用显式的
  `--allowedTools` 白名单。
- **CubeEgress CA**：保险柜模式要求沙箱信任 CubeEgress 根证书。使用
  `cubemastercli tpl create-from-image` 时默认会烧进去；若你显式设了 `--with-cube-ca=false`，
  需要在 Agent env 里设置 `SSL_CERT_FILE` 指向正确的 bundle。
- **出口副作用**：某些任务会 `npm install` 或拉取 MCP 工具——要么把它们预装进 template，
  要么把 `registry.npmjs.org`（以及具体 MCP host）加进 allow 规则。
- **交互式 TTY 能力**：Claude Code 的 TUI（多行编辑器、`/` 命令）走 E2B 协议无法使用，
  统一用 `--print` 无头模式，多轮对话由宿主脚本编排。
- **network-agent 自动挂起**：设置 `on_timeout: pause, auto_resume: True` 后，平台会自动
  挂起空闲沙箱并在下一次请求到达时唤醒——参见示例
  [`auto-resume.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-resume.py)。
- **Template 可写层大小**：仅 npm 缓存就可能几百 MB。`4G` 是安全默认值；长时间大依赖重构可能需要 `8G` 以上。

## 常见问题

| 现象 | 可能原因 | 解决 |
|---|---|---|
| 预检 `claude: command not found` | CLI 升级后 template 未重建 | 重构镜像并重新注册 template |
| `Invalid API key · Please run /login` | 直连模式没传 Key，或保险柜规则没生效 | 加 `envs={"ANTHROPIC_API_KEY": ...}`，或修正规则的 `sni` / `host` |
| `403 Forbidden — CubeEgress` | 默认拒绝但没有 allow 规则 | 添加 `Match(sni="api.anthropic.com", scheme="https")` |
| 访问 `api.anthropic.com` SSL 握手失败 | 沙箱不信任 CubeEgress 根证书 | 用 `--with-cube-ca=true`（默认）重建 template；或在沙箱内正确配置 `SSL_CERT_FILE` |
| Template 创建卡在 `PULLING` | Cube 节点拉不到 registry | 换 registry 或提供 `--registry-username/--registry-password` |
| 就绪探测超时 | 基础镜像没有 envd | 确保 `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| Agent 卡在 Bash 工具确认 | 没配 `--allowedTools`，也没有 TTY | 一律用 `--print --allowedTools '...'` |
| `sandbox.pause()` 在 0.2.x 报错 | 快照引擎需要 0.3.0+ | 升级 CubeSandbox 平台 |

## 参考资料

- 可运行示例：[`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
- 自带镜像：[`docs/zh/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template 流水线：[`docs/zh/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- 快照 · 克隆 · 回滚：[`docs/zh/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- 密钥保险柜与出口管控：[`docs/zh/guide/security-proxy.md`](../security-proxy.md)
- Claude Code CLI 参考：<https://docs.anthropic.com/en/docs/claude-code/cli-reference>
- Claude Code SDK / 无头模式：<https://docs.anthropic.com/en/docs/claude-code/sdk>
