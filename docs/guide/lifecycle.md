# Sandbox Lifecycle

A sandbox is the core runtime unit of Cube-Sandbox. This page covers a sandbox's full lifecycle — from creation to teardown — and how to let the platform manage it automatically to save resources.

> The SDK shape mirrors [e2b](https://e2b.dev/docs/sandbox) so existing e2b code can port with minimal changes.

## State Model

A sandbox is always in exactly one of these states:

| State        | Meaning                                                                                        |
|--------------|------------------------------------------------------------------------------------------------|
| `running`    | Active. Real CPU/memory in use. Accepts requests and executes code.                            |
| `pausing`    | Platform is taking the VM snapshot (transient).                                                |
| `paused`     | Snapshot persisted to disk. **Zero** CPU/memory cost. Full state preserved.                    |
| `resuming`   | Platform is restoring the snapshot (transient).                                                |
| `terminated` | Killed (`kill()`) or reaped after `on_timeout="kill"`. Cannot be brought back.                 |

Two settings drive transitions:

- **`timeout`**: how many seconds of inactivity trigger "timeout" (defaults to a fixed value in SDK Config, e.g. 300s).
- **`on_timeout`**: what happens at timeout — `"kill"` (default; destroy) or `"pause"` (snapshot for later).

```
                       ┌──────────────────────────────────────┐
                       │                                      │
   create()       ┌────▼────┐   timeout & on_timeout=pause   ┌─────────┐
  ───────────────►│ running │ ──────────────────────────────►│ paused  │
                  │         │◄──────── connect() or          │         │
                  └─┬─────┬─┘    auto_resume-triggered req   └────┬────┘
                    │     │                                       │
        kill()      │     │ timeout & on_timeout=kill             │ kill()
        ────────────┘     └─────────────────┐                     │
                                            ▼                     ▼
                                      ┌────────────┐
                                      │ terminated │
                                      └────────────┘
```

## Create

```python
from cubesandbox import Sandbox

# Create a sandbox that auto-destroys after 60 seconds of idle.
# (Default on_timeout is "kill".)
sandbox = Sandbox.create(
    template="<your-template-id>",
    timeout=60,               # seconds
)

print(sandbox.sandbox_id)
```

Key parameters of `Sandbox.create()`:

| Parameter               | Description                                                                                  |
|-------------------------|----------------------------------------------------------------------------------------------|
| `template`              | Template ID used to boot the sandbox; defaults to env var `CUBE_TEMPLATE_ID`.                |
| `timeout`               | Idle timeout in **seconds**. (Note: e2b's `timeoutMs` is milliseconds; Cube uses seconds.)   |
| `lifecycle`             | Lifecycle policy — see [Platform-managed auto-pause / auto-resume](#platform-managed-auto-pause-auto-resume) below. |
| `metadata`              | Arbitrary key/value pairs stored on the sandbox; readable from the list / detail endpoints. |
| `env_vars`              | Environment variables injected into the sandbox process.                                     |
| `allow_internet_access` | Whether outbound internet is allowed; `network` provides finer-grained egress control.       |

> Cube doesn't impose hard wall-clock ceilings (24h Pro / 1h Base) the way hosted e2b does. The idle `timeout` is still required — it prevents stranded sandboxes from holding resources indefinitely.

## Inspect a Running Sandbox

```python
info = sandbox.get_info()
print(info)
# {
#   "sandboxID": "iiny0783cype8gmoawzmx-ce30bc46",
#   "templateID": "rki5dems9wqfm4r03t7g",
#   "state": "running",
#   "startedAt": "2026-06-17T12:34:56Z",
#   "endAt":     "2026-06-17T12:39:56Z",
#   "metadata":  {...}
# }
```

`endAt` is the projected next-timeout instant given the current `timeout`. It is refreshed every time the sandbox receives a request (or when you call `set_timeout`, when available).

## List Running Sandboxes

```python
for sb in Sandbox.list():
    print(sb["sandboxID"], sb["state"])
```

## Explicit Shutdown

```python
sandbox.kill()
```

`kill()` is **irreversible**: unlike pause, a killed sandbox cannot be brought back, even when `lifecycle.on_timeout="pause"` was set — `kill()` always wins and discards the snapshot.

## Explicit Pause / Resume

```python
sandbox.pause()                       # snapshot manually, free CPU/memory
# ... time passes ...
sandbox.connect()                     # restore from snapshot
sandbox.run_code("print('back!')")    # carry on as if never paused
```

See [`examples/code-sandbox-quickstart/pause.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/pause.py) for a full demo.

## Platform-managed Auto-pause / Auto-resume

Most agent workloads aren't continuously busy: the user types code → the model thinks → the sandbox executes → it sits idle until the next turn. Auto-pausing during the idle stretch and **transparently resuming** on the next request can dramatically cut resource cost.

Cube exposes the exact same [`lifecycle`](https://e2b.dev/docs/sandbox/auto-resume) shape e2b uses:

```python
sandbox = Sandbox.create(
    template="<your-template-id>",
    timeout=300,                      # 5 min of idle triggers on_timeout
    lifecycle={
        "on_timeout": "pause",        # at timeout → pause (instead of kill)
        "auto_resume": True,          # next request after pause → resume
    },
)
```

### Behaviour

- **`on_timeout="pause"`**: after `timeout` seconds idle, the platform schedules a pause. State flips to `paused`, the VM memory is frozen to the snapshot store.
- **`auto_resume=True`**: when any request next arrives for a `paused` sandbox (HTTP, `run_code`, file I/O, …), the platform wakes it up before the request lands. Callers never see the pause; typical resume latency is sub-second to a few seconds.
- If `auto_resume=False` (or unset), the sandbox stays paused until you explicitly `Sandbox.connect(sandbox_id=...)`. Useful for "wait for the user" workflows.

### Timeout reset on auto-resume

Each successful auto-resume gives the sandbox a **fresh** `timeout` countdown (matching e2b semantics). The "resume → short use → idle out → pause again" loop can repeat indefinitely.

### What counts as activity

Any of these resets the idle clock:

- SDK calls: `sandbox.run_code(...)`, `sandbox.commands.run(...)`, `sandbox.files.read(...)` / `write(...)`.
- Direct HTTP traffic to a service inside the sandbox (e.g. via the URL returned by `getHost()`).

Sandboxes that don't opt in (no `lifecycle` argument) default to `on_timeout="kill"`: once they sit idle for `timeout` seconds the platform destroys them. This matches e2b's `lifecycle.on_timeout="kill"` semantic. If you want a sandbox that never gets reaped automatically, set `timeout` high enough or have the client send periodic activity to reset the idle clock.

### End-to-end examples

The platform ships two **mirror-image** end-to-end demos, one per `on_timeout` value:

- [`examples/code-sandbox-quickstart/auto-resume.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-resume.py) — `on_timeout="pause"` + `auto_resume=True`. Creates a sandbox, idles past the timeout to trigger **auto-pause**, then issues a fresh request to trigger **auto-resume**, and verifies that both kernel memory and the filesystem are byte-identical across the cycle.
- [`examples/code-sandbox-quickstart/auto-kill.py`](https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-kill.py) — `on_timeout="kill"` (the default). Creates a sandbox, idles past the timeout to trigger **auto-kill**, verifies that the next request fails fast with 410 Gone, that the sandbox no longer appears in `Sandbox.list()`, and spawns a control sandbox to rule out cluster-wide failures.

```bash
export CUBE_TEMPLATE_ID=<your-template>

# Auto-pause + auto-resume
python examples/code-sandbox-quickstart/auto-resume.py

# Auto-kill (irreversible)
python examples/code-sandbox-quickstart/auto-kill.py
```

## Operational Notes

- **Pause fidelity**: CPU registers, process memory, TCP state (with no external peer), and filesystem mutations all survive the snapshot. Outbound sockets the sandbox itself opened are dropped on pause and must be reopened by the application after resume.
- **Cluster coordination**: auto-pause is driven by `cube-proxy-sidecar`, co-resident with each CubeProxy container. It consumes lifecycle events CubeMaster publishes via Redis stream and broadcasts state to every CubeProxy instance. Cross-replica races are resolved by Redis `SETNX` state locks so the same sandbox is never paused or resumed twice concurrently.
- **Failure mode**: when an auto-resume RPC fails, CubeProxy returns `503 + Retry-After` to the client immediately rather than hanging on a long timeout. When the sandbox has already been killed (`killing` / `killed`) the proxy returns `410 Gone` instead, telling SDK clients to stop retrying.
- **Diagnostics**: `/data/log/cube-proxy/sidecar.log` is the sidecar's runtime log. Look for `create event applied`, `auto-paused sandbox`, `auto-resumed sandbox`, `timeout-killed sandbox`.

### Paused Resource Release & Scheduling Quota

When a sandbox is paused, its CPU and memory are physically reclaimed — but by default, the node resource accounting still counts `paused`/`pausing` sandboxes as "occupied" against the scheduler quota. This means: even after many idle sandboxes are paused, the host still shows no available capacity to create new ones.

To address this, Cube provides a **node-level tuning knob** `host.quota.paused_resource_release_ratio` (configured in `Cubelet/config/config.toml`), range `[0, 1]`, default `0`:

| Value | Behavior | Best For |
|---|---|---|
| `0.0` | Paused sandboxes retain full quota (identical to legacy behavior). Resume is always guaranteed — never rejected due to resource shortage. | Availability-critical environments where resume must never fail |
| `1.0` | Paused sandbox CPU/memory quota is **fully released** to the scheduler. Resume becomes best-effort — may be rejected when the node is full. | Maximizing deployment density; occasional resume failures are acceptable |
| `0 < r < 1` | Releases fraction `r`, reserves `(1-r)` as headroom. **Reserved quota still counts toward scheduler CPU/memory usage**, so pause-heavy nodes are **naturally deprioritized** — the scheduler won't keep piling new sandboxes onto nodes that already hold many paused ones. | Balancing availability against utilization |

**Configuration example**:

```toml
# Cubelet/config/config.toml
[host.quota]
paused_resource_release_ratio = 0.5   # release half, reserve half
```

**Resume admission check**:

When `ratio > 0`, every resume triggers a **local real-time admission check** — if the node lacks enough free capacity to accommodate the released fraction, the resume is rejected:

```
resume rejected by paused_resource_release_ratio policy: need 1024MB > quota 512MB
```

The rejection travels through the following chain to reach the client: `Cubelet (130409 Conflict)` → `CubeAPI (HTTP 409)` → `WebUI (capacity diagnostic)`. HTTP 409 is a retriable status — when other sandboxes are destroyed or paused later, freeing capacity, the resume can be retried.

**Important notes**:

- Disk and MvmNum are **not affected** by the ratio — pause snapshots still consume storage and the sandbox object still exists.
- `ratio=0` is the zero-value-safe default: if this setting is never configured, behavior is identical to previous versions. Upgrades won't cause surprises.
- This is a **node-level setting** — different nodes can use different ratios to accommodate heterogeneous hardware or tiered pools.
- When a large batch of sandboxes on a single node wakes up simultaneously and exceeds node capacity, the control plane returns 409 with precise quota numbers. Future releases will support **cross-node resume**, allowing sandboxes to migrate between nodes for true cluster-wide utilization maximization.

## Next Steps

- [Templates Overview](./templates.md) — sandboxes boot from templates; the template's build also shapes cold-start cost.
- [Quick Start](./quickstart.md) — the shortest path through "create sandbox → run code → tear down".
- Upstream references: [e2b · Sandbox lifecycle](https://e2b.dev/docs/sandbox), [e2b · Auto-resume](https://e2b.dev/docs/sandbox/auto-resume).
