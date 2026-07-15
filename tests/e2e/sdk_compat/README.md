# SDK Compatibility E2E Tests

This directory contains live end-to-end tests for Python SDK compatibility. The
same backend-neutral cases run against:

- `cubesandbox`: the CubeSandbox Python SDK from `sdk/python`.
- `e2b`: the E2B Python SDK (`e2b-code-interpreter` or `e2b`) against a CubeSandbox-compatible backend.

The suite is opt-in. Without `--run-e2e`, pytest collection is safe and all cases
are skipped. Live runs default to the `cubesandbox` backend so PR-gate runs stay
small and stable. Use `SDK_E2E_BACKENDS=e2b,cubesandbox` for dual-SDK
compatibility runs.

Documentation:

- [Framework design](docs/framework-design.md)
- [Case authoring guide](docs/case-authoring.md)
- [中文 README](README_zh.md)
- [中文框架设计](docs/framework-design.zh-CN.md)
- [中文用例编写指南](docs/zh/case-authoring.md)

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
- `SDK_E2E_COMMAND_TIMEOUT`: command timeout in seconds. Defaults to `30`.
- `SDK_E2E_RUN_CODE_TIMEOUT`: code execution timeout in seconds. Defaults to `60`.
- `SDK_E2E_NETWORK_PROBE_TIMEOUT`: TCP probe socket timeout in seconds for
  network policy cases. Defaults to `5`.
- `SDK_E2E_TCP_TARGET_IP`: primary public TCP probe address. Defaults to
  `8.8.8.8`.
- `SDK_E2E_TCP_TARGET_PORT`: public TCP probe port. Defaults to `53`.
- `SDK_E2E_ALTERNATE_TCP_TARGET_IP`: alternate public TCP probe address.
  Defaults to `1.1.1.1`.
- `SDK_E2E_KEEP_SANDBOX_ON_FAILURE`: preserve only sandboxes whose test setup
  or call phase failed. Passed and skipped tests are still cleaned up. Defaults
  to `false`.
- `SDK_E2E_TRACE`: print every SDK adapter operation and include traces for
  passed tests in JSONL. Equivalent to `--sdk-e2e-trace`. Defaults to `false`.
- `SDK_E2E_SKIP_INTERNET_TESTS`: skip tests marked `requires_internet` when
  the runner or environment has no stable public egress. Defaults to `false`.
- `SDK_E2E_REPORT_DIR`: JSONL report directory. Defaults to `reports/sdk-dual`.
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

The suite writes JSONL events to `SDK_E2E_REPORT_DIR/events.jsonl`.

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
```

Current capability domains:

- `cases/lifecycle/`: create/info smoke, connect, create options, pause/resume,
  kill, and platform-managed auto-pause/auto-resume/auto-kill coverage.
- `cases/commands/`: stdout, stderr, exit code, env, special characters, multiline output, missing command.
- `cases/filesystem/`: read/write, overwrite, multiline content, file API and shell interoperability.
- `cases/run_code/`: expression text, stdout, kernel state, Python error reporting.
- `cases/network/`: create-time network policy for allow/deny and public egress access.
- `cases/concurrency/`: simultaneous multi-sandbox isolation.

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
- Common capabilities include `lifecycle`, `commands`, `filesystem`, and `run_code`.
- Shared optional capabilities include `pause_resume`, `network_allow_deny`, and `network_public_access`.
- `platform_lifecycle` is available only to CubeSandbox platform-managed lifecycle cases.

## Cleanup

Each test creates its own sandbox and destroys it in teardown. If SDK teardown
fails, the suite falls back to `DELETE /sandboxes/{sandboxID}` against
`CUBE_API_URL`.

Set `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` to preserve sandboxes while debugging.
