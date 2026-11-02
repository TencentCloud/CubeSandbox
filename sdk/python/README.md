<p align="center">
  <strong>cubesandbox</strong> — Python SDK for CubeSandbox
</p>

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox"><img src="https://img.shields.io/badge/CubeSandbox-GitHub-blue" alt="CubeSandbox" /></a>
  <a href="../../LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0" /></a>
  <img src="https://img.shields.io/badge/Python-3.9%2B-blue" alt="Python 3.9+" />
  <img src="https://img.shields.io/badge/version-0.7.3-orange" alt="v0.7.3" />
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

### MCP gateway (preview)

Pass the E2B-compatible `mcp` option to start an MCP gateway inside the sandbox. It needs a template that ships `mcp-gateway`; when `template` is omitted, the alias `mcp-gateway` is tried first (override with `CUBE_MCP_TEMPLATE_ID`), then `CUBE_TEMPLATE_ID` if that template does not exist. `TemplateNotFoundError` is raised when neither exists. See [MCP Gateway](../../docs/guide/mcp-gateway.md).

```python
sb = Sandbox.create(mcp={"duckduckgo": {}, "arxiv": {"storagePath": "/tmp/papers"}})
url = sb.get_mcp_url()      # streamable HTTP endpoint
token = sb.get_mcp_token()  # send as "Authorization: Bearer <token>"
```

### Fork

```python
sb = Sandbox.create()

# One server-side call: the backend snapshots sb once and derives N copies.
forks = sb.fork(count=3)              # list[Sandbox | Exception], one per fork
forks = sb.fork(count=3, timeout=60)  # timeout: per-fork idle TTL in seconds

for f in forks:
    if isinstance(f, Sandbox):
        f.run_code("print('alive')")  # a running copy
    else:
        print(f)                      # that fork's exception
```

Partial success keeps the copies and does not raise; only whole-request
failures raise (e.g. unknown sandbox). `count` is 1..100 (default 1). Unlike
`clone`, the temporary snapshot is managed by the server.

### Compute node placement

Restrict scheduling to one or more compute node IDs or host IPs. A single
entry pins the sandbox to that node; creation fails if the node cannot run the
requested template.

```python
sb = Sandbox.create(
    distribution_scope=["node-a"],
)
```

### Network policy

Two layers can be combined inside `network=`:

- **L3/L4** — `allow_out` / `deny_out` lists of CIDRs or hostnames.
- **L7** — `rules` for host / path / SNI matching, audit, and credential
  injection. Use the typed `Rule` / `Match` / `Action` / `Inject` dataclasses.
- **Inbound Host** — `mask_request_host` customizes the Host CubeProxy forwards
  to user services; `${PORT}` expands to the requested sandbox port.

```python
sb = Sandbox.create(
    network={"mask_request_host": "localhost:${PORT}"},
)
```

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
    png = sb.files.read("/tmp/chart.png", format="bytes")  # bytearray

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
them into a sandbox via `Sandbox.create(volume_mounts={...})` (e2b mapping). Data survives
across sandbox restarts and can be shared between sandboxes.

```python
from cubesandbox import Sandbox, Volume, VolumeMount

# Create a volume — name is required by the E2B-compatible Python signature.
# Omitting driver is e2b-compatible: NO driver is sent, so the backend uses its
# first configured plugin. Pass a non-empty driver to pin a specific plugin.
vol = Volume.create("my-data")                   # default plugin
# vol = Volume.create("my-data", driver="cos")   # pin a plugin
print(vol.volume_id, vol.token)

# Mount it into a sandbox at a path
with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/note.txt", "persisted!")
    print(sb.files.read("/workspace/note.txt"))

# The value can be a Volume, a VolumeInfo, or a bare volume_id string.
# Wrap a value to make only this sandbox attachment read-only. Plain values remain read-write and e2b-compatible.
with Sandbox.create(
    volume_mounts={"/dataset": VolumeMount(vol, read_only=True)},
) as sb:
    print(sb.files.read("/dataset/note.txt"))

# List / get_info / connect / destroy
for v in Volume.list():                 # list[VolumeInfo], ID and name only
    print(v.volume_id, v.name)
Volume.get_info(vol.volume_id)          # -> VolumeAndToken (token and optional domain)
vol = Volume.connect(vol.volume_id)     # -> live Volume instance
Volume.destroy(vol.volume_id)           # -> bool; kill any mounting sandbox first (no auto-detach)
```

The access mode is selected per sandbox attachment. The same Volume can be read-write in one sandbox and read-only in another; existing e2b-shaped calls remain unchanged.

Volume `name` must match `^[a-zA-Z0-9_-]+$` and be at most 128 characters;
invalid names raise `ValueError` before any network call. See
[`docs/volume.md`](docs/volume.md) for the full API, parameters and error codes.

### Direct volume access without a sandbox

The synchronous `Volume` content methods access an E2B-shaped volume-content
gateway directly. They never create/connect a helper sandbox and do not require
a template or `CUBE_PROXY_NODE_IP`. Existing `/volumes` management routes alone
are insufficient: deploy a content gateway and obtain a valid volume-scoped token.
The repository's default S3 plugin does not issue that capability.

