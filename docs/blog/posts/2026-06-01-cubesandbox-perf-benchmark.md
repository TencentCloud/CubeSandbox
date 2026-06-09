---
title: "CubeSandbox Core Operations Performance Benchmark Report"
date: 2026-06-01
author: coolli
description: Performance benchmark data for CubeSandbox on a real bare-metal node, covering sandbox creation from template (cold-start latency, concurrency scaling, single-host density) and Snapshot operations (Snapshot creation, create-from-snapshot, Rollback, Clone). Each section includes the exact commands needed to reproduce the results.
featured: false
weight: 0
---

# CubeSandbox Core Operations Performance Benchmark Report

## 1. Overview

CubeSandbox is designed for AI Agent code execution, where ultra-fast cold-start and high concurrency are the two most critical metrics. This post presents performance benchmark data measured on a real bare-metal node, split into two parts:

- **Chapter 3: Create sandbox from Template** — cold-start latency, concurrency scaling, single-host deployment density
- **Chapter 4: Snapshot operations** — Snapshot creation, create-from-snapshot, Rollback, Clone

Every section includes the exact commands needed to reproduce the results on your own hardware.

**Important: all benchmark numbers are highly dependent on the test environment and workload.** Contributing factors include (but are not limited to) host CPU, memory, IO performance, and sandbox internal workload (e.g. the more complex the program running inside the sandbox and the more dirty pages generated, the longer snapshot creation takes). Please evaluate against your own hardware and workload when planning deployments.

---

## 2. Test Environment

### 2.1 Hardware

