---
title: "trpc-agent-go: A Secure Code Execution Backend Powered by Cube Sandbox"
author: joeyczheng
date: 2026-06-03
tags:
  - agent
  - code-execution
  - e2b
  - golang
lang: en-US
---

# trpc-agent-go: A Secure Code Execution Backend Powered by Cube Sandbox

## Business Context

[trpc-agent-go](https://github.com/trpc-group/trpc-agent-go) is Tencent's open-source Go framework for building production-grade AI agents. It provides a full stack — model invocation, tool orchestration, memory, and code execution (Code Interpreter) — for agentic applications. In real workloads, an agent frequently needs to execute LLM-generated Python / JavaScript / Bash code, for example:

- Data analysis and visualization (pandas, matplotlib);
- Business reporting (PDF, Excel, SVG charts);
- Ad-hoc scripts (crawlers, ETL, format conversion);
- Multi-turn code execution loops in Agentic RL / Tool-Use scenarios.

To support these needs, trpc-agent-go ships an E2B-protocol compatible `CodeExecutor` under `codeexecutor/e2b`. It feeds agent-generated code straight into a sandbox and pipes back stdout / stderr, rich-media results (PNG, PDF, SVG, HTML, Markdown) and workspace artifacts to the upper-layer agent.

## Key Challenges

Running LLM-generated code directly inside the agent process or its host container raises several production-grade issues:

- **Security**: Model-generated code is untrusted — it may read sensitive files, make arbitrary network calls, or burn host resources. Strong isolation is mandatory.
- **Dependency pollution**: Python data-science stacks, Node toolchains, fonts and temp files accumulate, and long-living processes easily end up in dirty states.
- **Cold start & concurrency**: Agent traffic is bursty and multi-session. Traditional containers with multi-second cold starts and high memory overhead struggle with high-concurrency, short-lived tasks.
- **Multi-turn consistency**: Multi-turn agents often need to reuse intermediate files across turns. A purely functional execution model can't keep workspace state.
- **SaaS dependency**: Hooking up directly to the E2B public cloud brings cross-border networking, compliance and billing concerns; rolling a fully custom alternative is expensive.

## Solution with Cube Sandbox

Cube Sandbox is wire-compatible with the E2B sandbox protocol, which means trpc-agent-go's `e2b.CodeExecutor` can be pointed at a self-hosted Cube Sandbox cluster **without any code change** — only Options need to be flipped:

```go
import (
    "context"
    "time"

    "trpc.group/trpc-go/trpc-agent-go/codeexecutor/e2b"
)

ce, err := e2b.New(
    // Point the E2B client at a self-hosted Cube Sandbox control plane
    e2b.WithAPIURL("https://cube-sandbox.your-domain.internal"),
    e2b.WithAPIKey("<token-issued-by-cube>"),

    // Pick a code-interpreter template already built in Cube
    e2b.WithTemplate("code-interpreter-v1"),

    // Sandbox-level and per-execution timeouts
    e2b.WithSandboxTimeout(10*time.Minute),
    e2b.WithExecutionTimeout(60*time.Second),

    // For multi-turn agents, enable a session-scoped workspace so files persist across turns
    e2b.WithWorkspacePersistence(e2b.WorkspacePersistencePerSession),
)
if err != nil {
    panic(err)
}
defer ce.Close()
```

The end-to-end call chain becomes:

```
LLM ──► Agent (trpc-agent-go)
            │
            │  CodeExecutor.ExecuteCode / Engine
            ▼
      E2B protocol (HTTP + envd)
            │
            ▼
   Cube Sandbox control plane ──► KVM micro-VM (per session / per turn)
                                    ├─ Python / Node / Bash kernels
                                    ├─ Workspace dir /tmp/run/<execID>
                                    └─ stdout / stderr / file artifacts
```

On top of that pipeline, trpc-agent-go contributes:

- **Multi-language adaptation**: `pickLanguage` maps fenced blocks like ```` ```python ````, ```` ```ts ````, ```` ```bash ```` to the language identifiers expected by the Cube Sandbox kernels.
- **Rich result decoding**: PNG / JPEG / PDF / SVG / LaTeX outputs returned by the sandbox are automatically decoded into `codeexecutor.File` instances and flow back to the agent as multi-modal content or attachments.
- **Workspace abstraction**: APIs such as `CreateWorkspace`, `PutFiles`, `StageDirectory` and `Collect` perform staging and collection entirely inside the sandbox. Combined with `WorkspacePersistencePerSession`, multi-turn conversations transparently share intermediate files.
- **Lifecycle control**: `WithSandboxID` reuses an existing Cube sandbox, while the default mode lets `CodeExecutor` own create/destroy — making it easy to implement a "session pool".

## Results and Benefits

Migrating the agent code-execution backend from public SaaS / vanilla containers to a self-hosted Cube Sandbox unlocks several wins at once:

- **Stronger security boundary**: Each session/turn runs inside an isolated KVM micro-VM with hardware-level isolation and network policy — substantially safer than shared-kernel containers, making it acceptable to run untrusted code.
- **Lower latency & cost**: Cube Sandbox delivers sandboxes in ~100ms with <5MB of extra memory, and sustains ~100 concurrent sandboxes per node. "One sandbox per agent thought" finally becomes a practical default, instead of relying on long-lived containers.
- **Zero-intrusion private deployment**: Thanks to E2B compatibility, business code on top of trpc-agent-go does not change. Switching `WithAPIURL` / `WithTemplate` is enough to move freely between public E2B and a private Cube Sandbox cluster — addressing compliance and cross-border networking requirements.
- **Smoother multi-turn agent UX**: Session-scoped workspaces remove the need to manually shuffle intermediate files for chains like "draw a chart → answer based on it → export it as a PDF". This is especially helpful for Agentic RL, Deep Research, and data-analysis assistants.
- **Manageable template story**: Cube Sandbox's template system (e.g. `code-interpreter-v1`) lets platform teams curate Python packages, fonts, JDKs etc. in unified images, while business teams just pick a template.

## References

- Framework repo: [trpc-group/trpc-agent-go](https://github.com/trpc-group/trpc-agent-go)
- Related sources: `https://github.com/trpc-group/trpc-agent-go/tree/main/codeexecutor/e2b`
- Compatible protocol: [E2B Sandbox](https://e2b.dev)