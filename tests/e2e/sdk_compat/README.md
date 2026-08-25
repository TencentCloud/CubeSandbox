# SDK Compatibility E2E Tests

This directory contains live end-to-end tests for Python SDK compatibility. The
same backend-neutral cases run against:

- `cubesandbox`: the CubeSandbox Python SDK from `sdk/python`.
- `e2b`: the E2B Python SDK (`e2b-code-interpreter` or `e2b`) against a CubeSandbox-compatible backend.

The suite is opt-in. Without `--run-e2e`, pytest collection is safe and every
live case is skipped; only hermetic pure-logic unit tests marked `framework`
run. Live runs default to the `cubesandbox` backend so PR-gate runs stay small
and stable. Use `SDK_E2E_BACKENDS=e2b,cubesandbox` for dual-SDK compatibility
runs.

Documentation:

- [Framework design](docs/framework-design.md)
- [Case authoring guide](docs/case-authoring.md)
- [Test coverage and improvement plan](docs/test-coverage.md)
- [中文 README](README_zh.md)
- [中文框架设计](docs/zh/framework-design.md)
- [中文用例编写指南](docs/zh/case-authoring.md)
- [中文测试覆盖盘点与优化建议](docs/zh/test-coverage.md)

## Backend Environment Variables

`cubesandbox` backend:

- `CUBE_API_URL`: CubeAPI endpoint. Defaults to `http://127.0.0.1:3000`.
- `CUBE_TEMPLATE_ID`: ready template ID used for sandbox creation.
- `CUBE_API_KEY`: API key if the target CubeAPI requires one.
- `CUBE_PROXY_NODE_IP`: optional CubeProxy node IP, useful when wildcard sandbox
  DNS is unavailable from the runner.

`e2b` backend:

- `SDK_E2E_BACKENDS=e2b` or `SDK_E2E_BACKENDS=e2b,cubesandbox`: enables the
  E2B backend.
- `CUBE_API_URL`: E2B-compatible CubeSandbox control-plane endpoint. The
  adapter passes this value to the E2B SDK explicitly.
- `E2B_API_KEY`: API key used by the E2B SDK. For a self-hosted CubeSandbox
  endpoint, use the key accepted by that endpoint.
- `CUBE_TEMPLATE_ID`: ready CubeSandbox template ID used for sandbox creation.
- `SSL_CERT_FILE`: local CA bundle for self-hosted HTTPS sandbox endpoints.