```python
import os
from contextlib import closing
from cubesandbox import Volume

vol = Volume(os.environ["CUBE_VOLUME_ID"], "dataset", os.environ["CUBE_VOLUME_CONTENT_TOKEN"])
opts = {"api_url": os.environ["CUBE_VOLUME_API_URL"]}
path = os.environ["CUBE_VOLUME_TEST_PATH"]
data = vol.read_file(path, format="bytes", **opts)
info = vol.get_info(path, **opts)
with closing(vol.read_file(path, format="stream", **opts)) as chunks:
    for chunk in chunks:
        pass
```

`vol.list(path)` is a content operation; `Volume.list()` is management.
Likewise, `vol.get_info(path)` returns `VolumeEntryStat`, while
`Volume.get_info(volume_id)` returns `VolumeAndToken`. Headers/token/address can
be supplied per call; file methods retain a synchronous iterator for streams.
HTTPX timeouts are transport phase/idle limits, not a guaranteed wall-clock deadline.
See [the fixed E2B contract](../../docs/e2b-volume-contract.md) and
[the direct-access scope](../../docs/volume-direct-access.md).

### Direct-volume E2E tests

From `sdk/python`, install the current source rather than a released SDK:

```sh
python3 -m pip install -e '.[dev]'
python3 -m pytest -q tests/test_volume.py tests/test_volume_content.py
python3 -m pytest -q tests/test_volume_direct_e2e.py --run-e2e
```

The live test requires `CUBE_VOLUME_API_URL` (or `E2B_VOLUME_API_URL`),
`CUBE_VOLUME_ID`, `CUBE_VOLUME_CONTENT_TOKEN`, and `CUBE_VOLUME_TEST_PATH` pointing
to a stable existing file. Inject credentials through the environment; do not
commit them or pass them on the command line. HTTPS is required outside loopback.

Optional settings:

| Variable | Purpose |
|---|---|
| `CUBE_VOLUME_TEST_SHA256` | Expected 64-character SHA-256 of the existing file |
| `CUBE_VOLUME_TEXT_PATH` | Additional existing UTF-8 file for text-format checks |
| `CUBE_VOLUME_TEST_DIR` | Directory for listing; defaults to the file's parent |
| `CUBE_VOLUME_TEST_MANAGEMENT=1` | Also query existing list/detail/connect APIs; requires explicit `CUBE_API_URL`/`E2B_API_URL` and management credentials if enforced |
| `CUBE_VOLUME_E2E_WRITE=1` | Explicitly enable write/mkdir/metadata/remove cases with a read-write token |
| `CUBE_VOLUME_WRITE_ROOT` | Existing writable parent for unique `sdk-e2e-*` directories; defaults to `/` |

Default execution reads only; it never creates/destroys a volume or sandbox.
Mutation tests remove only their unique files/directories, never the supplied
volume. Cleanup failures report paths for manual removal. A process crash may
leave test data. Calls to Sandbox creation/connection and non-volume HTTP routes
fail the tests; backend VM/lease behavior still needs server-side verification.
All live cases skip without `--run-e2e`; missing required settings fail once enabled.
These tests are not a live E2B differential comparison or a full tenant/symlink
isolation certification. Large-file checks buffer the baseline once; use a file
that fits the test runner's memory.

### List & health check

```python
from cubesandbox import Sandbox

print(Sandbox.health())     # {"status": "ok"}
print(Sandbox.list())       # list of running sandbox dicts
print(Sandbox.list_v2())    # v2 API (supports filtering)
```

## Configuration

| Environment Variable | Required | Default | Description |
|---|:---:|---|---|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | CubeAPI management plane address |
| `CUBE_TEMPLATE_ID` | ✅ | — | Template ID for sandbox creation |
| `CUBE_PROXY_NODE_IP` | remote | — | CubeProxy node IP, bypasses DNS for `*.cube.app` |
| `CUBE_API_KEY` | | — | API key for auth-enabled deployments |
| `CUBE_PROXY_PORT_HTTP` | | `80` | CubeProxy HTTP port |
| `CUBE_SANDBOX_DOMAIN` | | `cube.app` | Sandbox domain suffix |
| `CUBE_MCP_TEMPLATE_ID` | | `mcp-gateway` | Template tried first when `mcp` is set and no template is given; falls back to `CUBE_TEMPLATE_ID` if it does not exist |

You can also pass a `Config` object directly:

