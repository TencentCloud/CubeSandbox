# SDK Compatibility E2E Test Coverage And Improvement Plan

## 1. Scope And Counting

This document audits the online compatibility cases under
`tests/e2e/sdk_compat/cases/`. A shared test function is parametrized by
`SDK_E2E_BACKENDS`, so the number of pytest nodes can be larger than the number
of Python test functions.

Use collection commands to inspect the exact scope for the current environment:

```bash
cd tests/e2e/sdk_compat
pytest --collect-only -q
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --collect-only -q
```

Online execution always requires `--run-e2e`. Platform-managed lifecycle cases
also require:

```bash
SDK_E2E_PLATFORM_LIFECYCLE=true \
pytest --run-e2e -m "lifecycle and slow"
```

## 2. Current Coverage

### 2.1 Lifecycle

| File | Main Behavior | Capability / Prerequisite | Risk And Execution Guidance |
| --- | --- | --- | --- |
| `cases/lifecycle/test_create.py` | `info` after creation and Linux command smoke | `lifecycle` | P0 / PR gate candidate |
| `cases/lifecycle/test_connect.py` | connect to an existing sandbox, ID and file/command usability | `lifecycle` | P1 |
| `cases/lifecycle/test_create_options.py` | metadata, env vars, timeout, command after create options | `lifecycle` | P1 |
| `cases/lifecycle/test_pause_resume.py` | SDK pause, connect resume, file/env/kernel preservation | `pause_resume`, partially Code Interpreter | P1 |
| `cases/lifecycle/test_kill.py` | unusable after kill, list removal, idempotent terminal semantics | `lifecycle` | P1 |
| `cases/lifecycle/test_auto_lifecycle.py` | auto-pause, manual/auto resume, reentrant resume, auto-kill, manual pause before timeout | `platform_lifecycle`, CubeProxy, lifecycle-manager, partially Code Interpreter | P1 + `slow`, daily run |

