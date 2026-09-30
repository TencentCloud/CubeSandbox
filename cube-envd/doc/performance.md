# cube-envd SDK performance report

[中文](performance_zh.md) · [Component overview](../README.md) · [Functional validation](validation.md)

## Rust performance after the fix

This comparison measures the Rust performance fix made during development
on the existing feature branch. With the accepted-connection
`TCP_NODELAY` fix, Rust's median SDK latency was **14.348 ms for a short command**
and **1.813 ms for a 4 KiB read**, compared with Go's 16.026 ms and 1.451 ms.
Rust also had lower large-read latency in these runs; Go remained faster for
small file operations overall and substantially faster for large writes.

“Rust after fix” below denotes the measured implementation with this change,
not a separate release. The before/after columns show the improvement within
Rust development. They do not imply that Rust must outperform Go. Go remains
the default provider; Rust remains an explicit selection.

## Results

Latency is in milliseconds. Each implementation/workload has **500 successful
measured samples and 0 errors**, split across five independent groups. Percent
change is `(after / reference - 1) × 100`; negative means lower latency.
Aggregate medians and nearest-rank p95 values are descriptive pooled summaries;
the individual groups below are the basis for assessing consistency.

| Workload | Rust after fix median / p95 | Go median / p95 | Rust before fix median / p95 | After fix vs Go median change | After vs before fix median change |
| --- | ---: | ---: | ---: | ---: | ---: |
| Short command | 14.348 / 16.212 | 16.026 / 18.434 | 47.892 / 49.041 | -10.47% | -70.04% |
| 4 KiB read | 1.813 / 2.540 | 1.451 / 2.547 | 44.093 / 45.592 | +24.95% | -95.89% |
| 4 KiB write | 1.475 / 2.056 | 1.319 / 2.022 | 2.404 / 3.190 | +11.84% | -38.63% |
| 4 MiB read | 26.965 / 31.779 | 33.636 / 39.172 | 58.065 / 62.825 | -19.83% | -53.56% |
| 4 MiB write | 31.493 / 36.815 | 12.764 / 16.976 | 32.144 / 36.625 | +146.74% | -2.03% |

Short commands, small reads, small writes and large reads improved against
Rust before the fix in all five groups. The small-read median reduction ranged
from 95.40% to 96.05%; short commands from 68.18% to 72.01%.
Large-write medians moved only slightly: four groups improved and one worsened.
Their p95 increased in groups 1–3 and decreased in groups 4–5. **No stable
large-upload improvement is claimed.** After-fix large writes still took about
2.47 times Go's median latency; small reads remained about 25% slower than Go.

## Environment and measured builds

| Item | Configuration |
| --- | --- |
| Platform | Existing CubeSandbox installation, cube-runtime v0.5.0, Cloud Hypervisor |
| Architecture / guest | Native amd64; Ubuntu 22.04.5 LTS; guest kernel 6.6.69; cgroup v1 |
| Sandbox allocation | 1 vCPU, 512 MiB, verified through sandbox info |
| SDK / runner | Repository CubeSandbox Python SDK 0.7.0; Python 3.10.12 |
| Rust build | Rust 1.89.0, locked dependencies, release profile, static amd64 musl executable |
| Rust versions | Product 0.1.0; envd compatibility version 0.5.7 |
| Go reference | `e2b-dev/infra@2026.16`, source `b8ca332f435370397bf42be614b2a5b620d65d39`; Go 1.25.4; daemon version 0.5.13 |
| Runtime alignment | Same nginx application, package versions, entrypoint, default root user, log destination and SDK/CubeProxy route |

All three executables were rebuilt for the comparison. Go and both Rust
executables used the same runtime package layer. All 15 formal sandbox
identities and package lists were checked. Reads used warm caches and fixed
ASCII `x` payloads, with SDK default HTTP compression behavior; these results
are not cold-cache, incompressible-data or durable-disk throughput measurements.