Shared timeout, reporting, tracing, and lifecycle variables are listed in the
[Environment](#environment) section.

## Prepare Template

Create a Code Interpreter capable template before running live E2E tests. The
template must expose envd (`49983`) and Jupyter/Code Interpreter (`49999`):

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Use the generated template ID as `CUBE_TEMPLATE_ID` in the commands below.

`cases/network/test_mask_request_host.py` additionally builds a temporary
template that also exposes port `8765` (needed for CubeProxy port mapping when
requests hit the proxy via `CUBE_PROXY_NODE_IP=127.0.0.1`). Override the image
with `SDK_E2E_MASK_HOST_TEMPLATE_IMAGE` or `CUBE_TEMPLATE_E2E_IMAGE` if needed.
The suite still requires a ready `CUBE_TEMPLATE_ID` for preflight.

## Quick Start

```bash
cd tests/e2e/sdk_compat
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
export CUBE_PROXY_NODE_IP=127.0.0.1

pytest --run-e2e
```

The default command above runs only the `cubesandbox` backend. Equivalent explicit
form:

```bash
pytest --run-e2e --sdk-e2e-backends=cubesandbox
```

## Execution Scope

Recommended scopes:

```bash
# Fast environment smoke
pytest --run-e2e -m smoke

# PR gate: stable CubeSandbox backend coverage
pytest --run-e2e -m "smoke or p0" --sdk-e2e-backends=cubesandbox

# Daily dual-SDK compatibility
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e -m "p0 or p1"

# Platform lifecycle regression (cube-proxy + lifecycle manager)
SDK_E2E_PLATFORM_LIFECYCLE=true pytest --run-e2e -k lifecycle -m "p1 and slow"

# Volume plugin regression (manual plugin deploy/config; cubesandbox >= 0.6.0)
SDK_E2E_VOLUME_PLUGIN=true pytest --run-e2e -m volume --sdk-e2e-backends=cubesandbox

# Broader regression
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e -m "p0 or p1 or p2"
```

Run one test suite, file, or test case:

```bash
# One test suite by marker
pytest --run-e2e -m lifecycle

# One lifecycle test file
pytest --run-e2e cases/lifecycle/test_pause_resume.py

# One test function
pytest --run-e2e cases/lifecycle/test_pause_resume.py::test_pause_sets_state_paused

# One parametrized backend explicitly
pytest --run-e2e \
  --sdk-e2e-backends=cubesandbox \
  cases/lifecycle/test_pause_resume.py::test_pause_sets_state_paused[cubesandbox]

# Select tests by keyword
pytest --run-e2e -k "pause and resume"
```

Use `--collect-only -q` to inspect the exact node IDs before running a
parameterized test:

```bash
pytest --collect-only -q cases/lifecycle/test_pause_resume.py
```

Run platform-managed lifecycle cases (`auto-pause`, `auto-resume`, and
`auto-kill`):

```bash
# Required opt-in for the four slow cases in test_auto_lifecycle.py.
export SDK_E2E_PLATFORM_LIFECYCLE=true

# Recommended so preflight can probe CubeProxy admin heartbeat.
export CUBE_PROXY_NODE_IP=<cube-proxy-node-ip>

pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_auto_lifecycle.py
```

These cases are skipped unless `SDK_E2E_PLATFORM_LIFECYCLE=true` is set because
they depend on the full platform chain: CubeProxy, Redis, cube-lifecycle-manager,
CubeMaster, and Cubelet. They also require a `READY` template on all target
compute nodes. To run only one case:

```bash
pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_auto_lifecycle.py::test_lifecycle_auto_resume_preserves_state
```

Run dual backend after installing E2B:

```bash
pip install e2b-code-interpreter
export E2B_API_KEY=<your-e2b-api-key>
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
export SDK_E2E_BACKENDS=e2b,cubesandbox
pytest --run-e2e
```

## E2B SDK Compatibility

A single environment can only hold one version of a package, so any run above
validates whichever E2B SDK version happens to be installed. `e2b-versions.txt`
records the versions this suite is known to have been run against, so a consumer
reading it can tell which SDK versions are expected to work with a given
CubeSandbox release instead of discovering it by outage.

To check one version, install it and run the suite normally. The commands below
assume the E2B backend environment from "Quick Start" is already exported —
arriving here straight from `e2b-versions.txt`, set it first, or preflight ends
the session before a single sandbox is created (`e2b backend requires
E2B_API_KEY`, exit code 2):

```bash
export E2B_API_KEY=<your-e2b-api-key>
# Self-hosted HTTPS endpoints also need the local CA:
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem

# Full coverage: pair the interpreter package, because the run_code cases need it.
# Pick a pairing plain pip can resolve: `e2b-code-interpreter` declares
# `e2b>=2.26.0,<3.0.0`, so the 2.21.0 row in `e2b-versions.txt` installs only
# under a constraint override (that row documents it) and is the wrong thing to
# paste into a first run.
pip install 'e2b==2.26.0' 'e2b-code-interpreter==2.8.1'
pytest --run-e2e --sdk-e2e-backends=e2b -m "smoke or p0"

# Core SDK only: the interpreter-dependent cases MUST be deselected. The e2b
# backend declares the run_code capability unconditionally, so without the
# deselect they are collected, call Sandbox.run_code on the plain SDK, and fail
# for a missing package rather than a real incompatibility.
#
# Mind the scope, because it decides whether the deselect does anything. Inside
# `smoke or p0` the only interpreter-dependent cases are in cases/run_code/,
# which carry both markers, so the two spellings select the same set there and
# the choice is free. (No count here on purpose — run the diff below at this
# scope if you want the current one.) It stops being free the moment the
# selection widens — a core-only `p0 or p1` run (the dual-backend regression
# documented above) collects the p1 lifecycle cases, and only
# `requires_code_interpreter` deselects them.
#
# Deselect on `requires_code_interpreter`, not on the `run_code` marker. The
# run_code marker lives only in cases/run_code/, while interpreter-dependent
# cases also sit in cases/lifecycle/ (test_pause_resume.py, test_auto_lifecycle.py
# and test_rollback_clone.py at the time of writing) carrying the capability
# marker but not the run_code one. `requires_code_interpreter` is the lever that
# does not depend on which file a case happens to live in — which is the point,
# since new cases keep arriving. (It is a dedicated marker gated on the
# CODE_INTERPRETER capability, which the suite prefers over the generic
# `requires_capability(CODE_INTERPRETER)` — that spelling would gate identically,
# but the dedicated one gives a clearer skip message and is what the -m
# expressions here select on.)
#
# To see the current gap between the two spellings rather than trusting a number
# written here (that number rots — this note previously carried a stale one):
#
#   diff <(pytest --collect-only -q -m "(p0 or p1) and not run_code") \
#        <(pytest --collect-only -q -m "(p0 or p1) and not requires_code_interpreter")
#
# Not every case in that gap would actually fail on a missing package: for the
# e2b backend the auto_lifecycle ones need the platform_lifecycle capability and
# the rollback_clone ones need rollback_clone, neither of which the backend
# declares, so the fixture skips them first. Deselecting on
# `requires_code_interpreter`
# covers all of them regardless.
pip install 'e2b==2.29.5'
pytest --run-e2e --sdk-e2e-backends=e2b -m "(smoke or p0) and not requires_code_interpreter"
```

Repeat per version in a fresh virtualenv, and record the outcome in
`e2b-versions.txt` when cutting a release.

Notes that make the matrix more than a version list:

- **A version string is not a compatibility signal.** `e2b` 2.26/2.29
  `commands.run` hung silently against envd on v0.5.1-rc5 (the SDK's own timeout
  did not fire), and worked on the v0.5.1 release — while envd self-reported
  `0.5.11` on both builds. Compatibility therefore has to be stated per
  CubeSandbox release, not derived from envd's version.
- **`e2b-code-interpreter` versions its own way.** Its 2.9.0 requires
  `e2b>=2.26.0,<3.0.0`, so a row pinning an older `e2b` cannot also take the
  current interpreter package — the `run_code` cases are unavailable there.
- **Cases that need the interpreter.** Lines without `e2b-code-interpreter`
  should deselect on `requires_code_interpreter`
  (`-m "(smoke or p0) and not requires_code_interpreter"`) rather than report
  failures that are really a missing package. The `run_code` marker is not
  enough — it only covers `cases/run_code/`, while `cases/lifecycle/` has
  interpreter-dependent cases that carry `requires_code_interpreter` but not the
  `run_code` one. Note the suite gates this through the dedicated
  `requires_code_interpreter` marker rather than the generic
  `requires_capability(CODE_INTERPRETER)`, which is why the `-m` expressions
  select on the former.

## Environment

The suite automatically loads `tests/e2e/sdk_compat/.env` if the file exists.
Values already exported in the shell take precedence over `.env` values. Copy
`env.example` to `.env` for local runs:

```bash
cp env.example .env
```

The built-in `.env` loader is intentionally small: it supports one `KEY=VALUE`
entry per line and simple single/double quoted values. It does not support
multiline quoted values. Export multiline secrets, private keys, or complex
values in the shell instead of placing them in `.env`.

Required:

- `CUBE_API_URL`: CubeAPI endpoint.
- `CUBE_TEMPLATE_ID`: ready template ID used for sandbox creation.

Optional:

- `SDK_E2E_BACKENDS`: comma-separated backend list. Defaults to `cubesandbox`.
- `CUBE_API_KEY`: API key if the target environment requires one.
- `E2B_API_KEY`: required when running the `e2b` backend. For a self-hosted
  CubeSandbox endpoint, use the API key accepted by that endpoint.
- `SDK_E2E_E2B_VALIDATE_API_KEY`: enable the E2B SDK's client-side `e2b_*`
  API key format check. Defaults to `false` for self-hosted deployments that
  issue keys in another format. Server-side authentication remains enabled.
- `CUBE_PROXY_NODE_IP`: useful when wildcard sandbox DNS is unavailable from the runner.
- `CUBE_PROXY_PORT_HTTP`: defaults to `80`.
- `CUBE_SANDBOX_DOMAIN`: defaults to `cube.app`.
- `SDK_E2E_DEFAULT_TIMEOUT`: default timeout for operations such as explicit
  connect and cleanup resume. Defaults to `120`.
- `SDK_E2E_API_TIMEOUT`: CubeAPI control-plane request timeout in seconds for
  preflight, diagnostics, and cleanup. Defaults to `5`.
- `SDK_E2E_CREATE_TIMEOUT`: sandbox create timeout in seconds. Defaults to `120`.
- `SDK_E2E_CREATE_CAPACITY_RETRIES`: extra sandbox-create attempts when the
  scheduler transiently returns `no more resource` (error code `130597`),
  giving the just-freed node time to be reclaimed. Defaults to `5`. Set to `0`
  to disable and fail fast on the first capacity error.
- `SDK_E2E_CREATE_CAPACITY_BACKOFF`: base backoff in seconds for capacity
  retries; grows exponentially per attempt, with full jitter so parallel
  workers do not retry in lockstep. Defaults to `2`.
- `SDK_E2E_CREATE_CAPACITY_BACKOFF_MAX`: cap on the capacity-retry backoff in
  seconds. Defaults to `30`. A value `<= 0` disables the per-delay cap (delays
  then grow up to an internal `3600`s ceiling) — it does **not** mean "no
  backoff", so keep it positive unless you intend uncapped growth.
- `SDK_E2E_CREATE_CAPACITY_BUDGET`: total wall-clock seconds spent **sleeping**
  across all capacity retries for a single create. Defaults to `90`. Set to `0`
  to disable and rely on `RETRIES` alone. This caps only the cumulative backoff
  sleep, not the `create()` calls themselves: each attempt can still run up to
  `SDK_E2E_CREATE_TIMEOUT`, so a slow-to-reject scheduler can take up to roughly
  `(RETRIES + 1) × CREATE_TIMEOUT + BUDGET` per test. On the fast-rejection path
  (immediate HTTP 500) the budget effectively bounds per-create retry time.
- `SDK_E2E_COMMAND_TIMEOUT`: command timeout in seconds. Defaults to `30`.
- `SDK_E2E_RUN_CODE_TIMEOUT`: code execution timeout in seconds. Defaults to `60`.
- `SDK_E2E_NETWORK_PROBE_TIMEOUT`: TCP probe socket timeout in seconds for
  network policy cases. Defaults to `5`.
- `SDK_E2E_TCP_TARGET_IP`: primary public TCP probe address. Defaults to
  `8.8.8.8`.
- `SDK_E2E_TCP_TARGET_PORT`: public TCP probe port. Defaults to `53`.
- `SDK_E2E_ALTERNATE_TCP_TARGET_IP`: alternate public TCP probe address.
  Defaults to `1.1.1.1`.
- `SDK_E2E_PUBLIC_ACCESS_PORT`: exposed HTTP port used by restricted public
  access inbound tests. Defaults to `49983`.
- `SDK_E2E_PUBLIC_ACCESS_PATH`: path used by restricted public access inbound
  tests. Defaults to `/health`.
- `SDK_E2E_PUBLIC_ACCESS_EXPECTED_STATUS`: expected successful HTTP status for
  restricted public access inbound tests. Defaults to `204`.
- `SDK_E2E_PUBLIC_ACCESS_EXPECTED_BODY`: expected successful response body for
  restricted public access inbound tests. Defaults to an empty string.
  The default public URL uses HTTP, so traffic access tokens are sent in
  cleartext. Use an HTTPS endpoint for cross-network or multi-tenant CI.
- `SDK_E2E_KEEP_SANDBOX_ON_FAILURE`: preserve only sandboxes whose test setup
  or call phase failed. Passed and skipped tests are still cleaned up. Defaults
  to `false`.
- `SDK_E2E_TRACE`: print every SDK adapter operation and include traces for
  passed tests in JSONL. Equivalent to `--sdk-e2e-trace`. Defaults to `false`.
- `SDK_E2E_SKIP_INTERNET_TESTS`: skip tests marked `requires_internet` when
  the runner or environment has no stable public egress. Defaults to `false`.
- `SDK_E2E_REPORT_DIR`: JSONL report directory. Defaults to `reports/sdk-dual`.
- `SDK_E2E_WORKERS`: pytest-xdist worker count for `--run-e2e`. Parallelism is
  opt-in; unset (or `0`/`1`/`no`/`off`) runs serial to avoid overloading the
  co-located control plane. Set an integer, `auto`, or `logical` to fan out. An
  explicit `-n`/`--numprocesses` (or `-p no:xdist`) always wins. Ignored without
  `--run-e2e`, so the hermetic `framework` gate stays serial.
- `SDK_E2E_TEMPLATE_BUILD_CONCURRENCY`: max concurrent live template builds
  across xdist workers. Defaults to `1` (builds fully serialized so results match
  a serial run); values `< 1` or non-integer fall back to `1`. When the value is
  at least the worker count the throttle is skipped. POSIX-only (a no-op without
  `fcntl`). The throttle is namespaced per-UID, not per-run: two concurrent
  `--run-e2e` jobs of the same user on one host share the slots and serialize
  their builds against each other. This is intentional -- both jobs contend on
  the one shared build host -- and the `SDK_E2E_TEMPLATE_BUILD_WAIT` ceiling
  bounds how long a job waits before degrading to unthrottled.
- `SDK_E2E_TEMPLATE_BUILD_WAIT`: per-predecessor ceiling (seconds) on waiting for
  a build slot before a worker gives up and builds unthrottled, so one wedged
  peer cannot stall the whole suite. The effective wait scales by the worker
  count. Defaults to `1800`; `<= 0` waits forever.
- `CUBE_PYTHON_SDK_PATH`: override local CubeSandbox Python SDK path.
- `SDK_E2E_PLATFORM_LIFECYCLE`: enable platform-managed lifecycle cases
  (`auto-pause`, `auto-resume`, `auto-kill`). Defaults to `false`.
- `SDK_E2E_PLATFORM_LIFECYCLE_IDLE_TIMEOUT`: idle timeout in seconds for
  platform lifecycle cases. Defaults to `30`.
- `SDK_E2E_PLATFORM_LIFECYCLE_WAIT_MARGIN`: extra seconds to wait after the
  idle timeout for the lifecycle sweeper. Defaults to `20`.
- `SDK_E2E_PLATFORM_LIFECYCLE_POLL_TIMEOUT`: extra polling window after the
  initial wait. Defaults to `45`.
- `CUBE_PROXY_ADMIN_PORT`: CubeProxy admin port used by the lifecycle probe.
  Defaults to `8082`.
- `SDK_E2E_VOLUME_PLUGIN`: enable Volume Plugin cases (CRUD and sandbox
  `volumeMounts` bind/unbind). Defaults to `false`.
- `SDK_E2E_VOLUME_DRIVER`: driver name for `POST /volumes`. Defaults to `cos`.
- `SDK_E2E_VOLUME_REFCOUNT_WAIT`: seconds to wait for delete-while-bound `409`
  and post-unbind `204`. Defaults to `60`.

For self-hosted HTTPS sandbox endpoints, trust the local CA:

```bash
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

The E2B backend does not disable TLS verification. Self-hosted environments must
provide a trusted CA via `SSL_CERT_FILE` or the system trust store.

## Preflight

When `--run-e2e` is enabled, a session preflight runs once before per-test
sandbox creation. It checks:

- `CUBE_TEMPLATE_ID` or `--cube-template-id` is present.
- `GET /health` on `CUBE_API_URL` is reachable.
- `GET /templates/{template_id}` returns the selected template.
- If the template response exposes `status` or `state`, it is ready-like:
  `ready`, `active`, or `available`.

Preflight failures are recorded as `preflight_failed` and stop the run early with
a single diagnostic message. When `SDK_E2E_PLATFORM_LIFECYCLE=true`, preflight also
probes CubeProxy admin health (`heartbeat_last_pushed_ms`) when
`CUBE_PROXY_NODE_IP` is set.

## Reporting

The suite writes JSONL events to `SDK_E2E_REPORT_DIR`. A serial run writes a
single `events.jsonl`; under pytest-xdist each worker writes its own
`events-gw0.jsonl`, `events-gw1.jsonl`, ... to avoid interleaved lines, so read
or aggregate `events*.jsonl` rather than a fixed `events.jsonl`.

To generate a standard HTML report, pass pytest-html options explicitly:

```bash
pytest --run-e2e -m lifecycle \
  --html=reports/sdk-dual/report.html \
  --self-contained-html
```

To generate a JUnit XML report for CI systems:

```bash
pytest --run-e2e -m lifecycle \
  --junit-xml=reports/sdk-dual/junit.xml
```

Event types:

- `preflight_passed` / `preflight_failed`: live environment readiness.
- `sandbox_created`: backend, sandbox ID, and pytest node ID.
- `sandbox_cleanup` / `sandbox_kept`: teardown outcome.
- `test_result`: pytest phase, outcome, duration, backend, sandbox ID, and failure diagnostics.

Failed `test_result` events include `error` and best-effort `sandbox_info` when
available. They also include a bounded SDK operation trace with create/connect,
command, code, file, lifecycle, and cleanup calls. Sensitive keys and environment
values are redacted, large strings and collections are truncated, and file
contents are represented by length rather than plaintext.

Failed tests automatically print the most recent SDK operations to the terminal.
For live input/output tracing of every operation, use:

```bash
pytest --run-e2e --sdk-e2e-trace \
  cases/lifecycle/test_pause_resume.py::test_pause_sets_state_paused

# Equivalent environment form
SDK_E2E_TRACE=true pytest --run-e2e -m lifecycle
```

Trace mode may expose non-secret command/code output in the terminal. JSONL
redaction remains enabled in both normal and trace modes.

## Layout

```text
tests/e2e/sdk_compat/
  adapters/      # SDK-specific shims over a shared adapter interface
  framework/     # config, preflight, capability flags, cleanup, reporting
  cases/         # backend-neutral cases split by capability domain
  reports/       # local JSONL events, ignored except reports/.gitignore
  e2b-versions.txt  # E2B SDK versions this suite has been validated against
```

Current capability domains:

- `cases/lifecycle/`: create/info smoke, connect, create options, pause/resume,
  kill, and platform-managed auto-pause/auto-resume/auto-kill coverage.
- `cases/commands/`: stdout, stderr, exit code, env, special characters, multiline output, missing command.
- `cases/filesystem/`: read/write, overwrite, multiline content, file API and shell interoperability.
- `cases/run_code/`: expression text, stdout, kernel state, Python error reporting.
- `cases/network/`: create-time network policy for allow/deny and public egress access,
  plus in-place policy updates on a running sandbox including re-evaluation of
  already-established connections (`test_policy_update.py`, CubeSandbox only).
- `cases/concurrency/`: simultaneous multi-sandbox isolation.
- `cases/host-mount/`: host-directory mount extension — happy path plus create-time
  validation, runtime bind-mount failures, and cross-sandbox sharing boundary cases.
- `cases/volume/`: Volume Plugin CRUD plus sandbox `volumeMounts` bind/unbind and per-sandbox read-only attachment enforcement (opt-in via `SDK_E2E_VOLUME_PLUGIN=true`; CubeSandbox only). Requires a manually deployed/configured Volume Plugin and `cubesandbox` >= 0.6.0.
- `cases/auth/`: `CUBE_API_KEY` simple-key authentication against the CubeAPI
  control plane — `X-API-Key`/`Bearer` accept, wrong/missing 401, `/health`
  exempt (CubeSandbox only). Skipped unless the server was started with
  `CUBE_API_KEY` and the same key is exported for the runner.

Keep new cases backend-neutral. Add backend-specific behavior through capability
markers instead of branching inside test bodies. Future domains can be added next
to the existing directories, for example `proxy/`, `metadata/`, and `errors/`.

## Markers And Capabilities

Priority markers:

- `smoke`: minimum live-environment checks.
- `p0`: PR-gate compatibility coverage.
- `p1`: daily compatibility regression.
- `p2`: weekly or broader feature coverage.
- `p3`: release qualification and long-running scenarios.
- `slow`: tests that exceed the normal PR time budget.

Capability markers:

- `@pytest.mark.requires_capability("<name>")`: skip or deselect unsupported backends.
- `@pytest.mark.sandbox_create_options(...)`: pass SDK create-time options such as `network`, `env_vars`, or `lifecycle`.
- `@pytest.mark.requires_cubeproxy`: platform lifecycle cases that depend on cube-proxy and lifecycle-manager coordination. Skipped unless `SDK_E2E_PLATFORM_LIFECYCLE=true`.
- `@pytest.mark.volume`: Volume Plugin cases. Skipped unless `SDK_E2E_VOLUME_PLUGIN=true`.
- `@pytest.mark.auth`: `CUBE_API_KEY` simple-key auth cases. Skipped unless `CUBE_API_KEY` is set for the runner and the backend supports `auth_simple_key` (CubeSandbox only).
- Common capabilities include `lifecycle`, `commands`, `filesystem`,
  `filesystem_extended`, and `run_code`.
- Optional capabilities include `code_interpreter`, `pause_resume`, `set_timeout`,
  `rollback_clone`, `network_allow_deny`, `network_public_access`,
  `network_mask_request_host`, `platform_lifecycle`, `host_mount`,
  `volume_plugin`, and `auth_simple_key`.
- `platform_lifecycle` is available only to CubeSandbox platform-managed lifecycle cases.
- `host_mount` is a CubeSandbox-only extension; `cases/host-mount/` uses it via
  `@pytest.mark.requires_capability("host_mount")` to skip backends (e.g. e2b) that
  do not support host-directory mounts.
- `volume_plugin` is available only to CubeSandbox Volume Plugin cases.

## Cleanup

Each test creates its own sandbox and destroys it in teardown. If SDK teardown
fails, the suite falls back to `DELETE /sandboxes/{sandboxID}` against
`CUBE_API_URL`.

Set `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` to preserve sandboxes while debugging.
It only preserves sandboxes of *failed* tests created through the `sdk_sandbox`
fixture; passed and skipped tests are always cleaned up, and boundary tests that
create sandboxes directly (via their own helpers) always clean up regardless.
