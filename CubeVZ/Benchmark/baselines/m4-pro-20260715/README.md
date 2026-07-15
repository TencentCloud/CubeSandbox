# CubeVZ M4 Pro lifecycle baseline — 2026-07-15

This directory contains the machine-readable reports and per-sandbox API timing
logs for the reviewed CubeVZ lifecycle baseline.

## Provenance

- Source commit: `53f2792` (`fix(cube-vz): address review findings`)
- Host: Mac16,7, Apple M4 Pro, 48 GiB RAM
- Host OS: macOS 26.5.1 (25F80)
- Guest: 2 vCPU, 2048 MiB RAM, raw ext4 root disk
- Kernel: Linux 6.12.95-cube-vz, direct boot without an initramfs
- envd reference: `2026.16`
- Guest kernel SHA-256:
  `cdd94c6d382b40d1e7ddf77e0c7b01273d9808d97ea55435604fdda7dcc9ebef`
- Guest rootfs SHA-256:
  `6e28207570d00b1eebc62943ceed23f8bcedf93e3e68818351f0a49b23e640f8`
- Client: repository `examples/cube-bench`, Go 1.25.12
- Mode: `create-delete`, with 3 warmups per tier

The guest was rebuilt once from the source commit. The lifecycle benchmark was
then run three consecutive times with:

```bash
CUBEVZ_LIFECYCLE_SKIP_BUILD=1 CubeVZ/Benchmark/run-lifecycle-benchmark.sh
```

Each run contains 20 measured cycles at concurrency 1 and 200 measured cycles
at concurrency 10. All 660 measured lifecycle cycles succeeded.

## Results

| Run | Tier | Create avg | P95 | P99 | Throughput |
|---|---|---:|---:|---:|---:|
| 1 | concurrency 1 | 223.7 ms | 243.7 ms | 244.5 ms | 2.63 lifecycle/s |
| 1 | concurrency 10 | 283.9 ms | 382.3 ms | 502.6 ms | 17.76 lifecycle/s |
| 2 | concurrency 1 | 226.4 ms | 242.2 ms | 279.8 ms | 2.49 lifecycle/s |
| 2 | concurrency 10 | 330.2 ms | 401.7 ms | 413.0 ms | 22.42 lifecycle/s |
| 3 | concurrency 1 | 216.7 ms | 229.6 ms | 233.7 ms | 3.94 lifecycle/s |
| 3 | concurrency 10 | 349.8 ms | 417.7 ms | 438.4 ms | 25.22 lifecycle/s |

The headline value in `Benchmark/RESULTS.md` is the median of each run-level
metric, not a selectively chosen run and not a percentile recomputed from
pooled requests. The range is reported alongside the median to make sustained
host-load and tail variation visible.

## Files

Each `run-N/` directory contains:

- `c1.json`: complete `cube-bench` configuration, summary, distribution, and
  per-request create/delete timings for concurrency 1;
- `c10.json`: the equivalent report for concurrency 10;
- `api.log`: clone, VM construction, VM start, readiness, total, and guest
  milestone timings for all warmup and measured creates.

Phase summaries include warmups, matching the API timing methodology used by
the benchmark report: 23 creates for concurrency 1 and 203 creates for
concurrency 10 in each run.
