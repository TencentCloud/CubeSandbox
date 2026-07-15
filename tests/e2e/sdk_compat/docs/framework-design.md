# SDK Compatibility E2E Framework Design

## Purpose

The suite runs backend-neutral pytest cases against the CubeSandbox and E2B
Python SDKs. It is opt-in, isolates every test in its own sandbox, and records
sanitized operation traces for failure diagnosis.

## Architecture

```text
pytest cases
    -> SandboxAdapter
       -> CubeSandboxAdapter -> CubeSandbox SDK
       -> E2BAdapter         -> E2B SDK
       -> TracingSandboxAdapter -> TraceCollector
                                  -> terminal / JSONL
```

Cases use only the shared adapter methods: `info`, `run_command`, `run_code`,
`write_file`, `read_file`, `pause`, `resume_or_connect`, `kill`, and `close`.
SDK-specific translation belongs in `adapters/`.

## Fixture lifecycle

`sdk_sandbox`:

1. loads pytest, environment, and `.env` configuration;
2. runs session preflight when `--run-e2e` is set;
3. checks capabilities and platform markers;
4. merges `sandbox_create_options`;
5. creates one sandbox and attaches a `TraceCollector`;
6. yields the adapter;
7. preserves only setup/call failures when
   `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true`;
8. otherwise performs best-effort cleanup.

Tests must not depend on a fixed sandbox ID or another test's sandbox.

## Capabilities and markers

Use `requires_capability` for backend support boundaries:

```python
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
```

Available capability domains include `lifecycle`, `commands`, `filesystem`,
`run_code`, `pause_resume`, `network_allow_deny`, `network_public_access`, and
`platform_lifecycle`.

Use `smoke`, `p0`, `p1`, `p2`, `p3`, and `slow` to describe execution priority.
Use `requires_cubeproxy` only for cases that require CubeProxy/lifecycle-manager
coordination.

## Preflight and reporting

Preflight validates CubeAPI health and template readiness. Platform lifecycle
preflight can also probe the CubeProxy admin heartbeat.

`TraceCollector` records timestamp, operation, sanitized input/output,
duration, and success. Secrets are redacted, large values are truncated, and
file contents are represented by length only. JSONL events are written
to `SDK_E2E_REPORT_DIR/events.jsonl`.

Use `--sdk-e2e-trace` for live operation output:

```bash
pytest --run-e2e --sdk-e2e-trace cases/lifecycle/test_pause_resume.py
```

## Lifecycle readiness

Control-plane `state == "running"` does not guarantee that CubeShim, envd, or
the code interpreter is ready. Lifecycle tests should seed kernel/filesystem
state, wait for `paused`, perform the intended resume path, wait for `running`,
then execute and verify a data-plane operation.

Prefer `wait_for_platform_pause`, `wait_for_platform_destroy`, and
`wait_until_running` to fixed sleeps. A temporary sleep is acceptable for race
diagnosis but must be justified before merge.
