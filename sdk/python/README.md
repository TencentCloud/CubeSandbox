# cubesandbox

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox">
    <img alt="CubeSandbox" src="https://img.shields.io/badge/CubeSandbox-Python_SDK-blue">
  </a>
  <a href="https://opensource.org/licenses/Apache-2.0">
    <img alt="License" src="https://img.shields.io/badge/License-Apache_2.0-green">
  </a>
  <a href="https://www.python.org/downloads/">
    <img alt="Python" src="https://img.shields.io/badge/Python-3.9+-blue">
  </a>
</p>

Python SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) — an instant, concurrent, secure and lightweight sandbox service for AI agents, built on RustVMM and KVM.

> Create a hardware-isolated sandbox in under **60ms**, with less than **5MB** memory overhead.

---

## What is CubeSandbox?

CubeSandbox is a high-performance, out-of-the-box secure sandbox service. It supports single-node deployment and can scale to a multi-node cluster. Use this SDK to create sandboxes, execute code, stream output, manage file mounts, and control network policies — all from Python.

---

## Installation

```bash
pip install cubesandbox
# or from source
pip install git+https://github.com/TencentCloud/CubeSandbox.git#subdirectory=sdk/python
```

**Requirements:** Python 3.9+, `httpx>=0.27`, `requests>=2.28`

---

## Quick Start

### 1. Set up environment

```bash
export CUBE_API_URL=http://<YOUR_NODE_IP>:3000
export CUBE_TEMPLATE_ID=<YOUR_TEMPLATE_ID>
export CUBE_PROXY_NODE_IP=<YOUR_NODE_IP>
```

### 2. Run your first sandbox

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    sb.run_code("x = 1")
    result = sb.run_code("x + 1")
    print(result.text)   # "2"
```

---

## Features

### Execute code

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    # Basic execution
    result = sb.run_code("sum(range(101))")
    print(result.text)          # "5050"
    print(result.logs.stdout)   # []
    print(result.error)         # None

    # Stream stdout in real time
    sb.run_code(
        "for i in range(5): print(f'step {i}')",
        on_stdout=lambda msg: print(msg.text, end=""),
    )

    # Capture errors
    result = sb.run_code("1 / 0")
    print(result.error.name)    # "ZeroDivisionError"
    print(result.error.value)   # "division by zero"
```

### Share state with contexts

```python
with Sandbox.create() as sb:
    ctx = sb.create_context()

    sb.run_code("x = 100",       context=ctx)
    sb.run_code("y = x * 2",     context=ctx)
    result = sb.run_code("x + y", context=ctx)
    print(result.text)   # "300"

    sb.delete_context(ctx)
```

### Lifecycle management

```python
from cubesandbox import Sandbox

# Create
sb = Sandbox.create(timeout=600)
print(sb.sandbox_id)

# Pause (persists memory snapshot)
sb.pause()

# Resume later — auto-resumes if paused
sb2 = Sandbox.connect(sb.sandbox_id)
result = sb2.run_code("1 + 1")
print(result.text)   # "2"

# Destroy
sb2.kill()
```

### Host-directory mount

```python
import json

mounts = json.dumps([
    {"hostPath": "/data/shared", "mountPath": "/mnt/data", "readOnly": False}
])

with Sandbox.create(metadata={"hostdir-mount": mounts}) as sb:
    result = sb.run_code("open('/mnt/data/hello.txt').read()")
    print(result.text)
```

### Network policy

```python
import json

# Deny all outbound traffic
with Sandbox.create(metadata={"network-policy": "deny-all"}) as sb:
    result = sb.run_code(
        "import urllib.request; urllib.request.urlopen('http://example.com')"
    )
    print(result.error.name)   # "URLError"

# Custom allow-list
rules = json.dumps({"allow": ["pypi.org", "files.pythonhosted.org"]})
with Sandbox.create(
    metadata={"network-policy": "custom", "network-rules": rules}
) as sb:
    sb.run_code("import subprocess; subprocess.run(['pip', 'install', 'requests'])")
```

### List & health check

```python
from cubesandbox import Sandbox

# Health check
health = Sandbox.health()
print(health)   # {"status": "ok", "sandboxes": 2}

# List running sandboxes
sandboxes = Sandbox.list()
for s in sandboxes:
    print(s["sandboxID"], s["state"])
```

---

## Configuration

| Environment Variable   | Default                 | Description                                       |
|------------------------|-------------------------|---------------------------------------------------|
| `CUBE_API_URL`         | `http://127.0.0.1:3000` | CubeAPI management plane address                  |
| `CUBE_TEMPLATE_ID`     | —                       | Default template ID (required)                    |
| `CUBE_PROXY_NODE_IP`   | —                       | CubeProxy node IP — bypasses DNS for `*.cube.app` |
| `CUBE_PROXY_PORT_HTTP` | `80`                    | CubeProxy HTTP port                               |
| `CUBE_SANDBOX_DOMAIN`  | `cube.app`              | Sandbox domain suffix                             |

Or pass a `Config` object directly:

```python
from cubesandbox import Config, Sandbox

config = Config(
    api_url="http://<YOUR_NODE_IP>:3000",
    template_id="<YOUR_TEMPLATE_ID>",
    proxy_node_ip="<YOUR_NODE_IP>",
    timeout=300,
)

with Sandbox.create(config=config) as sb:
    result = sb.run_code("2 ** 10")
    print(result.text)   # "1024"
```

---

## API Reference

| Method | Description |
|--------|-------------|
| `Sandbox.create(template, *, timeout, env_vars, metadata, config)` | Create a new sandbox |
| `Sandbox.connect(sandbox_id, *, config)` | Connect to existing sandbox (auto-resume) |
| `Sandbox.list(config)` | List all running sandboxes (v1) |
| `Sandbox.list_v2(config)` | List all running sandboxes (v2) |
| `Sandbox.health(config)` | Check CubeAPI health |
| `sb.run_code(code, *, context, on_stdout, on_stderr, envs, timeout)` | Execute code, return `Execution` |
| `sb.create_context()` | Create kernel context for state sharing |
| `sb.delete_context(ctx)` | Delete kernel context |
| `sb.pause()` | Pause sandbox (snapshot memory) |
| `sb.resume()` | Resume sandbox *(deprecated, use `connect`)* |
| `sb.kill()` | Destroy sandbox |
| `sb.get_info()` | Get sandbox details |

### `Execution` object

```python
result = sb.run_code("1 + 1")

result.text           # "2"  — last expression value
result.logs.stdout    # ["hello\n"]  — list of stdout lines
result.logs.stderr    # []
result.error          # None or ExecutionError(name, value, traceback)
```

---

## Examples

See [`sdk/python/examples/`](sdk/python/examples/) for runnable scripts:

| Script | Description |
|--------|-------------|
| `create_and_run.py` | Create sandbox, run code, streaming, error handling, env vars |
| `lifecycle.py` | pause / connect (auto-resume) / kill |
| `context.py` | Kernel contexts, cross-call variable persistence |
| `volume.py` | Host-directory mount (read-write / read-only) |
| `network_policy.py` | deny-all / allow-all / custom allow-list |
| `list_and_health.py` | Health check, list v1/v2, verify after kill |
| `run_all.py` | Run all examples, output to `output.md` |

---

## License

Apache-2.0, Copyright (c) 2026 Tencent Inc.
