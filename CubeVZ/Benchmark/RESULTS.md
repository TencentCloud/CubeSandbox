# CubeVZ Apple Silicon baseline

The sysbench workload baseline below is a single end-to-end validation run, not
a statistically rigorous or cross-platform comparison. Its purpose is to prove
that the native CubeVZ path boots and executes real workloads on an M4 host. The
lifecycle section separately reports three independent runs and their range.

## Environment

- Timestamp: 2026-07-14 08:13 UTC
- Host: Mac16,7, Apple M4 Pro, 48 GiB RAM
- Host OS: macOS 26.5.1 (25F80)
- Guest: Alpine 3.22.5, Linux 6.12.95-0-virt, ARM64
- VM: 2 vCPUs, 2048 MiB RAM, 768 MiB raw ext4 root disk
- Runtime: Apple `Virtualization.framework`, one virtualization layer
- Benchmark: sysbench 1.0.20

## Result

| Measurement | Result |
|---|---:|
| APFS copy-on-write VM directory creation | 154.924 ms |
| Guest boot to benchmark init | 0.19 s |
| CPU, prime limit 20,000, 2 threads | 8,433.26 events/s |
| Memory write, 1 MiB blocks, 2 threads | 30,546.16 MiB/s |
| Direct random file reads, 16 KiB blocks | 2,944.70 reads/s (46.01 MiB/s) |
| Direct random file writes, 16 KiB blocks | 1,963.13 writes/s (30.67 MiB/s) |
| Full VM run including workloads and shutdown | 13,992.604 ms |

The guest reported `/dev/vda` as an ext4 root filesystem and emitted the
`CUBEVZ_BENCH_END` completion marker before powering down. Re-run the current
code and produce a timestamped full console/report with:

```bash
make cube-vz-benchmark
```

## CubeSandbox lifecycle benchmark

The native lifecycle path was also measured with the repository's official
`examples/cube-bench` client. A POST returns after an APFS-cloned 2-vCPU,
2-GiB sandbox has cold-booted and the real CubeSandbox envd service plus its
virtio-vsock relay are ready. DELETE stops the guest and removes its VM
directory.

- Source commit: `7bebde4`
- Timestamps: 2026-07-15 08:44–08:46 UTC
- Mode: `create-delete`
- Warmups: 3 per tier
- Independent runs: 3
- Success rate: 100% (660/660 measured sandbox lifecycle cycles)
- Tracked raw reports:
  [baselines/m4-pro-20260715/](baselines/m4-pro-20260715/)

| Run | Tier | Requests | Create avg | Create P95 | Create P99 | Throughput |
|---|---|---:|---:|---:|---:|---:|
| 1 | concurrency 1 | 20 | 223.7 ms | 243.7 ms | 244.5 ms | 2.63 lifecycle/s |
| 1 | concurrency 10 | 200 | 283.9 ms | 382.3 ms | 502.6 ms | 17.76 lifecycle/s |
| 2 | concurrency 1 | 20 | 226.4 ms | 242.2 ms | 279.8 ms | 2.49 lifecycle/s |
| 2 | concurrency 10 | 200 | 330.2 ms | 401.7 ms | 413.0 ms | 22.42 lifecycle/s |
| 3 | concurrency 1 | 20 | 216.7 ms | 229.6 ms | 233.7 ms | 3.94 lifecycle/s |
| 3 | concurrency 10 | 200 | 349.8 ms | 417.7 ms | 438.4 ms | 25.22 lifecycle/s |

Median run-level metrics, with the observed run range in parentheses:

| Tier | Create avg | Create P95 | Create P99 | Throughput |
|---|---:|---:|---:|---:|
| Concurrency 1 | 223.7 ms (216.7–226.4) | 242.2 ms (229.6–243.7) | 244.5 ms (233.7–279.8) | 2.63/s (2.49–3.94) |
| Concurrency 10 | 330.2 ms (283.9–349.8) | 401.7 ms (382.3–417.7) | 438.4 ms (413.0–502.6) | 22.42/s (17.76–25.22) |

The optimized path has one lifecycle mode:

- every sandbox gets a fresh machine identifier and cold-boots an APFS-cloned
  disk; saved-state preparation, restore, and adaptive branching are removed;
- the pinned Linux 6.12.95 kernel has the required ext4 and virtio drivers
  built in, boots without an initramfs, and omits guest KVM, VFIO, balloon,
  unused block-device, storage-stack, and filesystem subsystems;
- inactive MAC addresses are recycled so concurrent VZNAT clients remain
  distinct without growing the DHCP identity set indefinitely;
- envd starts in parallel with background DHCP; health is polled every 10 ms
  on guest loopback and the READY result is delivered over vsock;
- DELETE stops the VM through the host framework because the ephemeral disk is
  discarded immediately.

No hot pool or prewarmed VM is used. POST still includes APFS clone, VM startup,
and real envd plus relay readiness. Median per-run phase averages, including
warmups, were:

| Tier | APFS clone | VM construction | VM start | envd readiness | Total |
|---|---:|---:|---:|---:|---:|
| Concurrency 1 / cold boot | 1.0 ms | 3.1 ms | 80.2 ms | 139.9 ms | 224.6 ms |
| Concurrency 10 / cold boot | 1.3 ms | 3.5 ms | 135.8 ms | 185.5 ms | 326.3 ms |

Median guest telemetry was 94 ms to init and 120 ms to envd at concurrency 1;
at concurrency 10 those values were 120 ms and 163 ms. DHCP is deliberately
not part of POST readiness because CubeVZ control and envd traffic use vsock.
An outbound Internet command issued immediately after create may therefore
need a brief retry while VZNAT assigns the guest address.

For context, the repository's official
[BMI5 bare-metal report](../../docs/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md)
and [SA9 PVM report](../../docs/blog/posts/2026-06-03-cubesandbox-perf-benchmark-pvm.md)
publish these create latencies for the same 2-vCPU / 2-GiB sandbox size:

| Official Linux host / tier | Create avg | Create P95 | Relative M4 avg |
|---|---:|---:|---:|
| Tencent BMI5 bare metal, concurrency 1 | 47.8 ms | 57.4 ms | M4 median is 4.68x slower |
| Tencent BMI5 bare metal, concurrency 10 | 88.7 ms | 116.9 ms | M4 median is 3.72x slower |
| Tencent SA9 PVM, concurrency 1 | 66.7 ms | 78.2 ms | M4 median is 3.35x slower |
| Tencent SA9 PVM, concurrency 10 | 170.9 ms | 216.7 ms | M4 median is 1.93x slower |

The API and APFS clone are no longer dominant. The remaining create latency is
mostly Apple VM start plus scheduling the guest until envd is healthy. Because
CubeVZ does not wait for DHCP while the official Linux backend may have network
configured before POST returns, the relative table is useful context rather
than a claim of identical readiness semantics.

The bare-metal report used `create-only`, while the PVM report used the default
create/delete flow. `cube-bench` measures POST separately in either mode, but
the different retained-VM pressure means the bare-metal comparison is useful
context rather than an identical host experiment. Reproduce the macOS run with:

```bash
make cube-vz-lifecycle-benchmark
```