| Item | Detail |
|------|--------|
| Machine | Tencent Cloud [Memory-optimized Bare Metal Instance BMI5](https://cloud.tencent.com/document/product/386/63404) (available for purchase from the Tencent Cloud console) |
| OS | OpenCloudOS (TencentOS Server 4) kernel 6.6.119 x86_64 |
| CPU Model | Intel(R) Xeon(R) Platinum 8255C @ 2.50GHz |
| CPU Config | 2 Socket × 24 Core × 2 Thread = **96 logical cores** |
| NUMA Nodes | 2 (node0: 0-23,48-71 / node1: 24-47,72-95) |
| Total Memory | **375 GiB** DDR4-2933 MT/s ECC |
| Data Disk | `/dev/nvme0n1` 3.84 TB Intel SSDPE2KX040T8 NVMe SSD, formatted as XFS, mounted at `/data` |

> **BMI5** (Memory-optimized Bare Metal Instance 5) is a Tencent Cloud bare-metal instance series designed for large-memory, high-density deployment scenarios. It provides physical-machine-level isolation and performance with zero virtualization overhead, supports nested virtualization, and is particularly well-suited for running large numbers of lightweight VMs like CubeSandbox. To reproduce the tests in this post, visit the [Tencent Cloud Bare Metal purchase page](https://buy.cloud.tencent.com/bm) to select the same model.

> To install CubeSandbox, refer to the [Bare Metal Deployment Guide](../../guide/bare-metal-deploy.md).

### 2.2 Sandbox Spec and Template Creation

All tests use sandboxes with the following spec:

| Item | Detail |
|------|--------|
| Spec | 2 vCPU / 2 GiB memory |
| Test Image | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| Storage | CoW reflink (XFS, `/data/cubelet/storage/`) |
| Memory Tracking | soft-dirty (`/proc/PID/clear_refs`) |

Build the template before running any tests (use `cn` registry in China, `int` elsewhere):

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

After the build finishes, note the template ID:

```bash
# List templates and grab the first tpl- prefixed ID
cubemastercli tpl list
```

### 2.3 Metric Definitions

| Metric | Meaning |
|--------|---------|
| **avg** | Mean across all rounds |
| **min** | Minimum observed |
| **p95** | 95th percentile (95% of requests complete within this time) |
| **max** | Maximum observed |
| **wall** | End-to-end elapsed time for the entire batch (first request sent → last one done); used in concurrency scenarios |
| **per** | Amortized per-operation time (wall ÷ number of operations in the batch); used in concurrency scenarios |

All times are in **milliseconds (ms)**. A **warm-up** round is run before each scenario (results discarded) to eliminate page-cache cold-read noise. Concurrent test rounds run serially — no cross-round concurrency — to avoid mutual interference.

---

## 3. Create Sandbox from Template

This chapter measures the end-to-end time to start a ready-to-use sandbox — calling `POST /sandboxes` (with `template_id`) until the sandbox reaches `running`. This is the most common usage pattern.

### 3.1 Setup and Verification

**Step 1: Install the Python SDK and set environment variables**

```bash
pip install e2b-code-interpreter

export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000           # any non-empty string for local deploys
export CUBE_TEMPLATE_ID=<your-template-id>  # from cubemastercli tpl list
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem  # mkcert certificate path
```

**Step 2: Run a Hello World to verify the environment**

Before running any benchmarks, run the following script to confirm sandboxes can be created and execute code:

```python
import os
from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox, safely isolated!')")
    print(result)
    print("✅ Environment verification passed — ready for benchmarking")
```

Save as `hello.py` and run:

```bash
python hello.py
```

If you see `✅ Environment verification passed`, CubeSandbox is deployed correctly and you can proceed. If it errors, refer to the [Quick Start](../../guide/quickstart.md) to troubleshoot.

### 3.2 Cold-Start Latency and Concurrency Scaling

Use the [`cube-bench`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench) tool to measure sandbox creation latency at different concurrency levels. `cube-bench` drives CubeAPI via Go goroutines and reports full percentile statistics.

**Build (requires Go 1.21+):**

```bash
cd examples/cube-bench
make
# output: ./bin/cube-bench
```

**Run (examples for concurrency 1 and 50):**

```bash
# Set environment variables
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>

# 1-concurrent, 20 total, create-only (keep sandboxes alive)
./bin/cube-bench -c 1 -n 20 -w 3 -m create-only

# 50-concurrent, 500 total
./bin/cube-bench -c 50 -n 500 -w 3 -m create-only

# Export JSON report
./bin/cube-bench -c 50 -n 500 -w 3 -m create-only -o report_c50.json
```

> `-w 3` runs 3 warm-up rounds whose results are discarded before measurement.

**Results (Tencent Cloud BMI5 bare metal, 2 vCPU / 2 GiB sandbox):**

| Concurrency | Requests | avg | min | p95 | max | Per-sandbox amortized | Throughput |
|:----:|:------:|----:|----:|----:|----:|----------:|-----:|
| 1    | 20  | 47.8 ms | 43.5 ms | 57.4 ms  | 60.4 ms  | 55.8 ms | 17.9 /s |
| 10   | 200 | 88.7 ms | 45.8 ms | 116.9 ms | 119.1 ms | 9.9 ms  | 101.4 /s |
| 20   | 300 | 98.1 ms | 47.7 ms | 175.8 ms | 232.6 ms | 5.5 ms  | 180.9 /s |
| 50   | 500 | 276.1 ms | 60.6 ms | 508.4 ms | 681.3 ms | 6.8 ms  | 147.6 /s |

> "Per-sandbox amortized" = wall time for that tier ÷ request count; "Throughput" is the corresponding sandboxes created per second. Each tier is tested independently — all sandboxes are cleaned up and the resource pool is given time to recover between tiers to avoid interference. **100% success rate** across all tiers.

**Key findings:**
- Serial creation latency ~**48 ms** (min 43.5 / p95 57.4), consistently sub-100 ms
- Concurrency yields significant throughput gains: at 20-concurrent, throughput reaches **180.9 /s**, per-sandbox amortized drops from 55.8 ms (1-concurrent) to 5.5 ms
- At 50-concurrent, per-request latency increases with queue depth (p95 508 ms, max 681 ms), but throughput remains at 147.6 /s; for the best latency-throughput tradeoff, 20-concurrent is the throughput sweet spot for this machine

### 3.3 Single-Host Deployment Density (Memory Overhead)

CubeSandbox uses kernel sharing and Copy-on-Write (CoW) to compress its per-instance overhead to extremely low levels. This section measures net per-instance cost by "clearing the machine → launching sandboxes in batches → recording memory changes."

> ⚠️⚠️⚠️ **Important Safety Warning**
>
> **Before each batch, always run `free -h` to confirm sufficient remaining memory. Launch only a small batch at a time, observe memory after each batch, and only proceed when safe — never launch too many at once!** Running out of memory triggers OOM Killer, which at minimum kills processes and at worst corrupts the running environment, requiring redeployment. Decide batch sizes based on your machine's actual available memory.

**Step 1: Record the baseline (empty machine memory)**

```bash
# Set environment variables (same as §3.2; re-export if you open a new terminal)
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000              # any non-empty string for local deploys
export CUBE_TEMPLATE_ID=<your-template-id> # from cubemastercli tpl list

# Ensure no leftover sandboxes (SANDBOX_COUNT should be 0)
cubemastercli list

# Record empty-machine memory usage
free -h
# Also record shim process count (should be 0)
ps --no-headers -C containerd-shim-cube-rs | wc -l
```

**Step 2: Launch sandboxes in batches, record memory with `free -h` after each batch**

Use `cube-bench` in `create-only` mode to create sandboxes and keep them alive. Based on available memory, accumulate to 100 / 300 / 500 / 1000 (adjust to your environment):

```bash
# Set environment variables (same as §3.2; re-export if you open a new terminal)
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000              # any non-empty string for local deploys
export CUBE_TEMPLATE_ID=<your-template-id> # from cubemastercli tpl list

# Launch to 100
./bin/cube-bench -c 50 -n 100 -m create-only
free -h   # record current memory usage

# Launch 200 more (total 300)
./bin/cube-bench -c 50 -n 200 -m create-only
free -h

# Launch 200 more (total 500)
./bin/cube-bench -c 50 -n 200 -m create-only
free -h

# If memory permits, launch 500 more (total 1000)
./bin/cube-bench -c 50 -n 500 -m create-only
free -h
```

**Step 3: Calculate per-instance overhead**

Simply divide the memory increase by the number of sandboxes:

```
Per-VM amortized overhead = (current used - baseline used) ÷ VM count
```

**Results (Tencent Cloud BMI5, 2 vCPU / 2 GiB sandbox, create-only cumulative):**

| Live Sandboxes | System Available Memory (free available) | Per-VM Amortized Overhead |
|:---------:|:----------------------------:|:-------------:|
| 0 (baseline)  | 359.5 GiB                    | — |
| 100        | 357.4 GiB                    | **~21.5 MB** |
| 300        | 352.5 GiB                    | **~23.8 MB** |
| 500        | 347.3 GiB                    | **~25.0 MB** |
| 1000       | 334.3 GiB                    | **~25.7 MB** |

> All 1000 sandboxes created successfully with zero rollbacks. Per-VM amortized overhead converges at ~25 MB (slowly increasing from 21 MB to 26 MB with density) — always in the tens-of-MB range. This is the effect of CoW + kernel page sharing: a 2 GiB sandbox does **not** pre-allocate the full 2 GiB at idle — memory is allocated on-demand only when actually written. 1000 sandboxes consume only ~25 GiB, leaving ample headroom from the 375 GiB total.

**Scaling up the TAP pre-allocation pool:**

The TAP pool target is specified by `tap_init_num` in the cubelet configuration (default **500**). However, this parameter is actually consumed by **network-agent** (which reads the same cubelet config via `--cubelet-config` at startup) to pre-create TAP devices. After modifying it, you need to restart **network-agent** (not cubelet) for the change to take effect. The target density must not exceed this value — for example, to benchmark 1000 sandboxes, first set `tap_init_num` to 1000 (or higher).

```bash
# 1. Edit cubelet config, increase tap_init_num under [plugins."io.cubelet.internal.v1.network"]
vi /usr/local/services/cubetoolbox/Cubelet/config/config.toml
#   [plugins."io.cubelet.internal.v1.network"]
#     tap_init_num = 1000        # default 500; set to 1000+ for benchmarking 1000 sandboxes

# 2. Restart network-agent to pre-create TAP devices with the new target
systemctl restart cube-sandbox-network-agent.service
```

**Estimated single-host capacity (BMI5, 375 GiB memory):**

```
Full-load scenario (each sandbox writes full 2 GiB): 375 GiB ÷ (2 GiB + ~25 MB) ≈ 185 sandboxes
Idle/light-load scenario (CoW on-demand allocation): dominated by ~25 MB amortized overhead, can reach thousands; tested stable at 1000 consuming only ~25 GiB
```


---

## 4. Snapshot Operations

Snapshot is a core CubeSandbox feature, supporting memory + filesystem snapshots on running sandboxes that can be restored near-instantly (Clone / Rollback).

**Install dependencies:**

```bash
cd examples/snapshot-rollback-clone
pip install -r requirements.txt   # installs the cubesandbox SDK

# The following environment variables are prerequisites for all 4.x benchmark scripts;
# export in each new shell (or write to env.sh and source it)
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>          # from cubemastercli tpl list
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>       # use 127.0.0.1 when running on the CubeProxy host
export CUBE_PROXY_PORT_HTTP=80                      # CubeProxy listen port (default 80)
```

> Sections 4.1–4.5 below assume you have completed the above `export` in your current shell (scripts read these variables via `env.py`). Re-export if you open a new terminal.

### 4.1 Snapshot Creation vs Concurrency

**How it works:** calls `POST /sandboxes/{id}/snapshots` on a running sandbox. N concurrent requests target N independent sandboxes simultaneously, measuring wall time until all snapshots complete.

> CubeSandbox serializes snapshot requests on a **single sandbox** internally, so the concurrency test targets N distinct sandboxes (one snapshot request per sandbox), and the actual success count equals the concurrency.

**Run:** (script: [`bench_snapshot_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
# If this is a new terminal, set environment variables (same as the dependency installation section):
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# The script provides single-tier mechanism; control concurrency tiers from the command line:
python bench_snapshot_concurrency.py -c 1  -n 5
python bench_snapshot_concurrency.py -c 5  -n 5 --no-header
python bench_snapshot_concurrency.py -c 10 -n 5 --no-header
```

**Results** (fresh sandboxes snapshotted as-is; measured dirty pages ~**7 MB**, confirmed by `PagemapAnon snapshot saved` in `/data/log/CubeVmm/vmm.log`; this is the sandbox baseline anonymous memory page size and is not a variable in this section):

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-snapshot avg |
|:----:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 5    | 49.8 ms  | 47.3 ms  | 54.1 ms  | 54.1 ms  | 49.8 ms          |
| 5    | 5    | 71.0 ms  | 62.7 ms  | 81.0 ms  | 81.0 ms  | **14.2 ms**      |
| 10   | 5    | 127.2 ms | 79.6 ms  | 155.6 ms | 155.6 ms | **12.7 ms**      |

Serial snapshot ~**50 ms**; at 5-concurrent, batch wall ~**71 ms**, per-snapshot amortized drops to ~**14 ms**; at 10-concurrent, batch wall ~**127 ms**, amortized further drops to ~**13 ms** — significant concurrency amortization.

### 4.2 Snapshot Creation vs Dirty Page Size

**Background:** CubeSandbox uses the soft-dirty mechanism to save only memory pages modified since the last snapshot. Actual write volume = dirty page count × 4 KiB, typically far less than total sandbox memory (2 GiB).

The test precisely controls dirty page size by pre-writing data to `/dev/shm` (tmpfs). The "Dirty Page" column shows actual bytes written as read from `/data/log/CubeVmm/vmm.log` — it differs from the theoretical write size due to Guest OS background activity.

**Run:** (script: [`bench_snapshot_dirty.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_dirty.py))

```bash
cd examples/snapshot-rollback-clone
# If this is a new terminal, set environment variables (same as the dependency installation section):
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# The script provides single-tier mechanism; control dirty page size from the command line (-d = write size in MB):
python bench_snapshot_dirty.py -d 0    -n 3
python bench_snapshot_dirty.py -d 10   -n 3 --no-header
python bench_snapshot_dirty.py -d 50   -n 3 --no-header
python bench_snapshot_dirty.py -d 100  -n 3 --no-header
python bench_snapshot_dirty.py -d 200  -n 3 --no-header
python bench_snapshot_dirty.py -d 500  -n 3 --no-header
python bench_snapshot_dirty.py -d 800  -n 3 --no-header
python bench_snapshot_dirty.py -d 1024 -n 3 --no-header
```

> Tests run in serial mode; one warm-up is discarded before each data point, then 3 measured runs are averaged. The "create sandbox avg" column shows the time to create a new sandbox from that snapshot, reflecting whether dirty page size affects restore speed.

**Results:**

| Write Size | Dirty Page | snapshot avg | snapshot min | snapshot p95 | snapshot max | create sandbox avg | create sandbox min | create sandbox p95 | create sandbox max |
|:------:|----------:|------------:|------------:|------------:|------------:|------------------:|------------------:|------------------:|------------------:|
| 0 MB   | 7.1 MB    | 45.7 ms      | 43.9 ms      | 47.4 ms      | 47.4 ms      | 64.8 ms            | 61.5 ms            | 68.6 ms            | 68.6 ms            |
| 10 MB  | 38.9 MB   | 75.7 ms      | 73.2 ms      | 79.2 ms      | 79.2 ms      | 60.7 ms            | 57.3 ms            | 66.1 ms            | 66.1 ms            |
| 50 MB  | 120.7 MB  | 107.7 ms     | 104.8 ms     | 112.3 ms     | 112.3 ms     | 64.4 ms            | 60.4 ms            | 70.6 ms            | 70.6 ms            |
| 100 MB | 195.0 MB  | 138.6 ms     | 136.7 ms     | 139.9 ms     | 139.9 ms     | 66.5 ms            | 60.9 ms            | 71.1 ms            | 71.1 ms            |
| 200 MB | 296.7 MB  | 174.2 ms     | 173.1 ms     | 176.2 ms     | 176.2 ms     | 63.7 ms            | 60.6 ms            | 66.8 ms            | 66.8 ms            |
| 500 MB | 602.5 MB  | 289.4 ms     | 285.0 ms     | 293.1 ms     | 293.1 ms     | 64.0 ms            | 61.6 ms            | 66.5 ms            | 66.5 ms            |
| 800 MB | 908.4 MB  | 392.8 ms     | 392.1 ms     | 394.1 ms     | 394.1 ms     | 60.9 ms            | 54.6 ms            | 65.8 ms            | 65.8 ms            |
| 1024 MB | 1136.4 MB | 486.9 ms    | 471.5 ms     | 510.8 ms     | 510.8 ms     | 68.4 ms            | 58.9 ms            | 84.6 ms            | 84.6 ms            |

**Key findings:**
- **Snapshot creation time scales near-linearly with dirty page size**: baseline (7 MB dirty) ~47 ms, +~40 ms per 100 MB of additional dirty data, ~487 ms at 1024 MB
- **Create-from-snapshot time is independent of dirty page size**: stable at **60–85 ms** regardless of snapshot size, because restore uses CoW (copy-on-write) on-demand loading and does not depend on dirty page size

### 4.3 Create Sandbox from Snapshot

**How it works:** creates a snapshot first, then launches N sandboxes concurrently via `POST /sandboxes` (with `snapshot_id`), measuring end-to-end wall time until all sandboxes reach `running`.

**Run:** (script: [`bench_create_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_create_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
# If this is a new terminal, set environment variables (same as the dependency installation section):
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# The script provides single-tier mechanism; control concurrency from the command line:
python bench_create_concurrency.py -c 1  -n 3
python bench_create_concurrency.py -c 10 -n 3 --no-header
python bench_create_concurrency.py -c 20 -n 3 --no-header
python bench_create_concurrency.py -c 50 -n 3 --no-header
```

**Results:**

| Concurrency | n total | Rounds | wall avg | wall min | wall p95 | wall max | per-sandbox avg |
|:----:|:------:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 1      | 3    | 63.9 ms  | 62.5 ms  | 66.1 ms  | 66.1 ms  | 63.9 ms          |
| 10   | 10     | 3    | 89.9 ms  | 84.0 ms  | 93.6 ms  | 93.6 ms  | **9.0 ms**       |
| 20   | 20     | 3    | 118.9 ms | 92.7 ms  | 167.1 ms | 167.1 ms | **5.9 ms**       |
| 50   | 50     | 3    | 180.3 ms | 135.1 ms | 260.7 ms | 260.7 ms | **3.6 ms**       |

Single sandbox startup ~**64 ms**; at 20-concurrent, wall ~**119 ms**, amortized just **5.9 ms/sandbox**; at 50-concurrent, wall ~**180 ms**, amortized just **3.6 ms/sandbox** — demonstrating excellent concurrency scaling.

### 4.4 Rollback

**How it works:** calls `POST /sandboxes/{id}/rollback` on running sandboxes to restore memory and filesystem state in-place to the specified Snapshot, without recreating the sandbox.

> **Snapshot ownership constraint:** CubeSandbox only allows a sandbox to roll back to a checkpoint **it created itself**. Therefore each concurrent sandbox independently `create_snapshot()` to set a restore point, then `rollback()` to its own checkpoint; afterwards each sandbox's snapshot is deleted.

**Run:** (script: [`bench_rollback_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_rollback_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
# If this is a new terminal, set environment variables (same as the dependency installation section):
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# The script provides single-tier mechanism; control concurrency from the command line:
python bench_rollback_concurrency.py -c 1  -n 5
python bench_rollback_concurrency.py -c 5  -n 5 --no-header
python bench_rollback_concurrency.py -c 10 -n 5 --no-header
```

**Results:**

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-rollback avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-----------------:|
| 1    | 5    | 81.6 ms  | 74.7 ms  | 97.4 ms  | 97.4 ms  | 81.6 ms          |
| 5    | 5    | 189.6 ms | 161.8 ms | 243.2 ms | 243.2 ms | **37.9 ms**      |
| 10   | 5    | 266.1 ms | 236.1 ms | 305.1 ms | 305.1 ms | **26.6 ms**      |

> Single Rollback flow (`create_snapshot` → `rollback` to own checkpoint) ~**82 ms**; at 5-concurrent, batch wall ~**190 ms**, per-rollback amortized drops to ~**38 ms**; at 10-concurrent, batch wall ~**266 ms**, amortized drops to ~**27 ms/rollback**.
>
> Note: Because CubeSandbox requires sandboxes to roll back only to their own checkpoints, shared snapshots cannot be reused — each concurrent sandbox must independently complete the full "snapshot + rollback" flow.

### 4.5 Clone

**How it works:** calls `POST /sandboxes/{id}/clone` to fork N new sandboxes from a **running** source sandbox, fully preserving the source's memory and filesystem state.

> **Note:** disk files in this test were already in Page Cache; results exclude cold-read IO overhead.

**Run:** (script: [`bench_clone_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_clone_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
# If this is a new terminal, set environment variables (same as the dependency installation section):
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# The script provides single-scenario mechanism; control n/concurrency/rounds from the command line:
python bench_clone_concurrency.py -n 1   -c 1  --rounds 5
python bench_clone_concurrency.py -n 100 -c 10 --rounds 2 --no-header
python bench_clone_concurrency.py -n 100 -c 20 --rounds 2 --no-header
python bench_clone_concurrency.py -n 100 -c 50 --rounds 2 --no-header
```

**Results:**

| Scenario               | n   | Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-clone avg |
|:------------------|:---:|:----:|:----:|--------:|--------:|--------:|--------:|--------------:|
| 1 sandbox, 1-concurrent    | 1   | 1    | 5    | 219.6 ms | 213.6 ms | 234.7 ms  | 234.7 ms  | 219.6 ms       |
| 100 sandboxes, 10-concurrent   | 100 | 10   | 2    | 870.4 ms | 860.6 ms | 880.2 ms  | 880.2 ms  | **8.7 ms**    |
| 100 sandboxes, 20-concurrent   | 100 | 20   | 2    | 638.6 ms | 620.8 ms | 656.3 ms  | 656.3 ms  | **6.4 ms**     |
| 100 sandboxes, 50-concurrent   | 100 | 50   | 2    | 540.9 ms | 491.3 ms | 590.5 ms  | 590.5 ms  | **5.4 ms**     |

Clone (full memory + filesystem state): single clone ~**220 ms**; for 100 sandboxes, increasing concurrency significantly reduces batch wall time (10-concurrent ~870 ms → 50-concurrent ~541 ms), per-clone amortized drops from **8.7 ms** to **5.4 ms/sandbox**.

> **Counter-intuitive concurrency behavior:** 10-concurrent batch wall (870 ms) is actually **slower than** 20 / 50-concurrent (639 / 541 ms). This is because with a fixed n=100, 10-concurrent requires serializing 10 batches with accumulated scheduling overhead; at higher concurrency, fewer batches are needed and the source sandbox's memory pages are more effectively reused from Page Cache, making the overall operation faster. This shows that at this scale, Clone is not bottlenecked by concurrency — increasing concurrency is beneficial.

### 4.6 Pause / Resume

**How it works:** Creates `concurrency` sandboxes, pauses all of them concurrently via `POST /sandboxes/{id}/pause`, then resumes all concurrently via `POST /sandboxes/{id}/resume`. Records wall time and per-sandbox amortized latency for both operations.

> ⚠️ **Current implementation note:** Pause currently uses **full-memory-copy mode** — on pause, all anonymous memory pages of the sandbox are written to persistent storage. Latency scales linearly with sandbox memory size (~558 ms per sandbox at 2 GiB). A future release will upgrade to **soft-dirty incremental mode**, which only saves pages dirtied since the last checkpoint. For an idle sandbox this is expected to reduce pause latency by **80–90%** — down to ~**60 ms**, on par with Snapshot creation.

**Run:** (script: [`bench_pause_resume_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_pause_resume_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
# If this is a new terminal, set environment variables (same as the dependency installation section):
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# The script provides single-tier mechanism; control concurrency from the command line:
python bench_pause_resume_concurrency.py -c 1  -n 5
python bench_pause_resume_concurrency.py -c 5  -n 5 --no-header
python bench_pause_resume_concurrency.py -c 10 -n 5 --no-header
```

**Pause results:**

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-pause avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-------------:|
| 1    | 5    | 558.4 ms | 530.8 ms | 590.3 ms | 590.3 ms | 558.4 ms |
| 5    | 5    | 656.9 ms | 621.9 ms | 683.2 ms | 683.2 ms | **131.4 ms** |
| 10   | 5    | 682.1 ms | 674.1 ms | 699.3 ms | 699.3 ms | **68.2 ms** |

**Resume results:**

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-resume avg |
|:----:|:----:|--------:|--------:|--------:|--------:|---------------:|
| 1    | 5    | 41.8 ms  | 18.7 ms  | 65.1 ms  | 65.1 ms  | 41.8 ms |
| 5    | 5    | 28.2 ms  | 17.6 ms  | 34.2 ms  | 34.2 ms  | **5.6 ms** |
| 10   | 5    | 35.7 ms  | 30.6 ms  | 41.7 ms  | 41.7 ms  | **3.6 ms** |

**Key findings:**
- **Resume is extremely fast with excellent concurrency scaling:** single resume ~42 ms; at 10-concurrent, per-resume amortized just **3.6 ms/sandbox**
- **Pause scales well concurrently:** in full-copy mode, wall time grows only moderately as concurrency increases (1-concurrent 558 ms → 10-concurrent 682 ms), per-pause amortized drops from 558 ms to **68 ms/sandbox** — reflecting good IO parallelism on bare-metal NVMe
- **After soft-dirty mode lands:** pause latency is expected to drop to ~60 ms (on par with Snapshot creation), pushing per-pause at 10-concurrent into single-digit milliseconds

> **full-copy → soft-dirty optimization:** The current full-copy mode writes up to 2 GiB of VM anonymous memory to disk on every pause, creating high IO pressure. The soft-dirty incremental mode tracks dirty pages via `/proc/PID/clear_refs` since the last checkpoint; pause only writes actually modified pages (typically a few MB for an idle sandbox), reducing pause latency by **80–90%** and significantly increasing high-concurrency throughput.

---

## Appendix: Benchmark Script Index

All benchmark scripts used in this post are located in the repository directories:

- **[`examples/cube-bench/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench)** — Template-based concurrent creation benchmark tool (Go)
- **[`examples/snapshot-rollback-clone/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone)** — Snapshot / Rollback / Clone / Pause-Resume Python scripts
