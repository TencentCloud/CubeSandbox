# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from framework.assertions import assert_command_ok, assert_stdout_contains
from framework.capabilities import LIFECYCLE
from framework.lifecycle import metadata_from_info

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]


def test_create_metadata_visible_in_info(sdk_sandbox):
    metadata = metadata_from_info(sdk_sandbox.info().raw)

    assert metadata.get("test_suite") == "sdk_compat"
    assert metadata.get("test_backend")


@pytest.mark.sandbox_create_options(metadata={"sdk_compat_custom": "lifecycle-metadata"})
def test_create_custom_metadata_visible_in_info(sdk_sandbox):
    metadata = metadata_from_info(sdk_sandbox.info().raw)

    assert metadata.get("sdk_compat_custom") == "lifecycle-metadata"
    assert metadata.get("test_suite") == "sdk_compat"


@pytest.mark.sandbox_create_options(env_vars={"SDK_COMPAT_E2E_VAR": "lifecycle-env"})
def test_create_env_vars_visible_to_command(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        'printf "%s" "$SDK_COMPAT_E2E_VAR"',
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout == "lifecycle-env"


@pytest.mark.sandbox_create_options(timeout=120)
def test_create_timeout_visible_in_info(sdk_sandbox):
    requested_timeout = 120
    observed_at = datetime.now(timezone.utc)
    raw = sdk_sandbox.info().raw

    returned_timeout = raw.get("timeout")
    end_at = raw.get("endAt")

    if returned_timeout is not None:
        assert int(returned_timeout) == requested_timeout

    assert end_at or returned_timeout is not None, (
        "sandbox info must expose either timeout or endAt to verify the "
        f"requested idle timeout; raw={raw!r}"
    )

    if end_at:
        try:
            deadline = datetime.fromisoformat(str(end_at).replace("Z", "+00:00"))
        except ValueError as exc:
            raise AssertionError(f"invalid endAt returned by sandbox info: {end_at!r}") from exc

        if deadline.tzinfo is None:
            deadline = deadline.replace(tzinfo=timezone.utc)

        expected_deadline = observed_at + timedelta(seconds=requested_timeout)
        drift_seconds = abs((deadline - expected_deadline).total_seconds())
        allowed_drift_seconds = max(15, requested_timeout * 0.3)
        assert drift_seconds <= allowed_drift_seconds, (
            f"endAt is not consistent with timeout={requested_timeout}s: "
            f"expected around {expected_deadline.isoformat()}, got {deadline.isoformat()}, "
            f"drift={drift_seconds:.1f}s, allowed={allowed_drift_seconds:.1f}s"
        )


def test_create_command_smoke_after_options(sdk_sandbox, sdk_e2e_config):
    result = sdk_sandbox.run_command(
        "printf lifecycle-options",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert_stdout_contains(result, "lifecycle-options")
