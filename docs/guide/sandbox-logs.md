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

`cubecli` is built alongside Cubelet and installed as part of the standard one-click deployment. Run it **directly on the compute node**. It reads current sandbox logs from the host and enters the Cubelet mount namespace only when falling back to the legacy bundle path.

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
| Sandbox stdout | `/data/cubelet/log/<sandbox-id>/stdout` (host filesystem) |
| Sandbox stderr | `/data/cubelet/log/<sandbox-id>/stderr` (host filesystem) |
| Template stdout | `/data/log/template/<template-id>_0/stdout` (host filesystem) |
| Template stderr | `/data/log/template/<template-id>_0/stderr` (host filesystem) |

Older sandboxes may use the bundle path under `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/` in the Cubelet mount namespace. `cubecli` falls back to that path automatically.

## Retaining logs after teardown

Sandbox logs are deleted on teardown by default. To opt in to node-local retention, create the following empty marker file on **each compute node** before reproducing the issue:

```bash
sudo install -d -m 0755 /etc/cube-sandbox
sudo touch /etc/cube-sandbox/retain-sandbox-logs
```

On cleanup, CubeShim moves the live directory to `/data/cubelet/log-archive/<sandbox-id>/<segment-id>/logs/`. A new segment is created on each teardown, including pause and resume, so earlier output is not overwritten. A separate shim delete process uses the same marker after a crash. If archiving fails, the original directory is left in place and cleanup reports the error. The archive and live directory must be on the same filesystem for the rename to work. `cubecli logs` reads the live path; inspect archived `stdout` and `stderr` directly on the node.

Retention has no automatic expiry or size limit. Configure disk alerts and ship both live and archived logs to independent storage, then manage retention there and on the node. Removing the marker restores deletion for future cleanups; it does not remove existing archives. Archiving is best effort: it cannot recover output that was never forwarded from the guest, or survive loss of the node's disk.

Only the container's init stdout/stderr is forwarded here using the default RPC forwarding path. The opt-in passfd I/O path uses different output paths and is not covered by this archive. If using `cube-entrypoint.sh`, configure `ENVD_LOG_FILE=-` in the image or template before envd starts to include its output. The injected-envd wrapper instead writes `/var/log/envd.log` inside the guest and needs separate configuration or collection. Collect exec-task output separately through the SDK. Avoid logging secrets from environment variables or request bodies.

## Scope and limitations

- These logs capture only the **init process** (PID 1 inside the container). Output from processes spawned via `exec` calls is captured through the E2B SDK's `on_stdout` / `on_stderr` callbacks — refer to the [E2B SDK documentation](https://e2b.dev/docs) for details.
- Log forwarding must be enabled on the running CubeShim version (available since v0.4.0). On older deployments the log file will be missing.
- Logs are not streamed in real time — there is no `--follow` flag yet. Re-run the command to see new output.
- By default, log files are removed when the sandbox is deleted; node-local retention is opt-in as described above.

## Related

- [Service Management & Logs](./service-management.md) — host-side service logs, journalctl, and the diagnostic bundle
- [Template Inspection & Request Preview](./template-inspection-and-preview.md)
