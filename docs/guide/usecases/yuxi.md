---
title: "Yuxi: Sandbox Resource Management for a Multi-Tenant Agent Platform"
author: Zhang Wenjie
date: 2026-11-20
tags:
  - agent
  - multi-tenant
  - pause-resume
  - density
lang: en-US
---

# Yuxi: Sandbox Resource Management for a Multi-Tenant Agent Platform

## Business Background

Yuxi is a privately deployable multi-tenant knowledge agent platform (7.5K+ GitHub stars) unifying RAG, knowledge graphs, multi-agent orchestration, MCP/Skills, sandboxes and permission management. Agents in Yuxi install dependencies, process user files, and execute model-generated code, so every user needs an isolated execution environment. The previous Docker-container approach could not keep user environments alive long-term: reclaimed containers lose state, unreclaimed ones keep occupying memory. The project moved to CubeSandbox with the goal of "installed software and running state persist, sleep when idle, resume when a task arrives".

## Pain Points

- **Unclear what sleep actually saves**: with all user VMs asleep, QEMU still held about 6 GiB, and how much of it was releasable was unknown.
- **Default scheduling admission blocks scale-out**: the default `paused_resource_release_ratio=0.0` makes sleeping VMs keep their full quota, and the v0.7.3 CPU filter also rejects requests when remaining quota exactly equals the request.
- **Kernel overhead of the pre-built pool**: cgroup directories and kernel objects grow linearly with the pre-built scale.
- **Unknown capacity for concentrated restores**: no measured basis for memory peaks and latency when a hundred VMs resume at once.

## Solution on CubeSandbox

- **Scheduling admission tuning**: set `paused_resource_release_ratio` to 1.0 so sleeping quota can be reused; ten concurrent restores averaged 1.063 seconds per Usage request.
- **Shrink the cgroup pre-built pool**: cgroup directory count dropped from 6,076 to 167, SUnreclaim from 626,596 KiB to 167,832 KiB.
- **Free-page reporting reclaim**: `virtio-balloon-pci,free-page-reporting=on` plus `memory-backend-memfd` with `share=on`; dropping reporting granularity from 2 MiB to 4 KiB moved RSS from 5.725 GiB to 4.880 GiB, with 8 KiB chosen as the persistent granularity.
- **Residency attribution method**: correlate the guest's `/proc/kpageflags` with QEMU's `/proc/PID/pagemap`, classify by address mapping, and identify pages the guest freed but the host still holds (2.293 GiB).
- **Lifecycle control**: timeout -1 with `onTimeout: pause` plus `autoResume: false`; workers decide sleep by last activity time and actively resume; the adapter uses read-only APIs so monitoring never triggers a resume.

## Results

- All 220 real agent tasks completed (110 existing-user restores + 55 old-user re-restores + 55 first-time creations): no OOM, no 502s, container cap never touched.
- VM creation/resume averaged 0.18–0.27 s (P95 400 ms); total task time averaged 19–22 s.
- Measured page composition: 5.423 GiB file cache, 2.293 GiB free-but-resident pages; temporarily dropping granularity to 4 KiB moved RSS from 10.60 GiB to 8.32 GiB.
- A deployment basis for 100 lightweight VMs: QEMU 8-core/24 GiB inside an 8-core/32 GiB container, instance limit 512, creation concurrency 4, release ratio 1.0.

## References

- Full case article: [Sandbox Resource Management on a Multi-Tenant Agent Platform: Scheduling Admission, Free-Page Reclaim, and 100-VM Density Validation](/blog/posts/2026-11-20-yuxi)
- Yuxi project: [xerrors/Yuxi](https://github.com/xerrors/Yuxi)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
