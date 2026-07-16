---
title: How to Find CubeSandbox Component Logs (cubecli logs and the Guest Kernel Log)
author: ls-ggg
date: 2026-07-15
tags:
  - logging
  - cubelet
  - cubeshim
  - operations
lang: en-US
---

# How to Find CubeSandbox Component Logs (cubecli logs and the Guest Kernel Log)

## Symptom

CubeSandbox is made up of several components (CubeAPI, CubeMaster, Cubelet, CubeShim, the Hypervisor/VMM, network-agent, cube-proxy). When troubleshooting, it's often unclear which log file a given error actually lands in, where the guest kernel boot log can be found, and how to raise Cubelet's log verbosity — its default log level (`warn`) doesn't print enough detail to reproduce most issues.

This article lists the **exact paths** for every component's logs in a one-click (systemd-managed) deployment, explains how to use `cubecli logs` to read sandbox/template logs, shows where the guest kernel boot log lives, and covers how to turn on Cubelet debug logging.

## Environment

- Cube Sandbox version: v0.4.0 or newer (one-click deployment, systemd-managed)
- Deployment mode: all-in-one or multi-node cluster, applies to both
- Host OS / kernel: any Linux host running Cubelet
- Related components: CubeAPI, CubeMaster, Cubelet, CubeShim, Hypervisor (VMM), network-agent, cube-proxy

## Root Cause

CubeSandbox logs fall into three categories, each with a different source and a different way to read it:

1. **Business/behavioral logs**: requests, scheduling, audit, VMM creation, etc. Each component writes these directly to files under `/data/log/<Module>/`. They **never** show up in `journalctl`.
2. **Startup-time logs**: the stdout/stderr produced while a process is being brought up by systemd (or exiting on failure). These can only be viewed with `journalctl -u <unit>`.
3. **Sandbox/template container logs**: the stdout/stderr of the sandbox's init process (container PID 1), written by CubeShim into Cubelet's private mount namespace. These require `cubecli logs` to read; template build logs, by contrast, land on the host filesystem and need no namespace entry.

The guest kernel's boot log is, at its core, just another form of "container init process output": CubeShim forwards everything the guest kernel prints over its `console=hvc0` console — including the `Linux version ...` boot banner — into the very same `cube-shim-req.log`. So **the guest kernel log is already inside the CubeShim log**; no extra tooling is needed.

Cubelet defaults to a fairly quiet `warn` log level, so normal operation won't print detailed debug information. There are two ways to raise it to `debug`: edit the hot-reloaded dynamic config file, or flip it temporarily via a built-in HTTP endpoint (no restart required).

## Resolution

### 1. Component log cheat sheet

| Component | Log type | Path | How to view |
|---|---|---|---|
| CubeAPI | Business request log (daily rotation) | `/data/log/CubeAPI/cube-api-YYYY-MM-DD.log` | `tail -F` |
| CubeMaster | Business request log | `/data/log/CubeMaster/cubemaster-req.log` | `tail -F` |
| CubeMaster | Startup-time log | `/data/log/CubeMaster/cubemaster.log` | `tail -F` |
| Cubelet | Business request log | `/data/log/Cubelet/Cubelet-req.log` | `tail -F` |
| Cubelet | Stats/metrics log | `/data/log/Cubelet/Cubelet-stat.log` | `tail -F` |
| Cubelet | Startup-time log | — | `journalctl -u cube-sandbox-cubelet.service` |
| CubeShim | Business request log (includes guest kernel output) | `/data/log/CubeShim/cube-shim-req.log` | `tail -F` |
| CubeShim | Stats log | `/data/log/CubeShim/cube-shim-stat.log` | `tail -F` |
| Hypervisor (VMM) | VMM creation log | `/data/log/CubeVmm/vmm.log` | `tail -F` |
| network-agent | Business request log | `/data/log/network-agent/network-agent-req.log` | `tail -F` |
| cube-proxy | Access/error/sidecar logs (inside container) | container-internal `/data/log/cube-proxy/{access,error,sidecar}.log` | `docker exec <container> tail ...` (see below) |
| Sandbox container | init process stdout/stderr | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/{stdout,stderr}` (inside Cubelet's mount namespace) | `cubecli logs <sandbox-id>` (see below) |
| Template build | Build container stdout/stderr | `/data/log/template/<template-id>_0/{stdout,stderr}` (host filesystem) | `cubecli logs --tpl <template-id>` |

::: tip Business logs are not in journalctl
As also documented in `docs/guide/service-management.md`: all business/request logs above go **directly to files under `/data/log/<Module>/`**. `journalctl` only shows the small amount of stdout/stderr produced while the process is starting up or exiting.
:::

### 2. Reading sandbox / template logs with `cubecli logs`

`cubecli` is built alongside Cubelet and installed automatically by the one-click installer. Sandbox log files live inside Cubelet's private mount namespace, so `cubecli logs` **must be run directly on the compute node** — it cannot be invoked remotely via any API.

```bash
# Sandbox stdout, last 100 lines (default)
cubecli logs <sandbox-id>

