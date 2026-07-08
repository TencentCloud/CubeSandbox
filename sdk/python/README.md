<p align="center">
  <strong>cubesandbox</strong> — Python SDK for CubeSandbox
</p>

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox"><img src="https://img.shields.io/badge/CubeSandbox-GitHub-blue" alt="CubeSandbox" /></a>
  <a href="../../LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0" /></a>
  <img src="https://img.shields.io/badge/Python-3.9%2B-blue" alt="Python 3.9+" />
  <img src="https://img.shields.io/badge/version-0.4.0-orange" alt="v0.4.0" />
</p>

---

`cubesandbox` is the official Python SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox).
It provides a simple, Pythonic interface to create sandboxes, execute code,
and control the full sandbox lifecycle — including pause/resume with memory snapshot.

## Installation

```bash
pip install cubesandbox
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

### Run shell commands

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    result = sb.commands.run("echo hello cube")
    print(result.stdout)  # "hello cube\n"
```

When `user` is omitted, the SDK sends requests as `root` for compatibility
with envd versions that reject process/file requests without an explicit user.

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

Two layers can be combined inside `network=`:

- **L3/L4** — `allow_out` / `deny_out` lists of CIDRs or hostnames.
- **L7** — `rules` for host / path / SNI matching, audit, and credential
  injection. Use the typed `Rule` / `Match` / `Action` / `Inject` dataclasses.

```python
from cubesandbox import Sandbox, Rule, Match, Action, Inject

rules = [
    Rule(
        name="deepseek_api",
        match=Match(
            scheme="https",
            host="api.deepseek.com",
            method=["POST"],
            path="/v1/chat",
            sni="api.deepseek.com",
        ),
        action=Action(
            allow=True,
            audit="metadata",
            inject=[Inject(
                header="Authorization",
                format="Bearer ${SECRET}",
                secret="sk_xxxxxxxx",
            )],
        ),
    ),
]

with Sandbox.create(
    network={"allow_out": ["172.67.0.0/16"], "rules": rules},
) as sb:
    sb.run_code("import requests; requests.post('https://api.deepseek.com/v1/chat')")
```

Rules are evaluated **first-match-wins** in list order. Credential injection
only runs on HTTPS requests where SNI and Host match (server-enforced).

#### E2B per-host request transforms (compat shape)

