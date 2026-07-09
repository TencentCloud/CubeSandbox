---
title: CrewAI 集成指南
author: ruirui6946
date: 2026-06-23
tags:
  - integration
  - crewai
lang: zh-CN
---

# CrewAI 集成指南

## 集成对象与版本

[CrewAI](https://www.crewai.com/) 是一个用于编排角色化 AI Agent 的框架。本指南将 CrewAI 官方的 `E2BPythonTool` 接入 Cube Sandbox，让 Agent 在隔离的 MicroVM 中执行 Python，而不是在宿主机上运行生成代码。

该集成利用 Cube Sandbox 对 E2B API 的兼容能力。已有 CrewAI 代码可以继续使用 `E2BPythonTool`；基础设施侧只需将 `E2B_API_URL` 指向 CubeAPI，并选择一个 Cube 模板。

- 已验证 CrewAI 版本：`1.14.7`
- Python：`3.10+`
- Cube 客户端：由 `crewai-tools[e2b]` 安装的 `e2b-code-interpreter`
- 集成类型：隔离的 Python 执行工具

## 前置条件

1. 按照[部署指南](../bare-metal-deploy.md)部署 Cube Sandbox。
2. 创建一个包含 Python 和 Agent 所需依赖的代码解释器模板。Cube 模板 ID 使用 `tpl-<hex>` 格式：

   ```bash
   cubemastercli tpl create-from-image \
     --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
     --writable-layer-size 1G \
     --expose-port 49999 \
     --expose-port 49983 \
     --probe 49999
   ```

   国际访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。如果你在中国大陆，请使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。

3. 安装依赖：

   ```bash
   pip install "crewai>=1.14.7,<2" "crewai-tools[e2b]>=1.14.7,<2" python-dotenv
   ```

4. 配置 CubeAPI 与 LLM：

   ```bash
   export E2B_API_URL="http://<cube-api-host>:3000"
   export E2B_API_KEY="<cube-api-key>"
   export CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxxxxxxxxxx"

   export OPENAI_API_KEY="<your-llm-api-key>"
   export MODEL="openai/gpt-4o-mini"
   # 使用 OpenAI 兼容服务时可选：
   # export OPENAI_BASE_URL="https://your-provider.example/v1"
   ```

`E2B_API_URL` 必须指向通常监听 `3000` 端口的 Cube API Server，而不是 CubeProxy。

::: warning 保护 Cube API Key
上面的 `http://` 示例只适合在可信机器上进行本地开发。生产环境应为 CubeAPI 配置 TLS 并使用 `https://`，或将 CubeAPI 绑定到 loopback 并使用 `http://127.0.0.1:3000`，避免 `E2B_API_KEY` 以明文形式在网络中传输。
:::

## 接入步骤

### 1. 创建由 Cube 驱动的 CrewAI 工具

CrewAI 已经提供 `E2BPythonTool`，因此无需自行实现 `BaseTool`：

```python
import os

from crewai_tools import E2BPythonTool

cube_python = E2BPythonTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=False,
)
```

当 `persistent=False` 时，每次工具调用都会获得一个全新的 Cube MicroVM，并在执行结束后销毁。这是运行 Agent 生成代码时最安全的默认值。

### 2. 将工具绑定到 Agent

```python
import os

from crewai import Agent, Crew, LLM, Process, Task
from crewai_tools import E2BPythonTool

api_key = os.getenv("OPENAI_API_KEY")
if not api_key:
    raise RuntimeError("Missing required environment variable: OPENAI_API_KEY")

llm_options = {
    "model": os.getenv("MODEL", "openai/gpt-4o-mini"),
    "api_key": api_key,
}
if os.getenv("OPENAI_BASE_URL"):
    llm_options["base_url"] = os.environ["OPENAI_BASE_URL"]

cube_python = E2BPythonTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=False,
)

analyst = Agent(
    role="沙箱数据分析师",
    goal="使用隔离的 Python 执行环境生成可复现的答案",
    backstory=(
        "你会在 Cube Sandbox 中运行 Python 来验证所有数值结果，"
        "绝不在宿主机上执行生成的代码。"
    ),
    tools=[cube_python],
    llm=LLM(**llm_options),
    verbose=os.getenv("CREWAI_VERBOSE", "").lower() == "true",
)

task = Task(
    description=(
        "使用沙箱 Python 工具，以随机种子 7 模拟掷两枚公平骰子 10,000 次。"
        "给出点数和为 8 的估计概率，并与精确概率 5/36 比较。"
    ),
    expected_output=(
        "一份简短报告，包含模拟概率、精确概率、绝对误差和使用的 Python 方法。"
    ),
    agent=analyst,
)

try:
    result = Crew(
        agents=[analyst],
        tasks=[task],
        process=Process.sequential,
    ).kickoff()
except Exception as exc:
    raise RuntimeError(
        "Crew execution failed. Check LLM credentials, CubeAPI connectivity, "
        "and sandbox execution timeouts."
    ) from exc

print(result)
```

### 3. 在调用 LLM 前验证 Cube

排查连接问题时，可以直接调用工具：

```python
import json

result = cube_python.run(
    code=(
        "import json\n"
        "print(json.dumps({'runtime': 'cube', 'sum': sum(range(10))}, sort_keys=True))"
    ),
    timeout=30,
)
payload = json.loads(str(result).strip().splitlines()[-1])
if (
    not isinstance(payload, dict)
    or payload.get("runtime") != "cube"
    or payload.get("sum") != 45
):
    raise RuntimeError(f"Unexpected Cube smoke test payload: {payload!r}")
print(json.dumps(payload, sort_keys=True))
```

这样可以把 CubeAPI、模板和 SDK 配置问题，与 LLM 或 CrewAI 编排问题分开验证。

## 从 E2B 近零改动迁移

如果 Crew 已经使用 `E2BPythonTool`，Python 代码无需改变：

```diff
 from crewai_tools import E2BPythonTool

 tool = E2BPythonTool(
-    template="base",
+    template=os.environ["CUBE_TEMPLATE_ID"],
 )
```

只需修改环境变量：

```diff
-E2B_API_URL=https://api.e2b.dev
+E2B_API_URL=http://<cube-api-host>:3000
```

Agent、Task 和工具调用逻辑保持不变，代码执行环境则切换为 Cube 的 MicroVM 隔离。

## 进阶配置

### 跨工具调用保留状态

当 Agent 需要复用导入、变量或生成的文件时，可以启用持久模式：

```python
cube_python = E2BPythonTool(
    template=os.environ["CUBE_TEMPLATE_ID"],
    persistent=True,
    sandbox_timeout=300,
)

try:
    # 在一个或多个 Agent 中使用 cube_python。
    result = crew.kickoff()
finally:
    cube_python.close()
```

持久沙箱会让状态跨调用保留，因此也会放大提示词注入的影响。应保持较短的超时时间，且不要注入权限过大的凭证。

### 限制网络访问

Cube 在 E2B 创建接口上扩展了网络策略。需要这些能力时，可以直接创建沙箱，或在一个轻量 CrewAI 自定义工具中暴露相同参数：

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    allow_internet_access=False,
    network={"allow_out": ["10.0.1.0/24", "api.example.com", "*.example.org"]},
) as sandbox:
    execution = sandbox.run_code("print('isolated execution')")
