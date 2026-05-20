<p align="center">
  <strong>cubesandbox</strong> — Python SDK for CubeSandbox
</p>

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox"><img src="https://img.shields.io/badge/CubeSandbox-GitHub-blue" alt="CubeSandbox" /></a>
  <a href="../../LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0" /></a>
  <img src="https://img.shields.io/badge/Python-3.9%2B-blue" alt="Python 3.9+" />
  <img src="https://img.shields.io/badge/version-0.1.0-orange" alt="v0.1.0" />
</p>

---

`cubesandbox` is the official Python SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox).
It provides a simple, Pythonic interface to create sandboxes, execute code,
and control the full sandbox lifecycle — including pause/resume with memory snapshot.

## Installation

```bash
pip install cubesandbox
```

Or install from source:

```bash
cd sdk/python
pip install -e .
```

## Quick Start

Set the required environment variables:

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote access (bypasses DNS for *.cube.app)
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

Run your first sandbox:

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    result = sb.run_code("1 + 1")
    print(result.text)   # "2"
```

## Features

### Execute code

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    # Simple expression
    result = sb.run_code("x = 42\nx * 2")
    print(result.text)          # "84"

    # Capture stdout
    result = sb.run_code('print("hello")')
    print(result.logs.stdout)   # ["hello\n"]

    # Stream output in real time
    sb.run_code(
        'for i in range(3): print(i)',
        on_stdout=lambda msg: print("out:", msg.text),
    )
```

### Persistent variables within a sandbox

Variables assigned in one `run_code` call persist for the lifetime of the sandbox —
no separate context object needed:

```python
with Sandbox.create() as sb:
    sb.run_code("x = 100")
    result = sb.run_code("x + 1")
    print(result.text)   # "101"
```


### Pause & resume

```python
sb = Sandbox.create()

# Pause — preserves memory snapshot, polls until state=paused
sb.pause()                         # wait=True, timeout=30s by default
sb.pause(wait=False)               # fire-and-forget
sb.pause(timeout=60, interval=0.5) # custom poll params

# Resume by connecting — auto-resumes paused sandbox
sb2 = Sandbox.connect(sb.sandbox_id)
```

### Network policy

```python
import json
from cubesandbox import Sandbox

# Deny all outbound traffic
with Sandbox.create(metadata={"network-policy": "deny-all"}) as sb:
    result = sb.run_code(
        "import urllib.request; urllib.request.urlopen('http://example.com')"
    )
    print(result.error.name)   # "URLError"

# Custom allow-list — NOTE: only IP addresses are supported, not domain names
rules = json.dumps({"allow": ["151.101.0.0/16"]})
with Sandbox.create(
    metadata={"network-policy": "custom", "network-rules": rules}
) as sb:
    sb.run_code("import subprocess; subprocess.run(['pip', 'install', 'requests'])")
```

### Host-directory mount

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([{"hostPath": "/data/shared", "mountPath": "/mnt/data"}])
with Sandbox.create(metadata={"hostdir-mount": mounts}) as sb:
    result = sb.run_code("open('/mnt/data/hello.txt').read()")
    print(result.text)
```

### List & health check

```python
from cubesandbox import Sandbox

print(Sandbox.health())     # {"status": "ok", "sandboxes": 4}
print(Sandbox.list())       # list of running sandbox dicts
print(Sandbox.list_v2())    # v2 API (supports filtering)
```

## Configuration

| Environment Variable | Required | Default | Description |
|---|:---:|---|---|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | CubeAPI management plane address |
| `CUBE_TEMPLATE_ID` | ✅ | — | Template ID for sandbox creation |
| `CUBE_PROXY_NODE_IP` | remote | — | CubeProxy node IP, bypasses DNS for `*.cube.app` |
| `CUBE_PROXY_PORT_HTTP` | | `80` | CubeProxy HTTP port |
| `CUBE_SANDBOX_DOMAIN` | | `cube.app` | Sandbox domain suffix |

You can also pass a `Config` object directly:

```python
from cubesandbox import Config, Sandbox

cfg = Config(
    api_url="http://10.0.0.1:3000",
    template_id="tpl-xxxxxxxxxxxxxxxxxxxxxxxx",
    proxy_node_ip="10.0.0.1",
)
with Sandbox.create(config=cfg) as sb:
    print(sb.run_code("2 ** 10").text)   # "1024"
```

## API Reference

### `Sandbox` — class methods

| Method | Description |
|---|---|
| `Sandbox.create(template, *, timeout, env_vars, metadata, config)` | `POST /sandboxes` — create a new sandbox |
| `Sandbox.connect(sandbox_id, *, config)` | `POST /sandboxes/:id/connect` — connect (auto-resumes if paused) |
| `Sandbox.list(config)` | `GET /sandboxes` — list running sandboxes (v1) |
| `Sandbox.list_v2(config)` | `GET /v2/sandboxes` — list sandboxes (v2) |
| `Sandbox.health(config)` | `GET /health` — service health check |

### `Sandbox` — instance methods

| Method | Description |
|---|---|
| `sb.run_code(code, *, on_stdout, on_stderr, on_result, on_error, envs, timeout)` | `POST /execute` — execute code, returns `Execution` |
| `sb.get_info()` | `GET /sandboxes/:id` — get sandbox state and metadata |
| `sb.get_info()` | `GET /sandboxes/:id` — get sandbox detail |
| `sb.pause(*, wait, timeout, interval)` | `POST /sandboxes/:id/pause` — pause sandbox |
| `sb.resume(timeout)` | `POST /sandboxes/:id/resume` — resume (deprecated, use `connect`) |
| `sb.kill()` | `DELETE /sandboxes/:id` — destroy sandbox |
| `sb.get_host(port)` | Return virtual hostname `{port}-{id}.{domain}` |

### `Execution` object

| Attribute | Type | Description |
|---|---|---|
| `.text` | `str \| None` | Final expression value (main result) |
| `.logs.stdout` | `list[str]` | All stdout lines |
| `.logs.stderr` | `list[str]` | All stderr lines |
| `.error` | `ExecutionError \| None` | Exception info if execution failed |
| `.results` | `list[Result]` | All result events |

## Examples

| Script | Description |
|---|---|
| `examples/create_and_run.py` | Create sandbox and run code |
| `examples/context.py` | Kernel context (server-side not yet implemented) |
| `examples/lifecycle.py` | Pause / connect / kill |
| `examples/list_and_health.py` | List sandboxes and health check |
| `examples/network_policy.py` | Network policy (deny-all / custom) |
| `examples/volume.py` | Host-directory mount |
| `examples/run_all.py` | Run all examples |

## DNS Bypass (Remote Access)

When running outside the CubeSandbox node, `*.cube.app` cannot be resolved by the OS DNS.
Set `CUBE_PROXY_NODE_IP` to enable `IPOverrideTransport`: all data-plane connections are
routed directly to that IP with the virtual `Host` header preserved for CubeProxy routing.

```
Without CUBE_PROXY_NODE_IP:
  SDK → OS DNS (*.cube.app) → CubeProxy

With CUBE_PROXY_NODE_IP:
  SDK → TCP direct to CUBE_PROXY_NODE_IP:80
        Host: 49999-{sandboxID}.cube.app (preserved for routing)
```

## License

Apache-2.0 © 2026 Tencent Inc.
