---
title: Agno 集成指南
author: Lion-Leporidae
date: 2026-09-24
tags:
  - integration
  - agno
  - agent
  - sandbox
lang: zh-CN
---

# Agno 集成指南

[English](../../../guide/integrations/agno.md)

将 [Agno](https://github.com/agno-agi/agno) Agent 的 Python 执行工具接入
CubeSandbox MicroVM。Agno 可以把普通 Python 函数直接注册为工具；下文函数调用原生
`cubesandbox` SDK，因此模型 harness 留在宿主机，模型生成的代码只在 MicroVM 内执行。

## 集成对象与已验证版本

| 组件 | 本次验证版本 |
| --- | --- |
| Agno | `3.0.11` |
| OpenAI Python SDK | `2.54.0` |
| CubeSandbox Python SDK | `0.7.0` |
| Python | `3.12` |

示例预期支持 Agno 3.x 与 Python 3.10+。用于生产前应固定已验证的解析版本。

## 前置条件

- 可访问的 CubeSandbox 部署，CubeAPI 与 CubeProxy 均可连通。
- 包含 `python3` 且 envd 监听 `49983` 的模板。
- `CUBE_TEMPLATE_ID`；默认配置不匹配部署时，再设置 `CUBE_API_URL` 与
  `CUBE_PROXY_NODE_IP`。
- 完整 Agent 流程需要 OpenAI 兼容端点与 `OPENAI_API_KEY`。该密钥属于宿主机上的
  harness，不会被复制进沙箱。

## 配置与可运行示例

仓库提供完整示例：
[`examples/agno-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/agno-integration)。
构建其中的 Python 模板、创建 Cube 模板后，在宿主机安装依赖并配置环境变量：

```bash
cd examples/agno-integration
docker build --platform linux/amd64 -t <你的镜像仓库>/agno-cube:latest .
docker push <你的镜像仓库>/agno-cube:latest
cubemastercli tpl create-from-image \
  --image <你的镜像仓库>/agno-cube:latest \
  --writable-layer-size 1G --expose-port 49983 --probe 49983 --probe-path /health

python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
python agno_agent_demo.py --sandbox-only
python agno_agent_demo.py
```

`--sandbox-only` 使用确定性的 Python 计算验证 Cube 执行链路，不调用 LLM；普通运行则让
Agent 调用同一个工具。

## 集成模式

Agno 接受 `Agent(tools=[...])` 中的 Python 函数。将函数绑定到已创建的沙箱，可让一次
Agent 运行的多次工具调用复用同一个 MicroVM；上下文管理器负责生命周期清理：

```python
from agno.agent import Agent
from agno.models.openai import OpenAIChat
from cubesandbox import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
    run_python = make_run_python(sandbox)
    agent = Agent(
        model=OpenAIChat(id="gpt-4o-mini", api_key=os.environ["OPENAI_API_KEY"]),
        tools=[run_python],
        instructions=["所有代码执行任务都使用 run_python。"],
    )
    agent.print_response("计算 1 到 10 的平方和。")
```

可运行示例中的 `make_run_python` 会为每段代码分配独立路径，以命令超时执行 `python3`，并将
stdout、stderr 与非零退出码明确返回给模型；它不会在宿主机解释 `code` 参数。

## 生产控制点

- **网络：** 代码不需要联网时，使用 `allow_internet_access=False` 创建沙箱；确需出网时，
  使用收窄的原生 CubeSandbox 规则，而不是将 LLM 密钥放入 MicroVM。
- **超时与大小：** 示例限制单个命令为 120 秒、代码输入为 16 KiB。请按业务与平台策略进一步
  收紧。
- **持久状态：** 仅在 Agent 状态必须跨运行保留时，才使用 `Volume.create(...)` 与
  `volume_mounts={"/workspace": volume}`；否则新建沙箱可降低任务间状态泄漏。
- **清理：** 将 `Sandbox.create(...)` 保持在 `with` 块内。需要保存会话时，应显式使用
  CubeSandbox pause/resume，并在 Agent prompt 外部保存沙箱标识。

## 注意事项

- `CUBE_API_URL` 配置控制面；SDK 还需要可达的 CubeProxy 数据面。没有通配 DNS 的部署中，设置
  `CUBE_PROXY_NODE_IP`；端口非默认时也设置 `CUBE_PROXY_PORT_HTTP`。
- 模板必须包含 Python。只填写模板 ID 不会自动安装 Python，也不会启动 code-interpreter 服务。
- Agent 可能生成有害代码。隔离能降低宿主机暴露面，但不能使不可信代码天然安全；仍需在集群边界
  执行网络、身份与资源控制。

## 参考资料

- [Agno 自定义工具](https://docs.agno.com/tools/creating-tools/python-functions)
- [Agno Agent 工具](https://docs.agno.com/tools/agent)
- [CubeSandbox 网络策略](/zh/guide/network-policy)
- [CubeSandbox 持久存储](/zh/guide/persistent-storage)
