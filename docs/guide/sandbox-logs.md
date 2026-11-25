---
title: Sandbox Logs
lang: en-US
---

# Sandbox Logs

::: warning Work in progress
Log retrieval is still being actively iterated. The `cubecli logs` command described here is a **temporary solution** — the interface and underlying storage layout may change in future releases.
:::

CubeSandbox exposes three complementary logging layers:

| Layer | What it captures | How to access |
|---|---|---|
| **Sandbox log** | stdout/stderr of the container init process (the main entrypoint) | `cubecli logs` (this page) |
| **Sandbox events** | CubeShim request log lines for the sandbox: lifecycle and forwarded output | [Events API](#events-api) (this page) |
| **`envd` task log** | stdout/stderr of individual `exec` sub-tasks spawned inside the sandbox | E2B SDK (`on_stdout` / `on_stderr` callbacks) |

This page covers the **sandbox log** and **sandbox events**. For `envd` sub-task logs, refer to the [E2B SDK documentation](https://e2b.dev/docs).

## Events API

Sandbox events come from the CubeShim request log, not from the stdout/stderr files that `cubecli logs` reads. Unlike `cubecli`, they can be fetched remotely through the API on any deployment (single-node, multi-node, or K8s). CubeAPI forwards the request through CubeMaster to the Cubelet that owns the sandbox:

```bash
# Newest events (polling/tail mode)
curl "/cubeapi/v1/v2/sandboxes/<sandbox-id>/logs?tail=true"

# Paginate from the beginning, 200 per page
curl "/cubeapi/v1/v2/sandboxes/<sandbox-id>/logs?limit=200"

# Next page: pass back both nextCursor and nextCursorSeq from the previous response
curl "/cubeapi/v1/v2/sandboxes/<sandbox-id>/logs?cursor=<nextCursor>&cursorSeq=<nextCursorSeq>&limit=200"
```

Parameters:

- `limit` (default 1000, max 2000) is a hard cap on entries per page.
- `cursor` and `cursorSeq` (the previous `nextCursor` and `nextCursorSeq`) should be sent together. The pair returns only entries after that position, none repeated and none skipped. `cursorSeq` may be `0`, so always send it. `cursorSeq` without `cursor` is rejected with 400.
- `cursor` alone is still accepted for compatibility with earlier callers. A millisecond timestamp cannot tell apart events that share a millisecond, so this form skips every event sharing the cursor's millisecond; use the pair when completeness matters.
- `tail` returns the newest `limit` entries instead of paginating and is mutually exclusive with `cursor` and `cursorSeq`. Its `nextCursor`/`nextCursorSeq` can be passed back to read what arrives next.
- `direction` is accepted and ignored.

The response carries `logs` and `hasMore`. `nextCursor` and `nextCursorSeq` appear only when the backend returned a non-zero `nextCursor`; both are omitted otherwise, such as on an empty result.

::: warning Limitations
- The cursor is only valid within the active shim log file; it does not span log rotation (hourly).
- `tail` reads at most the last 16 MiB of the log. If the newest events' millisecond straddles that boundary, `nextCursorSeq` is undercounted and a follow-up read may repeat a few events, never skip any.
- Position is derived from log timestamps, so events written with a clock that moves backwards may be skipped.
- Internal endpoints (`/cube/sandbox/logs`, `/api/v1/sdk/sandboxes/{id}/logs`) accept `cursor` alone too, with the same lossy millisecond-skip semantics. Only this API documents `direction`; the internal endpoints do not support it.
:::

::: warning Upgrade order for this feature
Upgrade Cubelet first, then CubeMaster, then CubeAPI and CubeOps (the WebUI backend).

- If CubeMaster is upgraded before the sandbox's node Cubelet, events fail with "upgrade cubelet on the owning node first".
- If CubeAPI or CubeOps is upgraded before CubeMaster, the new parameters are ignored: `tail=true` returns the oldest page instead of the newest, and a composite cursor degrades to `cursor` alone, skipping events that share its millisecond. On Kubernetes the old CubeMaster has no local shim log, so the request fails or returns nothing.
- If the cluster mixes Cubelet versions, events work only for sandboxes on upgraded nodes.
:::

## Prerequisites

`cubecli` is built alongside Cubelet and installed as part of the standard one-click deployment. The `logs` sub-command accesses log files that live inside the **Cubelet mount namespace**, so it must be run **directly on the compute node** — `cubecli logs` cannot be executed remotely via the API or from a non-node host. For remote access to sandbox events, use the [Events API](#events-api).

## Reading sandbox logs

```bash
# Last 100 lines of stdout (default)
cubecli logs <sandbox-id>

# Last 100 lines of stderr
cubecli logs --stderr <sandbox-id>

# Full log (all lines)
cubecli logs --all <sandbox-id>

# Last N lines
cubecli logs --tail 50 <sandbox-id>
# Short form
cubecli logs -t 50 <sandbox-id>

# First N lines
cubecli logs --head 20 <sandbox-id>
# Short form
cubecli logs -H 20 <sandbox-id>
```

### Flag reference

| Flag | Short | Description |
|---|---|---|
| `--stderr` | `-e` | Read stderr instead of stdout |
| `--all` | `-a` | Print all lines; cannot be combined with `--tail` or `--head` |
| `--tail N` | `-t N` | Print the last N lines (default: 100 when no other flag is set) |
| `--head N` | `-H N` | Print the first N lines |

## Reading template build logs

During template construction the container's stdout/stderr are saved to the host filesystem under `/data/log/template/<templateID>_0/`. These files do **not** require entering the Cubelet mount namespace, so `--tpl` skips the namespace re-exec:

```bash
# Last 100 lines of template build stdout
cubecli logs --tpl <template-id>

# Full build stderr
cubecli logs --tpl --all --stderr <template-id>
```

## Where the log files live

| Context | Path |
|---|---|
| Sandbox stdout | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/stdout` (inside Cubelet mount namespace) |
| Sandbox stderr | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/stderr` (inside Cubelet mount namespace) |
| Template stdout | `/data/log/template/<template-id>_0/stdout` (host filesystem) |
| Template stderr | `/data/log/template/<template-id>_0/stderr` (host filesystem) |

::: tip Why the mount namespace?
Sandbox log files are written by CubeShim into the bundle directory which is only visible inside Cubelet's private mount namespace. `cubecli logs` automatically re-execs itself into that namespace before reading — you do not need to do anything special beyond running the command on the node.
:::

## Scope and limitations

- These logs capture only the **init process** (PID 1 inside the container). Output from processes spawned via `exec` calls is captured through the E2B SDK's `on_stdout` / `on_stderr` callbacks — refer to the [E2B SDK documentation](https://e2b.dev/docs) for details.
- Log forwarding must be enabled on the running CubeShim version (available since v0.4.0). On older deployments the log file will be missing.
- Logs are not streamed in real time — there is no `--follow` flag yet. Re-run the command to see new output.
- Log files are removed when the sandbox is deleted.

## Related

- [Service Management & Logs](./service-management.md) — host-side service logs, journalctl, and the diagnostic bundle
- [Template Inspection & Request Preview](./template-inspection-and-preview.md)
