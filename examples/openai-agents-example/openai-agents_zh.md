# OpenAI Agents SDK Sandbox 分析

[English](openai-agents.md)

> 来源：[OpenAI Sandbox Agents 官方文档](https://developers.openai.com/api/docs/guides/agents/sandboxes)

## 概述

OpenAI Agents SDK 中的 **Sandbox Agent** 在隔离的类 Unix 执行环境中运行，拥有文件系统、Shell、包管理、端口暴露、快照和可恢复状态。适用于 Agent 需要持久化工作空间（而非仅靠 prompt 上下文）的场景。

## 核心架构

Sandbox Agent 的关键设计是 **Harness（控制面）** 与 **Compute（执行面）** 的分离：

- **Harness（控制面）**：负责 Agent 循环、模型调用、工具路由、审批、追踪、恢复
- **Compute（执行面/Sandbox）**：负责文件读写、命令执行、依赖安装、端口暴露、状态快照

这种分离让敏感的控制面工作留在可信基础设施中，沙箱专注于执行。

## 何时使用 Sandbox

- 任务需要操作一组文档，而非单条 prompt
- Agent 需要写文件供后续检查
- 需要运行命令、安装包、执行脚本
- 工作流产生制品（Markdown、CSV、截图、网站等）
- 需要暴露端口预览服务（notebook、报告、应用）
- 工作需要暂停供人审核，之后恢复

## Sandbox 组成

| 组件 | 职责 |
|------|------|
| **SandboxAgent** | Agent 定义 + 沙箱默认配置 |
| **Manifest** | 工作空间初始内容（文件、Git 仓库、云存储挂载、环境变量） |
| **Capabilities** | 沙箱能力：Shell、Filesystem、Skills、Memory、Compaction |
| **Sandbox Client** | Provider 集成（本地 Unix、Docker、托管服务） |
| **Sandbox Session** | 活跃的执行环境 |
| **SandboxRunConfig** | 每次运行的配置（会话来源、客户端选项） |

### Manifest（工作空间声明）

| 类型 | 用途 |
|------|------|
| File, Dir | 小型合成输入、辅助文件、输出目录 |
| LocalFile, LocalDir | 从宿主机映射文件/目录到沙箱 |
| GitRepo | 克隆仓库到工作空间 |
| S3Mount, GCSMount, R2Mount, AzureBlobMount | 挂载外部云存储 |
| environment | 沙箱启动时的环境变量 |
| users, groups | 沙箱内的 OS 账户和组 |

### Capabilities（能力）

| 能力 | 场景 | 说明 |
|------|------|------|
| Shell | Agent 需要 Shell 访问 | 命令执行，支持交互式输入 |
| Filesystem | Agent 需要编辑文件或查看图片 | apply_patch + view_image |
| Skills | 需要技能发现和加载 | 从 Git 仓库或本地目录加载 |
| Memory | 后续运行需要读取/生成记忆 | 跨运行的学习能力 |
| Compaction | 长运行流程需要上下文压缩 | 自动裁剪上下文 |

默认能力：`Filesystem()` + `Shell()` + `Compaction()`

## 代码示例

### 基本用法（Unix-local）

```python
import asyncio

from agents import Runner
from agents.run import RunConfig
from agents.sandbox import Manifest, SandboxAgent, SandboxRunConfig
from agents.sandbox.capabilities import Shell
from agents.sandbox.entries import File
from agents.sandbox.sandboxes.unix_local import UnixLocalSandboxClient

manifest = Manifest(
    entries={
        "account_brief.md": File(
            content=(
                b"# Northwind Health\n\n"
                b"- Segment: Mid-market healthcare analytics provider.\n"
                b"- Renewal date: 2026-04-15.\n"
            )
        ),
    }
)

agent = SandboxAgent(
    name="Renewal Packet Analyst",
    model="gpt-5.4",
    instructions=(
        "Review the workspace before answering. Keep the response concise, "
        "business-focused, and cite the file names that support each conclusion."
    ),
    default_manifest=manifest,
    capabilities=[Shell()],
)


async def main():
    result = await Runner.run(
        agent,
        "Summarize the renewal blockers and recommend the next two actions.",
        run_config=RunConfig(
            sandbox=SandboxRunConfig(client=UnixLocalSandboxClient()),
            workflow_name="Unix-local sandbox review",
        ),
    )
    print(result.final_output)


asyncio.run(main())
```

### 切换到 Docker Provider

```python
from docker import from_env as docker_from_env

from agents import Runner
from agents.run import RunConfig
from agents.sandbox import SandboxRunConfig
from agents.sandbox.config import DEFAULT_PYTHON_SANDBOX_IMAGE
from agents.sandbox.sandboxes.docker import DockerSandboxClient, DockerSandboxClientOptions

docker_run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=DockerSandboxClient(docker_from_env()),
        options=DockerSandboxClientOptions(image=DEFAULT_PYTHON_SANDBOX_IMAGE),
    ),
    workflow_name="Docker sandbox review",
)

result = await Runner.run(
    agent,
    "Summarize the renewal blockers and recommend the next two actions.",
    run_config=docker_run_config,
)
```

### Resume（暂停恢复）

```python
# 第一次运行
async with session:
    first_result = await Runner.run(
        agent,
        "Build the first version of the app.",
        max_turns=20,
        run_config=RunConfig(
            sandbox=SandboxRunConfig(session=session),
        ),
    )

# 序列化会话状态
frozen_session_state = client.deserialize_session_state(
    client.serialize_session_state(session.state)
)

# 恢复并继续
resumed_session = await client.resume(frozen_session_state)
async with resumed_session:
    second_result = await Runner.run(
        agent,
        conversation + [{"role": "user", "content": "Add tests."}],
        max_turns=20,
        run_config=RunConfig(
            sandbox=SandboxRunConfig(session=resumed_session),
        ),
    )
```

## 状态管理

| 状态类型 | 恢复内容 | 使用场景 |
|----------|----------|----------|
| **RunState** | Harness 侧状态（模型对话、工具状态、审批、Agent 位置） | 跨暂停继续工作流 |
| **session_state** | 序列化的沙箱会话 | 应用/任务系统直接存储 Provider 会话状态 |
| **snapshot** | 保存的工作空间内容 | 从已有文件和制品开始新运行 |

会话解析优先级：
1. 传入 `session` → 直接复用活跃会话
2. 从 `RunState` 恢复 → 恢复存储的会话状态
3. 传入 `session_state` → 从序列化状态恢复
4. 以上都没有 → 创建新会话（使用 Manifest）

## Memory（跨运行记忆）

Agent 可以在运行之间保留学习成果：用户偏好、纠正、项目经验、任务摘要。

```python
from agents.sandbox.capabilities import Filesystem, Memory, Shell

agent = SandboxAgent(
    name="Memory-enabled reviewer",
    instructions="Inspect the workspace and retain useful lessons.",
    default_manifest=manifest,
    capabilities=[Memory(), Filesystem(), Shell()],
)
```

记忆文件结构：

```
workspace/
  sessions/
    <rollout-id>.jsonl
  memories/
    memory_summary.md       # 摘要（注入到每次运行开始）
    MEMORY.md               # 详细记忆
    raw_memories/            # 原始记忆
    rollout_summaries/       # 运行摘要
    skills/                  # 学到的技能
```

## 支持的 Sandbox Provider

| Provider | SDK Client | 说明 |
|----------|------------|------|
| Unix-local | UnixLocalSandboxClient | 本地开发，最快启动 |
| Docker | DockerSandboxClient | 本地容器隔离 |
| **E2B** | **E2BSandboxClient** | **托管沙箱（cube-sandbox 兼容）** |
| Modal | ModalSandboxClient | 托管沙箱 |
| Cloudflare | CloudflareSandboxClient | 边缘沙箱 |
| Daytona | DaytonaSandboxClient | 托管开发环境 |
| Vercel | VercelSandboxClient | Web 应用沙箱 |
| Blaxel | BlaxelSandboxClient | 托管沙箱 |
| Runloop | RunloopSandboxClient | 托管开发环境 |

## 与 cube-sandbox 的关系

cube-sandbox 实现了 **E2B 兼容的 API**，这意味着：

1. **协议兼容**：OpenAI Agents SDK 的 `E2BSandboxClient` 可直接对接 cube-sandbox
2. **架构对齐**：文档中强调的 harness/compute 分离，正是 cube-sandbox + mini-swe-agent 的做法
3. **能力覆盖**：Shell、Filesystem、端口暴露、状态恢复等能力均已支持
4. **差异优势**：cube-sandbox 提供**硬件级隔离**（非容器级），高密度部署可单机承载千级实例

### 值得关注的演进方向

- **Memory 持久化**：跨运行记忆对 RL 训练价值极大
- **Snapshot/Resume**：工作空间快照和恢复，支持暂停后继续
- **Skills 加载**：从 Git 仓库加载可复用技能包
- **Manifest 声明式工作空间**：支持挂载 S3/GCS/Azure Blob 等云存储

## 参考链接

- [OpenAI Sandbox Agents 官方文档](https://developers.openai.com/api/docs/guides/agents/sandboxes)
- [OpenAI Agents SDK GitHub](https://github.com/openai/openai-agents-python)
- [E2B Sandbox 文档](https://e2b.dev/docs)
- [E2B + OpenAI Agents SDK 指南](https://e2b.dev/docs/agents/openai-agents-sdk)
