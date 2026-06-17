# Service Management & Logs

This page is for users who have **already installed CubeSandbox** and want to keep the stack healthy in day-to-day operation.

After reading this page you will know:

- Which systemd services run on the host and how they depend on each other
- Which service to restart after editing a config file
- How to debug a service that keeps failing
- Where to find runtime logs, startup logs and in-container logs — and the boundaries between them
- How to stop / restart the whole stack cleanly

::: tip Scope
This page targets the **systemd-managed** one-click installer. If your machine still uses the legacy `up-with-deps.sh` / `down-with-deps.sh` scripts as the daily entry point, that is the pre-systemd version — re-running the latest one-click installer will migrate it to systemd automatically (the installer detects and takes over the old layout).
:::

## TL;DR cheat-sheet

```bash
# 1. Are all cube-sandbox services still alive?
sudo systemctl --no-legend list-units 'cube-sandbox-*'

# 2. Edited a config -> restart the matching service
sudo systemctl restart cube-sandbox-cube-api.service
sudo systemctl restart cube-sandbox-cubemaster.service
sudo systemctl restart cube-sandbox-cubelet.service

# 3. Runtime logs (requests / stats / audit / VMM) live under /data/log/, NOT in journalctl
sudo tail -F /data/log/Cubelet/Cubelet-req.log
sudo tail -F /data/log/CubeMaster/cubemaster-req.log
sudo tail -F /data/log/CubeAPI/cube-api-$(date +%F).log
sudo tail -F /data/log/CubeVmm/vmm.log              # sandbox VMM lifecycle

# 4. Startup failures / process exit reasons -> journalctl
sudo journalctl -u cube-sandbox-cube-api.service -n 200 --no-pager

# 5. One-shot diagnostic bundle (tails of /data/log + configs + dmesg + process snapshot)
sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh
```

::: warning Runtime logs are at `/data/log/`, NOT `journalctl`
This is the most common pitfall for new operators: each component **only sends startup-time stdout/stderr to journal**. Request / scheduling / stat / audit / VMM creation logs are written **directly to `/data/log/<Module>/`**. To find "who created a sandbox in the last hour", look at `/data/log/`, not `journalctl`.
:::

## Service overview

The one-click installer registers 14 systemd units under `/etc/systemd/system/` and aggregates them into two role-specific targets.

### Role targets

| Target | Purpose | Role |
|---|---|---|
| `cube-sandbox-control.target` | All control-plane services (default all-in-one) | `control` |
| `cube-sandbox-compute.target` | Minimum subset for compute-only nodes | `compute` |

::: tip How aggregation works
The target lists its child services via `Wants=`; each service declares membership via `PartOf=`. So `systemctl stop cube-sandbox-control.target` stops every `PartOf=cube-sandbox-control.target` service in one shot — no need to spell out the long list of unit names.
:::

### Service catalog

| Unit | Process form | Port / listen | Present on | Upstream deps |
|---|---|---|---|---|
| `cube-sandbox-mysql.service` | Docker container | `3306` | control | docker |
| `cube-sandbox-redis.service` | Docker container | `6379` | control | docker |
| `cube-sandbox-cubemaster.service` | Host process | `8089` | control | mysql, redis |
| `cube-sandbox-cube-api.service` | Host process | `3000` (E2B-compatible API) | control | cubemaster |
| `cube-sandbox-network-agent.service` | Host process | `19090` (health) | control / compute | network |
| `cube-sandbox-cubelet.service` | Host process | `9999` (gRPC) | control / compute | network-agent + `/data/cubelet` (XFS) |
| `cube-sandbox-coredns.service` | Docker container | `127.0.0.54:53` or `169.254.254.53:53` | control | docker |
| `cube-sandbox-cube-proxy.service` | Docker container | `443` (TLS) / `80` | control | docker, redis |
| `cube-sandbox-dns.service` | oneshot (no daemon) | — | control | coredns (`BindsTo`) |
| `cube-sandbox-webui.service` | Docker container | `12088` | control | docker, cube-api |

### Startup dependency map (control node)

```text
docker.service
   ├─ mysql.service ─┐
   ├─ redis.service ─┼─ cubemaster.service ─ cube-api.service ─ webui.service
   │                 └─ cube-proxy.service
   └─ coredns.service ─ dns.service (oneshot, BindsTo coredns)

network-online.target
   └─ network-agent.service ─ cubelet.service
```

