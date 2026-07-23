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

- **`timeout`** (optional): seconds of **idle** time before a timeout fires (e2b uses milliseconds via `timeoutMs`; Cube uses seconds). When omitted, the server decides — the SDK no longer injects a hard-coded default.
- **`on_timeout`**: what happens at timeout — `"kill"` (default; destroy) or `"pause"` (snapshot for later resume).

`timeout` values (e2b-aligned):

| Value | Behavior |
|-------|----------|
| omitted | Server default idle TTL; if the server has no positive default, the sandbox **never** times out |
| `NEVER_TIMEOUT` (`-1`) | **Never** time out — no idle reclamation |
| `0` | **Immediate** timeout — reclaimed on the first idle sweep |
| positive integer `N` | Timeout after **N** seconds of idle time |

Go: `cubesandbox.NeverTimeout`; Python: `from cubesandbox import NEVER_TIMEOUT`.

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
| `timeout`               | Optional idle timeout in seconds; see the table above |
| `lifecycle`             | Lifecycle policy — see [Platform-managed auto-pause / auto-resume](#platform-managed-auto-pause-auto-resume) below. |
| `metadata`              | Arbitrary key/value pairs stored on the sandbox; readable from the list / detail endpoints. |
| `env_vars`              | Environment variables injected into the sandbox process.                                     |
| `allow_internet_access` | Whether outbound internet is allowed; `network` provides finer-grained egress control.       |

> Cube doesn't impose hard wall-clock ceilings (24h Pro / 1h Base) the way hosted e2b does. When you omit `timeout`, the effective idle TTL is set by your cluster operator — see [Operational Notes](#operational-notes) below.

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

`endAt` is the projected next-timeout instant given the current `timeout`. It is refreshed every time the sandbox receives a request (or when you call `set_timeout`, when available). For **never-timeout** sandboxes there is no deadline, so `endAt` is **omitted** from the response rather than reported as equal to `startedAt`.

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

### Deleting a Paused Sandbox

Both `kill()` and `DELETE /sandboxes/{sandboxID}` can delete sandboxes in the `running` or `paused` state.

When deleting a `paused` sandbox, CubeSandbox first restores its runtime, then runs the standard destroy flow. This can take longer than deleting a `running` sandbox. The API remains synchronous: it returns `204 No Content` only after the sandbox and its resources have been cleaned up.

The internal restore is only used to complete deletion. It is not treated as a separate resume operation:

- It does not emit a `sandbox.resumed` lifecycle event.
- It does not reset the idle timeout.
- It does not change the synchronous DELETE semantics.

To leave enough time for the destroy flow, the internal restore runs for at most five seconds. If the request does not have enough time left for restore and destroy, CubeSandbox returns a retryable error before starting destroy.

When deleting a `paused` sandbox, the following responses are possible:

- **`204 No Content`**: The sandbox and its resources have been cleaned up.
- **`409 Conflict`**: The restore did not pass the resource admission check, for example because the node lacks capacity or the sandbox is missing required resource metadata. The sandbox remains `paused`. Follow the diagnostic in the response: retry after capacity is released, or repair or recreate the sandbox when resource metadata is missing.
- **`503 Service Unavailable` + `Retry-After: 2`**: The sandbox is entering the paused state, or another lifecycle operation has not completed. Wait at least two seconds before retrying.
- **`503 Service Unavailable` + `Retry-After: 5`**: The pre-delete restore or state preparation did not finish within its time budget, or too little time remains to start the destroy flow. Check the sandbox state and wait at least five seconds before retrying.

`Retry-After` is in seconds and tells the client when to retry. It does not mean that CubeSandbox continues deletion or starts a background retry.

Existing `404 Not Found`, `408 Request Timeout`, and `running` sandbox delete behavior remain unchanged.

If the restore reports an error but the runtime confirms that the sandbox is already `running`, CubeSandbox continues with destroy. A later destroy failure uses the existing destroy error semantics; the sandbox is not paused again and no background cleanup task is created.

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

Sandboxes that don't opt in (no `lifecycle` argument) default to `on_timeout="kill"`: once they sit idle for the effective `timeout` the platform destroys them. This matches e2b's `lifecycle.on_timeout="kill"` semantic. To avoid automatic reclamation, pass `timeout=NEVER_TIMEOUT`, omit `timeout` (with no positive server default), set a high `timeout`, or send periodic activity to reset the idle clock.

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

### Cluster default idle timeout (`default_timeout_insec`)

When the client omits `timeout`, CubeMaster applies `cubelet_conf.default_timeout_insec` in `CubeMaster/conf.yaml` (one-click installs: `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`).

| Config value | Effect when the client omits `timeout` |
|--------------|----------------------------------------|
| unset or `<= 0` | **No cluster-wide idle TTL** — sandboxes never time out from idle |
| positive `N` | Default idle TTL of **N** seconds |

The repository ships with **no cluster-wide idle timeout** (`default_timeout_insec: -1`). Set a positive value (for example `300`) if you want the cluster to reclaim sandboxes that never pass an explicit `timeout`. Restart `cube-sandbox-cubemaster.service` after edits.

`create_timeout_insec` in the same section is unrelated: it only bounds the create/scheduling RPC deadline, not sandbox idle TTL. See [Service management — CubeMaster settings](service-management.md#cubemaster-settings).

- **Pause fidelity**: CPU registers, process memory, TCP state (with no external peer), and filesystem mutations all survive the snapshot. Outbound sockets the sandbox itself opened are dropped on pause and must be reopened by the application after resume.
- **Cluster coordination**: auto-pause is driven by the `cube-lifecycle-manager` service that runs on the control node. It consumes lifecycle events CubeMaster publishes via Redis stream, discovers every live CubeProxy replica through a Redis-backed registration table, and broadcasts state to each of them. Cross-replica races are resolved by Redis `SETNX` state locks so the same sandbox is never paused or resumed twice concurrently.
- **Failure mode**: when an auto-resume RPC fails, CubeProxy returns `503 + Retry-After` to the client immediately rather than hanging on a long timeout. When the sandbox has already been killed (`killing` / `killed`) the proxy returns `410 Gone` instead, telling SDK clients to stop retrying.
- **Diagnostics**: `docker logs cube-lifecycle-manager` (control node) is the runtime log for the auto-pause coordinator. Look for `create event applied`, `auto-paused sandbox`, `auto-resumed sandbox`, `timeout-killed sandbox`. Each CubeProxy replica additionally exposes `GET http://<node-ip>:8082/admin/healthz` reporting `heartbeat_last_pushed_ms` (the last time it announced itself to the manager).

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
