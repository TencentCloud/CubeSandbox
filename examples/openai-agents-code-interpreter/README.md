# OpenAI Agents SDK + CubeSandbox: Code Interpreter Example

[中文文档](README_zh.md)

Run a data-analysis Agent inside [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) (which exposes an E2B-compatible API), letting the LLM perform real computation and plotting with pandas / matplotlib inside a cloud Python sandbox.

## The Two Demos

This directory ships two runnable implementations. They are functionally equivalent; the only difference is the **Python execution backend**:

| | [`code_interpreter_demo.py`](code_interpreter_demo.py) | [`code_interpreter_demo_ci.py`](code_interpreter_demo_ci.py) |
|---|---|---|
| Sandbox type | `E2BSandboxType.E2B` | `E2BSandboxType.CODE_INTERPRETER` |
| Underlying SDK | `e2b.AsyncSandbox` | `e2b_code_interpreter.AsyncSandbox` |
| Execution model | `session.write(script.py)` → `session.exec("python script.py")` | `sandbox.run_code(code)` (Jupyter kernel) |
| Session state | Each exec is a fresh process | **Variables / imports / DataFrames persist across cells** |
| Image capture | Agent calls `plt.savefig(...)` manually | Automatically decoded from `Execution.results` (base64) |
| Error reporting | Full stderr stream | Structured `Execution.error` (name / value / traceback) |
| Template ports | envd (49983) | envd (49983) + code-interpreter gateway (49999) |

**Which one to pick?**
- Template with plain Python only → use `code_interpreter_demo.py`
- Template with the e2b code-interpreter service → use `code_interpreter_demo_ci.py` (Jupyter kernel experience, state preserved across turns)

## Prerequisites

- Python 3.10+
- CubeSandbox platform deployed and CubeAPI reachable
- A sandbox template with the data-science stack (python3 + pandas + numpy + matplotlib)
- An API key for TokenHub or any OpenAI-compatible LLM

## Quick Start

### 1. Install dependencies

```bash
pip install -r requirements.txt
```

### 2. Configure environment variables

```bash
cp .env.example .env
```

| Variable | Description |
|----------|-------------|
| `TOKENHUB_API_KEY` | TokenHub API key (automatically mapped to `OPENAI_API_KEY`) |
| `OPENAI_BASE_URL` | LLM endpoint (defaults to `https://tokenhub.tencentmaas.com/v1`) |
| `E2B_API_URL` | CubeAPI URL, e.g. `http://<cube-host>:3000` |
| `E2B_API_KEY` | CubeAPI auth key |
| `CUBE_TEMPLATE_ID` | Sandbox template ID |
| `CUBE_SSL_CERT_FILE` | (Optional) path to the CubeSandbox CA bundle |

### 3. Create a sandbox template

```bash
cubemastercli template create-from-image \
  --image cube-sandbox-image.tencentcloudcr.com/demo/e2b-code-interpreter:v1.1-data \
  --writable-layer-size 1Gi \
  --template-id e2b-code-interpreter \
  --expose-port 49983 \
  --expose-port 49999 \
  --probe 49983
```

Copy the resulting template ID into `CUBE_TEMPLATE_ID` in your `.env`.

> Note: `code_interpreter_demo.py` only requires port 49983; `code_interpreter_demo_ci.py` additionally needs 49999 (the Jupyter kernel gateway).

### 4. Run

```bash
# Generic E2B variant (write + exec)
python code_interpreter_demo.py

# Jupyter kernel variant (run_code, state persists across turns)
python code_interpreter_demo_ci.py

# Custom prompt
python code_interpreter_demo.py --question "Plot units sold per product as a bar chart."

# Swap model
python code_interpreter_demo_ci.py --model openai/deepseek-v3.2

# Pause instead of destroy on exit (supports later resume)
python code_interpreter_demo.py --pause-on-exit
```

### Command-line arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--model` | `openai/glm-5.1` | LLM model name (TokenHub uses the `openai/` prefix) |
| `--question` | Sales-data analysis (see below) | Prompt sent to the Agent |
| `--template` | `CUBE_TEMPLATE_ID` env var | Sandbox template ID |
| `--timeout` | `600` | Sandbox lifetime (seconds) |
| `--pause-on-exit` | `False` | Pause the sandbox on exit instead of destroying it |

## Sample Run

```bash
python code_interpreter_demo_ci.py
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

## Architecture

```
SandboxAgent
├── WorkspaceShell            # session.exec("sh -lc ...") → ls/cat/pwd
└── PythonRunner              # Custom Capability (FunctionTool)
    ├── code_interpreter_demo.py:
    │     session.write(script.py)
    │     session.exec("python -I -B script.py")
    │     → returns exit_code / stdout / stderr / output/*
    └── code_interpreter_demo_ci.py:
          sandbox.run_code(code)
          → Execution.results (base64 images auto-decoded)
          → Execution.logs (stdout/stderr)
          → Execution.error (structured exception)
```

### Manifest seed files

At startup the Agent uses `Manifest` to seed the workspace with:

| File | Contents |
|------|----------|
| `sales.csv` | 14 rows of sales data (date, product, units, unit_price) |
| `README.md` | Task description guiding the agent through the analysis |

The default prompt asks the agent to use `sales.csv` to: compute revenue → plot the monthly trend → produce a Top-3 products table → summarise.

## CubeSandbox Adaptations

| Patch | Reason | Location |
|-------|--------|----------|
| `default_username = "root"` + Filesystem method wrappers | envd only serves the root user | Top of both files |
| `Commands.run` injects `on_stdout/on_stderr` | Stream sandbox output live | Top of both files |
| LLM client uses the system CA bundle | Isolates `SSL_CERT_FILE` | `make_model()` |
| Force `OpenAIChatCompletionsModel` | TokenHub does not support the Responses API | `make_model()` |

Extra adaptations specific to `code_interpreter_demo_ci.py`:

| Adaptation | Description |
|------------|-------------|
| `_wait_for_jupyter_ready()` | Probes `127.0.0.1:49999/health` from inside the sandbox to confirm Jupyter is ready |
| `_find_code_interpreter_sandbox()` | BFS through SDK wrappers to reach the underlying `AsyncSandbox` |
| `_run_code_annotated()` | Rewrites cube 502 errors from "sandbox timeout" into "sandbox evicted" |
| Kernel CWD alignment | `os.chdir` to the envd workspace root so relative paths match the Manifest files |

## Related Documents

- [OpenAI Agents SDK × CubeSandbox Integration Guide](../openai-agents-example/openai-agents-sandbox-cube-integration.md)
- [OpenAI Sandbox Agents Concepts](../openai-agents-example/openai-agents.md)
- [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
