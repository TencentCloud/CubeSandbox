---
title: "CubeSandbox Core Operations Performance Benchmark Report (PVM Cloud Server)"
date: 2026-06-03
author: coolli
description: Performance benchmark data for CubeSandbox on a Tencent Cloud SA9.4XLARGE32 standard CVM (PVM kernel), covering sandbox creation from template (cold-start latency, concurrency scaling, single-host density) and Snapshot operations (Snapshot creation, create-from-snapshot, Rollback, Clone, Pause/Resume). Each section includes the exact commands needed to reproduce the results.
featured: false
weight: 0
---

# CubeSandbox Core Operations Performance Benchmark Report (PVM Cloud Server)

## 1. Overview

CubeSandbox is designed for AI Agent code execution, where ultra-fast cold-start and high concurrency are the two most critical metrics. This post presents performance benchmark data measured on a Tencent Cloud standard CVM (running a PVM kernel), split into two parts:

- **Chapter 3: Create sandbox from Template** — cold-start latency, concurrency scaling, single-host deployment density
- **Chapter 4: Snapshot operations** — Snapshot creation, create-from-snapshot, Rollback, Clone

Every section includes the exact commands needed to reproduce the results on your own hardware.

**Important: all benchmark numbers are highly dependent on the test environment and workload.** Contributing factors include (but are not limited to) host CPU, memory, IO performance, and sandbox internal workload (e.g. the more complex the program running inside the sandbox and the more dirty pages generated, the longer snapshot creation takes). Please evaluate against your own hardware and workload when planning deployments.

Compared to the [bare-metal benchmark report](./2026-06-01-cubesandbox-perf-benchmark.md), this post uses a standard virtualized CVM (SA9.4XLARGE32) with fewer CPU cores and less memory, and can serve as a reference baseline for small-to-medium scale deployments.

---

## 2. Test Environment

### 2.1 Hardware