Dependencies only express **startup ordering** via `After=` / `Wants=`. If an upstream service crashes at runtime, downstreams are **not** automatically restarted — cubelet won't be cycled just because cube-api died, and vice versa.

## Restarting services

### Scenario A: edited a config and want it to take effect

The two most common config entry points:

- Top-level env: `/usr/local/services/cubetoolbox/.one-click.env`
- Per-component: `Cubelet/config/config.toml`, `Cubelet/dynamicconf/conf.yaml`, `CubeMaster/conf.yaml`, `network-agent/network-agent.yaml`, `cubeproxy/global.conf`, `coredns/Corefile`

Restart the **service that consumes that config**:

```bash
# Cubelet config
sudo systemctl restart cube-sandbox-cubelet.service

# CubeMaster config
sudo systemctl restart cube-sandbox-cubemaster.service

# CUBE_API_* in .one-click.env
sudo systemctl restart cube-sandbox-cube-api.service

# cubeproxy/global.conf
sudo systemctl restart cube-sandbox-cube-proxy.service

# coredns/Corefile
sudo systemctl restart cube-sandbox-coredns.service
```

::: warning Editing the systemd unit file itself
If you change `/etc/systemd/system/cube-sandbox-*.service`, run `daemon-reload` so systemd picks up the new content:

```bash
sudo systemctl daemon-reload
sudo systemctl restart cube-sandbox-<service>.service
```

If you only edited the helper script (`/usr/local/services/cubetoolbox/scripts/systemd/*.sh`), `daemon-reload` is **not** needed — the next `restart` re-invokes the script.
:::

### Scenario B: a service is failing or restart-looping

Every service has `Restart=on-failure`, so a single crash is auto-recovered. If the unit is restart-looping, find the root cause first.

#### 1. Inspect current state

```bash
sudo systemctl status cube-sandbox-cube-proxy.service --no-pager
```

Watch for:

- `Active: failed` / `Active: activating (start-post)` (still trying)
- `Restart Counter` climbing rapidly (restart loop)
- The last 10 journal lines printed at the bottom

#### 2. Read startup logs

```bash
sudo journalctl -u cube-sandbox-cube-proxy.service -n 200 --no-pager
```

Best for: scripting bugs, ExecStart failures, `docker pull` errors, `apk` / `apt` network errors, ExecStartPost health-check timeouts.

#### 3. Read runtime logs

If the service starts but misbehaves, **runtime logs live under `/data/log/`, not in journal**:

```bash
sudo tail -200 /data/log/Cubelet/Cubelet-req.log
sudo tail -200 /data/log/CubeMaster/cubemaster-req.log
sudo tail -200 /data/log/CubeAPI/cube-api-$(date +%F).log
```

#### 4. Reset the failed counter and restart

```bash
sudo systemctl reset-failed cube-sandbox-cube-proxy.service
sudo systemctl restart cube-sandbox-cube-proxy.service
```

### Scenario C: full restart / post-maintenance recovery

```bash
# Control node
sudo systemctl restart cube-sandbox-control.target

# Compute node
sudo systemctl restart cube-sandbox-compute.target
```

Or from the release-bundle directory:

```bash
sudo ./down.sh
sudo systemctl start cube-sandbox-control.target
```

::: tip Restarting the target = ordered restart of every PartOf service
A target has no process of its own. Restarting it makes systemd cycle every `PartOf=cube-sandbox-control.target` service in dependency order — a shorthand for "restart everything".
:::

### Scenario D: full shutdown

```bash
# Recommended: use the bundled script (auto-detects role)
sudo /root/cube-sandbox-one-click-<version>/down.sh

# Equivalent
sudo systemctl stop cube-sandbox-control.target   # control node
sudo systemctl stop cube-sandbox-compute.target   # compute node
```

`down.sh` does **not** delete data: MySQL / Redis volumes, `/data/cubelet`, `/data/log/...` are all preserved and resumed on the next `start`.

## Reading logs

CubeSandbox emits three log streams. **Don't mix them up:**

| Source | Contains | How to read |
|---|---|---|
| **Runtime logs (primary entry point)** | requests, scheduling decisions, stats, audit, VMM creation | **`/data/log/<Module>/`** |
| Startup logs | systemd start / hooks / ExecStartPost / exit codes / container build output | `journalctl -u <unit>` |
| In-container logs | `cube-proxy` only: nginx access/error inside the container | `docker exec cube-proxy tail /data/log/cube-proxy/error.log` |

