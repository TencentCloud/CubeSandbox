# cube-e2b

Python SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) — a self-hosted, KVM-based code execution environment.

## Installation

```bash
pip install cube-e2b
# or from source
pip install -e .
```

**Dependencies:** `httpx>=0.27`, `requests>=2.28`

---

## Quick Start

### Create and run code

```python
from cube_e2b import Sandbox

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
    result = sb.run_code(
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

| Environment Variable    | Default                   | Description                                      |
|-------------------------|---------------------------|--------------------------------------------------|
| `CUBE_API_URL`          | `http://127.0.0.1:3000`   | cube-api address                                 |
| `CUBE_TEMPLATE_ID`      | —                         | Default template ID                              |
| `CUBE_PROXY_NODE_IP`    | —                         | CubeProxy IP — bypasses DNS for remote clients   |
| `CUBE_PROXY_PORT_HTTP`  | `80`                      | CubeProxy HTTP port                              |
| `CUBE_SANDBOX_DOMAIN`   | `cube.app`                | Sandbox domain suffix                            |

### Remote client setup

When accessing CubeSandbox from a machine that cannot resolve `*.cube.app`:

```bash
export CUBE_API_URL=http://9.135.79.34:3000
export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
export CUBE_PROXY_NODE_IP=9.135.79.34
```

The SDK will connect directly to `CUBE_PROXY_NODE_IP:80` and set the `Host` header
to `49999-<sandboxID>.cube.app` so CubeProxy can route the request correctly.

---

## API Reference

### `Sandbox.create(template, *, timeout, env_vars, metadata, config)`

Create a new sandbox. Returns a `Sandbox` instance.

### `Sandbox.connect(sandbox_id, *, config)`

Connect to an existing sandbox. Auto-resumes if paused.

### `sandbox.run_code(code, *, on_stdout, on_stderr, on_result, on_error, envs, timeout)`

Execute code. Returns an `Execution` object.

```python
result = sb.run_code("1 + 1")
result.text           # "2" — last expression value
result.logs.stdout    # list of stdout lines
result.logs.stderr    # list of stderr lines
result.error          # ExecutionError or None
```

### `sandbox.pause()` / `sandbox.resume()` / `sandbox.kill()`

Lifecycle management.

### `sandbox.get_info()`

Return current sandbox details from the API.

---

## Generate client from OpenAPI

```bash
# Install generator
pip install openapi-generator-cli
# or
npm install -g @openapitools/openapi-generator-cli

# Generate Python client
openapi-generator-cli generate \
  -i openapi.yaml \
  -g python \
  -o ./cube-api-client \
  --additional-properties=packageName=cube_api_client
```

> Note: The generated client covers the REST lifecycle API only.
> The streaming `/execute` endpoint is handled by this SDK directly.

---

## License

Apache-2.0
