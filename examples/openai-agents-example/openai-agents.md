# OpenAI Agents SDK Sandbox Analysis

[中文文档](openai-agents_zh.md)

> Source: [OpenAI Sandbox Agents official docs](https://developers.openai.com/api/docs/guides/agents/sandboxes)

## Overview

A **Sandbox Agent** in the OpenAI Agents SDK runs inside an isolated Unix-like execution environment that provides a filesystem, a shell, package management, port exposure, snapshots, and resumable state. It fits scenarios where an Agent needs a persistent workspace, not just prompt context.

## Core Architecture

The key design of Sandbox Agents is the separation between **Harness (control plane)** and **Compute (execution plane)**:

- **Harness (control plane)**: runs the Agent loop, model calls, tool routing, approvals, tracing, and resume.
- **Compute (execution plane / Sandbox)**: handles filesystem I/O, command execution, dependency installation, port exposure, and state snapshots.

This separation keeps sensitive control-plane work in trusted infrastructure while the sandbox focuses purely on execution.

## When to Use a Sandbox

- The task needs to operate on a body of documents, not just a single prompt
- The Agent needs to write files for later inspection
- You need to run commands, install packages, or execute scripts
- The workflow produces artifacts (Markdown, CSV, screenshots, websites, etc.)
- You need to expose ports to preview services (notebooks, reports, apps)
- The work may be paused for human review and resumed later

## Sandbox Components

| Component | Responsibility |
|-----------|----------------|
| **SandboxAgent** | Agent definition + default sandbox configuration |
| **Manifest** | Initial workspace content (files, Git repos, cloud-storage mounts, env vars) |
| **Capabilities** | Sandbox capabilities: Shell, Filesystem, Skills, Memory, Compaction |
| **Sandbox Client** | Provider integration (local Unix, Docker, managed services) |
| **Sandbox Session** | The live execution environment |
| **SandboxRunConfig** | Per-run configuration (session source, client options) |

### Manifest (workspace declaration)

| Type | Purpose |
|------|---------|
| File, Dir | Small synthetic inputs, helper files, output directories |
| LocalFile, LocalDir | Map files/dirs from the host into the sandbox |
| GitRepo | Clone a repository into the workspace |
| S3Mount, GCSMount, R2Mount, AzureBlobMount | Mount external cloud storage |
| environment | Environment variables set at sandbox startup |
| users, groups | OS accounts and groups inside the sandbox |

### Capabilities

| Capability | Scenario | Description |
|------------|----------|-------------|
| Shell | Agent needs shell access | Command execution, supports interactive input |
| Filesystem | Agent needs to edit files or view images | apply_patch + view_image |
| Skills | Skill discovery and loading | Load from Git repos or local directories |
| Memory | Read/write memory across runs | Learning across runs |
| Compaction | Long-running flows need context compaction | Automatic context pruning |

Default capabilities: `Filesystem()` + `Shell()` + `Compaction()`

## Code Examples

### Basic usage (Unix-local)

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

### Switching to the Docker provider

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

### Resume (pause and continue)

```python
# First run
async with session:
    first_result = await Runner.run(
        agent,
        "Build the first version of the app.",
        max_turns=20,
        run_config=RunConfig(
            sandbox=SandboxRunConfig(session=session),
        ),
    )

# Serialize session state
frozen_session_state = client.deserialize_session_state(
    client.serialize_session_state(session.state)
)

# Resume and continue
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

## State Management

| State type | What it restores | Use case |
|------------|------------------|----------|
| **RunState** | Harness-side state (model transcript, tool state, approvals, agent position) | Resume a workflow across pauses |
| **session_state** | Serialized sandbox session | Apps/task systems persist provider session state directly |
| **snapshot** | Saved workspace contents | Start a new run from pre-existing files and artifacts |

Session resolution priority:
1. Explicit `session` → reuse the live session directly
2. Recover from `RunState` → restore the stored session state
3. Explicit `session_state` → restore from serialized state
4. None of the above → create a new session (using the Manifest)

## Memory (cross-run memory)

Agents can retain what they learn across runs: user preferences, corrections, project experience, task summaries.

```python
from agents.sandbox.capabilities import Filesystem, Memory, Shell

agent = SandboxAgent(
    name="Memory-enabled reviewer",
    instructions="Inspect the workspace and retain useful lessons.",
    default_manifest=manifest,
    capabilities=[Memory(), Filesystem(), Shell()],
)
```

Memory file layout:

```
workspace/
  sessions/
    <rollout-id>.jsonl
  memories/
    memory_summary.md       # Summary (injected at the start of each run)
    MEMORY.md               # Detailed memory
    raw_memories/            # Raw memories
    rollout_summaries/       # Run summaries
    skills/                  # Learned skills
```

## Supported Sandbox Providers

| Provider | SDK Client | Description |
|----------|------------|-------------|
| Unix-local | UnixLocalSandboxClient | Local development, fastest startup |
| Docker | DockerSandboxClient | Local container isolation |
| **E2B** | **E2BSandboxClient** | **Managed sandbox (cube-sandbox compatible)** |
| Modal | ModalSandboxClient | Managed sandbox |
| Cloudflare | CloudflareSandboxClient | Edge sandbox |
| Daytona | DaytonaSandboxClient | Managed dev environments |
| Vercel | VercelSandboxClient | Web-app sandbox |
| Blaxel | BlaxelSandboxClient | Managed sandbox |
| Runloop | RunloopSandboxClient | Managed dev environments |

## Relationship with cube-sandbox

cube-sandbox implements an **E2B-compatible API**, which means:

1. **Protocol compatibility**: the OpenAI Agents SDK's `E2BSandboxClient` can talk to cube-sandbox directly.
2. **Architectural alignment**: the harness/compute separation highlighted in the docs is exactly what cube-sandbox + mini-swe-agent do.
3. **Capability coverage**: Shell, Filesystem, port exposure, and state resume are all supported.
4. **Advantage of cube-sandbox**: it provides **hardware-level isolation** (not container-level), and high-density deployments can fit thousands of instances on a single host.

### Evolution directions worth watching

- **Memory persistence**: cross-run memory is extremely valuable for RL training.
- **Snapshot/Resume**: workspace snapshots and resume for pause-and-continue flows.
- **Skills loading**: loading reusable skill packs from Git repositories.
- **Declarative workspaces via Manifest**: mounting S3/GCS/Azure Blob and other cloud storage.

## References

- [OpenAI Sandbox Agents official docs](https://developers.openai.com/api/docs/guides/agents/sandboxes)
- [OpenAI Agents SDK on GitHub](https://github.com/openai/openai-agents-python)
- [E2B Sandbox docs](https://e2b.dev/docs)
- [E2B + OpenAI Agents SDK guide](https://e2b.dev/docs/agents/openai-agents-sdk)