```

`allow_out` 接收 IPv4/CIDR 目标和 DNS 域名目标，也支持前缀 `*.` 通配域名。通配域名只匹配子域名，例如 `api.example.org`，不匹配顶级的 `example.org`。域名目标会通过 DNS A 记录学习为临时 IP 放行项；如果白名单需要严格生效，请使用 `allow_internet_access=False` 或显式 deny-all 兜底。`deny_out` 仍然是 IPv4/IP-CIDR 策略。

### 挂载宿主机数据

宿主机目录挂载是通过沙箱 metadata 编码的 Cube 扩展能力：

::: warning 校验宿主机挂载
应把 `hostPath` 视为高权限配置，在传给 `Sandbox.create()` 前先与小范围 allowlist 校验；优先使用 `readOnly: true`，并避免让受提示词控制的 Agent 输入构造 host-mount metadata。宿主机挂载会把对应宿主机路径暴露给沙箱，虽然其他路径仍受 MicroVM 隔离保护；读写挂载还可能让沙箱内代码修改宿主机状态。
:::

```python
import json

mounts = json.dumps([
    {
        "hostPath": "/srv/agent-input",
        "mountPath": "/mnt/input",
        "readOnly": True,
    }
])

with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    metadata={"host-mount": mounts},
) as sandbox:
    execution = sandbox.run_code(
        "from pathlib import Path; print(list(Path('/mnt/input').iterdir()))"
    )
```

宿主机路径必须预先存在于 Cubelet 节点。Agent 输入应优先使用只读挂载。

### 限制执行时间

这里有两种不同的超时：

- `E2BPythonTool` 的 `sandbox_timeout` 控制持久模式下的沙箱空闲生命周期。
- 传给 `tool.run(...)` 的 `timeout` 控制单次代码执行时间。

临时模式下应设置单次执行超时；当 `persistent=True` 时，还应设置较短的 `sandbox_timeout`，避免空闲的持久沙箱长期存活。

## 注意事项

- 模板必须包含 Cube 的 `envd` 服务。`python:3.12-slim` 等普通镜像本身不是有效的 Cube 模板。
- Cube 模板 ID 是 `tpl-...` 形式的生成 ID，而不是 Docker 镜像名。
- LLM API Key 属于 CrewAI 进程；只向沙箱传入任务级、最小权限的凭证。
- 即使 MicroVM 能保护宿主机，也应把不可信提示词生成的代码和输出视为不可信内容。
- 除非任务明确需要跨调用状态，否则应使用临时模式。

## 可运行示例

完整的中英文示例位于 [`examples/crewai-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/crewai-integration)。先运行 `smoke_test.py` 验证 Cube，再运行 `main.py` 启动 CrewAI Agent。

## 参考资料

- [CrewAI E2B Sandbox Tools](https://docs.crewai.com/v1.15.2/en/tools/ai-ml/e2bsandboxtools.md)
- [CrewAI 自定义工具](https://docs.crewai.com/v1.15.2/en/learn/create-custom-tools.md)
- [Cube Sandbox Python 示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/code-sandbox-quickstart)
- [Cube 网络策略示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/network-policy)
- [Cube 宿主机挂载示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount)
