---
title: "trpc-agent-go：基于 Cube Sandbox 的安全代码执行后端"
author: joeyczheng
date: 2026-06-03
tags:
  - agent
  - code-execution
  - e2b
  - golang
lang: zh-CN
---

# trpc-agent-go：基于 Cube Sandbox 的安全代码执行后端

## 业务背景

[trpc-agent-go](https://github.com/trpc-group/trpc-agent-go) 是腾讯开源的 Go 语言 Agent 开发框架，提供从模型调用、工具编排、记忆管理到代码执行（Code Interpreter）的完整能力。在真实业务中，Agent 经常需要执行 LLM 生成的 Python / JavaScript / Bash 代码，例如：

- 数据分析与可视化（pandas、matplotlib）；
- 业务报表生成（PDF、Excel、SVG 图表）；
- 动态脚本与一次性任务（爬虫、清洗、转换）；
- Agentic RL / Tool-Use 场景下的多轮代码执行与反馈闭环。

为此 trpc-agent-go 在 `codeexecutor/e2b` 包中实现了与 [E2B](https://e2b.dev) 协议兼容的 `CodeExecutor`，支持把 Agent 生成的代码直接送入沙箱执行，并把 stdout / stderr、富媒体结果（PNG、PDF、SVG、HTML、Markdown）和工作区产物回流给上层 Agent。

## 核心痛点

直接在 Agent 进程或宿主容器里跑 LLM 生成的代码，会遇到几个生产级问题：

- **安全性**：模型生成的代码不可信，可能读敏感文件、发起任意网络请求、消耗主机资源，必须有强隔离边界。
- **依赖与污染**：Python 数据科学栈、Node 工具链、字体、临时文件相互影响，长生命周期进程易出现状态污染。
- **冷启动与并发**：Agent 应用流量呈"突发 + 多会话"特征，传统容器秒级冷启动和较高内存占用难以扛住高并发短任务。
- **多轮一致性**：多轮 Agent 经常需要"上一步生成的中间文件，下一步继续读"，纯 Function 化执行难以保留工作区状态。
- **海外 SaaS 依赖**：直接接入 E2B 公有云会带来跨境网络、合规、计费等问题；自建一套兼容方案的成本不低。

## 基于 Cube Sandbox 的方案

Cube Sandbox 兼容 E2B 沙箱协议，因此 trpc-agent-go 的 `e2b.CodeExecutor` 可以**零代码改动**接入自建的 Cube Sandbox 集群，仅通过 Option 切换后端：

```go
import (
    "context"
    "time"

    "trpc.group/trpc-go/trpc-agent-go/codeexecutor/e2b"
)

ce, err := e2b.New(
    // 把 E2B 客户端指向自建的 Cube Sandbox 控制面
    e2b.WithAPIURL("https://cube-sandbox.your-domain.internal"),
    e2b.WithAPIKey("<token-issued-by-cube>"),

    // 选择已经在 Cube 中构建好的 code-interpreter 模板
    e2b.WithTemplate("code-interpreter-v1"),

    // 沙箱级与执行级超时
    e2b.WithSandboxTimeout(10*time.Minute),
    e2b.WithExecutionTimeout(60*time.Second),

    // 多轮 Agent 推荐开启会话级工作区，跨 turn 复用文件
    e2b.WithWorkspacePersistence(e2b.WorkspacePersistencePerSession),
)
if err != nil {
    panic(err)
}
defer ce.Close()
```

接入后，整体调用链如下：

```
LLM ──► Agent (trpc-agent-go)
            │
            │  CodeExecutor.ExecuteCode / Engine
            ▼
      E2B 协议 (HTTP + envd)
            │
            ▼
   Cube Sandbox 控制面 ──► KVM 微虚机（per session / per turn）
                              ├─ Python / Node / Bash 内核
                              ├─ 工作区目录 /tmp/run/<execID>
                              └─ stdout / stderr / 文件产物 回流
```

trpc-agent-go 在这套链路上额外提供：

- **多语言适配**：`pickLanguage` 自动把模型输出的 ```` ```python ````、```` ```ts ````、```` ```bash ```` 等转成 Cube Sandbox 内核可识别的语言标识。
- **富结果还原**：把沙箱返回的 PNG / JPEG / PDF / SVG / LaTeX 等结果，自动解码成 `codeexecutor.File` 交给上层 Agent，作为多模态消息或附件回到对话流。
- **工作区抽象**：`CreateWorkspace`、`PutFiles`、`StageDirectory`、`Collect` 等接口将文件 staging、收集产物的能力封装在沙箱内部进行，配合 `WorkspacePersistencePerSession` 让多轮对话天然共享中间文件。
- **生命周期可控**：通过 `WithSandboxID` 复用已存在的 Cube 沙箱，或由 `CodeExecutor` 自己负责创建/销毁，便于实现"会话池"。

## 效果与收益

将 Agent 代码执行后端从公共 SaaS / 容器方案迁移到自建 Cube Sandbox 后，可以同时拿到几方面收益：

- **更强的安全边界**：每次会话/每个 turn 一台独立 KVM 微虚机，硬件级隔离 + 网络策略，远好于共享内核的容器方案，可放心执行不可信代码。
- **更低的执行延迟与成本**：Cube Sandbox 单实例 100ms 级交付、< 5MB 额外内存开销、单机百并发，让"Agent 一次思考一次开沙箱"成为可行的默认实践，避免长驻容器的资源浪费。
- **零侵入的私有化能力**：得益于 E2B 协议兼容，trpc-agent-go 上层业务代码无需改动，只需切换 `WithAPIURL` / `WithTemplate` 即可在公有云 E2B 与私有化 Cube Sandbox 之间自由切换，满足合规与跨境网络要求。
- **更顺畅的多轮 Agent 体验**：会话级工作区让"画一张图—基于这张图再回答—再导出 PDF"这类任务无需手动搬运中间文件，对 Agentic RL、Deep Research、数据分析助手等场景尤其友好。
- **可演进的模板治理**：Cube Sandbox 的模板体系（如 `code-interpreter-v1`）让算法/平台团队可以在统一镜像中管理 Python 包、字体、JDK 等依赖，业务侧只需挑选模板即可。

## 参考资料

- 框架仓库：[trpc-group/trpc-agent-go](https://github.com/trpc-group/trpc-agent-go)
- 相关源码：`https://github.com/trpc-group/trpc-agent-go/tree/main/codeexecutor/e2b`
- 兼容协议：[E2B Sandbox](https://e2b.dev)
