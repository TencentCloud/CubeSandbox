# cube-envd functional validation report

[中文](validation_zh.md) · [Component overview](../README.md) · [Build and integration](usage.md)

This report describes completed local functional validation,
its results, boundaries and reproduction entry points. Tests were not rerun while
preparing this report. The accompanying [sanitized log excerpts](validation/functional-excerpts.md)
retain test names, outcomes, daemon identity, and the failed run followed by its retest.

## Outcome and delivery scope

Rust cube-envd completed base-image integration, new template creation and READY,
SDK sandbox creation, daemon identity and health checks, command execution and
file read/write on a local CubeSandbox platform. The Go reference passed the
same functional assertions in the full chain and three independent scenarios.

**Go remains the default; users build and explicitly select Rust.** This delivery
does not implement the original requirement to make Rust the default. The Rust
build uses this repository's source without compiling upstream Go envd; the
default Go build retains its upstream dependency. The PR should state this scope adjustment.

This report evaluates functionality only. It contains no performance comparison
or claim of a performance advantage or performance acceptance.

## Environment and source identity

| Item | Tested configuration |
| --- | --- |
| Platform | Existing local CubeSandbox installation, cube-runtime v0.5.0, Cloud Hypervisor |
| Platform architecture | Native x86_64 / amd64 |
| Guest | Ubuntu 22.04.5 LTS; kernel `6.6.69-opencloudos9.cubesandbox.pvm.guest-gb85200d80fa2`; cgroup v1 |
| Sandbox resources | 1 vCPU, 512 MiB, confirmed through sandbox info |
| SDK / runner | Repository CubeSandbox Python SDK 0.7.0; Python 3.10.12 |
| Rust toolchain | Rust/Cargo 1.89.0 with matching rustfmt and Clippy |
| Rust daemon | Product version 0.1.0; envd compatibility version 0.5.7 |
| Go reference | `e2b-dev/infra@2026.16`, source `b8ca332f435370397bf42be614b2a5b620d65d39`; executable reports 0.5.13 |
| arm64 image execution | Full-system QEMU TCG, 2 vCPUs, 1536 MiB; not native arm64 platform acceptance |

SDK requests traversed CubeProxy without bypassing it through guest IPs. Images
were built and used locally, not published. The Go derivative used for the final
independent scenarios included socat/libwrap0 to align packages; the default Go
Dockerfile was unchanged.

The tested Rust binary reports `-commit` as `2a377149915e550a7f7705824739737de3458898`,
the HEAD at image build time. The build also included uncommitted source changes
subsequently committed, so it must not be described as a clean build of that base.
The implementation was delivered as `0e8190ca68f2de19bb277c60fdd4ff3dfcb552cc`,
Git tree `c73154f791c4595a9eada655e67c752c58c90b74`. This report does not claim a
complete rebuild and retest after that commit. Subsequent documentation and CI
configuration edits are also outside this run's evidence.

## Live platform acceptance

| Scenario | Assertions | Rust | Go |
| --- | --- | --- | --- |
| New template chain | Create template from image, observe READY, create sandbox, inspect running daemon identity, health 204, commands, files and cleanup | Passed | Passed |
| Independent health and identity | Running `/usr/bin/envd`, source revision and health 204 through the proxy | Passed | Passed |
| Independent commands | stdout/stderr, nonzero exit, environment, default root, explicit UID 1000 and timeout | Passed | Passed |
| Independent files | UTF-8, empty files, overwrite/truncation, missing file, SDK/command read/write in both directions, binary writes verified through commands | Passed | Passed |

This comprises **two full chains and six independent scenario runs**. Individual
assertions in each row are not separate platform test runs. Binary verification
does not establish SDK binary-read support. Template creation, sandbox operations
and cleanup use the existing SDK E2E framework. Full-chain logs include cleanup
events with empty error lists.

Test entry point: [test_public.py](../../tests/e2e/sdk_compat/cases/envd/test_public.py).
Assertions: [envd_acceptance.py](../../tests/e2e/sdk_compat/framework/envd_acceptance.py).

## Component, framework and image validation

| Layer | Result | Boundary |
| --- | --- | --- |
| Rust component CI, amd64 | fmt, Clippy, build and check completed; 184 Rust tests passed, 3 ignored | Ignored tests are long-running VM fixtures, not passes |
| Nested Python scenarios | 6 passed | Invoked by Rust test entry points; do not add them to a separate combined total |
| Nested guest service scenarios | 9 passed, 1 IPv6 skip | Part of the same CI run; skip is not a pass |
| SDK E2E framework tests | 122 passed | Framework tests, not 122 live platform scenarios |
| amd64 images, Rust / Go | 18/18 each | Image and shared supervisor tests |
| arm64 image, Rust | 18/18 on QEMU | Not native arm64 platform validation |
| arm64 image, Go | Original run: 7 image tests passed; 1 of 11 supervisor tests failed | Original run did not fully pass |
| Supervisor retest after fixture fix | amd64 11/11; QEMU arm64 11/11 | Separate shared supervisor reruns, not a full Go arm64 image rerun |

Component tests also cover Process/PTY lifecycle, input/output and signals,
filesystem metadata and directory watchers, HTTP transfers, authentication,
protocol encoding and error behavior. See the excerpts for individual test names.
Interpret these results separately from live platform calls through CubeProxy.

The original Go arm64 failure occurred when a test worker read an incompletely
written exit-code file, raising `ValueError: invalid literal for int() with base 10: ''`.
After atomic publication was added to the fixture, the shared supervisor suite
passed on both architectures. The attachment retains the failure and retests.

## Compatibility and limitations

- Compatibility covers the implemented HTTP and Process/Filesystem protocols
  and the SDK calls listed here. Different Go/Rust version strings do not imply
  coverage of every upstream release or feature.
- On guest cgroup v1, Rust's cgroup v2 setup logs a warning and uses a no-op
  manager, matching Go's initialization fallback. Children inherit existing
  cgroups without envd's additional grouping. Placement errors after successful
  v2 initialization still reject the affected process; platform and VM isolation remain.
- Native arm64 component/platform acceptance, Firecracker platform acceptance
  and remote GitHub Actions execution were not performed. The three ignored VM
  fixtures and IPv6 skip are not verified capabilities.
- Local CI used read-only source mounts, private cgroups and removal of SYS_TIME.
  Passing results did not require modifying host cgroups or the host clock.
  Reproduction should use the repository's isolation configuration.

## Build and reproduce

1. Follow the [component build instructions](../README.md#build-the-component)
   and explicitly select the Rust image with `make cube-base-rust`.
2. Follow the [template and SDK guide](usage.md) with your platform endpoints,
   credentials and an image accessible to your template builder.
3. Run the full chain or the three independent scenarios using
   [selected envd acceptance](../../tests/e2e/sdk_compat/README.md#selected-envd-acceptance).
   Save that run's `events.jsonl` and test output.
4. Use the [README test entry points](../README.md#tests) for component and image
   tests. Check toolchain, architecture and isolation requirements, and record
   failures, skips and unrun cases as such.

The attached logs are historical evidence, not build inputs. Reproduction needs
no author-specific directory layout, image cache or original platform resource
IDs. New runs should retain their own results rather than being attributed to this report.