# Sandbox stderr, last 100 lines
cubecli logs --stderr <sandbox-id>

# Full sandbox log
cubecli logs --all <sandbox-id>

# Last N / first N lines
cubecli logs --tail 50 <sandbox-id>
cubecli logs --head 20 <sandbox-id>

# Template build log: read straight from the host filesystem, skipping namespace entry
cubecli logs --tpl <template-id>
cubecli logs --tpl --all --stderr <template-id>
```

Flags:

| Flag | Shorthand | Description |
|---|---|---|
| `--tpl` | — | Treat the id as a template ID, reading from `/data/log/template/<id>_0/` with no namespace entry |
| `--stderr` | `-e` | Read stderr instead of stdout |
| `--all` | `-a` | Print all lines; cannot be combined with `--tail`/`--head` |
| `--tail N` | `-t N` | Print the last N lines (default 100) |
| `--head N` | `-H N` | Print the first N lines |

`cubecli logs` only covers the output of the **container init process (PID 1)**. Sub-tasks started inside a sandbox via the E2B `exec` interface produce their own stdout/stderr, which must be captured with the E2B SDK's `on_stdout`/`on_stderr` callbacks — that's out of scope here; see [Sandbox Logs](../sandbox-logs.md).

Log files are removed together with the sandbox once it's deleted, and log forwarding requires CubeShim v0.4.0 or newer.

### 3. Guest kernel log: right there in the CubeShim log

When CubeShim boots a VM, it takes over the guest kernel's console output via the `console=hvc0` kernel parameter, and — just like the sandbox's init-process output — forwards it into the same CubeShim request log file:

```bash
LC_ALL=C sudo grep -a -E "(<sandbox-id or InstanceId>|Linux version)" /data/log/CubeShim/cube-shim-req.log
```

`cube-shim-req.log` is newline-delimited JSON; the actual log text lives in the `LogContent` field. A typical guest kernel boot record looks like:

```json
{"Module":"Shim","InstanceId":"<sandbox-id>","ContainerId":"<sandbox-id>","Timestamp":"...","LogContent":"[    0.000000] Linux version 6.12.33-cube.sandbox.pvm.guest-... #2 SMP PREEMPT_DYNAMIC ...","FunctionType":""}
```

Filtering by `InstanceId` (the sandbox ID) gets you the full kernel boot output for that sandbox — device probing, `EXT4-fs`, rootfs mount, and so on — with no extra tooling or namespace entry required.

::: tip Why it's here and not somewhere else
`console=hvc0` is the VM's serial console; every kernel `printk` goes through it. CubeShim forwards that console stream straight into its own log pipeline (sharing the same writer as its ordinary request logs), so reading the guest kernel log is just a `grep` on `cube-shim-req.log` — no extra mount or debug tool needed.
:::

### 4. cube-proxy's in-container logs

`cube-proxy` is an OpenResty-based nginx container. Its logs live inside the container at `/data/log/cube-proxy/`, **not on the host filesystem**, so you need `docker exec` to read them:

```bash
docker exec -it cube-proxy tail /data/log/cube-proxy/error.log
docker exec -it cube-proxy tail /data/log/cube-proxy/sidecar.log
docker exec -it cube-proxy tail /data/log/cube-proxy/access.log
```

The container name is fixed at `cube-proxy` (created by systemd via `docker create`); you can confirm the actual container name/ID with `docker ps` first if needed.

| File | Content |
|---|---|
| `access.log` | nginx access log |
| `error.log` | nginx error log |
| `sidecar.log` | cube-proxy sidecar logic (auth/routing, etc.) |

### 5. Enabling Cubelet debug-level logging

Cubelet's default log level is `warn`, which usually isn't verbose enough for troubleshooting. There are two ways to raise it to `debug`, neither of which requires **restarting the Cubelet process**:

#### Option 1: Edit the dynamic config file (persistent, hot-reloaded)

Cubelet polls its dynamic config file `dynamicconf/conf.yaml` every 10 seconds and hot-reloads it (including re-applying the log level) whenever it changes. Add a `log_level` line under the `common:` section:

```yaml
common:
  enable_pf_mode: false
  log_level: debug   # add this line
  sandbox_exec_cmd_time_out: 5s
  ...