The Rust before-fix source was `eb1cb446ab9276b4303a7834b51a5429e4a741be`.
The after-fix build used that source plus the accepted-socket change in
[IdleListener](../src/guest/idle.rs) and its focused test. Both binaries embedded
the same build-base commit string, so that string alone does not distinguish
them. The following executable identities identify the actual measured builds;
subsequent commit ordering or documentation edits do not relabel these runs.
Tests were not rerun while preparing this report.

| Measured executable | SHA-256 |
| --- | --- |
| Rust after fix | `ca4d2338470fd97d24378ac2a383dc940280fed84b0fe4f072bb6da83b5b78c1` |
| Rust before fix | `cf41b1be4570434eb6c1afeeb4310b0d7cfad8056b81089d099d1422c9c9f23e` |
| Go | `28c469f5c9d42c09db1b6bf756d77a9decb674226d73f437c3d92fbfa5fb86e9` |

## Measurement method

The comparison reused the existing [SDK performance case](../../tests/e2e/sdk_compat/cases/performance/test_envd_comparison.py),
[timer and statistics helper](../../tests/e2e/sdk_compat/framework/envd_performance.py)
and [template/sandbox acceptance helpers](../../tests/e2e/sdk_compat/framework/envd_acceptance.py).
All requests used the original SDK/CubeProxy path, not direct guest access.

- Five groups, each with a new sandbox for Go, Rust before the fix and Rust after
  the fix: 15 independent sandboxes. Groups 1/3/5 ran Go → before → after;
  groups 2/4 ran after → before → Go. Each sandbox was created, measured and
  cleaned up before the next; only one was resident during measurements.
- Concurrency 1. Each sandbox/workload had one separately recorded first
  invocation, five warmups and 100 measured attempts. “First” means first
  workload invocation, after identity/health checks, not a cold daemon start.
- Workload order: `printf 'envd-perf'`, 4 KiB write/read, then 4 MiB write/read.
  Payload sizes were exactly 4096 and 4194304 bytes. Commands had to return
  complete stdout, empty stderr and exit code 0. Writes were verified by a full
  SDK read outside the write timer; read timers included full SDK consumption.
- Timing used `perf_counter_ns`; result-record serialization, verification and lifecycle
  operations were outside the timer. SDK retries and return consumption stayed
  unchanged; no operation retry was added and failed attempts were not replaced.
  Timings include SDK-internal behavior, without separate retry instrumentation.
- Median used `statistics.median`; p95 used sorted successful samples at
  `ceil(0.95 × n)`. Errors are reported separately. Overall statistics are not
  averages of group medians or proof of stable production tail latency.

The completed comparison had **7,950 successful attempts**: 75 first invocations,
375 warmups and 7,500 measured samples; **0 operation errors** and **18 cleanup
records without errors** (15 sandboxes and three templates). An earlier attempt
with three simultaneously resident sandboxes failed during platform guest-time
reset before collecting any samples. Its failure is excluded from these success
counts. The failed runtime was cleaned up and the entire comparison rerun with
identical sequential residency for all implementations.

Builds, packet capture and unrelated VMs were stopped during formal measurement.
Required platform services remained running. Across 296 host monitoring samples,
CPU idle min/median/max was 53/78/97%; median swap-in was 44 KiB/s, maximum
10,924 KiB/s, and maximum iowait 9%. This was not a noise-free dedicated host;
small differences and tail quantiles need that qualification.

## Individual groups

Each cell is median / p95 in ms. **Every implementation/workload/group has
100 successes and 0 errors.** These counts apply separately to each of the
three implementation columns, not to a pooled group of 100.

