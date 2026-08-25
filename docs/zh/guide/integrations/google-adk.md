---
title: Google ADK 集成指南
author: ztt0216
date: 2026-08-25
tags:
  - integration
  - google-adk
  - agent
lang: zh-CN
---

# Google ADK 集成指南

[English](../../../guide/integrations/google-adk.md)

本文介绍如何把 CubeSandbox 用作
[Google Agent Development Kit](https://github.com/google/adk-python) agent 的代码执行环境。Google ADK 支持 Python 函数工具，CubeSandbox 提供 E2B-compatible API，因此 ADK agent 可以像调用普通 Python 工具一样调用 CubeSandbox，而实际生成代码会在隔离的 CubeSandbox MicroVM 中运行。

这个方案采用“agent 在宿主机运行，代码进沙箱执行”的模式。ADK runtime、模型凭证和本地项目保留在宿主机上；只有工具中的代码执行进入 CubeSandbox。

## 集成对象与版本

| 组件 | 基线 |
| --- | --- |
| Google ADK | Python package `google-adk>=2.0.0` |
| Python | 3.10+ |
| CubeSandbox | E2B-compatible CubeAPI、可访问的 CubeProxy 数据面，以及支持 `run_code` 的模板 |
| SDK 路径 | 通过 `E2B_API_URL` 指向 CubeAPI 的 `e2b==2.26.0` 和 `e2b-code-interpreter==2.8.1` |

Google ADK 仍在快速演进。示例把 E2B 相关包固定到一个 plain `pip` 可直接安装、且已记录在仓库 SDK compatibility notes 中的组合。修改任一版本前请重新验证。

## 前置条件

- 一个可用的 [CubeSandbox 部署](/zh/guide/quickstart)，CubeAPI 通常位于 `http://<cube-host>:3000`。
- 官方 E2B SDK 需要可访问的数据面。一键本地部署自带 CoreDNS；生产环境应配置泛域名 DNS；
  本地无法配置泛域名 DNS 时，可以使用
  [E2B 开发 sidecar](/zh/guide/connect-existing-cluster)。
- 一个 CubeSandbox 模板 ID。请使用支持 E2B code interpreter `run_code` 路径的模板。
- 运行 ADK agent 的机器上有 Python 3.10+。
- Google API key，或其他 ADK 支持的模型配置。

::: warning 控制面与数据面
`E2B_API_URL` 用于选择 CubeAPI 控制平面端点。官方 E2B SDK 在执行 `run_code` 时还会访问每个沙箱的数据面域名。
生产环境应配置泛域名 DNS；本地开发若没有泛域名 DNS，可以使用
[E2B 开发 sidecar](/zh/guide/connect-existing-cluster)。如果你的部署使用自签 TLS 证书，请设置
`CUBE_SSL_CERT_FILE`，示例会在打开沙箱前导出 `SSL_CERT_FILE`。
:::

## 配置步骤

安装示例依赖：

```bash
cd examples/google-adk-integration
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

配置 `.env`：

| 变量 | 用途 |
| --- | --- |
| `E2B_API_URL` | CubeAPI 控制面地址，例如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI auth callback 接受的 E2B-compatible key；未启用认证时可用 `e2b_000000` |
| `CUBE_TEMPLATE_ID` | 工具调用创建临时沙箱时使用的 CubeSandbox 模板 ID |
| `GOOGLE_API_KEY` | ADK 使用的 Google 模型 API key |
| `GOOGLE_ADK_MODEL` | ADK 模型名，例如 `gemini-2.5-flash` |
| `CUBE_SANDBOX_TIMEOUT` | 可选的沙箱生命周期上限，单位秒，会传给 `Sandbox.create(timeout=...)`；默认值为 `300` |
| `CUBE_SSL_CERT_FILE` | 自签部署可选的 CubeSandbox CA bundle |

## 集成代码片段

把 CubeSandbox 执行函数暴露为 ADK 工具：

```python
from google.adk import Agent

from cube_code_tool import run_python_in_cube

root_agent = Agent(
    name="cube_code_agent",
    model="gemini-2.5-flash",
    instruction=(
        "When Python code needs to run, call run_python_in_cube so execution "
        "happens inside an isolated CubeSandbox MicroVM."
    ),
    tools=[run_python_in_cube],
)
```

工具函数把 Cube 相关逻辑集中在一处：

```python
import os

from e2b_code_interpreter import Sandbox

def run_python_in_cube(code: str) -> dict:
    template_id = os.environ["CUBE_TEMPLATE_ID"]
    with Sandbox.create(template=template_id, timeout=300) as sandbox:
        execution = sandbox.run_code(code)
        stdout = "".join(str(item) for item in execution.logs.stdout)
        return {
            "stdout": stdout,
            "text": execution.text,
            "error": str(execution.error) if execution.error else None,
        }
```

与宿主机本地 Python 工具相比，只有工具函数内部实现发生变化。ADK agent 看到的仍是一个普通的结构化函数工具。

### 迁移已有本地代码执行工具

```diff
- def run_python(code: str) -> dict:
-     completed = subprocess.run(["python3", "-c", code], capture_output=True, text=True)
-     return {"stdout": completed.stdout, "stderr": completed.stderr}
+ def run_python(code: str) -> dict:
+     return run_python_in_cube(code)
```

如果原 agent 已经引用这个代码执行工具，agent 定义和 prompt 可以保持不变。

## 可运行 Demo

先运行离线接线检查：

```bash
cd examples/google-adk-integration
python smoke_test.py
```

期望输出：

```text
GOOGLE_ADK_CUBE_SMOKE_OK
```

然后从父目录运行 ADK agent：

```bash
cd examples
adk run google-adk-integration
```

向 agent 提问：

```text
Run Python in the sandbox to calculate the first 10 Fibonacci numbers.
```

agent 会调用 `run_python_in_cube`，基于 `CUBE_TEMPLATE_ID` 创建临时 CubeSandbox，执行生成的 Python 代码，返回工具输出，并在工具调用结束后删除临时沙箱。

## 进一步配置

- **按会话复用沙箱：** 示例为了便于 review，每次工具调用创建一个临时沙箱。多轮 notebook 类任务可以把 sandbox handle 放入会话级服务，并在 ADK session 结束时删除。
- **超时：** 可设置 `CUBE_SANDBOX_TIMEOUT`，或在 `Sandbox.create(...)` 中传入固定 timeout，控制沙箱生命周期。
  这个值不是单次 `run_code` 调用的执行超时。
- **网络策略：** 如果生成代码只能访问指定主机，请在创建沙箱时配置 Cube 的出站控制。参见[网络策略](/zh/guide/network-policy)。
- **凭证处理：** 模型提供商 key 应保留在宿主机。如果沙箱内代码必须调用外部 API，优先使用 Cube 的 security proxy 和凭证注入流程，不要把原始密钥写入 VM。
- **模板：** 把重依赖预装进模板镜像，让每次 ADK 工具调用都能从 ready snapshot 快速启动。

## 注意事项

- 示例离线验证导入和接线。完整 live run 需要可访问的 CubeSandbox 部署和支持 `run_code` 的模板。
- Google ADK 原生 Agent Runtime Code Execution tool 面向 Google Cloud Agent Runtime。本文使用自定义 ADK 函数工具，让 CubeSandbox 提供执行后端。
- 默认示例每次工具调用创建并删除一个沙箱，便于 review，但不是长多步任务的最低延迟形态。

## 参考

- 可运行示例：
  [`examples/google-adk-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/google-adk-integration)
- Google ADK Python：
  <https://github.com/google/adk-python>
- ADK custom tools：
  <https://adk.dev/tools-custom/>
- ADK Agent Runtime Code Execution：
  <https://adk.dev/integrations/code-exec-agent-runtime/>
