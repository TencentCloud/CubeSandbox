# SDK Compatibility E2E Tests

This directory contains live end-to-end tests for Python SDK compatibility. The
same backend-neutral cases run against:

- `cubesandbox`: the CubeSandbox Python SDK from `sdk/python`.
- `e2b`: the E2B Python SDK (`e2b-code-interpreter` or `e2b`) against a CubeSandbox-compatible backend.

The suite is opt-in. Without `--run-e2e`, pytest collection is safe and all cases
are skipped. Live runs default to the `cubesandbox` backend so PR-gate runs stay
small and stable. Use `SDK_E2E_BACKENDS=e2b,cubesandbox` for dual-SDK
compatibility runs.

## Quick Start

```bash
cd tests/e2e/sdk_compat
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

export CUBE_API_URL=http://10.0.1.5:3000
export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
export CUBE_PROXY_NODE_IP=10.0.1.2

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

# Broader regression
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e -m "p0 or p1 or p2"
```

Run dual backend after installing E2B:

```bash
pip install e2b-code-interpreter
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
export SDK_E2E_BACKENDS=e2b,cubesandbox
pytest --run-e2e
```

## Environment

Required:

- `CUBE_API_URL`: CubeAPI endpoint.
- `CUBE_TEMPLATE_ID`: ready template ID used for sandbox creation.

Optional:

- `SDK_E2E_BACKENDS`: comma-separated backend list. Defaults to `cubesandbox`.
- `CUBE_API_KEY`: API key if the target environment requires one.
- `CUBE_PROXY_NODE_IP`: useful when wildcard sandbox DNS is unavailable from the runner.
- `CUBE_PROXY_PORT_HTTP`: defaults to `80`.
- `CUBE_SANDBOX_DOMAIN`: defaults to `cube.app`.
- `SDK_E2E_CREATE_TIMEOUT`: sandbox create timeout in seconds. Defaults to `120`.
- `SDK_E2E_COMMAND_TIMEOUT`: command timeout in seconds. Defaults to `30`.
- `SDK_E2E_RUN_CODE_TIMEOUT`: code execution timeout in seconds. Defaults to `60`.
- `SDK_E2E_KEEP_SANDBOX_ON_FAILURE`: preserve failed sandboxes for debugging. Defaults to `false`.
- `SDK_E2E_REPORT_DIR`: JSONL report directory. Defaults to `reports/sdk-dual`.
- `CUBE_PYTHON_SDK_PATH`: override local CubeSandbox Python SDK path.

For self-hosted HTTPS sandbox endpoints, prefer trusting the local CA:

```bash
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
export SDK_E2E_E2B_INSECURE_TLS=false
```

If the local CA is unavailable, `SDK_E2E_E2B_INSECURE_TLS=true` disables TLS
certificate verification for the E2B SDK sandbox transport. Use it only for local
test environments.

## Preflight

When `--run-e2e` is enabled, a session preflight runs once before per-test
sandbox creation. It checks:

- `CUBE_TEMPLATE_ID` or `--cube-template-id` is present.
- `GET /health` on `CUBE_API_URL` is reachable.
- `GET /templates/{template_id}` returns the selected template.
- If the template response exposes `status` or `state`, it is ready-like:
  `ready`, `active`, or `available`.

Preflight failures are recorded as `preflight_failed` and stop the run early with
a single diagnostic message.

## Reporting

The suite writes JSONL events to `SDK_E2E_REPORT_DIR/events.jsonl`.

Event types:

- `preflight_passed` / `preflight_failed`: live environment readiness.
- `sandbox_created`: backend, sandbox ID, and pytest node ID.
- `sandbox_cleanup` / `sandbox_kept`: teardown outcome.
- `test_result`: pytest phase, outcome, duration, backend, sandbox ID, and failure diagnostics.

Failed `test_result` events include `error` and best-effort `sandbox_info` when
available.

## Layout

```text
tests/e2e/sdk_compat/
  adapters/      # SDK-specific shims over a shared adapter interface
  framework/     # config, preflight, capability flags, cleanup, reporting
  cases/         # backend-neutral cases split by capability domain
  reports/       # local JSONL events, ignored except reports/.gitignore
```

Current capability domains:

- `cases/lifecycle/`: create/info smoke checks, kill, set_timeout, pause/resume coverage.
- `cases/commands/`: stdout, stderr, exit code, env, cwd, special characters, multiline and large output, missing command.
- `cases/filesystem/`: read/write, overwrite, multiline, exists, list, make_dir, remove, file API and shell interoperability.
- `cases/run_code/`: expression text, stdout, kernel state, multiline blocks, string/list output, imports, Python error reporting.
- `cases/metadata/`: env var propagation, cross-API metadata interoperability.
- `cases/errors/`: sandbox lifecycle error handling (info after kill, double-kill idempotency).
- `cases/concurrency/`: sequential reuse and parallel sandbox isolation.

Keep new cases backend-neutral. Add backend-specific behavior through capability
markers instead of branching inside test bodies. Future domains can be added next
to the existing directories, for example `network/` and `proxy/`.

## Markers And Capabilities

Priority markers:

- `smoke`: minimum live-environment checks.
- `p0`: PR-gate compatibility coverage.
- `p1`: daily compatibility regression.
- `p2`: weekly or broader feature coverage.
- `p3`: release qualification and long-running scenarios.

Capability markers:

- Common capabilities include `lifecycle`, `commands`, `filesystem`, `run_code`, and `metadata`.
- CubeSandbox-specific capabilities currently include `pause_resume`, `set_timeout`, `network_policy`, and `proxy_url`.

## Cleanup

Each test creates its own sandbox and destroys it in teardown. If SDK teardown
fails, the suite falls back to `DELETE /sandboxes/{sandboxID}` against
`CUBE_API_URL`.

Set `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` to preserve sandboxes while debugging.
