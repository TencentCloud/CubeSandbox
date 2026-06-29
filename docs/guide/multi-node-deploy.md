# Multi-Node Cluster Deployment

This guide explains how to expand a single-node Cube Sandbox deployment into a multi-node cluster by adding **compute nodes**. Compute nodes run only the sandbox runtime components (`Cubelet`, `network-agent`, `CubeShim`) and register themselves to the control plane on the first machine.

::: warning Production Use
If you plan to use Cube Sandbox in a production environment, please refer to the [Network Hardening](./network-hardening.md) guide to secure your deployment before exposing services to untrusted networks.
:::

::: tip Prerequisite
You must have a working control node deployed via the [Self-Build Deployment Guide](./self-build-deploy.md) before adding compute nodes.
:::

## Architecture Overview

```
┌─────────────────────────────────────────┐
│           Control Node                  │
│  CubeMaster, cube-api, CubeProxy,       │
│  CoreDNS, MySQL, Redis,                 │
│  Cubelet, network-agent                 │
└──────────────────┬──────────────────────┘
                   │  /internal/meta API
       ┌───────────┼───────────┐
       ▼           ▼           ▼
┌────────────┐┌────────────┐┌────────────┐
│ Compute #1 ││ Compute #2 ││ Compute #N │
│ Cubelet    ││ Cubelet    ││ Cubelet    │
│ net-agent  ││ net-agent  ││ net-agent  │
└────────────┘└────────────┘└────────────┘
```

- The **control node** runs the full stack: orchestration (CubeMaster), API gateway (cube-api), proxy (CubeProxy + CoreDNS), databases (MySQL + Redis), and also acts as a compute node itself.
- Each **compute node** runs only `Cubelet` and `network-agent`. It registers to the control-plane `CubeMaster` and receives sandbox scheduling requests.

## Prerequisites

Each compute node must meet the same hardware and software requirements as the control node:

- **Physical machine or bare-metal server** (nested virtualization is not supported)
- **x86_64** architecture with **KVM enabled** (`ls /dev/kvm`)
- **Docker** installed and running
- **Network connectivity** to the control node (specifically to `CubeMaster` on port `8089` by default)