```

In a one-click deployment, the default path for this file is:

```
/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml
```

After saving, wait roughly 10 seconds (the config poll interval) for Cubelet to pick up `debug` level; `/data/log/Cubelet/Cubelet-req.log` will then start emitting much finer-grained records. Debug logging can increase per-request log volume by an order of magnitude, causing disk I/O contention and CPU overhead under high throughput — watch `/data/log/` disk usage while debug is active. Remember to remove the line or reset it back to `info`/`warn` once you're done, so you don't accumulate excessive log volume long-term.

#### Option 2: Flip it temporarily via the debug port (no file change, resets on restart)

Cubelet exposes a built-in debug HTTP port (listening on `:9966` by default, matching `[debug] address` in `config.toml`) with a `/debug/loglevel` endpoint you can call directly to switch levels on the fly:

```bash
# Switch to debug
curl -X POST 'http://127.0.0.1:9966/debug/loglevel?level=debug'

# Switch back to info once done
curl -X POST 'http://127.0.0.1:9966/debug/loglevel?level=info'
```

This takes effect immediately, with no polling delay — but it's **only valid for the lifetime of the current process**. Once Cubelet restarts, the level reverts to whatever the config file specifies (`warn` by default). This is the right choice when you want to capture a short window of detailed logs without touching the config file.

The same debug port also hosts `pprof` profiling endpoints (`/debug/pprof/*`), which are useful when investigating performance issues. **Security note:** pprof exposes goroutine stacks, heap profiles, and source snippets — keep port `:9966` bound to localhost only; never proxy it through a reverse proxy or bind it to `0.0.0.0`.

::: warning Only affects newly created sandboxes
No matter which method you use to switch to `debug`, **sandboxes that are already running will not retroactively emit more detailed logs.** Their creation/startup-phase log lines were already written at whatever level was in effect at the time, and switching the level afterwards doesn't cause them to be reprinted. To capture full `debug`-level output, switch the level **before** creating/starting the sandbox you want to inspect — i.e. reproduce the issue with a freshly created sandbox after raising the level, rather than expecting an already-running sandbox to suddenly log more.
:::

## References

- Related docs:
  - [Sandbox Logs](../sandbox-logs.md)
  - [Service Management and Logs](../service-management.md)
- Related code:
  - `Cubelet/cmd/cubecli/commands/cubebox/logs.go` (`cubecli logs` implementation)
  - `Cubelet/services/server/server.go` (the `/debug/loglevel` HTTP endpoint)
  - `Cubelet/pkg/config/config.go`, `Cubelet/pkg/hotswap/file.go` (dynamic config hot reload)
  - `shim/src/log/mod.rs` (CubeShim's log/console-forwarding implementation)
  - `shim/src/hypervisor/config.rs` (the `console=hvc0` kernel parameter setup)
