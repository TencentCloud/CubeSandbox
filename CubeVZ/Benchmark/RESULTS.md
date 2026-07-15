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
`examples/cube-bench` client. A POST returns after an APFS-cloned 2-vCPU,
2-GiB sandbox has cold-booted and the real CubeSandbox envd service plus its
virtio-vsock relay are ready. DELETE stops the guest and removes its VM
directory.

- Timestamp: 2026-07-15 08:02 UTC
- Mode: `create-delete`
- Warmups: 3 per tier
- Success rate: 100% (220/220 measured sandbox lifecycle cycles)
- Raw reports: `_output/cube-vz/lifecycle-results/20260715T080214Z/`

| Host / tier | Requests | Create avg | Create P95 | Create P99 | Throughput |
|---|---:|---:|---:|---:|---:|
| M4 Pro, concurrency 1 | 20 | 222.8 ms | 245.8 ms | 246.6 ms | 2.59 lifecycle/s |
| M4 Pro, concurrency 10 | 200 | 282.9 ms | 363.8 ms | 414.7 ms | 17.80 lifecycle/s |

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
and real envd readiness. Per-phase API timings, including warmups, averaged:

| Tier | APFS clone | VM construction | VM start | envd readiness | Total |
|---|---:|---:|---:|---:|---:|
| Concurrency 1 / cold boot | 1.0 ms | 3.0 ms | 80.0 ms | 139.5 ms | 223.4 ms |
| Concurrency 10 / cold boot | 1.2 ms | 3.2 ms | 108.7 ms | 167.5 ms | 280.6 ms |

Guest telemetry averaged 92 ms to init and 120 ms to envd at concurrency 1;
at concurrency 10 those values were 109 ms and 149 ms. DHCP is deliberately
not part of POST readiness because CubeVZ control and envd traffic use vsock.
An outbound Internet command issued immediately after create may therefore
need a brief retry while VZNAT assigns the guest address.

For context, the repository's official
[BMI5 bare-metal report](../../docs/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md)
and [SA9 PVM report](../../docs/blog/posts/2026-06-03-cubesandbox-perf-benchmark-pvm.md)
publish these create latencies for the same 2-vCPU / 2-GiB sandbox size:

| Official Linux host / tier | Create avg | Create P95 | Relative M4 avg |
|---|---:|---:|---:|
| Tencent BMI5 bare metal, concurrency 1 | 47.8 ms | 57.4 ms | M4 is 4.66x slower |
| Tencent BMI5 bare metal, concurrency 10 | 88.7 ms | 116.9 ms | M4 is 3.19x slower |
| Tencent SA9 PVM, concurrency 1 | 66.7 ms | 78.2 ms | M4 is 3.34x slower |
| Tencent SA9 PVM, concurrency 10 | 170.9 ms | 216.7 ms | M4 is 1.66x slower |

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
