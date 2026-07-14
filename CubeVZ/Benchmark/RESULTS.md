# CubeVZ Apple Silicon baseline

This is a single end-to-end validation run, not a statistically rigorous or
cross-platform comparison. Its purpose is to prove that the native CubeVZ path
boots and executes real workloads on an M4 host.

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
`examples/cube-bench` client. A POST returns only after an APFS-cloned 2-vCPU,
2-GiB sandbox has restored from `machine.vzstate` and sent READY over virtio
vsock. DELETE shuts down the guest and removes its VM directory.

- Timestamp: 2026-07-14 08:53 UTC
- Mode: `create-delete`
- Warmups: 3 per tier
- Success rate: 100% (220/220 measured sandbox lifecycle cycles)
- Raw reports: `_output/cube-vz/lifecycle-results/20260714T085302Z/`

| Host / tier | Requests | Create avg | Create P95 | Create P99 | Throughput |
|---|---:|---:|---:|---:|---:|
| M4 Pro, concurrency 1 | 20 | 326.6 ms | 336.9 ms | 341.7 ms | 1.72 lifecycle/s |
| M4 Pro, concurrency 10 | 200 | 1,094.3 ms | 1,543.6 ms | 2,027.1 ms | 5.93 lifecycle/s |

For context, the official Linux reports for the same 2-vCPU / 2-GiB sandbox
size publish these create latencies:

| Official Linux host / tier | Create avg | Create P95 | Relative M4 avg |
|---|---:|---:|---:|
| Tencent BMI5 bare metal, concurrency 1 | 47.8 ms | 57.4 ms | M4 is 6.83x slower |
| Tencent BMI5 bare metal, concurrency 10 | 88.7 ms | 116.9 ms | M4 is 12.34x slower |
| Tencent SA9 PVM, concurrency 1 | 66.7 ms | 78.2 ms | M4 is 4.90x slower |
| Tencent SA9 PVM, concurrency 10 | 170.9 ms | 216.7 ms | M4 is 6.40x slower |

The API and APFS clone are not the dominant serial cost: 50 complete template
directory clones took 0.28 seconds (5.6 ms each, including a fresh CLI process).
Most of the remaining ~327 ms serial latency is therefore VM saved-state restore,
memory mapping, and guest readiness inside `Virtualization.framework`. At
concurrency 10, Apple VM restore and 2-GiB-per-VM memory pressure contend much
more heavily than Linux KVM's shared-kernel/lazy-memory design.

The bare-metal report used `create-only`, while the PVM report used the default
create/delete flow. `cube-bench` measures POST separately in either mode, but
the different retained-VM pressure means the bare-metal comparison is useful
context rather than an identical host experiment. Reproduce the macOS run with:

```bash
make cube-vz-lifecycle-benchmark
```