| Group | Workload | Rust after fix median / p95 | Go median / p95 | Rust before fix median / p95 |
| --- | --- | ---: | ---: | ---: |
| 1 | Short command | 14.430 / 17.455 | 15.743 / 18.438 | 47.917 / 49.057 |
| 1 | 4 KiB read | 1.857 / 2.215 | 1.503 / 3.558 | 44.137 / 45.919 |
| 1 | 4 KiB write | 1.500 / 1.916 | 1.328 / 1.809 | 2.544 / 3.213 |
| 1 | 4 MiB read | 27.367 / 38.155 | 33.927 / 42.070 | 58.168 / 63.474 |
| 1 | 4 MiB write | 31.844 / 37.079 | 13.103 / 17.099 | 31.688 / 36.435 |
| 2 | Short command | 14.352 / 15.969 | 16.096 / 18.575 | 48.006 / 49.237 |
| 2 | 4 KiB read | 2.029 / 2.810 | 1.434 / 2.381 | 44.128 / 45.158 |
| 2 | 4 KiB write | 1.590 / 2.282 | 1.187 / 1.557 | 2.343 / 3.279 |
| 2 | 4 MiB read | 27.130 / 33.992 | 34.322 / 39.201 | 57.395 / 60.893 |
| 2 | 4 MiB write | 31.788 / 37.269 | 12.722 / 16.783 | 31.940 / 34.865 |
| 3 | Short command | 13.415 / 14.801 | 15.922 / 18.455 | 47.924 / 48.989 |
| 3 | 4 KiB read | 1.747 / 2.336 | 1.533 / 2.456 | 44.038 / 45.649 |
| 3 | 4 KiB write | 1.404 / 1.958 | 1.452 / 2.164 | 2.359 / 2.898 |
| 3 | 4 MiB read | 27.031 / 31.779 | 33.468 / 38.874 | 58.212 / 61.622 |
| 3 | 4 MiB write | 31.534 / 36.536 | 12.604 / 17.223 | 32.141 / 35.738 |
| 4 | Short command | 14.274 / 15.959 | 16.332 / 18.369 | 47.831 / 49.069 |
| 4 | 4 KiB read | 1.743 / 2.395 | 1.361 / 2.372 | 44.083 / 45.540 |
| 4 | 4 KiB write | 1.400 / 1.915 | 1.182 / 2.011 | 1.733 / 2.708 |
| 4 | 4 MiB read | 27.262 / 31.715 | 33.935 / 39.345 | 58.271 / 62.825 |
| 4 | 4 MiB write | 31.131 / 35.273 | 13.021 / 17.578 | 32.232 / 38.021 |
| 5 | Short command | 14.934 / 16.306 | 15.921 / 18.216 | 46.928 / 48.825 |
| 5 | 4 KiB read | 1.800 / 2.178 | 1.490 / 2.368 | 44.088 / 45.584 |
| 5 | 4 KiB write | 1.522 / 1.909 | 1.527 / 2.102 | 2.612 / 3.285 |
| 5 | 4 MiB read | 25.885 / 28.275 | 31.819 / 36.691 | 58.166 / 62.636 |
| 5 | 4 MiB write | 31.141 / 36.278 | 12.332 / 15.426 | 32.668 / 37.436 |

## Why the connection change helps

The fix disables Nagle buffering with `TCP_NODELAY` for each accepted HTTP socket, allowing small
streamed body chunks and process completion events to leave without waiting
for acknowledgements of earlier response bytes. It retains idle tracking,
authentication, user permissions, output/exit semantics and cgroup initialization
fallback. Setting the option unsuccessfully logs a warning and preserves the
usable connection.

Separate diagnostic packet traces support this mechanism. For one before-fix
4 KiB read, the guest sent its first 212 response bytes 2.413 ms after receiving
the request, then waited 40.739 ms for the proxy's ACK; the remaining 39 bytes
followed 0.186 ms later. In an after-fix example, first-response preparation took
1.026 ms and the remaining 39 bytes followed just 0.024 ms later, **before** the
ACK. A short-command trace showed the same approximately 40 ms ACK-dependent
wait before the fix. Authentication/routing/filesystem time was bounded by the
pre-first-response interval, not separately profiled. Diagnostic timings are
not formal samples. An initially incomplete after-fix capture was repeated;
the complete recapture recorded 501/501 packets with no kernel drops.

## CPU, memory and concurrency

