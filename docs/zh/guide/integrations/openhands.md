---
title: OpenHands 集成指南
author: Fan-hr
date: 2026-07-28
tags:
  - integration
  - openhands
  - coding-agent
  - agent
lang: zh-CN
---

# OpenHands 集成指南

[English](../../../guide/integrations/openhands.md)

## 集成对象与版本

[OpenHands](https://www.openhands.dev/) 是一个自主软件开发智能体平台。其
[Agent SDK](https://github.com/OpenHands/software-agent-sdk) 通过一个
**agent server** 来执行智能体动作（bash、文件编辑、脚本），该服务默认运行在
本地 Docker 容器中。

本指南把 agent server 接入 Cube Sandbox MicroVM：服务被预装进 Cube 模板，并
连同运行状态一起冻结进模板的启动快照——每个沙箱热启动时 agent server 已经
就绪，同时获得完整的硬件级隔离。

已验证版本：`openhands-sdk` / `openhands-tools` /
`openhands-agent-server` **1.38.0**，Cube Sandbox **v0.6.0**。

## 前置条件

- Cube Sandbox 部署：任意可用部署（单机即可），`cubemastercli` 与 E2B 兼容
  API 可访问。
- SDK 或 CLI 依赖：Docker（构建镜像）、[`uv`](https://docs.astral.sh/uv/)
  或较新的 pip >= 26（宿主机脚本——较旧的 pip（如 Ubuntu 24.04 自带的
  24.0）会因上游 `lmnr`/`opentelemetry` 冲突而解析失败）。
- 必需环境变量：`E2B_API_URL`、`E2B_API_KEY`、`CUBE_TEMPLATE_ID`；完整智能体
  演示另需 `LLM_MODEL`、`LLM_API_KEY`、可选 `LLM_BASE_URL`（任意 OpenAI 兼容
  端点）。若只想验证集成本身，`smoke_test.py` 与 `pause_resume.py`
  **完全不需要任何 LLM 配置**。

## 集成步骤

1. **构建模板镜像** ——
   [`examples/openhands-integration/Dockerfile`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/openhands-integration/Dockerfile)
   基于 `cubesandbox-base`（保留 `:49983` 上的 envd），用 uv 安装独立的
   Python 3.12，钉住 `openhands-agent-server` 版本，并以非特权 `user` 账户
   将其作为镜像 CMD 启动在 `:8000`。

2. **注册为模板**，探针直接指向 agent server 本身，确保启动快照只在服务完全
   就绪后拍摄：

   ```bash
   cubemastercli tpl create-from-image \
     --image <registry>/openhands-sandbox:latest \
     --writable-layer-size 2G \
     --expose-port 8000 --expose-port 49983 \
     --probe 8000 --probe-path /ready
   ```

3. **接入 OpenHands SDK**：`CubeSandboxWorkspace` 继承 `RemoteWorkspace`
   （与官方 `DockerWorkspace` 相同的扩展点），通过 E2B 兼容 SDK 创建沙箱，
   并把 workspace 指向代理出来的 agent-server 地址。

4. **运行演示**（位于 `examples/openhands-integration/`）：`smoke_test.py`
   （无需 LLM；热启动延迟、bash/文件往返）、`pause_resume.py`（无需 LLM；
   在运行中的服务下整机冻结/解冻）、`main.py`（完整编码任务 + 沙箱内独立
   核验结果）。

## 关键代码片段

对既有 OpenHands SDK 程序来说，最小改动就是替换 workspace——其余代码不变：

```python
from openhands.sdk import LLM, Conversation
from openhands.tools.preset.default import get_default_agent
from cubesandbox_workspace import CubeSandboxWorkspace  # 本示例提供

agent = get_default_agent(
    llm=LLM(model="openai/deepseek-chat", api_key=..., base_url=...),
    cli_mode=True,  # bash + 文件编辑；模板中不含浏览器栈
)

with CubeSandboxWorkspace(template="tpl-...") as workspace:  # 第 2 步生成的模板
    conversation = Conversation(agent=agent, workspace=workspace)
    conversation.send_message("创建 fib.py 并运行，修复所有报错。")
    conversation.run()

    workspace.pause()   # 冻结 agent server + shell + 执行中的进程
    workspace.resume()  # 逐位解冻，会话原样继续
```

workspace 核心实现（节选自 `cubesandbox_workspace.py`）：

```python
class CubeSandboxWorkspace(RemoteWorkspace):
    template: str
    agent_server_port: int = 8000

    def model_post_init(self, context):
        self._sandbox = Sandbox.create(template=self.template, ...)
        host = self._sandbox.get_host(self.agent_server_port)
        object.__setattr__(self, "host", f"http://{host}")
        self._wait_for_ready(timeout=self.health_check_timeout)
        super().model_post_init(context)
```

## 注意事项

- **智能体主循环在 MicroVM 内运行。**LLM 调用从沙箱内发起，因此沙箱需要
  有到 LLM 端点的出口；可用网络白名单封禁其余出口
  （[网络策略](../network-policy.md)）。
- **LLM key 会进入沙箱。**会话载荷会把 `LLM(api_key=...)` 发给 VM 内的
  服务端——这是上游 agent-server 架构的既有设计（官方 `DockerWorkspace`
  同样如此）。若要求真 key 不进 VM，可给智能体配置占位符，由 CubeEgress
  [凭证注入](../security-proxy.md)在线上附加真实请求头；模板已信任拦截
  CA。
- **入口访问控制。**`private_traffic=True` 以沙箱级 traffic token 保护
  workspace API，`SESSION_API_KEY` 提供服务端鉴权。适用范围与限制见示例
  README 的[安全对齐](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/openhands-integration/README_zh.md#安全对齐)一节。
- **版本配对。**宿主机 `openhands-sdk`/`openhands-tools` 与模板内
  `openhands-agent-server` 保持同一版本（已验证 1.38.0）；
  `workspace.get_server_info()` 可排查不匹配。
- **宿主机安装。**使用 uv 或较新的 pip（>= 26）；旧版 pip 会在上游
  `lmnr`/`opentelemetry` 钉版上解析失败。

## 参考资料

- 相关文档：[模板概览](../templates.md) ·
  [从 OCI 镜像创建模板](../tutorials/template-from-image.md) ·
  [网络策略](../network-policy.md) ·
  [限制公网访问](../restrict-public-access.md) ·
  [安全代理](../security-proxy.md)
- 示例仓库：[`examples/openhands-integration`](https://github.com/tencentcloud/CubeSandbox/tree/master/examples/openhands-integration)
- 上游项目：[OpenHands](https://github.com/All-Hands-AI/OpenHands) ·
  [OpenHands Agent SDK](https://github.com/OpenHands/software-agent-sdk)
