---
title: "CubeSandbox Core Operations Performance Benchmark"
date: 2026-06-01
author: coolli
description: Performance benchmark data for CubeSandbox on a bare-metal node, covering Snapshot creation, sandbox creation from snapshot, Rollback, and Clone — with full test environment and methodology details.
featured: false
weight: 0
---

# CubeSandbox Core Operations Performance Benchmark

## 1. Overview

CubeSandbox is designed for AI Agent code execution, where fast cold-start and high concurrency are the two most critical metrics. This post presents benchmark data measured on a real bare-metal node for four core operations: Snapshot creation, sandbox creation from snapshot, Rollback, and Clone.

**Important: all numbers are highly environment- and workload-dependent.** Factors include host CPU, memory, IO performance, and the sandbox's internal state (e.g. more dirty pages → slower snapshot). Please evaluate against your own hardware and workload before drawing conclusions.

---

## 2. Test Environment

### 2.1 Hardware

| Item | Detail |
|------|--------|
| Machine | Tencent Cloud BMI5 Bare Metal Server |
| OS | OpenCloudOS (TencentOS Server 4) kernel 6.6.119 x86_64 |
| CPU | Intel(R) Xeon(R) Platinum 8255C @ 2.50GHz |
| CPU config | 2 Socket × 24 Core × 2 Thread = **96 logical cores** |
| NUMA | 2 nodes (node0: 0-23,48-71 / node1: 24-47,72-95) |
| Memory | **375 GiB** DDR4-2933 MT/s ECC |
| Data disk | `/dev/nvme0n1` 3.84 TB Intel SSDPE2KX040T8 NVMe SSD, XFS, mounted at `/data` |

### 2.2 Sandbox Spec

| Item | Detail |
|------|--------|
| Spec | 2 vCPU / 2 GiB memory |
| Image | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| Storage | CoW reflink (XFS, `/data/cubelet/storage/`) |
| Memory tracking | soft-dirty (`/proc/PID/clear_refs`) |

---

## 3. Methodology

### 3.1 General Conventions

- **All times in milliseconds (ms)**
- `wall`: end-to-end elapsed time for the entire batch (first request sent → last one done)
- `per`: amortized per-operation time (wall ÷ number of successful operations in the round)
- A **warm-up** round is run before each scenario and discarded, eliminating first-access page-cache spikes
- Different rounds run serially — no cross-round concurrency — to avoid mutual interference

### 3.2 Snapshot Creation

Calls `POST /sandboxes/{id}/snapshots` on a running sandbox and records wall time from request dispatch to snapshot write completion.

Concurrency test: N concurrent snapshot requests are issued against the **same sandbox**; `wall` = time until all complete, `per-snapshot` = amortized time per successful snapshot. CubeSandbox serializes snapshot requests on a single sandbox internally, so the number of successful snapshots may be less than the requested concurrency.

**Dirty Page note:** CubeSandbox uses the soft-dirty mechanism to only save memory pages modified since the last snapshot. The "write size" in tests refers to data written to `/dev/shm` (tmpfs) to precisely control dirty-page count; the "Dirty Page" column is the actual bytes written as read from `vmm.log`.

### 3.3 Create Sandbox from Snapshot

Calls `POST /sandboxes` (with `snapshot_id`) and records time from request dispatch until the sandbox reaches `running`.

Concurrency test: N sandboxes created simultaneously; `wall` = time until all are ready, `per-sandbox` = amortized time.

### 3.4 Rollback

Calls `POST /sandboxes/{id}/rollback` on a running sandbox to restore it to a given snapshot.

### 3.5 Clone

Calls `POST /sandboxes/{id}/clone` to fork a new sandbox from a running one, preserving full memory and filesystem state.

**Note:** disk files involved in Clone tests were already in Page Cache; results exclude cold-read IO overhead.

---

## 4. Results

### 4.1 Snapshot Creation vs Concurrency

| Concurrency | Rounds | wall avg | wall min | wall p50 | wall p80 | wall max | per-snapshot avg | Dirty Page |
|:-----------:|:------:|--------:|--------:|--------:|--------:|--------:|----------------:|----------:|
| 1           | 5      | 43.3 ms  | 42.1 ms  | 43.5 ms  | 44.3 ms  | 44.3 ms  | 43.3 ms          | 10.4 MB   |
| 5           | 5      | 288.0 ms | 162.1 ms | 288.4 ms | 417.3 ms | 417.3 ms | **94.4 ms**      | 10.4 MB   |

Serial: ~**43 ms** per snapshot. At concurrency 5, due to internal serialization only ~3 requests succeed per round, giving per-snapshot ~**94 ms** amortized and wall ~**288 ms** (p80 ~417 ms).

### 4.2 Snapshot Creation vs Dirty Page Size

> Serial mode; one warm-up run discarded before each data point. "create sandbox avg" reflects dirty-page impact on restore time.