For the full requirements list, see [Self-Build Deployment — Prerequisites](./self-build-deploy.md#prerequisites).

## Step 1: Prepare the Release Bundle

Use the **same release bundle** that was built for the control node. Copy it to the compute node and extract:

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
```

## Step 2: Configure Environment Variables

```bash
cp env.example .env
```

Edit `.env` and set the following variables:

```bash
ONE_CLICK_DEPLOY_ROLE=compute
CUBE_SANDBOX_NODE_IP=<current-node-ip>
ONE_CLICK_CONTROL_PLANE_IP=<control-plane-ip>
```

| Variable | Description |
|----------|-------------|
| `ONE_CLICK_DEPLOY_ROLE` | Must be set to `compute` for compute-only nodes |
| `CUBE_SANDBOX_NODE_IP` | This node's primary network interface IP |
| `ONE_CLICK_CONTROL_PLANE_IP` | The control node's IP; automatically expanded to `<ip>:8089` for CubeMaster |

You can also specify the CubeMaster endpoint explicitly if it uses a non-default port:

```bash
ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR=<control-plane-ip>:8089
```

`ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` takes precedence over `ONE_CLICK_CONTROL_PLANE_IP` when both are set.

## Step 3: Install

```bash
sudo ./install-compute.sh
```

The compute-node install script will:

1. Install only `Cubelet`, `network-agent`, `cube-shim`, `cube-image`, `cube-kernel-scf`, and the runtime scripts
2. Start only the host processes `network-agent` and `cubelet`
3. Automatically point `Cubelet`'s `meta_server_endpoint` to the control-plane `CubeMaster`
4. Register the node and report status through the control plane `/internal/meta` API

## Verifying the Deployment

### Health Check

```bash
sudo ./smoke.sh
```

In compute-node mode, `quickcheck.sh` verifies:

- Local `network-agent` health
- Reachability of the control-plane `CubeMaster`
- That the current node appears under `/internal/meta/nodes/{node_id}` on the control plane

### Verify from the Control Node

On the control node, you can confirm the compute node has registered:

```bash
curl http://127.0.0.1:8089/internal/meta/nodes
```

The response should include the compute node's IP and a healthy status.

## Configure CubeMaster Scheduler Scoring

For multi-node deployments, configure CubeMaster's `scheduler.score` on the control node. If scoring is omitted, CubeMaster filters eligible nodes and then selects from the filtered node order, which can concentrate new sandboxes on the first eligible node until resource filters push traffic elsewhere.

Merge the following scheduler fields into the existing `scheduler` section of `cubemaster.yaml`. Keep your existing `filter`, timeout, overcommit, and other scheduler settings.

```yaml
scheduler:
  # Keep your existing filter, timeout, overcommit, and other scheduler settings.
  priority_select_num: 3
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 2
      local_create_num: 3
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1.0
        enable_weight_factors:
          - mvm_num
          - local_create_num
          - quota_cpu_usage
          - quota_mem_usage
```

For multi-node clusters, set `scheduler.priority_select_num` to a value greater than `1` so CubeMaster randomly selects from the top scored nodes. The shipped default config uses `priority_select_num: 1`, which means scoring only determines which single node receives the next sandbox. Use `3` as a starting point for small clusters and tune it based on your node count. `scheduler.least_select_name` defaults to `random`, so it usually does not need to be set explicitly.

After updating `cubemaster.yaml`, restart CubeMaster with your normal deployment procedure so the scheduler loads the new scoring configuration.

## Common Operations

### Stop Compute Node Services

```bash
sudo ./down.sh
```

In compute-node mode, this only stops `cubelet` and `network-agent`. It does not affect the control plane or other compute nodes.

### Reinstall

To reinstall a compute node, simply run `install-compute.sh` again. The script automatically stops the existing deployment before installing.

### View Logs

| Component | Log Path |
|-----------|----------|
| Cubelet | `/data/log/Cubelet/` |
| CubeShim | `/data/log/CubeShim/` |
| Hypervisor (VMM) | `/data/log/CubeVmm/` |
| Runtime PID files | `/var/run/cube-sandbox-one-click/` |
| Process stdout/stderr | `/var/log/cube-sandbox-one-click/` |

For control-node log paths, see [Self-Build Deployment — View Logs](./self-build-deploy.md#view-logs).

## Configuration Reference

Compute nodes use the same `.env` file format. The following variables are specific to or particularly relevant for compute-node deployments:

| Variable | Default | Description |
|----------|---------|-------------|
| `ONE_CLICK_DEPLOY_ROLE` | `control` | Must be set to `compute` |
| `ONE_CLICK_CONTROL_PLANE_IP` | empty | Control-plane host IP; expanded to `<ip>:8089` by default |
| `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` | empty | Explicit CubeMaster address; takes precedence over `ONE_CLICK_CONTROL_PLANE_IP` |
| `CUBE_SANDBOX_NODE_IP` | `10.0.0.10` | **Required.** This node's primary network interface IP |
| `CUBE_SANDBOX_NETWORK_CIDR` | `192.168.0.0/18` (from `config.toml`) | cubevs local network CIDR. Should match the control-plane value. IPv4 CIDR format (e.g., `10.100.0.0/18`), mask range /8–/30. Auto-detected for host network conflicts at install time. |
| `CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK` | `0` | Set to `1` to skip CIDR conflict detection (not recommended). |
| `ONE_CLICK_RUN_QUICKCHECK` | `1` | Run health check after installation |

For the full configuration reference (build-time options, database, proxy, etc.), see [Self-Build Deployment — Configuration Reference](./self-build-deploy.md#configuration-reference).

## Troubleshooting

### Compute Node Cannot Reach CubeMaster

Verify network connectivity:

```bash
curl http://<control-plane-ip>:8089/internal/meta/nodes
```

If this fails, check:
- Firewall rules on the control node (port `8089` must be accessible)
- The `ONE_CLICK_CONTROL_PLANE_IP` or `ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR` value in `.env`

### Node Not Appearing in Control Plane

If `smoke.sh` passes locally but the node does not appear on the control plane:

1. Check Cubelet logs: `/data/log/Cubelet/`
2. Verify `meta_server_endpoint` in the Cubelet config points to the correct CubeMaster address
3. Ensure `CUBE_SANDBOX_NODE_IP` is correctly set to a routable IP (not `127.0.0.1`)

For general troubleshooting (Docker, KVM, DNS, etc.), see [Self-Build Deployment — Troubleshooting](./self-build-deploy.md#troubleshooting).
