---
title: LangChain 集成指南
author: peerless-hero
date: 2026-07-07
tags:
  - integration
  - langchain
  - agent
lang: zh-CN
---

# LangChain 集成指南

将调用 Python 工具的 [LangChain](https://github.com/langchain-ai/langchain) Agent 运行在
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) 的 MicroVM 中。由于 Cube 暴露了
**与 E2B 兼容的 API**，把 LangChain 应用从 E2B 迁移到 Cube 通常只需改几个环境变量，同时还能为
Agent 生成的每一行代码获得 KVM 级隔离。本文附带可运行的
[`examples/langchain-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration)
示例工程。

## 两个 LangChain 大版本（0.x 与 1.x）

本指南提供**两个变体**，仅在 LangChain 的 Agent API 与所需 Python 版本上不同。两者共用同一个
`run_python` 工具（基于官方 `cubesandbox` SDK）和同一个沙箱模板。

| 变体 | LangChain | langchain-openai | Python | Agent API | 示例路径 |
|---|---|---|---|---|---|
| **0.x**（传统） | `0.3.x` | `0.3.x` | 3.9+ | `AgentExecutor` + `create_react_agent` | `examples/langchain-integration/0.x` |
| **1.x**（现代） | `1.x` | `1.x` | 3.10+ | `langchain.agents.create_agent` + `@tool` | `examples/langchain-integration/1.x` |

若你在 Python 3.9 上，或已有 LangChain 0.3.x 代码 → 用 **0.x**。若在 Python 3.10+ 且从零开始
（或已在 LangChain 1.x 上）→ 用 **1.x**。

### 集成对象与版本

| 组件 | 0.x | 1.x |
|---|---|---|
| LangChain | `langchain==0.3.23` | `langchain>=1.3.14,<2.0` |
| langchain-openai | `langchain-openai==0.3.12` | `langchain-openai>=1.0,<2.0` |
| LangGraph（`create_agent` 运行时） | —（未使用；`AgentExecutor` + `create_react_agent` 不依赖 LangGraph） | `langgraph>=0.2`（已在 `1.x/requirements.txt` 显式声明，保证示例自包含） |
| cubesandbox SDK（主驱动） | `cubesandbox>=0.6.0` | `cubesandbox>=0.6.0` |
| CubeSandbox 基础镜像 | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` | 相同 |
| CubeSandbox 平台 | `>= 0.3.0`（核心）；可选特性见下表 | `>= 0.3.0`（核心）；可选特性见下表 |

各特性的平台最低版本（上面的 SDK 下限覆盖基础工作流；仅当你使用该特性时才提高
`cubesandbox`/平台版本）：

| 特性 | 最低平台 | 最低 SDK |
|---|---|---|
| 基础工作流（通过 49983 端口的 envd 进程 API 调用 `commands.run` / `files.write`、上下文管理器销毁） | `>= 0.3.0` | `>= 0.6.0` |
| CubeEgress 密钥注入（高级用法，默认拒绝出口流量） | `>= 0.4.0` | `>= 0.3.0` |
| 卷挂载（`volume_mounts=`、`Volume` API） | `>= 0.6.0` | `>= 0.6.0` |

## 前置条件

- 已部署 CubeSandbox，CubeAPI 可从 `http://<node>:3000` 访问。
- `cubemastercli` 已在 `$PATH` 且已连通集群。
- 构建机装有 Docker，且 registry 能被 Cube 节点拉取。
- Python 3.10+（1.x 变体）。0.x 变体在 Python 3.9+ 上也可运行。
- 能访问 Cube 集群节点。`cubesandbox` SDK 经 `CUBE_API_URL` 访问 CubeAPI、经 `CUBE_PROXY_NODE_IP`
  访问数据面（使 `*.cube.app` 无需 DNS 即可解析）；若代理未监听 80 端口，需设置 `CUBE_PROXY_PORT_HTTP`。
- 一个 OpenAI 兼容的 LLM 端点（示例使用 TokenHub；任何 OpenAI 兼容端点均可经
  `OPENAI_BASE_URL` / `OPENAI_API_KEY` 接入）。

## 为什么把 LangChain Agent 放进沙箱

LangChain Agent 经常会暴露代码执行工具（数据分析、文件转换、调用 shell）。在宿主机上运行该工具，
会把 Agent 的影响范围与你的开发机混在一起。放进 CubeSandbox 后，你将获得：

| 关注点 | CubeSandbox 提供 |
|---|---|
| **隔离** | 每个会话一个 KVM MicroVM，独立客户机内核——Agent 代码无法触碰宿主机 |
| **可复现** | 每次会话都从同一个模板快照启动 |
| **快速启动** | 冷启动低于 60ms，因此并行跑多个 Agent 成本很低 |
| **长任务** | `sandbox.pause()` 快照 VM + 根文件系统，稍后恢复 |
| **密钥卫生** | CubeEgress 可在网络层注入 LLM 鉴权头——VM 内永远看不到真实密钥 |
| **出口审计** | 每一次出站请求都会记录到出口审计日志 |

## 接入步骤

### 1. 构建模板镜像

在 `cubesandbox-base` 之上叠加 Python 数据科学栈（envd 已在 `:49983` 监听）。

```dockerfile
# examples/langchain-integration/Dockerfile
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        python3 python3-pip ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# 为可复现构建锁定版本，按需升级
RUN python3 -m pip install --no-cache-dir --upgrade pip \
    && python3 -m pip install --no-cache-dir --break-system-packages \
        pandas==2.2.3 numpy==1.26.4 matplotlib==3.9.3 scikit-learn==1.6.1

WORKDIR /workspace

# 预置演示数据集，使示例 Agent 无需外部输入即可运行
COPY sales.csv /workspace/sales.csv

EXPOSE 49983
```

构建并推送：

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/langchain-cube:latest \
  examples/langchain-integration
docker push <your-registry>/langchain-cube:latest
```

### 2. 注册为 Cube 模板

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/langchain-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

`create-from-image` 默认会阻塞并等待任务完成；如需提交后立即退出、稍后再轮询，请加 `--detach`，
之后用 `cubemastercli tpl watch --job-id <job_id>` 查看进度。

任务进入 `READY` 后，记下 `template_id`——每次 `Sandbox.create()` 都要传入它。`2G` 可写层适合中等
分析任务；若 Agent 运行时安装大体积包，可调到 `4G+`。

### 3. 安装依赖并配置环境变量

```bash
cd examples/langchain-integration/1.x      # 或 0.x，取决于你的 LangChain 版本
cp ../.env.example .env
# 填写 CUBE_API_URL、CUBE_API_KEY、CUBE_TEMPLATE_ID、CUBE_PROXY_NODE_IP 以及你的 LLM 密钥
pip install -r requirements.txt
```

| 变量 | 流向 | 说明 |
|---|---|---|
| `CUBE_API_URL` | `cubesandbox` SDK | CubeAPI 地址（`http://<node>:3000`） |
| `CUBE_API_KEY` | `cubesandbox` SDK | （可选）`X-API-Key` 请求头；仅需在启用了鉴权的 CubeAPI 后端设置——鉴权关闭时 SDK 不发送任何认证头 |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | 来自步骤 2 |
| `CUBE_PROXY_NODE_IP` | `cubesandbox` SDK | CubeProxy 节点 IP，使 `*.cube.app` 无需 DNS 解析 |
| `CUBE_PROXY_PORT_HTTP` | `cubesandbox` SDK | 代理 HTTP 端口（默认 `80`；若代理监听 8081 则填 `8081`） |
| `OPENAI_API_KEY` / `TOKENHUB_API_KEY` | LLM 客户端 | OpenAI 兼容密钥 |
| `OPENAI_BASE_URL` | LLM 客户端 | 如 `https://tokenhub.tencentmaas.com/v1` |
| `CHAT_MODEL` | LLM 客户端 | 如 `deepseek-v3` |
| `CUBE_SSL_CERT_FILE` | demo | （可选）HTTPS CubeAPI 的自签名 CA 证书路径。demo 会将其导出为 `SSL_CERT_FILE`（进程全局）以供 gRPC 客户端信任——该 bundle 应同时包含公共根证书，否则 LLM 端点也会受影响 |

### 4. 把代码执行工具接到 Cube

Agent 逻辑在各版本间不变，只是工具体从本地 REPL 换成 Cube 沙箱。下面这段是 `run_python` 工具的
**参考实现**——基于官方 **`cubesandbox` Python SDK**（`from cubesandbox import Sandbox`），
用 `sandbox.files.write` 上传代码、`sandbox.commands.run` 执行，因此 **无需裸 HTTP**。
（已发布示例 0.x/1.x 在 `build_agent` 内部内联了等价逻辑。）

```python
# run_python 工具 —— 官方 cubesandbox SDK（以下 snippet 使用此实现）
import itertools
from cubesandbox import Sandbox

def make_run_python(sandbox: Sandbox):
    """返回一个绑定到已创建 `sandbox` 的 run_python 工具。"""
    script_counter = itertools.count()
    def run_python(code: str) -> str:
        """在 Cube Sandbox MicroVM 内执行 Python；返回 stdout，若存在 stderr 则以分隔符附在其后。

        镜像已预装 pandas / numpy / matplotlib / scikit-learn。每次调用把代码片段写入唯一的
        /workspace/_agent_<n>.py 并运行，避免并发的工具调用互相覆盖。图表可保存到
        /workspace（如 /workspace/revenue.png）。
        """
        script = f"/workspace/_agent_{next(script_counter)}.py"
        sandbox.files.write(script, code)
        result = sandbox.commands.run(f"python3 {script}", timeout=120, cwd="/workspace")
        out = result.stdout
        # 用分隔符把 stderr 与 stdout 区分开，避免库警告（退出码为 0）混入
        # LLM 看到的真实输出。
        if result.stderr:
            out += "\n--- stderr ---\n" + result.stderr
        if result.exit_code != 0:
            out += f"\n[非零退出码: {result.exit_code}]"
        return out
    return run_python
```

> **一个沙箱跑完整轮、自动销毁。** 整个 Agent 循环只创建一个 MicroVM（`Sandbox.create(template=...)`），
> 并在 `with` 代码块退出时由上下文管理器调用 `sandbox.kill()`（`DELETE /sandboxes/:sandboxID`），
> 因此不会泄漏沙箱。随附示例正是采用这一模式。

### 5. 运行 Agent

两个变体都构建同一个 `ChatOpenAI` 客户端并复用共用的 `run_python` 工具，区别仅在 Agent 的构建方式。为整轮运行创建一个 `Sandbox`，并用 `make_run_python(sandbox)` 把工具绑定上去：

#### 1.x（现代 —— `langchain.agents.create_agent`）

```python
import os
from dotenv import load_dotenv

from langchain_openai import ChatOpenAI
from langchain.agents import create_agent
from langchain_core.tools import tool
from cubesandbox import Sandbox

load_dotenv()
_llm_key = os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY")
if not _llm_key:
    raise SystemExit("Missing LLM API key (set OPENAI_API_KEY or TOKENHUB_API_KEY)")

llm = ChatOpenAI(
    model=os.getenv("CHAT_MODEL") or "deepseek-v3",
    api_key=_llm_key,
    base_url=os.getenv("OPENAI_BASE_URL") or "https://tokenhub.tencentmaas.com/v1",
    timeout=60,
    max_retries=2,
    temperature=0,
)

SANDBOX_CONTEXT = (
    "You are a data analyst. You can execute Python inside a Cube Sandbox "
    "MicroVM via the run_python tool. Environment facts:\n"
    "- Working directory: /workspace\n"
    "- Demo dataset: /workspace/sales.csv with columns month,product,units,price\n"
    "  (6 rows: 3 months x 2 products; revenue is defined as units * price)\n"
    "- Preinstalled: pandas, numpy, matplotlib, scikit-learn\n"
    "- Save any charts/artifacts under /workspace\n"
    "When the user mentions 'the dataset' without a path, use /workspace/sales.csv.\n"
    "Modeling conventions for this tiny demo dataset (follow them unless the "
    "user explicitly specifies otherwise):\n"
    "- Regression/forecast target: monthly TOTAL revenue. Aggregate to one row "
    "per month, then use a numeric month index (0, 1, 2, ...) as the only "
    "feature.\n"
    "- Never use the target itself or its direct components (units, price) as "
    "features when predicting revenue - that is data leakage and yields a "
    "meaningless RMSE of 0.\n"
    "- The dataset is too small for a train/test split; fit and evaluate on "
    "all rows and explicitly state that the metric is in-sample.\n"
    "- Report only numbers actually printed by the executed code; never "
    "invent or estimate metric values."
)

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
    run_python = tool(make_run_python(sandbox))
    agent = create_agent(llm, [run_python], system_prompt=SANDBOX_CONTEXT)
    result = agent.invoke({"messages": [{"role": "user", "content":
        "从 /workspace 读取 sales.csv，按月计算总营收，并在最终回答中报告各月营收数字。"}]})
    for msg in reversed(result["messages"]):
        if msg.content:
            print(msg.content)
            break
    else:
        print("(no final answer in messages)")
```

#### 0.x（传统 —— `AgentExecutor` + `create_react_agent`）

```python
import os
from dotenv import load_dotenv

from langchain_openai import ChatOpenAI
from langchain.agents import AgentExecutor, create_react_agent
from langchain.tools import Tool
from langchain_core.prompts import PromptTemplate
from cubesandbox import Sandbox

load_dotenv()
_llm_key = os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY")
if not _llm_key:
    raise SystemExit("Missing LLM API key (set OPENAI_API_KEY or TOKENHUB_API_KEY)")

llm = ChatOpenAI(  # 与上面相同的参数
    model=os.getenv("CHAT_MODEL") or "deepseek-v3",
    api_key=_llm_key,
    base_url=os.getenv("OPENAI_BASE_URL") or "https://tokenhub.tencentmaas.com/v1",
    timeout=60, max_retries=2, temperature=0,
)

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
    run_python = make_run_python(sandbox)
    tools = [Tool(name="run_python", func=run_python,
                  description="在 Cube Sandbox MicroVM 中执行 Python 代码。")]
    agent = create_react_agent(llm, tools, PromptTemplate.from_template(
        "You are a data analyst. Answer the question using tools.\n\n"
        "Environment facts:\n"
        "- Demo dataset: /workspace/sales.csv with columns month,product,units,price\n"
        "  (6 rows: 3 months x 2 products; revenue is defined as units * price)\n"
        "- Preinstalled: pandas, numpy, matplotlib, scikit-learn\n"
        "- Save any charts/artifacts under /workspace\n"
        "When the user mentions 'the dataset' without a path, use /workspace/sales.csv.\n"
        "Modeling conventions (unless the user specifies otherwise):\n"
        "- Regression target: monthly TOTAL revenue; aggregate per month and use "
        "a numeric month index (0, 1, 2, ...) as the only feature.\n"
        "- Never use the target or its components (units, price) as features - "
        "that is data leakage and yields a meaningless RMSE of 0.\n"
        "- Too small for a train/test split; evaluate in-sample and say so.\n"
        "- Report only numbers actually printed by the executed code.\n\n"
        "Tools: {tools}\n\n"
        "Use the following format:\n\n"
        "Question: the input question you must answer\n"
        "Thought: you should always think about what to do\n"
        "Action: the action to take, should be one of [{tool_names}]\n"
        "Action Input: the input to the action\n"
        "Observation: the result of the action\n"
        "... (this Thought/Action/Action Input/Observation can repeat N times)\n"
        "Thought: I now know the final answer\n"
        "Final Answer: the final answer to the original input question\n\n"
        "Begin!\n\n"
        "Question: {input}\n\nThought: {agent_scratchpad}"))
    executor = AgentExecutor(agent=agent, tools=tools, verbose=True, handle_parsing_errors=True)
    result = executor.invoke({"input":
        "从 /workspace 读取 sales.csv，按月计算总营收，并在最终回答中报告各月营收数字。"})
    print(result["output"])
```

运行（在所选变体目录下）：

```bash
python langchain_agent_demo.py
# 自定义问题：
python langchain_agent_demo.py "在数据集上训练一个线性模型并报告 RMSE。"
```

### 预期效果

使用默认提示词时，宿主机上打印的 **Agent 最终回答**会包含各月营收数字
（2780.0 / 3375.5 / 3872.0）。（1.x 的 `create_agent` 默认不显示详细过程；0.x 的
`AgentExecutor(verbose=True)` 还会把完整推理过程打印到控制台。）

销毁是尽力而为：若 `kill()` 失败（例如沙箱已过期），SDK 的上下文管理器会**静默吞掉**错误——不会打印
任何警告。如果你需要感知销毁失败，请显式调用 `sandbox.kill()` 并自行处理 `CubeSandboxError`。

## 关键代码片段

### 把 E2B 应用迁移到 Cube

如果你的 LangChain 应用已经在驱动 E2B 沙箱，代码几乎完全一致——只要把 import 换成 `cubesandbox`
SDK，并指向 CubeAPI：

```diff
- from e2b_code_interpreter import Sandbox
- # E2B 云端（托管）
- export E2B_API_KEY="e2b_xxx"
- export E2B_API_URL="https://api.e2b.dev"   # 默认值，通常省略
+ from cubesandbox import Sandbox
+ # Cube Sandbox（自托管，MicroVM 隔离）
+ export CUBE_API_URL="http://<your-cube-host>:3000"
+ # export CUBE_API_KEY="<your-api-key>"  # 仅用于启用了鉴权的后端
+ export CUBE_TEMPLATE_ID="<your-cube-template-id>"
+ export CUBE_PROXY_NODE_IP="<your-cube-host>"
```

`Sandbox` 的 API（`Sandbox.create`、`commands.run`、`files.write`、`kill`）形态保持一致，
因此工具的其他代码无需改动。上面的示例正是使用这个 SDK。

### 本地基线（被替换的部分）

```python
# 改造前：代码经 langchain_experimental 在宿主机运行
from langchain_experimental.tools import PythonREPLTool
tools = [PythonREPLTool()]
```

把 `PythonREPLTool` 换成上面的 `run_python` Cube 工具，即可免费获得隔离能力。

## 注意事项

- **envd 用户。** `cubesandbox` SDK 的 `files.write` / `commands.run` 默认以 `root` 运行，
  无需额外配置。（基础镜像还预置了一个非 root 的 `uid=1000` 用户；若让 SDK 以该用户运行，
  文件权限行为会随之变化。）
- **清理安全。** `with Sandbox.create(...) as sandbox:` 上下文管理器在退出时调用 `sandbox.kill()`
  （`DELETE /sandboxes/:sandboxID`，**没有** `/kill` 子路径），即使 Agent 抛异常也不会泄漏沙箱。
- **一个沙箱跑完整轮、跨调用复用。** 整个 Agent 循环只创建一个 MicroVM，并在每次 `run_python` 调用间
  复用，因此只需支付一次生命周期开销。
- **状态不跨轮次保留。** `commands.run` 每次都是全新的 `python` 进程，变量/导入不会在工具调用间存续。
  把片段需要的一切内联进去，或把中间状态写到 `/workspace` 再读回。
- **模板必须包含所需栈。** 把 pandas/numpy/matplotlib 预装进镜像；在默认拒绝出口策略下，运行时
  `pip install` 会失败。
- **超时。** 同时设置沙箱 `timeout`（平台回收）和单次命令 `timeout`；长 Agent 循环可能耗尽任意一个。

## 进阶用法

### 网络隔离 + 密钥注入（原生 SDK）

在共享集群上，优先使用原生 `cubesandbox` SDK，配合默认拒绝出口与网络层密钥注入，让 LLM 密钥永
不进入 VM：

```python
import os
from cubesandbox import Sandbox, Rule, Match, Action, Inject

rules = [
    Rule(
        name="allow_llm",
        match=Match(scheme="https", sni="api.openai.com", host="api.openai.com"),
        action=Action(allow=True, audit="metadata", inject=[
            Inject(header="Authorization",
                   secret=os.environ["OPENAI_API_KEY"],
                   format="Bearer ${SECRET}"),
        ]),
    ),
]
with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    timeout=600,
    allow_internet_access=False,           # 默认拒绝；规则中的主机自动放行
    network={"rules": rules},
) as sandbox:
    run_agent(sandbox)                     # 退出 with 块时执行 sandbox.kill()
```

### 文件挂载

`Sandbox.create()` 没有 `mounts` 参数，请改用 Cube 特有的 API：

- **宿主机目录挂载**通过 `metadata["host-mount"]` 传入（一个 JSON 编码的
  `{hostPath, mountPath, readOnly}` 描述符列表），让 Agent 直接读写宿主机共享数据，
  无需经 `files.write` 来回搬运。注意 `hostPath` 必须位于**允许的前缀**之下
  （默认仅 `/data/shared/`；如需其他路径，请先在 CubeMaster 配置中扩展
  `allowed_host_mount_prefixes`）：

```python
import json
from cubesandbox import Sandbox

sandbox = Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    metadata={
        "host-mount": json.dumps([
            {"hostPath": "/data/shared/datasets", "mountPath": "/workspace/datasets",
             "readOnly": True},
        ]),
    },
)
```

- **卷挂载**使用 `volume_mounts={挂载路径: 卷ID}`（参见
  [`examples/volume`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/volume)）：

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-workspace")
sandbox = Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    volume_mounts={"/workspace": vol},
)
```

### 长任务暂停 / 恢复

`pause()` 返回 `None`——沙箱 ID 不会改变。恢复时对同一 ID 调用 `Sandbox.connect()` 即可：

```python
sandbox = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=1800)
try:
    turn_1(sandbox)
    sandbox.pause()
    sandbox = Sandbox.connect(sandbox.sandbox_id)   # 恢复后 /workspace 完好
    turn_2(sandbox)