| Write size | Dirty Page  | snapshot avg | snapshot min | snapshot max | create sandbox avg | create sandbox min | create sandbox max |
|:----------:|------------:|------------:|------------:|------------:|------------------:|------------------:|------------------:|
| 0 MB       | 6.8 MB      | 41.2 ms      | 40.1 ms      | 42.3 ms      | 59.8 ms            | 57.2 ms            | 62.4 ms            |
| 10 MB      | 33.9 MB     | 59.6 ms      | 59.3 ms      | 59.8 ms      | 60.4 ms            | 57.7 ms            | 63.7 ms            |
| 50 MB      | 115.2 MB    | 90.8 ms      | 88.0 ms      | 96.2 ms      | 60.5 ms            | 56.7 ms            | 66.3 ms            |
| 100 MB     | 180.6 MB    | 115.1 ms     | 114.1 ms     | 116.0 ms     | 63.9 ms            | 57.7 ms            | 67.1 ms            |
| 200 MB     | 282.5 MB    | 155.4 ms     | 150.2 ms     | 165.8 ms     | 67.3 ms            | 66.0 ms            | 68.7 ms            |
| 500 MB     | 587.9 MB    | 263.3 ms     | 255.8 ms     | 277.4 ms     | 68.7 ms            | 62.5 ms            | 79.1 ms            |
| 800 MB     | 893.0 MB    | 371.3 ms     | 359.3 ms     | 391.4 ms     | 70.5 ms            | 57.5 ms            | 85.6 ms            |
| 1024 MB    | 1121.1 MB   | 448.1 ms     | 446.1 ms     | 449.3 ms     | 66.7 ms            | 63.5 ms            | 68.6 ms            |

**Key findings:**
- **Snapshot time scales near-linearly with dirty-page size**: baseline ~41 ms, +~40 ms per 100 MB of dirty data.
- **Sandbox restore time is independent of dirty-page size**: stable at **59–71 ms** regardless of snapshot size, because restore only loads the memory snapshot without re-reading dirty pages.

### 4.3 Create Sandbox from Snapshot vs Concurrency

| Concurrency | n total | Rounds | wall avg | wall min | wall p50 | wall p80 | wall max | per-sandbox avg |
|:-----------:|:-------:|:------:|--------:|--------:|--------:|--------:|--------:|----------------:|
| 1           | 1       | 3      | 69.9 ms  | 66.2 ms  | 70.2 ms  | 73.2 ms  | 73.2 ms  | 69.9 ms          |
| 10          | 10      | 3      | 89.7 ms  | 78.1 ms  | 88.6 ms  | 102.4 ms | 102.4 ms | **9.0 ms**       |
| 20          | 20      | 3      | 97.3 ms  | 86.9 ms  | 90.7 ms  | 114.4 ms | 114.4 ms | **4.9 ms**       |

Serial: ~**70 ms**. At concurrency 20, wall ~**97 ms**, amortized just **4.9 ms/sandbox**.

### 4.4 Rollback vs Concurrency

| Concurrency | Rounds | wall avg | wall min | wall p50 | wall p80 | wall max | per-rollback avg |
|:-----------:|:------:|--------:|--------:|--------:|--------:|--------:|-----------------:|
| 1           | 5      | 71.1 ms  | 40.2 ms  | 76.4 ms  | 102.6 ms | 102.6 ms | 71.1 ms           |
| 10          | 5      | 126.8 ms | 88.4 ms  | 105.1 ms | 194.2 ms | 194.2 ms | **12.7 ms**       |

Serial: ~**71 ms**. At concurrency 10, amortized ~**12.7 ms/rollback**.

### 4.5 Clone vs Concurrency

| Scenario                   | n   | Concurrency | Rounds | wall avg | wall min | wall p50 | wall p80 | wall max | per-clone avg | Dirty Page |
|:--------------------------|:---:|:-----------:|:------:|--------:|--------:|--------:|--------:|--------:|--------------:|----------:|
| 1 sandbox, 1-concurrent   | 1   | 1           | 5      | 284.5 ms | 217.1 ms | 224.2 ms | 506.4 ms | 506.4 ms | 284.5 ms       | 10.4 MB   |
| 100 sandboxes, 10-concurrent | 100 | 10        | 2      | 856.0 ms | 847.4 ms | 864.7 ms | 864.7 ms | 864.7 ms | **8.6 ms**     | 10.4 MB   |

Single clone (full memory + filesystem): ~**285 ms**. 100 sandboxes at 10-concurrent: wall ~**856 ms**, amortized just **8.6 ms/sandbox**.

---

## 5. Summary

| Operation | Serial | 10-concurrent amortized | 20-concurrent amortized |
|-----------|-------:|------------------------:|------------------------:|
| Snapshot creation (~10 MB dirty) | ~43 ms | — | — |
| Create sandbox from snapshot | ~70 ms | ~9.0 ms | ~4.9 ms |
| Rollback | ~71 ms | ~12.7 ms | — |
| Clone (~10 MB dirty) | ~285 ms | ~8.6 ms (10-conc) | — |

CubeSandbox's concurrency scaling comes from its **resource-pooling + single-node closed-loop** design (see [architecture post](./2026-05-22-from-serverless-to-agent.md)): snapshot restore, CoW disk clone, and cgroup/TAP device pre-allocation all run in parallel, so wall time grows minimally as concurrency increases.

Questions or results to share? Join the conversation on [GitHub Discussions](https://github.com/TencentCloud/CubeSandbox/discussions).

---

## Appendix: Benchmark Scripts

All tests in this post were run with the following open-source scripts, available under [`examples/snapshot-rollback-clone/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone):

| Script | Section |
|--------|---------|
| [`bench_snapshot_dirty.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_dirty.py) | 4.2 Snapshot creation vs dirty-page size |
| [`bench_snapshot_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_concurrency.py) | 4.1 Snapshot creation vs concurrency |
| [`bench_create_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_create_concurrency.py) | 4.3 Create sandbox from snapshot |
| [`bench_rollback_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_rollback_concurrency.py) | 4.4 Rollback |
| [`bench_clone_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_clone_concurrency.py) | 4.5 Clone |
| [`rollback_demo.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/rollback_demo.py) | Functional verification |
| [`clone_demo.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/clone_demo.py) | Functional verification |