| Item | Detail |
|------|--------|
| Machine | Tencent Cloud [Standard CVM SA9.4XLARGE32](https://cloud.tencent.com/document/product/213/11518) (available for purchase from the Tencent Cloud console) |
| Availability Zone | — |
| OS | OpenCloudOS 9.4 |
| Kernel | `6.6.69-cube.pvm.host.005.x` |
| CPU Model | AMD EPYC 9K65 |
| CPU Config | 1 Socket × 16 Core × 1 Thread = **16 logical cores** |
| NUMA Nodes | 1 (node0: 0-15) |
| Total Memory | **32 GiB** |
| System Disk | `/dev/vda` 200 GiB Enhanced SSD cloud disk, formatted as XFS, mounted at `/` |

> **SA9.4XLARGE32** is a Tencent Cloud ninth-generation standard instance powered by AMD EPYC 9K65 processors, suited for general-purpose computing. This post runs a PVM (Parallel Virtual Machine) kernel that supports nested virtualization, enabling CubeSandbox to run on an ordinary cloud server. To reproduce the tests in this post, visit the [Tencent Cloud CVM purchase page](https://buy.cloud.tencent.com/cvm) to select the same model.

> To install CubeSandbox, refer to the [Quick Start Guide](../../guide/quickstart.md).

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

**Run:**

```bash
# Set environment variables
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>

# 1-concurrent, 20 total (create then immediately delete)
./bin/cube-bench -c 1 -n 20 -w 3

# 10-concurrent, 200 total
./bin/cube-bench -c 10 -n 200 -w 3

# 20-concurrent, 300 total
./bin/cube-bench -c 20 -n 300 -w 3
```

> `-w 3` runs 3 warm-up rounds whose results are discarded before measurement.

**Results (Tencent Cloud SA9.4XLARGE32 PVM, 2 vCPU / 2 GiB sandbox):**

| Concurrency | Requests | avg | min | P50 | P90 | P95 | P99 | max |
|:----:|:------:|----:|----:|----:|----:|----:|----:|----:|
| 1    | 20  | 66.7 ms | 55.9 ms | 64.5 ms | 77.5 ms | 78.2 ms | 80.2 ms | 80.2 ms |
| 10   | 200 | 170.9 ms | 85.4 ms | 168.5 ms | 206.4 ms | 216.7 ms | 286.1 ms | 323.5 ms |
| 20   | 300 | 364.6 ms | 116.5 ms | 356.2 ms | 459.0 ms | 521.4 ms | 673.8 ms | 744.0 ms |

> Each tier is tested independently — all sandboxes are cleaned up and the resource pool is given time to recover between tiers to avoid interference. **100% success rate** across all tiers.

**Key findings:**
- Serial creation latency ~**67 ms** (min 55.9 / P95 78.2), extremely low and stable
- At 10-concurrent, avg 171 ms — amortized per-sandbox just **17.1 ms**, showing strong concurrency scaling
- At 20-concurrent, avg 365 ms — amortized per-sandbox **18.2 ms**, P99 674 ms reflects minor tail latency under queue pressure

### 3.3 Single-Host Deployment Density (Memory Overhead)

CubeSandbox uses kernel sharing and Copy-on-Write (CoW) to compress its per-instance overhead to extremely low levels. This section measures net per-instance cost by "clearing the machine → launching sandboxes in batches → recording memory changes."

> ⚠️⚠️⚠️ **Important Safety Warning**
>
> **Before each batch, always run `free -h` to confirm sufficient remaining memory. Launch only a small batch at a time, observe memory after each batch, and only proceed when safe — never launch too many at once!** Running out of memory triggers OOM Killer, which at minimum kills processes and at worst corrupts the running environment, requiring redeployment. Decide batch sizes based on your machine's actual available memory.

**Step 1: Record the baseline (empty machine memory)**

```bash
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>

# Ensure no leftover sandboxes
cubemastercli list

# Record empty-machine memory usage
free -h
# Also record shim process count (should be 0)
ps --no-headers -C containerd-shim-cube-rs | wc -l
```

**Step 2: Launch sandboxes in batches, record memory with `free -h` after each batch**

Use `cube-bench` in `create-only` mode to create sandboxes and keep them alive:

```bash
# Set environment variables (same as §3.2; re-export if you open a new terminal)
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000              # any non-empty string for local deploys
export CUBE_TEMPLATE_ID=<your-template-id> # from cubemastercli tpl list

./bin/cube-bench -c 1  -n 1  -m create-only && free -m   # cumulative: 1
./bin/cube-bench -c 4  -n 4  -m create-only && free -m   # cumulative: 5
./bin/cube-bench -c 5  -n 5  -m create-only && free -m   # cumulative: 10
./bin/cube-bench -c 10 -n 10 -m create-only && free -m   # cumulative: 20
```

**Step 3: Calculate per-instance overhead**

```
Per-VM amortized overhead = (current used - baseline used) ÷ VM count
```

**Results (Tencent Cloud SA9.4XLARGE32 PVM, 2 vCPU / 2 GiB sandbox):**

| Live Sandboxes | System Available (MB) | Per-VM Amortized Overhead |
|:---------:|:------------------:|:-------------:|
| 0 (baseline) | 25570 MB           | — |
| 1            | 25536 MB           | **~34 MB** |
| 5            | 25436 MB           | **~27 MB** |
| 10           | 25252 MB           | **~32 MB** |
| 20           | 24990 MB           | **~29 MB** |

> Measured per-VM amortized overhead is approximately **27–34 MB**. CoW on-demand allocation is clearly effective — a 2 GiB sandbox at idle uses only ~30 MB in practice.

**Estimated single-host capacity (SA9.4XLARGE32, 32 GiB memory):**

```
Total memory:                     32768 MB
System baseline usage (measured): 7198 MB  (= 32768 - 25570, from empty-machine available)
Safety headroom reserved (10%):   3276 MB
Available for sandboxes:         22294 MB  (= 32768 - 7198 - 3276)

Idle/light-load scenario (CoW on-demand allocation, ~30 MB amortized per sandbox):
  22294 ÷ 30 ≈ 743 sandboxes

Full-load scenario (each sandbox writes the full 2 GiB):
  22294 ÷ (2048 + 30) ≈ 10 sandboxes
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
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

python bench_snapshot_concurrency.py -c 1  -n 5
python bench_snapshot_concurrency.py -c 5  -n 5 --no-header
python bench_snapshot_concurrency.py -c 10 -n 5 --no-header
```

**Results** (fresh sandboxes snapshotted as-is; measured dirty pages ~**8 MB**, confirmed by `PagemapAnon snapshot saved` in `/data/log/CubeVmm/vmm.log`; this is the sandbox baseline anonymous memory page size and is not a variable in this section):

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-snapshot avg |
|:----:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 5    | 41.4 ms  | 37.6 ms  | 48.7 ms  | 48.7 ms  | 41.4 ms |
| 5    | 5    | 58.2 ms  | 51.0 ms  | 66.1 ms  | 66.1 ms  | **11.6 ms** |
| 10   | 5    | 114.1 ms | 66.2 ms  | 285.2 ms | 285.2 ms | **11.4 ms** |

Serial snapshot ~**41 ms**; at 5-concurrent, batch wall ~**58 ms**, per-snapshot amortized drops to ~**11.6 ms**; at 10-concurrent, batch wall ~**114 ms**, amortized further drops to ~**11.4 ms** — significant concurrency amortization.

### 4.2 Snapshot Creation vs Dirty Page Size

**Background:** CubeSandbox uses the soft-dirty mechanism to save only memory pages modified since the last snapshot. Actual write volume = dirty page count × 4 KiB, typically far less than total sandbox memory (2 GiB).

The test precisely controls dirty page size by pre-writing data to `/dev/shm` (tmpfs). The "Dirty Page" column shows actual bytes written as read from `/data/log/CubeVmm/vmm.log` — it differs from the theoretical write size due to Guest OS background activity.

**Run:** (script: [`bench_snapshot_dirty.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_dirty.py))

```bash
cd examples/snapshot-rollback-clone
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

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
| 0 MB    | 8.3 MB    | 42.1 ms  | 37.6 ms  | 45.9 ms  | 45.9 ms  | 71.6 ms | 65.4 ms | 77.7 ms | 77.7 ms |
| 10 MB   | 41.2 MB   | 55.3 ms  | 54.1 ms  | 56.6 ms  | 56.6 ms  | 73.1 ms | 60.4 ms | 82.5 ms | 82.5 ms |
| 50 MB   | 122.6 MB  | 67.7 ms  | 66.5 ms  | 69.6 ms  | 69.6 ms  | 70.3 ms | 63.9 ms | 81.4 ms | 81.4 ms |
| 100 MB  | 195.2 MB  | 85.7 ms  | 82.5 ms  | 88.7 ms  | 88.7 ms  | 68.3 ms | 62.3 ms | 71.6 ms | 71.6 ms |
| 200 MB  | 296.8 MB  | 100.9 ms | 98.5 ms  | 102.6 ms | 102.6 ms | 65.9 ms | 62.7 ms | 71.2 ms | 71.2 ms |
| 500 MB  | 602.6 MB  | 168.6 ms | 165.4 ms | 172.9 ms | 172.9 ms | 68.1 ms | 54.5 ms | 75.7 ms | 75.7 ms |
| 800 MB  | 908.3 MB  | 215.8 ms | 212.1 ms | 217.6 ms | 217.6 ms | 68.1 ms | 60.9 ms | 79.1 ms | 79.1 ms |
| 1024 MB | 1136.3 MB | 257.5 ms | 251.2 ms | 267.6 ms | 267.6 ms | 62.3 ms | 56.5 ms | 69.6 ms | 69.6 ms |

**Key findings:**
- **Snapshot creation time scales near-linearly with dirty page size**: baseline (8.3 MB dirty) ~42 ms, +~22 ms per 100 MB of additional dirty data, ~258 ms at 1024 MB
- **Create-from-snapshot time is independent of dirty page size**: stable at **54–83 ms** regardless of snapshot size, because restore uses CoW (copy-on-write) on-demand loading and does not depend on dirty page size

### 4.3 Create Sandbox from Snapshot

**How it works:** creates a snapshot first, then launches N sandboxes concurrently via `POST /sandboxes` (with `snapshot_id`), measuring end-to-end wall time until all sandboxes reach `running`.

**Run:** (script: [`bench_create_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_create_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

python bench_create_concurrency.py -c 1  -n 3
python bench_create_concurrency.py -c 10 -n 3 --no-header
python bench_create_concurrency.py -c 20 -n 3 --no-header
```

**Results:**

| Concurrency | n total | Rounds | wall avg | wall min | wall p95 | wall max | per-sandbox avg |
|:----:|:------:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 1   | 3 | 66.7 ms  | 65.8 ms  | 68.3 ms  | 68.3 ms  | 66.7 ms |
| 10   | 10  | 3 | 387.9 ms | 364.4 ms | 420.3 ms | 420.3 ms | **38.8 ms** |
| 20   | 20  | 3 | 701.3 ms | 660.5 ms | 742.4 ms | 742.4 ms | **35.1 ms** |

Single sandbox startup ~**67 ms**; at 10-concurrent, wall ~**388 ms**, amortized just **38.8 ms/sandbox**; at 20-concurrent, wall ~**701 ms**, amortized just **35.1 ms/sandbox** — demonstrating good concurrency scaling.

### 4.4 Rollback

**How it works:** calls `POST /sandboxes/{id}/rollback` on running sandboxes to restore memory and filesystem state in-place to the specified Snapshot, without recreating the sandbox.

> **Snapshot ownership constraint:** CubeSandbox only allows a sandbox to roll back to a checkpoint **it created itself**. Therefore each concurrent sandbox must independently complete the full "snapshot + rollback" flow.

**Run:** (script: [`bench_rollback_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_rollback_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

python bench_rollback_concurrency.py -c 1  -n 5
python bench_rollback_concurrency.py -c 5  -n 5 --no-header
python bench_rollback_concurrency.py -c 10 -n 5 --no-header
```

**Results:**

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-rollback avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-----------------:|
| 1    | 5 | 90.0 ms  | 82.0 ms  | 98.3 ms  | 98.3 ms  | 90.0 ms |
| 5    | 5 | 325.5 ms | 322.9 ms | 329.4 ms | 329.4 ms | **65.1 ms** |
| 10   | 5 | 821.4 ms | 778.7 ms | 858.1 ms | 858.1 ms | **82.1 ms** |

> Single Rollback flow ~**90 ms**; at 5-concurrent, batch wall ~**326 ms**, per-rollback amortized drops to ~**65 ms**; at 10-concurrent, batch wall ~**821 ms**, amortized ~**82 ms/rollback**.
>
> Note: Because CubeSandbox requires sandboxes to roll back only to their own checkpoints, shared snapshots cannot be reused — each concurrent sandbox must independently complete the full "snapshot + rollback" flow.

### 4.5 Clone

**How it works:** calls `POST /sandboxes/{id}/clone` to fork N new sandboxes from a **running** source sandbox, fully preserving the source's memory and filesystem state (including dirty pages).

> **Note:** disk files in this test were already in Page Cache; results exclude cold-read IO overhead.

**Run:** (script: [`bench_clone_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_clone_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

python bench_clone_concurrency.py -n 1  -c 1  --rounds 5
python bench_clone_concurrency.py -n 10 -c 5  --rounds 3 --no-header
python bench_clone_concurrency.py -n 20 -c 10 --rounds 3 --no-header
```

**Results (source sandbox dirty pages ~10 MB):**

| Scenario               | n   | Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-clone avg |
|:------------------|:---:|:----:|:----:|--------:|--------:|--------:|--------:|--------------:|
| 1 sandbox, 1-concurrent     | 1   | 1  | 5 | 270.6 ms | 260.8 ms | 280.5 ms | 280.5 ms | 270.6 ms |
| 10 sandboxes, 5-concurrent  | 10  | 5  | 3 | 541.6 ms | 522.9 ms | 557.7 ms | 557.7 ms | **54.2 ms** |
| 20 sandboxes, 10-concurrent | 20  | 10 | 3 | 789.7 ms | 757.2 ms | 815.3 ms | 815.3 ms | **39.5 ms** |

Single sandbox clone ~**271 ms**; 10 sandboxes at 5-concurrent, batch wall ~**542 ms**, per-clone amortized drops to ~**54 ms**; 20 sandboxes at 10-concurrent, batch wall ~**790 ms**, amortized further drops to ~**40 ms/sandbox** — significant concurrency amortization.

### 4.6 Pause / Resume

**How it works:** Creates `concurrency` sandboxes, pauses all of them concurrently via `POST /sandboxes/{id}/pause`, then resumes all concurrently via `POST /sandboxes/{id}/resume`. Records wall time and per-sandbox amortized latency for both operations.

> ⚠️ **Current implementation note:** Pause currently uses **full-memory-copy mode** — on pause, all anonymous memory pages of the sandbox are written to persistent storage. Latency scales linearly with sandbox memory size (~371 ms per sandbox at 2 GiB on PVM). A future release will upgrade to **soft-dirty incremental mode**, which only saves pages dirtied since the last checkpoint. For an idle sandbox this is expected to reduce pause latency by **80–90%** — significantly.

**Run:** (script: [`bench_pause_resume_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_pause_resume_concurrency.py))

```bash
cd examples/snapshot-rollback-clone
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

python bench_pause_resume_concurrency.py -c 1  -n 5
python bench_pause_resume_concurrency.py -c 10 -n 5 --no-header
```

**Pause results:**

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-pause avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-------------:|
| 1    | 5    | 370.8 ms | 351.0 ms | 384.0 ms | 384.0 ms | 370.8 ms |
| 10   | 5    | 1586.0 ms | 1529.5 ms | 1679.8 ms | 1679.8 ms | **158.6 ms** |

**Resume results:**

| Concurrency | Rounds | wall avg | wall min | wall p95 | wall max | per-resume avg |
|:----:|:----:|--------:|--------:|--------:|--------:|---------------:|
| 1    | 5    | 18.9 ms | 9.5 ms  | 32.8 ms | 32.8 ms | 18.9 ms |
| 10   | 5    | 26.6 ms | 19.3 ms | 39.9 ms | 39.9 ms | **2.7 ms** |

**Key findings:**
- **Resume is extremely fast with excellent concurrency scaling:** single resume ~19 ms; at 10-concurrent, per-resume amortized just **2.7 ms/sandbox**
- **Pause is the current bottleneck:** in full-copy mode, single pause ~371 ms, 10-concurrent per-pause amortized **158.6 ms/sandbox**
- **After soft-dirty mode lands:** pause latency is expected to drop significantly, with 10-concurrent per-pause falling into single-digit milliseconds

> **full-copy → soft-dirty optimization:** The current full-copy mode writes up to 2 GiB of VM anonymous memory to disk on every pause, creating high IO pressure. The soft-dirty incremental mode tracks dirty pages via `/proc/PID/clear_refs` since the last checkpoint; pause only writes actually modified pages (typically a few MB for an idle sandbox), reducing pause latency by **80–90%** and significantly increasing high-concurrency throughput.

---

## Appendix: Benchmark Script Index

All benchmark scripts used in this post are located in the repository directories:

- **[`examples/cube-bench/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench)** — Template-based concurrent creation benchmark tool (Go)
- **[`examples/snapshot-rollback-clone/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone)** — Snapshot / Rollback / Clone / Pause-Resume Python scripts