### `/data/log/` runtime logs (primary)

> ⚠️ Cubelet / CubeMaster / CubeAPI / network-agent / CubeShim / VMM all write **business request + stat + audit + VMM lifecycle** logs to `/data/log/`. They do **not** show up in `journalctl` — read the files directly.

| Module | Directory | Main files |
|---|---|---|
| Cubelet | `/data/log/Cubelet/` | `Cubelet-req.log` (requests)<br>`Cubelet-stat.log` (metrics/stats) |
| CubeMaster | `/data/log/CubeMaster/` | `cubemaster-req.log` |
| CubeAPI | `/data/log/CubeAPI/` | `cube-api-YYYY-MM-DD.log` (daily-rotated) |
| network-agent | `/data/log/network-agent/` | `network-agent-req.log` |
| CubeShim | `/data/log/CubeShim/` | `cube-shim-req.log`, `cube-shim-stat.log` |
| Hypervisor (VMM) | `/data/log/CubeVmm/` | `vmm.log` (one entry per sandbox creation) |
| cube-proxy | container `/data/log/cube-proxy/` | `error.log`, `access.log` (see below) |

Common commands:

```bash
# Follow Cubelet requests
sudo tail -F /data/log/Cubelet/Cubelet-req.log

# Follow daily-rotated CubeAPI log (E2B-compatible layer)
sudo tail -F /data/log/CubeAPI/cube-api-$(date +%F).log

# Slow / failing sandbox start: check VMM log
sudo tail -200 /data/log/CubeVmm/vmm.log
```

### `journalctl` startup logs

journalctl captures **stdout/stderr from when systemd starts the process until it stabilizes (or exits)**, useful for:

- Startup failure exit codes / error messages
- Output from `ExecStart` / `ExecStartPost` / `ExecStop` hooks
- `docker pull` / `docker build` / `apk update` failures
- Auto-restart counter and reasons

```bash
# Last 200 lines
sudo journalctl -u cube-sandbox-cubelet.service -n 200 --no-pager

# Live tail
sudo journalctl -u cube-sandbox-cubemaster.service -f

# Everything since the last boot
sudo journalctl -u cube-sandbox-cube-api.service -b
```

::: warning No business request logs in journalctl
Once a process is stable, its stdout/stderr volume is tiny because each component writes business logs straight to `/data/log/<Module>/`. To find "which sandboxes were created in the last hour", **journalctl is the wrong place** — go to `/data/log/CubeMaster/cubemaster-req.log` or `/data/log/Cubelet/Cubelet-req.log`.
:::

### `cube-proxy` in-container logs

`cube-proxy` is an OpenResty/nginx container. Its access/error logs are inside the container at `/data/log/cube-proxy/`, **not** on the host filesystem. Use `docker exec`:

```bash
sudo docker exec cube-proxy tail -200 /data/log/cube-proxy/error.log
sudo docker exec cube-proxy tail -200 /data/log/cube-proxy/access.log
```

The container name is fixed to `cube-proxy` (created by systemd via `docker create`).

### One-shot diagnostic bundle

Use the bundled diagnostic collector when sharing logs with the community or filing issues:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh
```

It collects everything into `cube-diag-<timestamp>/`:

- Tails of `/data/log/CubeMaster|Cubelet|CubeAPI|CubeShim|CubeVmm|network-agent/`
- `cube-proxy` container's `error.log` / `access.log`
- `dmesg` / process list / ports / mounts / cgroup / cpuinfo
- Major config files (with secrets redacted)

Pack and share:

```bash
tar czf cube-diag-<ts>.tar.gz cube-diag-<ts>/
```

Selective collection — e.g. only cubelet + dmesg:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh \
  --module cubelet --module dmesg --lines 500
```

See `--help` for full options.

## Operations cheat-sheet

