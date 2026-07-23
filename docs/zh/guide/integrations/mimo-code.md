---
title: MiMo Code 集成指南
author: Young-Allen
date: 2026-07-22
tags:
  - integration
  - mimo-code
  - coding-agent
  - agent
lang: zh-CN
---

# MiMo Code 集成指南

[English](../../../guide/integrations/mimo-code.md)

在 CubeSandbox MicroVM 中运行
[MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code)。本指南覆盖可复现模板、
无头 Agent 执行、MiMo Platform 鉴权、受限网络出口，以及跨 CubeSandbox 快照
续接同一个对话。

可运行实现位于
[`examples/mimo-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mimo-code-integration)。

## 集成目标与版本

| 组件 | 已测试版本 |
| --- | --- |
| MiMo Code | `@mimo-ai/cli@0.1.7` |
| MiMo 模型 | `mimo/mimo-v2.5-pro` |
| Node.js | 24（npm 安装运行时） |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK | `e2b>=2.4.1` |
| CubeSandbox SDK | `cubesandbox>=0.3.0` |
| CubeSandbox 平台 | `>= 0.3.0` 支持 pause/resume；`>= 0.4.0` 支持 CubeEgress |

MiMo Code 基于 OpenCode 演进，并加入持久记忆、上下文 Checkpoint、子 Agent 编排和
Compose 工作流。本集成使用 MiMo 自身的 CLI、统一 profile、NDJSON 事件、
sessionID 和 Compose 模式，而不是再次实现通用 OpenCode executor 或插件。

## 前置条件

- CubeSandbox 已运行，且 CubeAPI 可访问；
- 构建机上有 `cubemastercli`、Docker 和节点可拉取的镜像仓库；
- Host runner 使用 Python 3.10+；
- 从 <https://platform.xiaomimimo.com> 获取 MiMo Platform API Key。

## 为什么在 CubeSandbox 中运行 MiMo Code

MiMo 可以修改文件、执行 Shell、安装依赖和启动子 Agent。MicroVM 把这些能力限制在
可丢弃环境中：

| 风险或需求 | CubeSandbox 机制 |
| --- | --- |
| Agent 命令隔离 | 独立 KVM MicroVM 和 Guest Kernel |
| 工具环境复现 | 固定版本模板 |
| 长任务连续性 | `pause()` 保存 VM 内存和根文件系统 |
| MiMo 状态连续性 | `$MIMOCODE_HOME` 与 `/workspace` 在恢复后保留 |
| 密钥隔离 | CubeEgress 在 VM 外注入真实 `api-key` |
| 网络管控 | 精确 Host 规则和默认拒绝出口 |

## 集成步骤

### 1. 构建模板

```bash
export MIMO_IMAGE="<your-registry>/mimo-code-cube:0.1.7"
./examples/mimo-code-integration/build-template.sh
cubemastercli tpl watch --job-id <job_id>
```

Dockerfile 固定 CLI 版本，并在构建时验证平台二进制：

```dockerfile
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG MIMO_VERSION=0.1.7
RUN npm install -g --no-audit --no-fund \
      "@mimo-ai/cli@${MIMO_VERSION}" \
      --registry https://registry.npmjs.org \
    && mimo --version

ENV MIMOCODE_HOME=/root/.mimocode
WORKDIR /workspace
EXPOSE 49983
```

完整 Dockerfile 还会安装开发工具，并关闭与本示例无关的 MiMo 网络功能。

### 2. 配置 Host

```bash
cd examples/mimo-code-integration
install -m 600 .env.example .env
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

设置 `E2B_API_URL`、`E2B_API_KEY`、`CUBE_TEMPLATE_ID` 和 `MIMO_API_KEY`。
远程 CubeAPI 使用真实 API Key 时应启用 HTTPS；明文 HTTP 只适合受信任的本地部署。
首版集成只面向 MiMo Platform：

- Base URL：`https://api.xiaomimimo.com/v1`；
- 模型：`mimo/mimo-v2.5-pro`；
- 鉴权请求头：`api-key`。

明确固定该契约，可以避免把凭证发送给根据不可信 URL 猜出的 Host。其他
OpenAI-compatible Provider 可在后续作为显式模式加入，并单独指定 Host 和鉴权方式。

### 3. 执行无头任务

```bash
python run_mimo_code.py
```

Host 会执行：

```bash
mimo run --format json --dir /workspace \
  --model mimo/mimo-v2.5-pro \
  --agent build \
  --dangerously-skip-permissions "<prompt>"
```

`--format json` 会输出 `tool_use`、`text`、`error`、`step_finish` 等 NDJSON
事件，每个事件都带有 `sessionID`。示例会先缓冲 SDK 的任意 stdout 分块，再解析
完整 JSON 行。

直接 runner 只在命令环境中传递 Key。这适合作为开发流程，但拥有开放出口的工具仍
可能泄露进程环境中的密钥。

### 4. 将 MiMo 状态集中到一个 profile

模板使用绝对路径：

```text
/root/.mimocode/
├── config/
├── data/    # 会话数据库、鉴权（如使用）、记忆、Checkpoint
├── state/
└── cache/
```

`MIMOCODE_HOME` 是 MiMo 集成的关键特性：整个 profile 可以作为一个单元检查、保留
或删除。共享模板绝不能预置开发者会话或凭证。

### 5. 暂停并续接同一个对话

```bash
python resume_mimo_code.py
```

Runner 会提取第一轮的 sessionID、暂停 VM、重新连接并显式续接：

```python
first_result, events = run_turn(
    sandbox,
    workspace=workspace,
    prompt=first_prompt,
    envs=mimo_env,
    timeout=900,
)
session_id = session_id_from_events(events)

sandbox_id = sandbox.sandbox_id
paused = sandbox.pause()
if isinstance(paused, str) and paused:
    sandbox_id = paused
sandbox = Sandbox.connect(sandbox_id=sandbox_id)

second_result, events = run_turn(
    sandbox,
    workspace=workspace,
    prompt=second_prompt,
    envs=mimo_env,
    timeout=900,
    session_id=session_id,
)
```

实际实现会兼容 SDK 不同的 pause 返回类型，也不会使用 `Sandbox.create()` 上下文
管理器，因为其 `__exit__` 会 kill 已暂停沙箱。

测试 token 不会写入 `/workspace`，所以成功回忆能证明 MiMo 对话连续性，而不只是
文件仍然存在。Runner 还会验证 workspace、profile data 和
`mimo session list --format json`。

MiMo Checkpoint 和 CubeSandbox 快照互相补充：

- MiMo Checkpoint 用于重建长模型上下文和持久记忆；
- CubeSandbox 快照保存完整 VM，包括进程内存、根文件系统、workspace、数据库和
  MiMo profile。

### 6. 使用默认拒绝出口和凭证注入

```bash
python network_policy.py
```

原生 CubeSandbox SDK 会添加一个精确的 MiMo Platform 规则：

```python
Rule(
    name="allow_mimo_platform",
    match=Match(
        scheme="https",
        sni="api.xiaomimimo.com",
        host="api.xiaomimimo.com",
    ),
    action=Action(
        allow=True,
        audit="metadata",
        inject=[
            Inject(
                header="api-key",
                secret=MIMO_API_KEY,
                format="${SECRET}",
            )
        ],
    ),
)
```

沙箱使用 `allow_internet_access=False` 创建。VM 中只有占位
`MIMO_API_KEY`，真实值只存在于 Host 侧 CubeEgress 规则。MiMo 运行时必须信任
拦截 CA，因此示例设置
`NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt`。

示例关闭分享、遥测、自动更新、模型清单下载、LSP 下载和外部 Skills/插件，以减少
辅助请求；CubeEgress 规则才是白名单的实际强制边界。

### 7. 在适合的任务中使用 MiMo Compose

```bash
python run_mimo_code.py --agent compose --prompt \
  "Inspect the project, implement the change, test it, and write result.md containing CUBE_MIMO_RUN_OK."
```

Compose 是 MiMo 的主 Agent，可用于无头模式。具体委派行为由模型决定，所以生产流程
应验证最终产物和测试，而不是要求固定的子 Agent trace。

## 使用场景与最佳实践

- **隔离式自主开发：** 向 MiMo 提供一次性仓库副本，不挂载 Host 文件系统。
- **执行并回收结果：** 将输出统一写入 `/workspace`，再通过 `sandbox.files` 或受控
  命令读取。
- **长任务断点续跑：** 在一轮 MiMo 完成后 pause，同时保存 sandbox ID 和 MiMo
  session ID，恢复时显式指定。
- **并行方案：** 只有在每个分支都有明确所有者和清理策略时，才 fork MiMo session
  或 clone CubeSandbox 快照。
- **预装依赖：** 把工具链放入模板，避免窄出口策略额外开放 npm、PyPI 或下载域名。
- **把 profile 当作敏感数据：** 记忆和会话数据库可能含有提示词、代码、路径和
  命令输出。

## 限制与注意事项

- `--dangerously-skip-permissions` 会移除交互确认，只能在一次性沙箱中使用；必要时
  仍应保留显式 deny 规则。
- E2B 命令通道不提供 MiMo TUI，应使用 `mimo run`。
- OAuth 会把 access/refresh token 写入 `auth.json`，快照也会保存它们。本示例
  因此采用 API Key 出口注入。
- Pause 会断开现有网络连接；恢复后的 MiMo 命令会重建模型连接，但保留会话状态。
- `MIMOCODE_HOME` 必须是绝对路径。
- Compose 委派和自动记忆整理由模型决定，不适合作为确定性健康检查。

## 常见问题

| 现象 | 原因 | 处理方式 |
| --- | --- | --- |
| `mimo: command not found` | 模板过旧 | 重新构建并注册固定版本镜像 |
| 平台二进制无法执行 | 镜像架构错误 | 按 Cube 节点架构构建 |
| 鉴权失败 | Key 无效或使用 Bearer 请求头 | MiMo Platform 要求 `api-key` |
| `403 Forbidden - CubeEgress` | 请求 Host 未命中 | 使用精确 MiMo 端点并查看出口审计 |
| TLS 校验失败 | 运行时不信任 CubeEgress CA | 正确设置 `MIMO_NODE_EXTRA_CA_CERTS` |
| 出现 models.dev/更新错误 | 辅助网络功能被开启 | 保留示例提供的 disable 开关 |
| 模板停在 `PULLING` | 镜像仓库不可达 | 使用节点可访问的仓库和拉取凭证 |
| Probe 超时 | 缺少 Cube entrypoint/envd | 继承 CubeSandbox base image |
| 没有 session ID | CLI 或输出发生变化 | 固定 MiMo 版本并使用 `--format json` |
| 恢复后找不到会话 | Profile 或 workspace 改变 | 复用相同绝对路径和 sandbox ID |
| 任务超时 | 模型或工具执行超预算 | 同时增大 exec 和 sandbox timeout |

## 参考

- 可运行示例：[`examples/mimo-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mimo-code-integration)
- [MiMo Code 仓库](https://github.com/XiaomiMiMo/MiMo-Code)
- [MiMo Code 模型](https://mimo.xiaomi.com/mimocode/models-provider)
- [MiMo Code 会话](https://mimo.xiaomi.com/mimocode/sessions)
- [自定义镜像](../tutorials/bring-your-own-image.md)
- [快照 / 克隆 / 回滚](../snapshot-rollback-clone.md)
- [CubeEgress 安全代理](../security-proxy.md)
