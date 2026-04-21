# OpenAI Agents SDK × CubeSandbox 集成指南

[English](openai-agents-sandbox-cube-integration.md)

> 基于本仓库的三个可运行 demo（[`simple_demo.py`](simple_demo.py)、[`code_interpreter_demo.py`](../openai-agents-code-interpreter/code_interpreter_demo.py)、[`code_interpreter_demo_ci.py`](../openai-agents-code-interpreter/code_interpreter_demo_ci.py)）提炼出来的落地文档。
>
> 概念性介绍见 [`openai-agents_zh.md`](openai-agents_zh.md)（OpenAI 官方 Sandbox Agents 文档的中文解析）。本文主要讲"怎么把它跑在 CubeSandbox 上"。

## 快速概览

[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) 的 API 与 [E2B](https://e2b.dev) 兼容，因此可以直接复用 OpenAI Agents SDK 官方的 `E2BSandboxClient`，**无需自己编写 Provider**。核心改动只有三步：

1. 将 `E2B_API_URL` 指向 CubeAPI 地址；
2. 在 `E2BSandboxClientOptions` 中填入 CubeSandbox 模板 ID；
3. 添加少量运行时补丁以适配 CubeSandbox（root 用户、流式输出、SSL 隔离等）。

支持两种 Python 执行形态：

| 形态 | 类型 | 特点 |
|------|------|------|
| **通用 E2B** | `E2BSandboxType.E2B` | 脚本 `write + exec`，兼容性最好 |
| **Code Interpreter** | `E2BSandboxType.CODE_INTERPRETER` | Jupyter kernel，状态跨轮保留、图像自动捕获 |

## 1. 背景：

```
┌─────────────────────────┐                     ┌──────────────────────────┐
│  OpenAI Agents SDK      │  E2B-compatible     │  CubeSandbox             │
│  (控制面 / Harness)      │  gRPC + HTTPS       │  (执行面 / Compute)       │
│                         │ ──────────────▶     │                          │
│  SandboxAgent           │                     │  envd (49983)            │
│  ├── Runner             │ session.exec()      │  ├── sh / python         │
│  ├── Capability         │ session.write()     │  ├── fs read/write       │
│  └── Manifest           │ session.read()      │  └── expose port         │
│                         │                     │                          │
│  E2BSandboxClient ◀─────┘                     │  code-interpreter (49999)│
│                                               │  └── run_code()          │
└─────────────────────────┘                     └──────────────────────────┘
```

OpenAI Agents SDK 负责模型调用、工具路由和会话恢复；CubeSandbox 负责文件读写、命令执行和端口暴露。两者通过 E2B 兼容协议对接，CubeSandbox 在底层提供更强的安全隔离。

## 2. 集成总览

| 环节 | Agents SDK 侧 | CubeSandbox 侧 |
|---|---|---|
| 客户端 | `E2BSandboxClient()` | CubeAPI，指向 `E2B_API_URL` |
| 模板 | `E2BSandboxClientOptions(template=...)` | `cubemastercli template create-from-image` 生成 |
| 生命周期 | `RunConfig(sandbox=SandboxRunConfig(...))` | envd 拉起 + 模板镜像启动 |
| 工作区种子 | `Manifest(entries={...})` | envd 在 rootfs 写入 |
| 工具 | `Capability` + `FunctionTool` | `session.exec / write / read` |
| 会话恢复 | `pause_on_exit=True` + resume | 沙箱状态保留 |

## 3. 最小可用集成

来自 [`simple_demo.py`](simple_demo.py) —— 一个只带 `Shell` 能力的 agent。

```python
from agents import Runner
from agents.run import RunConfig
from agents.sandbox import Manifest, SandboxAgent, SandboxRunConfig
from agents.sandbox.capabilities import Shell
from agents.extensions.sandbox import (
    E2BSandboxClient, E2BSandboxClientOptions, E2BSandboxType,
)

agent = SandboxAgent(
    name="Cube Shell Agent",
    model=make_model("openai/glm-5.1"),
    instructions="You can run shell commands in the sandbox.",
    default_manifest=Manifest(entries={}),
    capabilities=[Shell()],
)

run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=E2BSandboxClient(),
        options=E2BSandboxClientOptions(
            sandbox_type=E2BSandboxType.E2B,
            template=os.environ["CUBE_TEMPLATE_ID"],
            timeout=600,
        ),
    ),
)
result = await Runner.run(agent, "What Linux distro is this?", run_config=run_config)
```

只要 `E2B_API_URL` 指向 CubeAPI，以上代码**和 E2B SaaS 上的写法完全一致**。

## 4. Code Interpreter 两种形态

本仓库 `openai-agents-code-interpreter/` 目录下有两份等价 demo，差异如下：

| | [`code_interpreter_demo.py`](../openai-agents-code-interpreter/code_interpreter_demo.py) | [`code_interpreter_demo_ci.py`](../openai-agents-code-interpreter/code_interpreter_demo_ci.py) |
|---|---|---|
| 沙箱类型 | `E2BSandboxType.E2B` | `E2BSandboxType.CODE_INTERPRETER` |
| 底层 SDK | `e2b.AsyncSandbox` | `e2b_code_interpreter.AsyncSandbox` |
| 执行方式 | `session.write(script.py)` → `session.exec("python script.py")` | `sandbox.run_code(code)`（Jupyter kernel） |
| 会话状态 | 每次 `exec` 都是新进程 | **变量 / imports / DataFrame 跨 cell 保留** |
| 图像捕获 | agent 手动 `plt.savefig(...)` | 自动从 `Execution.results` 解 base64 |
| 模板端口 | envd (49983) | envd (49983) + code-interpreter (49999) |

两份都用同样的 `WorkspaceShell` + `PythonRunner` 组合，只是 `PythonRunner._invoke` 实现不同。

### 通用 E2B 写法（`code_interpreter_demo.py`）

```308:349:openai-agents-code-interpreter/code_interpreter_demo.py
    async def _invoke(self, _ctx: Any, raw_args: str) -> str:
        session = self.session
        if session is None:
            raise RuntimeError("python_runner is not bound to a sandbox session")

        request = PythonRunRequest.model_validate_json(raw_args)

        await session.mkdir(Path(".scratch"), parents=True)
        await session.mkdir(Path("output"), parents=True)

        script_rel = Path(f".scratch/{uuid.uuid4().hex}.py")
        await session.write(
            script_rel,
            io.BytesIO(request.code.encode("utf-8")),
        )
        ...
```

思路：`write` 一个临时脚本 → `exec("python script.py")` → `find output/` diff 出新产物。
**任何装了 python 的 cube 模板都能跑**。

### Jupyter kernel 写法（`code_interpreter_demo_ci.py`）

关键是要绕过 SDK 的 wrapper，拿到底层 `e2b_code_interpreter.AsyncSandbox`：

```415:469:openai-agents-code-interpreter/code_interpreter_demo_ci.py
def _find_code_interpreter_sandbox(session: Any, *, max_depth: int = 4) -> Any | None:
    """Locate the underlying `e2b_code_interpreter.AsyncSandbox` on the session.

    Different openai-agents versions stack several wrappers before the raw
    sandbox:
        SandboxSession._inner -> E2BSandboxSession._sandbox -> AsyncSandbox
```

拿到后就可以 `await inner.run_code(code, on_stdout=..., on_stderr=..., timeout=...)`，返回的 `Execution` 结构携带 `logs / results / error`，图像结果直接 base64 解码到 `output/figure_*.png`。

## 5. 关键集成代码详解（`code_interpreter_demo_ci.py`）

以下代码片段摘自 [`code_interpreter_demo_ci.py`](../openai-agents-code-interpreter/code_interpreter_demo_ci.py)，展示 OpenAI Agents SDK 与 CubeSandbox 对接的完整链路。


### 5.1 LLM 模型构造（SSL 隔离 + Chat Completions API）

CubeSandbox 的 gRPC 需要自定义 CA，但 `SSL_CERT_FILE` 会污染 LLM 的 HTTPS 请求。解法是给 LLM 客户端单独用系统 CA：

```python
def make_model(model_name: str) -> OpenAIChatCompletionsModel:
    import ssl
    ssl_ctx = ssl.create_default_context()  # 系统 CA，不受 SSL_CERT_FILE 影响
    client = AsyncOpenAI(
        timeout=httpx.Timeout(120, connect=15),
        http_client=httpx.AsyncClient(verify=ssl_ctx),
    )
    bare = model_name.split("/", 1)[-1] if "/" in model_name else model_name
    return OpenAIChatCompletionsModel(model=bare, openai_client=client)
```

> 注意：必须用 `OpenAIChatCompletionsModel` 而非直接传模型字符串——后者会走 Responses API（`/v1/responses`），而 TokenHub 只支持 Chat Completions API（`/v1/chat/completions`）。

### 5.2 Agent 构建 + RunConfig 对接 CubeSandbox

这是整个集成的核心入口，把 Agent、Capability、Manifest 和 CubeSandbox 连在一起：

```python
agent = SandboxAgent(
    name="Cube Code Interpreter Analyst (kernel)",
    model=make_model(model),                     # §5.1 的 LLM 模型
    instructions=SYSTEM_INSTRUCTIONS,
    default_manifest=build_manifest(),            # 种子文件 (sales.csv, README.md)
    capabilities=[WorkspaceShell(), PythonRunner()],  # 自定义能力
    model_settings=ModelSettings(tool_choice="auto"),
)

run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=E2BSandboxClient(),                # 直接用官方 E2B 客户端
        options=E2BSandboxClientOptions(
            sandbox_type=E2BSandboxType.CODE_INTERPRETER,  # Jupyter kernel 模式
            template=template,                    # CubeSandbox 模板 ID
            timeout=timeout,                      # 沙箱生命周期（秒）
            pause_on_exit=pause_on_exit,          # True = 暂停而非销毁，支持恢复
        ),
    ),
)

result = await Runner.run(agent, question, run_config=run_config)
```

### 5.3 Manifest 种子文件

通过 `Manifest` 在沙箱启动时自动写入工作区文件，agent 可直接以相对路径读取：

```python
def build_manifest() -> Manifest:
    return Manifest(
        entries={
            "sales.csv": File(content=SALES_CSV.encode("utf-8")),
            "README.md": File(content=b"# Sales review\n\n`sales.csv` has ..."),
        }
    )
```

### 5.4 PythonRunner：Jupyter kernel 执行能力

`PythonRunner` 是一个自定义 `Capability`：

**调用 `run_code` 并捕获图像结果**：

```python
execution = await _run_code_annotated(
    inner, request.code,
    on_stdout=_on_stdout, on_stderr=_on_stderr,
    timeout=request.timeout_s,
)

# 自动将 matplotlib 图表从 base64 解码并保存
for result in execution.results:
    for kind, ext in _IMAGE_KINDS:        # png, jpeg, svg
        encoded = getattr(result, kind, None)
        if not encoded:
            continue
        blob = base64.b64decode(encoded)
        rel_path = Path(f"output/figure_{counter:02d}.{ext}")
        await session.write(rel_path, io.BytesIO(blob))
```

### 5.5 集成调用链路总结

```
main()
 ├── load_env()                          # 环境变量：E2B_API_URL → CubeAPI
 ├── make_model()                        # LLM 客户端（SSL 隔离）
 ├── build_manifest()                    # 种子文件
 ├── SandboxAgent(capabilities=[...])    # Agent + 自定义能力
 ├── RunConfig(client=E2BSandboxClient)  # 对接 CubeSandbox
 └── Runner.run(agent, question)
      ├── CubeSandbox 创建沙箱实例
      ├── envd 写入 Manifest 文件
      ├── LLM 生成工具调用
      │    ├── WorkspaceShell → session.exec("sh -lc ...")
      │    └── PythonRunner   → inner.run_code(code)
      │         ├── Jupyter 就绪探测
      │         ├── kernel CWD 对齐
      │         ├── 执行 Python 代码
      │         └── 自动捕获图表 → output/figure_*.png
      └── 返回最终分析结果
```

## 6. 端到端运行示例

### 6.1 Code Interpreter：数据分析 + 图表生成

```bash
python3 code_interpreter_demo_ci.py
```

```
Model:    openai/glm-5.1
Template: e2b-code-interpreter-2
CubeAPI:  http://localhost:3000
Question: Analyze sales.csv with Python: (1) compute revenue = units * unit_price,
          (2) plot monthly total revenue as a line chart, (3) report the top-3
          products by total revenue as a Markdown table, (4) finish with a
          2-sentence executive summary.

Creating sandbox & running agent ...
[python] code-interpreter ready: probe #3: HTTP_OK status=200 body[:300]='"OK"'
[python] bootstrapped kernel cwd -> /workspace
[python] running 121 bytes in kernel
[python] (14, 4)
[python] ['date', 'product', 'units', 'unit_price']
[python] date           object
[python] product        object
[python] units           int64
[python] unit_price    float64
[python] dtype: object
[python] (ok)
[python] running 567 bytes in kernel
[python] saved output/figure_01.png (32115 bytes)
[python] (ok)
[python] running 505 bytes in kernel
[python] | Product      |   Total Revenue |
[python] |:-------------|----------------:|
[python] | Gamma Gizmo  |          2178   |
[python] | Beta Gadget  |          2156   |
[python] | Alpha Widget |          1512.4 |
[python]
[python] Total revenue: $7,007.40
[python] Peak month: 2025-04 ($2,333.50)
[python] (ok)
================================================================
Here are the results:

**Monthly Revenue Line Chart** → `output/figure_01.png`

**Top-3 Products by Total Revenue**

| Product      | Total Revenue |
|:-------------|--------------:|
| Gamma Gizmo  |       2,178.00 |
| Beta Gadget  |       2,156.00 |
| Alpha Widget |       1,512.40 |

**Executive Summary**

Total revenue across the period was **$7,007.40**, with April 2025 as the
strongest month at **$2,333.50**. Gamma Gizmo narrowly leads product revenue
($2,178), followed closely by Beta Gadget ($2,156), while Alpha Widget
trails at $1,512.40.
================================================================
```

整个流程：Manifest 种入 `sales.csv` → Jupyter kernel 就绪探测 → 3 轮 `run_code`（加载数据 → 画图 → 汇总表格）→ 模型输出最终分析。图表自动从 kernel 结果中捕获并保存到 `output/`。

## 7. 模板要求

cube template 需要预装 / 暴露什么，取决于跑哪种形态：

| 形态 | 预装软件 | 暴露端口 | 探针 |
|---|---|---|---|
| 通用 E2B | python3 + 业务依赖（pandas / numpy / matplotlib …） | 49983 (envd) | 49983 |
| Code Interpreter | 以上 + 官方 `e2b-code-interpreter` runtime | 49983 + **49999** | 49983 |

注册模板示例（取自 `openai-agents-code-interpreter/README.md`）：

```bash
cubemastercli template create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/e2b-code-interpreter:v1.1-data \
  --writable-layer-size 1Gi \
  --template-id e2b-code-interpreter \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49983
```

关键环境变量（在各 demo 的 `.env.example`）：

| 变量 | 用途 |
|---|---|
| `E2B_API_URL` | CubeAPI 地址，例如 `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI 鉴权 key |
| `CUBE_TEMPLATE_ID` | cube 模板 ID |
| `CUBE_SSL_CERT_FILE` | （可选）cube 的 CA bundle 路径 |
| `TOKENHUB_API_KEY` | LLM 密钥（会自动映射到 `OPENAI_API_KEY`） |
| `OPENAI_BASE_URL` | 默认指向 TokenHub，`https://tokenhub.tencentmaas.com/v1` |


## 8. 延伸

- [OpenAI Sandbox Agents 官方文档](https://developers.openai.com/api/docs/guides/agents/sandboxes)
- [E2B Sandbox 文档](https://e2b.dev/docs) / [E2B × OpenAI Agents SDK](https://e2b.dev/docs/agents/openai-agents-sdk)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