| Goal | Command |
|---|---|
| List all cube services on this node | `systemctl --no-legend list-units 'cube-sandbox-*'` |
| Status of one service | `systemctl status cube-sandbox-<service>.service` |
| Dependency tree of a target | `systemctl list-dependencies cube-sandbox-control.target` |
| Start / stop / restart one service | `systemctl {start\|stop\|restart} cube-sandbox-<service>.service` |
| Start / stop / restart the whole stack | `systemctl {start\|stop\|restart} cube-sandbox-{control,compute}.target` |
| Why did the service fail | `journalctl -u cube-sandbox-<service>.service -n 200 --no-pager` |
| Live tail startup output | `journalctl -u cube-sandbox-<service>.service -f` |
| Reset failed counter | `systemctl reset-failed cube-sandbox-<service>.service` |
| Run health check | `sudo /root/cube-sandbox-one-click-*/smoke.sh`<br>or `sudo /usr/local/services/cubetoolbox/scripts/one-click/quickcheck.sh` |
| Full shutdown | `sudo /root/cube-sandbox-one-click-*/down.sh` |
| Collect diagnostic bundle | `sudo /usr/local/services/cubetoolbox/scripts/cube-diag/collect-logs.sh` |

## Typical troubleshooting flows

### Sandbox creation fails / times out

Walk through the layers in order:

1. **Is the role target active?**

   ```bash
   sudo systemctl status cube-sandbox-control.target
   ```

2. **Run the health check**

   ```bash
   sudo /root/cube-sandbox-one-click-*/smoke.sh
   ```

3. **Did CubeAPI receive the request?**

   ```bash
   sudo tail -F /data/log/CubeAPI/cube-api-$(date +%F).log
   ```

4. **CubeMaster scheduling chain**

   ```bash
   sudo tail -F /data/log/CubeMaster/cubemaster-req.log
   ```

5. **Is the node (Cubelet) online and being scheduled?**

   ```bash
   curl http://127.0.0.1:8089/internal/meta/nodes
   sudo tail -F /data/log/Cubelet/Cubelet-req.log
   ```

6. **VMM startup errors**

   ```bash
   sudo tail -200 /data/log/CubeVmm/vmm.log
   ```

### A service is stuck in `activating (start-post)`

```bash
sudo systemctl status cube-sandbox-<service>.service
sudo journalctl -u cube-sandbox-<service>.service -n 200 --no-pager
```

Common root causes:

- Container build needs the network (e.g. `cube-proxy`'s `apk update`) and the upstream mirror is flaky — see [Deployment Troubleshooting](./troubleshooting/deployment.md)
- `ExecStartPost` health probe timeout (port already in use, upstream not yet ready)
- For `cube-sandbox-cube-proxy.service`, `CUBE_PROXY_HTTP_PORT` is the actual nginx HTTP proxy listener used by the post-start TCP check. `CUBE_PROXY_HOST_PORT` is deprecated and ignored; set `CUBE_PROXY_HTTP_PORT` instead if you need a non-default check port.
- `/data/log` or `/data/cubelet` missing / wrong permissions / XFS not mounted

### Dashboard / API unreachable

```bash
# WebUI container
sudo systemctl status cube-sandbox-webui.service
sudo ss -lntp 'sport = :12088'

# CubeAPI listener
sudo systemctl status cube-sandbox-cube-api.service
sudo ss -lntp 'sport = :3000'
```

## Appendix

### Path quick-reference

| Use | Path |
|---|---|
| Install root | `/usr/local/services/cubetoolbox/` |
| Runtime env file | `/usr/local/services/cubetoolbox/.one-click.env` |
| systemd unit install dir | `/etc/systemd/system/cube-sandbox-*` |
| systemd helper scripts | `/usr/local/services/cubetoolbox/scripts/systemd/*.sh` |
| **Runtime logs (primary)** | **`/data/log/<Module>/`** |
| Cubelet container layer (XFS) | `/data/cubelet/` |
| Sandbox images / snapshots | `/data/cube-shim/disks/`, `/data/snapshot_pack/disks/` |
| systemd PID files | `/run/cube-sandbox-systemd/` |

### Role / service matrix

| Service | `control` node | `compute` node |
|---|:-:|:-:|
| `mysql` / `redis` | ✅ | — |
| `cubemaster` | ✅ | — |
| `cube-api` | ✅ | — |
| `webui` | ✅ | — |
| `cube-proxy` / `coredns` / `dns` | ✅ | — |
| `network-agent` | ✅ | ✅ |
| `cubelet` | ✅ | ✅ |

### See also

- [Quick Start](./quickstart.md) — installation entry point
- [Multi-Node Cluster](./multi-node-deploy.md) — service subset on compute nodes
- [Deployment Troubleshooting](./troubleshooting/deployment.md) — XFS, CIDR conflicts, etc.
- [Templates Troubleshooting](./troubleshooting/templates.md) — template-build issues
