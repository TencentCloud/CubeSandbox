# SDK Compatibility E2E Case Authoring Guide

## Select a domain

Use an existing domain whenever possible:

```text
commands/      command output and exit behavior
filesystem/    file API and shell interoperability
lifecycle/     create, pause, resume, connect, and kill
network/       create-time egress policy
run_code/      interpreter output and kernel state
```

Create a new domain only when the behavior has a distinct API, capability
boundary, or execution scope.

## Use the shared adapter

Do not import a concrete SDK in a shared case:

```python
def test_command_output(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf hello",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "hello"
```

Put backend-specific behavior in `adapters/` and expose unsupported behavior
with `requires_capability`.

## Configure and mark the case

Typical module markers:

```python
pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
]
```

Pass create-time options through the marker:

```python
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_internet
@pytest.mark.sandbox_create_options(
    network={
        "allow_out": ["8.8.8.8/32"],
        "deny_out": ["0.0.0.0/0"],
    }
)
```

Do not hard-code a template ID in a shared case. Use `CUBE_TEMPLATE_ID` or
`--cube-template-id`.

## Assertions and state

Assert observable results and include useful output:

```python
assert_command_ok(result)
assert result.stdout == "expected", (
    f"stdout={result.stdout!r} stderr={result.stderr!r}"
)
```

For lifecycle or kernel cases, seed state before the transition and verify it
afterward:

```python
seed = sdk_sandbox.run_code("value = 41")
assert_code_ok(seed)
sdk_sandbox.write_file("/tmp/checkpoint", "before")

# pause/resume or connect

result = resumed.run_code("value + 1")
assert_code_ok(result)
assert result.text == "42"
assert resumed.read_file("/tmp/checkpoint") == "before"
```

`state == "running"` is a control-plane result, not a data-plane readiness
guarantee. Use `wait_until_running` and record the first data-plane operation
separately when investigating a readiness race.

## Network cases

Use the protocol under test:

- TCP: `socket.connect_ex`;
- UDP/DNS: send a DNS query and wait for a matching response;
- HTTP/HTTPS: assert status code and response output;
- L7: verify host, path, method, SNI, rule order, and injection.

For a strict domain allowlist, combine `allow_out` with
`allow_internet_access=False` or `deny_out=["0.0.0.0/0"]`. A successful TCP
connection alone does not prove an HTTP or L7 policy.

## Lifecycle and cleanup

Platform lifecycle cases normally use `slow` and `requires_cubeproxy`:

```bash
SDK_E2E_PLATFORM_LIFECYCLE=true \
pytest --run-e2e --sdk-e2e-trace cases/lifecycle/test_auto_lifecycle.py
```

Prefer lifecycle helpers over fixed sleeps. Let the fixture clean up the
sandbox. If a test creates a resumed adapter, close it in `finally`:

```python
resumed = sdk_sandbox.resume_or_connect()
try:
    ...
finally:
    resumed.close()
```

## Review checklist

- shared adapter and required capability markers are used;
- assertions are deterministic and include failure context;
- no fixed sandbox ID or cross-test dependency exists;
- setup, call, skip, and cleanup paths are safe;
- the intended backend combinations are collected;
- `pytest --collect-only -q` and the narrowest live scope have been run.