```python
from cubesandbox import Config, Sandbox

cfg = Config(
    api_url="http://10.0.0.1:3000",
    api_key="my-secret-key",
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
| `Sandbox.create(template, *, timeout, env_vars, envs, metadata, distribution_scope, volume_mounts, config)` | `POST /sandboxes` — create a new sandbox (optionally restricted to specified compute nodes or mounting volumes); `envs` is the E2B-compatible alias for `env_vars` |
| `Sandbox.connect(sandbox_id, timeout=None, *, config)` | `POST /sandboxes/:id/connect` — connect (auto-resumes if paused) and optionally reset the idle timeout |
| `Sandbox.list(config)` | `GET /sandboxes` — list running sandboxes (v1) |
| `Sandbox.list_v2(config)` | `GET /v2/sandboxes` — list sandboxes (v2) |
| `Sandbox.health(config)` | `GET /health` — service health check |

### `Sandbox` — instance methods

| Method | Description |
|---|---|
| `sb.run_code(code, *, on_stdout, on_stderr, on_result, on_error, envs, timeout)` | `POST /execute` — execute code, returns `Execution` |
| `sb.get_info()` | `GET /sandboxes/:id` — get sandbox state and metadata as a `SandboxInfo` |
| `sb.pause(*, wait, timeout, interval)` | `POST /sandboxes/:id/pause` — pause sandbox |
| `sb.resume(timeout)` | `POST /sandboxes/:id/resume` — resume (deprecated, use `connect`) |
| `sb.set_timeout(timeout)` | `POST /sandboxes/:id/timeout` — set sandbox idle timeout |
| `sb.kill()` | `DELETE /sandboxes/:id` — destroy sandbox |
| `sb.get_host(port)` | Return virtual hostname `{port}-{id}.{domain}` |

### `sb.files` — Filesystem

| Method | Description |
|---|---|
| `sb.files.read(path, *, format=...)` | Download via `GET /files`; `format` is `text` (default) / `bytes` / `stream` (e2b-compatible) |
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
| `Volume.create(name, **opts)` | `POST /volumes` — create a volume → `Volume`; optional Cube `driver` extension |
| `Volume.connect(volume_id, **opts)` | `GET /volumes/:id` — connect without creating a sandbox → `Volume` |
| `Volume.list(**opts)` | `GET /volumes` → `list[VolumeInfo]`, ID/name only |
| `Volume.get_info(volume_id, **opts)` | `GET /volumes/:id` → `VolumeAndToken`, token and optional domain |
| `Volume.destroy(volume_id, **opts)` | `DELETE /volumes/:id` → `bool` |

Mounting via `Sandbox.create(volume_mounts={path: vol})` is separate from direct content access. `VolumeMount(vol, read_only=True)` controls only that sandbox attachment. `create` / `connect` return a `Volume`, `list` returns `VolumeInfo` entries, and class-level `get_info` returns `VolumeAndToken`. Only the handle/detail carries token; list entries do not. Instance content methods: `read_file`, `write_file`, `list`, `make_dir`, `get_info`, `update_metadata`, `exists`, `remove`. Full reference: [`docs/volume.md`](docs/volume.md).

### `Execution` object

| Attribute | Type | Description |
|---|---|---|
| `.text` | `str \| None` | Final expression value (main result) |
| `.logs.stdout` | `list[str]` | All stdout lines |
| `.logs.stderr` | `list[str]` | All stderr lines |
| `.error` | `ExecutionError \| None` | Exception info if execution failed |
| `.results` | `list[Result]` | All result events |

### `SandboxInfo` object

Returned by `sb.get_info()`. Exposes E2B-compatible `snake_case` attributes with
typed values. It is also a `dict` containing the raw CubeAPI JSON snapshot, so
existing code using iteration, `len()`, `copy()`, or JSON serialization remains
backward-compatible. The sensitive `envdAccessToken` is intentionally omitted
from iteration and serialization, while explicit `info["envdAccessToken"]`
lookup remains supported.

| Attribute | Type | Description |
|---|---|---|
| `.sandbox_id` | `str` | Sandbox ID (raw `sandboxID`) |
| `.template_id` | `str` | Template ID (raw `templateID`) |
| `.sandbox_domain` | `str \| None` | Sandbox domain (raw `domain`) |
| `.started_at` | `datetime \| None` | Start time (raw `startedAt` string) |
| `.end_at` | `datetime \| None` | Expiry time (raw `endAt` string) |
| `.cpu_count` | `int \| None` | vCPU count, truncates sub-core to 0 (raw `cpuCount`) |
| `.cpu_milli` | `int \| None` | Exact CPU allocation in millicores, e.g. 500 for 0.5 vCPU (raw `cpuMilli`) |
| `.memory_mb` | `int \| None` | Memory in MiB (raw `memoryMB`; the historical field name is retained) |
| `.disk_size_mb` | `int \| None` | Disk size in MB (raw `diskSizeMB`) |
| `.envd_version` | `str` | envd version (raw `envdVersion`) |
| `.state` | `SandboxState \| str \| None` | Lifecycle state; unknown values fall back to the raw string |
| `.name` | `str \| None` | Alias (raw `alias`) |
| `.metadata` | `dict[str, str]` | User metadata |

```python
info = sandbox.get_info()

# E2B-style typed attribute access
info.sandbox_id
info.end_at            # datetime
info.state             # SandboxState.RUNNING

# Backward-compatible dict access to raw CubeAPI JSON
info["sandboxID"]
info.get("state")      # "running"
```

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
