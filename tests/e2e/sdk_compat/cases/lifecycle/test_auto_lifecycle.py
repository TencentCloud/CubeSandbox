# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_code_ok, assert_command_ok
from framework.capabilities import LIFECYCLE, PAUSE_RESUME, PLATFORM_LIFECYCLE, RUN_CODE
from framework.lifecycle import (
    PLATFORM_LIFECYCLE_SKIP_REASON,
    fetch_state,
    idle_past_timeout,
    managed_control_sandbox,
    sandbox_listed,
    wait_for_platform_destroy,
    wait_for_platform_pause,
    wait_until_paused,
    wait_until_running,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.slow,
    pytest.mark.requires_cubeproxy,
    pytest.mark.requires_capability(LIFECYCLE),
]

_SEED_CODE = """\
hash_val = "sdk-compat-auto-lifecycle"
pi_approx = 3.1415926535
print(f"hash_val={hash_val}")
print(f"pi_approx={pi_approx:.10f}")
"""
_RESUME_CODE = """\
print(f"hash_val={hash_val}")
print(f"pi_approx={pi_approx:.10f}")
"""
_CHECKPOINT = "/tmp/sdk-compat-auto-lifecycle.txt"


def _assert_manual_pause_survives_lifecycle_timeout(
    adapter,
    backend,
    config,
) -> None:
    result = adapter.run_command(
        "printf manual-pause-before-timeout",
        timeout=config.command_timeout,
    )
    assert_command_ok(result)

    adapter.pause(timeout=config.default_timeout)
    assert wait_until_paused(adapter, timeout=config.default_timeout) == "paused"

    idle_past_timeout(
        config.platform_lifecycle_idle_timeout,
        margin=config.platform_lifecycle_wait_margin,
    )

    assert fetch_state(adapter) == "paused"
    assert sandbox_listed(adapter.sandbox_id, backend, config) is True


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_capability(PLATFORM_LIFECYCLE)
@pytest.mark.requires_code_interpreter
@pytest.mark.sandbox_create_options(lifecycle={"on_timeout": "pause", "auto_resume": True})
def test_lifecycle_auto_resume_preserves_state(sdk_sandbox, sdk_e2e_config):
    seed = sdk_sandbox.run_code(_SEED_CODE, timeout=sdk_e2e_config.run_code_timeout)
    assert_code_ok(seed)
    assert any("hash_val=" in line for line in seed.stdout)

    sdk_sandbox.write_file(_CHECKPOINT, "checkpoint-before-idle")
    before_file = sdk_sandbox.read_file(_CHECKPOINT)

    if not wait_for_platform_pause(sdk_sandbox, sdk_e2e_config):
        pytest.skip(PLATFORM_LIFECYCLE_SKIP_REASON)

    resumed = sdk_sandbox.run_code(_RESUME_CODE, timeout=sdk_e2e_config.run_code_timeout)
    assert_code_ok(resumed)
    assert any("hash_val=sdk-compat-auto-lifecycle" in line for line in resumed.stdout)
    assert any("pi_approx=3.1415926535" in line for line in resumed.stdout)

    after_file = sdk_sandbox.read_file(_CHECKPOINT)
    assert after_file == before_file == "checkpoint-before-idle"


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_capability(PLATFORM_LIFECYCLE)
@pytest.mark.requires_code_interpreter
@pytest.mark.sandbox_create_options(
    lifecycle={"on_timeout": "pause", "auto_resume": False},
)
def test_lifecycle_auto_pause_manual_connect_allows_command_and_run_code(
    sdk_sandbox,
    sdk_e2e_config,
):
    seed = sdk_sandbox.run_code(
        "manual_connect_value = 41",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(seed)

    sdk_sandbox.write_file(_CHECKPOINT, "checkpoint-before-manual-connect")
    if not wait_for_platform_pause(sdk_sandbox, sdk_e2e_config):
        pytest.skip(PLATFORM_LIFECYCLE_SKIP_REASON)

    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        assert wait_until_running(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
        ) == "running"

        command = resumed.run_command(
            "printf manual-connect",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(command)
        assert command.stdout == "manual-connect"

        code = resumed.run_code(
            "manual_connect_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(code)
        assert code.text == "42"
        assert resumed.read_file(_CHECKPOINT) == "checkpoint-before-manual-connect"
    finally:
        resumed.close()


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_capability(PLATFORM_LIFECYCLE)
@pytest.mark.requires_code_interpreter
@pytest.mark.sandbox_create_options(
    lifecycle={"on_timeout": "pause", "auto_resume": True}
)
def test_lifecycle_auto_pause_auto_resume_allows_command_and_run_code(
    sdk_sandbox,
    sdk_e2e_config,
):
    seed = sdk_sandbox.run_code(
        "auto_resume_value = 41",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(seed)

    sdk_sandbox.write_file(_CHECKPOINT, "checkpoint-before-auto-resume")
    if not wait_for_platform_pause(sdk_sandbox, sdk_e2e_config):
        pytest.skip(PLATFORM_LIFECYCLE_SKIP_REASON)

    command = sdk_sandbox.run_command(
        "printf auto-resumed",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(command)
    assert command.stdout == "auto-resumed"

    code = sdk_sandbox.run_code(
        "auto_resume_value + 1",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(code)
    assert code.text == "42"
    assert sdk_sandbox.read_file(_CHECKPOINT) == "checkpoint-before-auto-resume"


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_capability(PLATFORM_LIFECYCLE)
@pytest.mark.requires_code_interpreter
@pytest.mark.sandbox_create_options(
    lifecycle={"on_timeout": "pause", "auto_resume": True}
)
def test_lifecycle_auto_resume_is_reentrant(sdk_sandbox, sdk_e2e_config):
    seed = sdk_sandbox.run_code(
        "reentrant_value = 7",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(seed)

    for expected_value, request in (
        ("reentrant-1", lambda: sdk_sandbox.run_command(
            "printf reentrant-1",
            timeout=sdk_e2e_config.command_timeout,
        )),
        ("8", lambda: sdk_sandbox.run_code(
            "reentrant_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )),
    ):
        if not wait_for_platform_pause(sdk_sandbox, sdk_e2e_config):
            pytest.skip(PLATFORM_LIFECYCLE_SKIP_REASON)

        result = request()
        if expected_value.startswith("reentrant-"):
            assert_command_ok(result)
            assert result.stdout == expected_value
        else:
            assert_code_ok(result)
            assert result.text == expected_value


@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.requires_capability(PLATFORM_LIFECYCLE)
@pytest.mark.sandbox_create_options(
    lifecycle={"on_timeout": "pause", "auto_resume": False},
)
def test_manual_pause_before_auto_pause_timeout_remains_paused(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    _assert_manual_pause_survives_lifecycle_timeout(
        sdk_sandbox,
        sdk_backend,
        sdk_e2e_config,
    )


@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.requires_capability(PLATFORM_LIFECYCLE)
@pytest.mark.sandbox_create_options(lifecycle={"on_timeout": "kill"})
def test_manual_pause_before_auto_kill_timeout_remains_paused(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    # TODO: Once auto-kill supports deleting explicitly paused sandboxes,
    # change this regression to expect a terminal state and an absent listing.
    _assert_manual_pause_survives_lifecycle_timeout(
        sdk_sandbox,
        sdk_backend,
        sdk_e2e_config,
    )


@pytest.mark.sandbox_create_options(lifecycle={"on_timeout": "kill"})
def test_lifecycle_auto_kill_makes_sandbox_unusable(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    sandbox_id = sdk_sandbox.sandbox_id
    control_state = "unknown"

    result = sdk_sandbox.run_command(
        "printf auto-kill-seed",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)

    destroyed, details = wait_for_platform_destroy(
        sdk_sandbox,
        sandbox_id,
        sdk_backend,
        sdk_e2e_config,
    )

    with managed_control_sandbox(sdk_backend, sdk_e2e_config) as control:
        control_state = fetch_state(control)
        control_result = control.run_command(
            "printf control-ok",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(control_result)

    if not destroyed:
        pytest.skip(
            f"{PLATFORM_LIFECYCLE_SKIP_REASON}; last_observed={details!r}, "
            f"control_state={control_state!r}"
        )

    assert control_state == "running"
