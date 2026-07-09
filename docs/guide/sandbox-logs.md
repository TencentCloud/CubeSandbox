---
title: Sandbox Logs
lang: en-US
---

# Sandbox Logs

::: warning Work in progress
Log retrieval is still being actively iterated. The `cubecli logs` command described here is a **temporary solution** — the interface and underlying storage layout may change in future releases.
:::

CubeSandbox exposes two complementary logging layers:

| Layer | What it captures | How to access |
|---|---|---|
| **Sandbox log** | stdout/stderr of the container init process (the main entrypoint) | `cubecli logs` (this page) |
| **`envd` task log** | stdout/stderr of individual `exec` sub-tasks spawned inside the sandbox | E2B SDK (`on_stdout` / `on_stderr` callbacks) |

This page covers the **sandbox-level log** only. For `envd` sub-task logs, refer to the [E2B SDK documentation](https://e2b.dev/docs).

## Prerequisites

`cubecli` is built alongside Cubelet and installed as part of the standard one-click deployment. The `logs` sub-command accesses log files that live inside the **Cubelet mount namespace**, so it must be run **directly on the compute node** — it cannot be executed remotely via the API or from a non-node host.

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