The current `platform_lifecycle` prerequisite reflects this branch's execution
configuration: it depends on CubeProxy and lifecycle-manager coordination, but
E2B is not enabled yet because its lifecycle create parameters must be aligned
with the CubeAPI create fields. That compatibility work is tracked by
[PR #988](https://github.com/TencentCloud/CubeSandbox/pull/988). After that
change is merged and verified in the target environment, E2B capabilities and
dual-backend lifecycle coverage should be revisited.

Lifecycle coverage is strongest when it verifies control-plane state together
with file, kernel and command data-plane behavior. A sandbox reported as
`running` is expected to have a working data plane; data-plane failures after
`running` should be reported as backend regressions, not hidden behind extra
readiness sleeps.

### 2.2 Commands

`cases/commands/test_run.py` covers:

- stdout, stderr and non-zero exit code;
- environment variables;
- special characters and multiline output;
- missing command exit code `127`;
- command timeout error semantics.

These cases verify adapter-level command result normalization and are good P0/P1
data-plane regressions.

### 2.3 Filesystem

`cases/filesystem/test_read_write.py` covers:

- file write/read round-trip;
- overwrite, multiline text and deep paths;
- larger text content;
- interoperability between file API and shell;
- missing file error semantics.

The current coverage is text-file focused. Directories, permissions, binary
round-trips, atomic replace and concurrent file access are not covered yet.

### 2.4 Run Code

`cases/run_code/test_python.py` covers:

- expression result text;
- stdout and stderr capture;
- Python errors and syntax errors;
- stateful kernel variable preservation.

These scenarios require Code Interpreter support and validate normalized
`CodeResult` values rather than SDK-private response objects.

### 2.5 Network

`cases/network/test_policy.py` covers create-time network policy behavior:

- `allow_out` punching through `deny_out=0.0.0.0/0`;
- deny-all blocking public TCP;
- `allow_internet_access=False` blocking public TCP;
- explicit allowlist still working when public internet is disabled;
- denying a selected target while preserving other public access;
- internal command execution without public internet;
- restricted public URL access: missing or invalid token returns 403, while
  `e2b-traffic-access-token` and `cube-traffic-access-token` both work with the
  correct token.

The current network suite uses configurable public TCP targets for L3/L4 egress
checks. Cases are marked `requires_internet`; runners without stable public
egress should set `SDK_E2E_SKIP_INTERNET_TESTS=true`.

### 2.6 Concurrency

`cases/concurrency/test_isolation.py` currently validates file isolation between
two sandboxes writing different content to the same path. The peer sandbox is
created and cleaned up through `managed_control_sandbox`.

This proves basic instance isolation, but it is not a concurrency stress test,
resource contention test or multi-worker safety validation.

## 3. Coverage Boundaries

The current suite mainly validates synchronous Python SDK paths and the
CubeSandbox/E2B compatibility surface:

- covered: create, info, command, text files, Python code, pause/resume, kill,
  partial platform lifecycle, basic egress policy and two-sandbox file
  isolation;
- not covered: async SDK, streaming output, complete template/snapshot/metadata
  APIs, directory and binary filesystem operations, full HTTP/HTTPS/L7/UDP-DNS
  network semantics, cancellation/resource limits, real concurrency load and
  multi-node failure recovery.

This is not a defect list by itself. Whether to add a case depends on API
contract maturity, environment reproducibility and CI budget.

## 4. Recommended Additional Cases

### P0: Prevent Compatibility False Positives

| Topic | Suggested Case | Benefit | Suggested Location |
| --- | --- | --- | --- |
| Adapter contract | Hermetic tests for parameter mapping, result normalization, exception mapping and unsupported capability | Find adapter regressions without a live cluster | `tests/e2e/sdk_compat/tests/` or adapter-near unit tests |
| Cleanup semantics | running, paused, resume failure, REST fallback and repeated teardown | Prevent leaked sandboxes in online suites | `framework/cleanup` unit tests |
| Preflight | missing SDK/key, unhealthy API, missing/non-ready template, stale lifecycle heartbeat | Fail configuration errors before creating sandboxes | `framework/preflight` unit tests |
| Lifecycle terminal states | connect, command, list and REST consistency after auto-kill | Prevent “request failed but instance leaked” regressions | `cases/lifecycle/` |
| Reporting contract | JSONL schema, trace truncation, redaction and file length summaries | Keep CI-consumable diagnostics safe and stable | `framework/trace`, `framework/reporting` unit tests |

### P1: Daily Compatibility Coverage

| Topic | Suggested Case | Benefit | Suggested Location |
| --- | --- | --- | --- |
| Lifecycle races | pause vs timeout, reentrant resume, kill vs resume | Expose state-machine and distributed lock issues | `cases/lifecycle/` |
| Lifecycle timing | timeout refresh, manual pause before timeout, new idle cycle after manual connect, `endAt` drift | Validate lifecycle contract across time windows | `cases/lifecycle/` |
| Filesystem | directories, non-root permissions, binary round-trip, empty files, delete/rename | Expand SDK file API compatibility | `cases/filesystem/` |
| Commands | working directory, user switching, signal/cancel, large output and resource cleanup after timeout | Catch process/stream cleanup bugs | `cases/commands/` |
| Run code | kernel usability after errors, timeout, resource limit, rich output, language parameters | Validate interpreter resilience | `cases/run_code/` |
| Network DNS/UDP | domain allow/deny, DNS failure, UDP DNS request/response | Avoid using TCP as a proxy for DNS/UDP policy | `cases/network/` |
| Network HTTP/HTTPS | HTTP host/path/method, HTTPS SNI and certificates, proxy behavior | Validate real application traffic | `cases/network/` |
| Two sandboxes | concurrent create/run/kill, file/env/network isolation | Find ID/session/network policy cross-talk | `cases/concurrency/` |

### P2: Platform, Performance And Release Qualification

| Topic | Suggested Case | Benefit | Suggested Location |
| --- | --- | --- | --- |
| L7 policy | rule priority, header injection, audit, deny/allow precedence | Cover policy behavior beyond L3/L4 | `cases/network/` |
| Multi-node | create/connect/pause/resume when sandboxes land on different compute nodes | Find template and routing inconsistencies | `cases/lifecycle/` or new `scheduling/` |
| Scale | bounded small concurrency, for example 5 or 10 create-command-kill loops | Detect resource contention while controlling CI cost | `cases/concurrency/` |
| Fault injection | temporary CubeProxy outage, CubeAPI 5xx, unstable network target | Validate error classification and reporting quality | Dedicated chaos environment |
| Long-running | repeated auto-pause/resume cycles, state retention after disk writes, continuous idle timer refresh | Find release-time leaks and drift | `p3` lifecycle suite |

## 5. Framework Improvements

### 5.1 P0: Add Offline Tests For The Framework

Current framework and adapter correctness mostly depends on online E2E coverage.
Add pure Python contract/unit tests for adapters, cleanup, preflight, waits and
trace redaction, and include them in the default PR gate.

### 5.2 P0: Version The Report Schema

JSONL is the main diagnostic output, but event fields are mostly code
conventions today. Define an event schema/version near `framework/reporting.py`,
add schema tests and write the schema version into every report.

### 5.3 P1: Turn External Dependencies Into Executable Preconditions

Network targets, CubeProxy admin health, template readiness and Code Interpreter
availability should be probed early where possible. Failures should report the
target and suggested action instead of surfacing as unrelated business
assertions.

### 5.4 P1: Improve Wait Diagnostics

Platform wait helpers use backoff polling, but failures often keep only the last
state. Record a bounded state timeline with state, time, exception and list
results, and attach it to assertion failures and JSONL events.

### 5.5 P1: Provide A Standard Peer Sandbox Fixture

`managed_control_sandbox` works for a few cases. If multi-sandbox tests grow,
add `sdk_peer_sandbox` or a named peer factory fixture with uniform metadata and
cleanup.

### 5.6 P2: Make The CI Matrix Explicit

README describes smoke/P0/P1/P2 scopes, but the directory does not own an
auditable CI matrix. Version jobs for offline framework tests, CubeSandbox P0,
dual-SDK P1, network, platform lifecycle and P3 long-running coverage.

### 5.7 P2: Define Parallel Execution Admission

Lifecycle, shared public network targets and cluster capacity are sensitive to
parallel execution. Explicitly define which markers can use `pytest-xdist` and
which must stay serial; record worker IDs and sandbox counts in reports.

## 6. Recommended Implementation Order

1. Add offline unit tests for trace/reporting, preflight, cleanup and adapters.
2. Expand lifecycle terminal/timing semantics and network DNS/HTTP/HTTPS cases.
3. Add a standard peer fixture before growing small-concurrency isolation cases.
4. Add multi-node, fault-injection and P3 long-running coverage last.

When adding new coverage, update the [case authoring guide](case-authoring.md)
and this document’s current coverage inventory.