A separate check used three independent sandboxes per Rust variant, alternating
variant order, concurrency 1 and 4, and 40 validated operations per workload and
window: **2,400 successes, 0 errors**. Daemon CPU was measured through process
user/system tick deltas; it includes control calls and write readback, excludes
command children, and has coarse guest-clock resolution. VmHWM is cumulative
daemon peak RSS, not whole-VM memory. Peak VmHWM across this check was 3,972 KiB
for each variant; CPU per operation was comparable or lower after the fix.

Unpaced concurrency-4 writes had **higher isolated write latency** after the fix:

| Workload | Before median / p95 ms | After median / p95 ms |
| --- | ---: | ---: |
| 4 KiB write | 2.682 / 5.540 | 4.435 / 7.541 |
| 4 MiB write | 74.538 / 117.309 | 76.675 / 120.160 |

Each write was followed by untimed readback. Faster after-fix readback increased
request density: batches of 40 validated small writes took 0.475–0.480 s before
versus 0.080–0.099 s after. Thus isolated write latency here compares different
offered loads. Large validated-write batches also completed faster in all three
groups, despite the isolated write-latency increase.

An additional equal-arrival diagnostic retained those unfavorable results and
used 40 four-request bursts per size and sandbox, spaced 100 ms for 4 KiB and
250 ms for 4 MiB, across three variant pairs: **1,920 successes, 0 errors**.
This diagnostic does not replace the formal comparison or the unpaced check.

| Workload | Before median / p95 ms | After median / p95 ms | Before / after daemon CPU ms per operation | Before / after peak VmHWM KiB |
| --- | ---: | ---: | ---: | ---: |
| 4 KiB write | 5.768 / 9.327 | 5.749 / 8.635 | 1.375 / 1.437 | 1780 / 1776 |
| 4 MiB write | 100.026 / 131.678 | 102.932 / 131.086 | 29.375 / 29.812 | 4752 / 4676 |

At equal arrivals, there was no consistent p95 deterioration or large resource
increase. Large-write median changes across the three groups were +5.8%, -1.3%
and +5.0%; their p95 changes were +2.0%, -1.1% and -1.7%. These mixed results do
not establish better upload scaling or improvement in every concurrent latency.

## Functional checks and scope

The measured change passed the full isolated component checks: fmt, Clippy,
build and check; **185 Rust tests passed, three VM fixtures were ignored**.
Nested Python scenarios passed 6/6; guest scenarios passed 9 with one IPv6 skip.
The latter are nested checks, not additional independent Rust tests. The
amd64 production base-image/supervisor suite passed 18/18. Existing public SDK
acceptance passed three independent scenarios for Go and three for after-fix
Rust, plus Rust's new-template full chain. Independent Standards and Spec reviews
found no blocking issues. Component/image tests retained read-only source,
private cgroups and removal of SYS_TIME before starting test daemons.

No native ARM, Firecracker or remote CI performance certification is claimed.
The measurements do not cover cold-cache storage, arbitrary payloads, sustained
production capacity or all SDK features. Large-upload processing remains an
optimization opportunity; the present fix does not justify a dependency fork,
cache or worker-pool redesign.

## Repeating the comparison

Use the tracked [build instructions](../README.md#build-the-component),
[template/SDK guide](usage.md) and [SDK E2E instructions](../../tests/e2e/sdk_compat/README.md).
The existing opt-in performance entry point currently runs **three Go/Rust pairs
with ten measured attempts per workload per sandbox**. Running that command
alone does not reproduce this report's five-group, three-build sample set.

To repeat this report's design, build the specified Go reference and both Rust
variants, align their runtime packages and resources, and extend that same case's
provider list and sample loops to the five-group/100-sample schedule above.
Report all three implementations and all five groups; preserve the existing
`measure` helper, SDK operations, full consumption, write readback and failure
handling. Record first/warmup/measured samples separately, actual running build
identities, host load and cleanup outcomes. Diagnostic tracing and resource
experiments must run separately. The tables here provide the reported results
without requiring machine-specific files; new runs must retain their own raw
evidence and be reported separately from this comparison.
