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
| Sandbox stdout | `/data/cubelet/log/<sandbox-id>/stdout` |
| Sandbox stderr | `/data/cubelet/log/<sandbox-id>/stderr` |
| Template stdout | `/data/log/template/<template-id>_0/stdout` (host filesystem) |
| Template stderr | `/data/log/template/<template-id>_0/stderr` (host filesystem) |

Older deployments may keep sandbox logs inside Cubelet's bundle mount namespace; `cubecli logs` can also read those files.

## Delayed cleanup of sandbox logs

Delayed cleanup is disabled by default. To enable it, deploy the updated Cubelet and CubeShim and set the following in Cubelet's `config.toml`:

```toml
[plugins."io.cubelet.internal.v1.cubebox"]
  sandbox_log_cleanup_enabled = true
  sandbox_log_retention = "24h"
  sandbox_log_cleanup_interval = "1m"
```

Both durations must be positive. Restart Cubelet after changing the configuration. The setting is inherited by newly started shims; already running shims retain the setting they started with. When disabling the feature, logs from previously enabled shims may remain and require operator cleanup.

Cubelet starts the retention period only after it confirms the sandbox runtime has exited. Its background scan removes marked log directories after that period, once sandbox metadata and the shim are gone. Paused sandboxes, unmarked logs and logs whose runtime state is uncertain are retained. A Cubelet restart preserves the cleanup deadline in the log directory. Without the option, no scan is started and shim deletion follows its existing behavior.

The feature only retains init stdout/stderr that CubeShim forwards to the host; it does not collect files written solely inside the guest. Retained logs are node-local and can consume disk space: monitor usage and export them elsewhere if longer retention or node-loss protection is needed.

Directories without a valid cleanup marker and namespace, including logs left before this feature was enabled or by older marker formats, are never automatically removed. Inspect them before any manual cleanup.

An informational `sandbox log directory missing` message means there was no directory to mark. The sandbox may have failed before it produced init logs, or an older shim may have removed them; check the deployed shim version and the sandbox lifecycle logs. With delayed cleanup enabled, explicitly requested sandbox IDs must be valid and have no surrounding whitespace. Concurrent same-ID operations can cause Create to wait or return a retryable state error if its deadline expires.

## Scope and limitations

- These logs capture only the **init process** (PID 1 inside the container). Output from processes spawned via `exec` calls is captured through the E2B SDK's `on_stdout` / `on_stderr` callbacks — refer to the [E2B SDK documentation](https://e2b.dev/docs) for details.
- Log forwarding must be enabled on the running CubeShim version (available since v0.4.0). On older deployments the log file will be missing.
- Logs are not streamed in real time — there is no `--follow` flag yet. Re-run the command to see new output.
- By default log files are removed when the sandbox is deleted; delayed cleanup is opt-in.

## Related

- [Service Management & Logs](./service-management.md) — host-side service logs, journalctl, and the diagnostic bundle
- [Template Inspection & Request Preview](./template-inspection-and-preview.md)
