# OpenAI Agents SDK × CubeSandbox Integration Guide

[中文文档](openai-agents-sandbox-cube-integration_zh.md)

> A field-tested guide distilled from the three runnable demos in this repository ([`simple_demo.py`](simple_demo.py), [`code_interpreter_demo.py`](../openai-agents-code-interpreter/code_interpreter_demo.py), [`code_interpreter_demo_ci.py`](../openai-agents-code-interpreter/code_interpreter_demo_ci.py)).
>
> For the conceptual background, see [`openai-agents.md`](openai-agents.md) (an analysis of OpenAI's official Sandbox Agents documentation). This document focuses on "how to run it on CubeSandbox".

## TL;DR

[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) exposes an API that is compatible with [E2B](https://e2b.dev), so you can reuse the OpenAI Agents SDK's official `E2BSandboxClient` **without writing your own provider**. You only need three changes:

1. Point `E2B_API_URL` at the CubeAPI endpoint.
2. Pass a CubeSandbox template ID via `E2BSandboxClientOptions`.
3. Apply a small number of runtime patches to align with CubeSandbox (root user, streaming output, SSL isolation, etc.).

Two Python execution modes are supported:

| Mode | Type | Characteristics |
|------|------|-----------------|
| **Generic E2B** | `E2BSandboxType.E2B` | `write + exec` a script, maximum compatibility |
| **Code Interpreter** | `E2BSandboxType.CODE_INTERPRETER` | Jupyter kernel, state persists across turns, images captured automatically |

## 1. Background

```
┌─────────────────────────┐                     ┌──────────────────────────┐
│  OpenAI Agents SDK      │  E2B-compatible     │  CubeSandbox             │
│  (Control plane/Harness)│  gRPC + HTTPS       │  (Execution plane/Compute)│
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

The OpenAI Agents SDK handles model calls, tool routing, and session resume. CubeSandbox provides filesystem operations, command execution, and port exposure. The two sides communicate via the E2B-compatible protocol, with CubeSandbox offering stronger isolation underneath.

## 2. Integration Overview

| Concern | Agents SDK side | CubeSandbox side |
|---------|-----------------|------------------|
| Client | `E2BSandboxClient()` | CubeAPI, pointed to by `E2B_API_URL` |
| Template | `E2BSandboxClientOptions(template=...)` | Produced by `cubemastercli template create-from-image` |
| Lifecycle | `RunConfig(sandbox=SandboxRunConfig(...))` | envd boot + template image startup |
| Workspace seed | `Manifest(entries={...})` | envd writes into the rootfs |
| Tools | `Capability` + `FunctionTool` | `session.exec / write / read` |
| Session resume | `pause_on_exit=True` + resume | Sandbox state retained |

## 3. Minimal Working Integration

From [`simple_demo.py`](simple_demo.py) — an agent with only the `Shell` capability.

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

As long as `E2B_API_URL` points at the CubeAPI, the code is **identical to what you would write against E2B SaaS**.

## 4. Two Code Interpreter Variants

The `openai-agents-code-interpreter/` directory contains two equivalent demos that differ as follows:

| | [`code_interpreter_demo.py`](../openai-agents-code-interpreter/code_interpreter_demo.py) | [`code_interpreter_demo_ci.py`](../openai-agents-code-interpreter/code_interpreter_demo_ci.py) |
|---|---|---|
| Sandbox type | `E2BSandboxType.E2B` | `E2BSandboxType.CODE_INTERPRETER` |
| Underlying SDK | `e2b.AsyncSandbox` | `e2b_code_interpreter.AsyncSandbox` |
| Execution model | `session.write(script.py)` → `session.exec("python script.py")` | `sandbox.run_code(code)` (Jupyter kernel) |
| Session state | Each `exec` is a fresh process | **Variables / imports / DataFrames persist across cells** |
| Image capture | Agent calls `plt.savefig(...)` manually | Auto-decoded from `Execution.results` (base64) |
| Template ports | envd (49983) | envd (49983) + code-interpreter (49999) |

Both share the same `WorkspaceShell` + `PythonRunner` combination; only the implementation of `PythonRunner._invoke` differs.

### Generic E2B style (`code_interpreter_demo.py`)

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

Idea: `write` a temporary script → `exec("python script.py")` → diff `output/` to collect new artifacts.
**Any cube template with Python installed can run this.**

### Jupyter kernel style (`code_interpreter_demo_ci.py`)

The trick is to punch through the SDK wrappers to reach the underlying `e2b_code_interpreter.AsyncSandbox`:

```415:469:openai-agents-code-interpreter/code_interpreter_demo_ci.py
def _find_code_interpreter_sandbox(session: Any, *, max_depth: int = 4) -> Any | None:
    """Locate the underlying `e2b_code_interpreter.AsyncSandbox` on the session.

    Different openai-agents versions stack several wrappers before the raw
    sandbox:
        SandboxSession._inner -> E2BSandboxSession._sandbox -> AsyncSandbox
```

Once you have it, you can call `await inner.run_code(code, on_stdout=..., on_stderr=..., timeout=...)`. The returned `Execution` structure carries `logs / results / error`, and image results are base64-decoded straight into `output/figure_*.png`.

## 5. Key Integration Code Walkthrough (`code_interpreter_demo_ci.py`)

The snippets below are excerpted from [`code_interpreter_demo_ci.py`](../openai-agents-code-interpreter/code_interpreter_demo_ci.py) and show the complete integration chain between the OpenAI Agents SDK and CubeSandbox.

### 5.1 LLM model construction (SSL isolation + Chat Completions API)

CubeSandbox's gRPC requires a custom CA, but setting `SSL_CERT_FILE` would contaminate the LLM's HTTPS requests. The fix is to give the LLM client its own system-CA context:

```python
def make_model(model_name: str) -> OpenAIChatCompletionsModel:
    import ssl
    ssl_ctx = ssl.create_default_context()  # System CAs, unaffected by SSL_CERT_FILE
    client = AsyncOpenAI(
        timeout=httpx.Timeout(120, connect=15),
        http_client=httpx.AsyncClient(verify=ssl_ctx),
    )
    bare = model_name.split("/", 1)[-1] if "/" in model_name else model_name
    return OpenAIChatCompletionsModel(model=bare, openai_client=client)
```

> Note: you must use `OpenAIChatCompletionsModel` rather than passing a plain model string — the latter routes via the Responses API (`/v1/responses`), but TokenHub only supports the Chat Completions API (`/v1/chat/completions`).

### 5.2 Agent construction + RunConfig wired to CubeSandbox

This is the entry point of the integration, tying together Agent, Capability, Manifest and CubeSandbox:

```python
agent = SandboxAgent(
    name="Cube Code Interpreter Analyst (kernel)",
    model=make_model(model),                     # The LLM model from §5.1
    instructions=SYSTEM_INSTRUCTIONS,
    default_manifest=build_manifest(),            # Seed files (sales.csv, README.md)
    capabilities=[WorkspaceShell(), PythonRunner()],  # Custom capabilities
    model_settings=ModelSettings(tool_choice="auto"),
)

run_config = RunConfig(
    sandbox=SandboxRunConfig(
        client=E2BSandboxClient(),                # Use the official E2B client directly
        options=E2BSandboxClientOptions(
            sandbox_type=E2BSandboxType.CODE_INTERPRETER,  # Jupyter kernel mode
            template=template,                    # CubeSandbox template ID
            timeout=timeout,                      # Sandbox lifetime (seconds)
            pause_on_exit=pause_on_exit,          # True = pause instead of destroy (resumable)
        ),
    ),
)

result = await Runner.run(agent, question, run_config=run_config)
```

### 5.3 Manifest seed files

The `Manifest` writes workspace files automatically when the sandbox starts; the agent can then read them by relative path:

```python
def build_manifest() -> Manifest:
    return Manifest(
        entries={
            "sales.csv": File(content=SALES_CSV.encode("utf-8")),
            "README.md": File(content=b"# Sales review\n\n`sales.csv` has ..."),
        }
    )
```

### 5.4 PythonRunner: Jupyter kernel execution capability

`PythonRunner` is a custom `Capability`.

**Call `run_code` and capture image results**:

```python
execution = await _run_code_annotated(
    inner, request.code,
    on_stdout=_on_stdout, on_stderr=_on_stderr,
    timeout=request.timeout_s,
)

# Automatically decode matplotlib figures from base64 and save them
for result in execution.results:
    for kind, ext in _IMAGE_KINDS:        # png, jpeg, svg
        encoded = getattr(result, kind, None)
        if not encoded:
            continue
        blob = base64.b64decode(encoded)
        rel_path = Path(f"output/figure_{counter:02d}.{ext}")
        await session.write(rel_path, io.BytesIO(blob))
```

### 5.5 End-to-end call graph

```
main()
 ├── load_env()                          # Env vars: E2B_API_URL → CubeAPI
 ├── make_model()                        # LLM client (SSL isolated)
 ├── build_manifest()                    # Seed files
 ├── SandboxAgent(capabilities=[...])    # Agent + custom capabilities
 ├── RunConfig(client=E2BSandboxClient)  # Wire to CubeSandbox
 └── Runner.run(agent, question)
      ├── CubeSandbox creates a sandbox instance
      ├── envd writes Manifest files
      ├── LLM emits tool calls
      │    ├── WorkspaceShell → session.exec("sh -lc ...")
      │    └── PythonRunner   → inner.run_code(code)
      │         ├── Jupyter readiness probe
      │         ├── Kernel CWD alignment
      │         ├── Execute Python code
      │         └── Auto-capture figures → output/figure_*.png
      └── Return the final analysis result
```

## 6. End-to-End Sample Run

### 6.1 Code Interpreter: data analysis + chart generation

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

End-to-end flow: Manifest seeds `sales.csv` → Jupyter kernel readiness probe → 3 rounds of `run_code` (load data → plot → summary table) → model emits the final analysis. The chart is captured automatically from kernel results and saved to `output/`.

## 7. Template Requirements

What your cube template must ship / expose depends on which mode you run:

| Mode | Preinstalled software | Exposed ports | Probe |
|------|-----------------------|---------------|-------|
| Generic E2B | python3 + business deps (pandas / numpy / matplotlib ...) | 49983 (envd) | 49983 |
| Code Interpreter | All of the above + the official `e2b-code-interpreter` runtime | 49983 + **49999** | 49983 |

Template registration example (taken from `openai-agents-code-interpreter/README.md`):

```bash
cubemastercli template create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/e2b-code-interpreter:v1.1-data \
  --writable-layer-size 1Gi \
  --template-id e2b-code-interpreter \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49983
```

Key environment variables (see each demo's `.env.example`):

| Variable | Purpose |
|----------|---------|
| `E2B_API_URL` | CubeAPI URL, e.g. `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI auth key |
| `CUBE_TEMPLATE_ID` | cube template ID |
| `CUBE_SSL_CERT_FILE` | (Optional) path to the cube CA bundle |
| `TOKENHUB_API_KEY` | LLM key (auto-mapped to `OPENAI_API_KEY`) |
| `OPENAI_BASE_URL` | Defaults to TokenHub, `https://tokenhub.tencentmaas.com/v1` |


## 8. Further Reading

- [OpenAI Sandbox Agents official docs](https://developers.openai.com/api/docs/guides/agents/sandboxes)
- [E2B Sandbox docs](https://e2b.dev/docs) / [E2B × OpenAI Agents SDK](https://e2b.dev/docs/agents/openai-agents-sdk)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
