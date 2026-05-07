# cubesandbox

Python SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) — a self-hosted, KVM-based secure sandbox service for AI agents.

Compatible with the `e2b_code_interpreter` interface.

## Installation

```bash
pip install cubesandbox
# or from source
pip install -e .
```

**Dependencies:** `httpx>=0.27`, `requests>=2.28`

---

## Quick Start

### Create and run code

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    sb.run_code("x = 1")
    result = sb.run_code("x + 1")
    print(result.text)          # "2"
    print(result.logs.stdout)   # []
    print(result.error)         # None
```

### Stream output

```python
with Sandbox.create() as sb:
    sb.run_code(
        "for i in range(5): print(i)",
        on_stdout=lambda msg: print(">>", msg.text, end=""),
    )
```

### Connect to an existing sandbox

```python
# Create once, reuse later
sb = Sandbox.create(timeout=3600)
sandbox_id = sb.sandbox_id

# In another process / later
sb = Sandbox.connect(sandbox_id)
result = sb.run_code("1 + 1")
sb.kill()
```

---

## Configuration

All options can be set via environment variables or passed as a `Config` object.

| Environment Variable   | Default                 | Description                                    |
|------------------------|-------------------------|------------------------------------------------|
| `CUBE_API_URL`         | `http://127.0.0.1:3000` | CubeAPI management plane address               |
| `CUBE_TEMPLATE_ID`     | —                       | Default template ID (required)                 |
| `CUBE_PROXY_NODE_IP`   | —                       | CubeProxy node IP — bypasses DNS for `*.cube.app` |
| `CUBE_PROXY_PORT_HTTP` | `80`                    | CubeProxy HTTP port                            |
| `CUBE_SANDBOX_DOMAIN`  | `cube.app`              | Sandbox domain suffix                          |

### Remote client setup

When accessing CubeSandbox from a machine that cannot resolve `*.cube.app`,
set `CUBE_PROXY_NODE_IP` to the node IP. The SDK routes all data-plane
connections directly to that IP while preserving the `Host` header so
CubeProxy can identify the target sandbox.

```bash
export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
export CUBE_TEMPLATE_ID=<YOUR_TEMPLATE_ID>
export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
```

---

## API Reference

### `Sandbox.create(template=None, *, timeout, env_vars, metadata, config)`

Create a new sandbox. Returns a `Sandbox` instance.

```python
sb = Sandbox.create(template="<template-id>", timeout=300)
```

### `Sandbox.connect(sandbox_id, *, config)`

Connect to an existing sandbox. Auto-resumes if paused.

### `Sandbox.list(config=None)` / `Sandbox.list_v2(config=None)`

List all running sandboxes (v1 / v2 API).

### `Sandbox.health(config=None)`

Check CubeAPI health. Returns `{"status": "ok", "sandboxes": N}`.

### `sb.run_code(code, *, context, on_stdout, on_stderr, on_result, on_error, envs, timeout)`

Execute Python code. Returns an `Execution` object.

```python
result = sb.run_code("1 + 1")
result.text           # "2"  — last expression value
result.logs.stdout    # list of stdout lines
result.logs.stderr    # list of stderr lines
result.error          # ExecutionError or None
```

### `sb.create_context()` / `sb.delete_context(ctx)`

Create or delete a kernel context for sharing state across `run_code` calls.

```python
ctx = sb.create_context()
sb.run_code("x = 42", context=ctx)
result = sb.run_code("x * 2", context=ctx)  # "84"
sb.delete_context(ctx)
```

### `sb.pause()` / `sb.resume()` / `sb.kill()`

Lifecycle management. `resume()` is deprecated — use `Sandbox.connect()` instead.

### `sb.get_info()`

Return current sandbox details (state, resources, metadata).

---

## Examples

See [`examples/`](examples/) for runnable scripts:

| Script | Description |
|--------|-------------|
| `create_and_run.py` | Create sandbox, run code, streaming, error handling |
| `lifecycle.py` | pause / connect (auto-resume) / kill |
| `context.py` | Kernel contexts, variable persistence |
| `volume.py` | Host-directory mount (rw / ro) |
| `network_policy.py` | deny-all / allow-all / custom network policies |
| `list_and_health.py` | health check, list v1/v2 |
| `run_all.py` | Run all examples, output to `output.md` |

---

## License

Apache-2.0, Copyright (c) 2026 Tencent Inc.