finally:
    sandbox.kill()
```

## 故障排查

| 现象 | 可能原因 | 解决办法 |
|---|---|---|
| 文件操作 `permission denied` | envd 用户不匹配 | `cubesandbox` SDK 的 `files.write` / `commands.run` 默认以 `root` 运行。若使用 E2B SDK，请设置 `e2b.envd.rpc.default_username = "root"`。 |
| `command not found: python` | 模板缺少 Python | 重新构建包含 `python3` 的镜像 |
| `ModuleNotFoundError: pandas` | 镜像未含该栈 | 在 Dockerfile 中加入 pandas/numpy/matplotlib |
| `403 Forbidden - CubeEgress` | 默认拒绝且无放行规则 | 放行 LLM 主机（及其他所需主机） |
| 连接 CubeAPI `Connection refused` | `CUBE_API_URL` 错误 | 设为 `http://<node>:3000` |
| 模板卡在 `PULLING` | 集群无法访问仓库 | 推送到集群可访问的仓库 |
| `run_python` 无返回 | 脚本没有 stdout | 让 Agent 显式 `print()` 结果 |

## 参考资料

- 可运行示例 —— `0.x`（LangChain 0.3.x）：[`examples/langchain-integration/0.x`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration/0.x) · `1.x`（LangChain 1.x）：[`examples/langchain-integration/1.x`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langchain-integration/1.x)
- 自带镜像：[`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- 从镜像创建模板：[`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- 快照 / 克隆 / 回滚：[`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- 凭证保管 + 出口控制：[`docs/guide/security-proxy.md`](../security-proxy.md)
- LangChain：<https://github.com/langchain-ai/langchain>
- E2B SDK：<https://github.com/e2b-dev/e2b>
- `langchain_e2b`（E2B 官方 LangChain 集成包）：<https://github.com/e2b-dev/langchain_e2b>