For drop-in compatibility with E2B's
[per-host request transforms](https://e2b.dev/docs/network/internet-access#per-host-request-transforms),
`network["rules"]` also accepts a host-keyed mapping. Each `transform.headers`
entry is converted into a CubeEgress L7 rule whose `action.inject` injects
the same headers on outbound HTTPS requests to that host:

```python
from cubesandbox import Sandbox

with Sandbox.create(
    network={
        # The host must still be referenced via allow_out — registering a
        # rule alone does not grant egress.
        "allow_out": ["api.example.com"],
        "deny_out": ["0.0.0.0/0"],
        "rules": {
            "api.example.com": [
                {"transform": {"headers": {"X-Header": "Content"}}},
            ],
        },
    },
) as sb:
    sb.run_code("import requests; requests.get('https://api.example.com/')")
```

The compat shape is interchangeable with the typed-Rule shape: pick whichever
fits the codebase. Mixing the two on a single `Sandbox.create` call is not
supported — pass either a list of `Rule` (typed) **or** a host-keyed dict
(E2B-shaped).

The legacy `metadata={"network-policy": ...}` interface is still accepted
for IP-only deny-all / custom allow-list scenarios.

### Filesystem

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    # Read & write
    sb.files.write("/tmp/hello.txt", "Hello, world!")
    print(sb.files.read("/tmp/hello.txt"))  # "Hello, world!"

    # Batch write
    sb.files.write_files([
        ("/tmp/a.txt", "aaa"),
        ("/tmp/b.txt", b"bbb"),  # bytes also accepted
    ])

    # Directory operations
    sb.files.make_dir("/tmp/mydir")
    entries = sb.files.list("/tmp")          # list of dicts
    info = sb.files.stat("/tmp/hello.txt")   # dict with name, type, size, ...
    print(sb.files.exists("/tmp/hello.txt")) # True
    sb.files.rename("/tmp/hello.txt", "/tmp/renamed.txt")
    sb.files.remove("/tmp/renamed.txt")

    # Watch for changes
    with sb.files.watch_dir("/tmp") as watcher:
        for event in watcher:
            print(event.name, event.type)  # e.g. "a.txt" "EVENT_TYPE_CREATE"
```

### Host-directory mount

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([{"hostPath": "/data/shared", "mountPath": "/mnt/data"}])
with Sandbox.create(metadata={"host-mount": mounts}) as sb:
    result = sb.run_code("open('/mnt/data/hello.txt').read()")
    print(result.text)
```

### Persistent volumes

Volumes are e2b-compatible persistent stores backed by a volume plugin
(COS, NFS, …). Manage their lifecycle with the `Volume` helper, then mount
them into a sandbox via `Sandbox.create(volume_mounts=[...])`. Data survives
across sandbox restarts and can be shared between sandboxes.

```python
from cubesandbox import Sandbox, Volume, VolumeMount

# Create a volume — name is optional (server generates a UUID when omitted).
# Omitting driver is e2b-compatible: NO driver is sent, so the backend uses its
# first configured plugin. Pass a non-empty driver to pin a specific plugin.
vol = Volume.create("my-data")                   # default plugin
# vol = Volume.create("my-data", driver="cos")   # pin a plugin
print(vol.volume_id, vol.token)

# Mount it into a sandbox at a path
with Sandbox.create(
    volume_mounts=[VolumeMount(name=vol.volume_id, path="/workspace")],
) as sb:
    sb.files.write("/workspace/note.txt", "persisted!")
    print(sb.files.read("/workspace/note.txt"))

# A plain dict works too: {"name": <volumeID>, "path": <mount-path>}

# List / get / delete
for v in Volume.list():
    print(v.volume_id, v.name)
Volume.get(vol.volume_id)
Volume.delete(vol.volume_id)   # kill any mounting sandbox first — delete does not auto-detach
```

Volume `name` must match `^[a-zA-Z0-9_-]+$` and be at most 128 characters;
invalid names raise `ValueError` before any network call. See
[`docs/volume.md`](docs/volume.md) for the full API, parameters and error codes.

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
| `Sandbox.create(template, *, timeout, env_vars, metadata, volume_mounts, config)` | `POST /sandboxes` — create a new sandbox (optionally mounting volumes) |
| `Sandbox.connect(sandbox_id, *, config)` | `POST /sandboxes/:id/connect` — connect (auto-resumes if paused) |
| `Sandbox.list(config)` | `GET /sandboxes` — list running sandboxes (v1) |
| `Sandbox.list_v2(config)` | `GET /v2/sandboxes` — list sandboxes (v2) |
| `Sandbox.health(config)` | `GET /health` — service health check |

### `Sandbox` — instance methods

| Method | Description |
|---|---|
| `sb.run_code(code, *, on_stdout, on_stderr, on_result, on_error, envs, timeout)` | `POST /execute` — execute code, returns `Execution` |
| `sb.get_info()` | `GET /sandboxes/:id` — get sandbox state and metadata |
| `sb.pause(*, wait, timeout, interval)` | `POST /sandboxes/:id/pause` — pause sandbox |
| `sb.resume(timeout)` | `POST /sandboxes/:id/resume` — resume (deprecated, use `connect`) |
| `sb.set_timeout(timeout)` | `POST /sandboxes/:id/timeout` — set sandbox idle timeout |
| `sb.kill()` | `DELETE /sandboxes/:id` — destroy sandbox |
| `sb.get_host(port)` | Return virtual hostname `{port}-{id}.{domain}` |

### `sb.files` — Filesystem

| Method | Description |
|---|---|
| `sb.files.read(path)` | Download file content via `GET /files` |
| `sb.files.write(path, data)` | Upload via `POST /files` (octet-stream, multipart fallback) |
| `sb.files.write_files(files)` | Batch write `[(path, data), ...]`, stops on first error |
| `sb.files.list(path)` | List directory entries → `list[dict]` |
| `sb.files.stat(path)` | File/directory metadata → `dict` |
| `sb.files.exists(path)` | `True` if path exists (stat + 404 check) |
| `sb.files.make_dir(path)` | Create directory → `dict` |
| `sb.files.rename(old, new)` | Move/rename → `dict` |
| `sb.files.remove(path)` | Delete file or directory |
| `sb.files.watch_dir(path)` | Stream filesystem events → `Watcher` (context manager + iterator) |

### `Volume` — persistent volumes (class methods)

| Method | Description |
|---|---|
| `Volume.create(name=None, *, driver=None, config=None)` | `POST /volumes` — create a volume; omit `driver` for e2b-compatible behavior (backend's first plugin), or pass a non-empty `driver` (e.g. `"cos"`) to pin a plugin → `VolumeInfo` |
| `Volume.list(config)` | `GET /volumes` — list volumes → `list[VolumeInfo]` (no token) |
| `Volume.get(volume_id, *, config)` | `GET /volumes/:id` — get one volume (with token) |
| `Volume.delete(volume_id, *, config)` | `DELETE /volumes/:id` — delete a volume |

Mount a volume into a sandbox with `Sandbox.create(volume_mounts=[VolumeMount(name, path)])`.
`VolumeInfo` exposes `.volume_id`, `.name`, `.token`. Full reference:
[`docs/volume.md`](docs/volume.md).

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
