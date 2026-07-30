# SDK E2E Case Reference

## Domain Mapping

Use existing domains unless the new behavior has a clear API or capability boundary.

```text
commands/      command stdout/stderr, exit code, timeout, environment
concurrency/   multi-sandbox isolation and concurrent behavior
filesystem/    file API behavior and shell interoperability
host-mount/    host-directory mount extension, allowlist, and boundary behavior
lifecycle/     create, connect, pause/resume, kill, platform lifecycle
network/       network policy, public access, traffic access token
run_code/      Code Interpreter output, errors, stateful kernel behavior
```

## Module Marker Template

```python
import pytest

from framework.capabilities import COMMANDS

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
    pytest.mark.commands,
    pytest.mark.requires_capability(COMMANDS),
]
```

Common priority and environment markers:

- `smoke`: minimum live environment validation.
- `p0`: stable PR-gate coverage.
- `p1`: daily regression coverage.
- `p2` / `p3`: broader, slower, or release qualification coverage.
- `slow`: longer than the normal PR budget.
- `requires_internet`: public egress is required.
- `requires_cubeproxy`: CubeProxy routing or platform lifecycle is required.
- `requires_code_interpreter`: Code Interpreter/Jupyter template is required.
- `host_mount`: host-directory mount extension; pair with `pytest.mark.requires_capability("host_mount")`.

## Template and Create Options

Use the default `CUBE_TEMPLATE_ID` unless a case needs a specific template capability.

```python
@pytest.mark.sandbox_template_id("tpl-code-interpreter-xxxxxxxx")
@pytest.mark.requires_code_interpreter
def test_kernel_state(sdk_sandbox):
    ...
```

Use `sandbox_create_options` for backend-neutral create options that are part of the contract under test.

```python
pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
    pytest.mark.lifecycle,
    pytest.mark.sandbox_create_options(timeout=120),
]
```

## Authoring Checklist

- [ ] The behavior is assigned to the correct domain.
- [ ] The test name describes the contract and expected outcome.
- [ ] Shared behavior is not guarded by backend-specific branches.
- [ ] Unsupported prerequisites use capability or environment markers.
- [ ] The case uses existing fixtures and helpers before introducing new ones.
- [ ] Assertions include useful diagnostics without exposing secrets.
- [ ] The case has deterministic cleanup through existing fixtures.
- [ ] External dependencies are explicitly marked and configurable.
- [ ] New environment variables are added to `env.example` and README docs.
- [ ] New markers are registered in `pytest.ini`.

## Review Checklist

- [ ] Does this test fail for the right product bug?
- [ ] Does it avoid broad exception swallowing?
- [ ] Does it avoid fixed sleeps when polling is needed?
- [ ] Does it keep live E2E cost appropriate for its priority marker?
- [ ] Does it avoid logging `CUBE_API_KEY`, `E2B_API_KEY`, traffic access tokens, or sensitive infrastructure details?
- [ ] Does it preserve the distinction between setup/call failures and teardown cleanup failures?
- [ ] Does documentation match the actual command, marker, and environment behavior?

## Execution Commands

Run collection after any structural or marker change:

```bash
cd tests/e2e/sdk_compat
pytest --collect-only -q
```

Run focused live validation for a changed file:

```bash
pytest --run-e2e cases/lifecycle/test_create.py -q
pytest --run-e2e cases/network/test_policy.py -q
```

Run selected priorities:

```bash
pytest --run-e2e -m "smoke or p0"
pytest --run-e2e -m "p1 and not slow"
```

Run dual-backend compatibility only when both SDK environments are configured:

```bash
SDK_E2E_BACKENDS=e2b,cubesandbox pytest --run-e2e -q
```

Useful debug options:

```bash
SDK_E2E_TRACE=true pytest --run-e2e cases/lifecycle/test_create.py -q
SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true pytest --run-e2e cases/network/test_policy.py -q
```

## Output Expectations

When reporting results to the user, include:

- Files changed.
- Validation commands run and whether they passed.
- Any live E2E commands that were not run, with the reason.
- Remaining risks, especially environment assumptions or backend-specific gaps.
