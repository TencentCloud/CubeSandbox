# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from datetime import datetime, timezone

import pytest

from adapters import connect_adapter
from framework.assertions import assert_command_ok
from framework.capabilities import LIFECYCLE, PAUSE_RESUME
from framework.lifecycle import wait_until_paused, wait_until_running

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]

# E2B keeps the existing timeout when a running sandbox receives a shorter
# value; use a longer explicit value so this shared case is valid for both SDKs.
_EXPLICIT_TIMEOUT = 180


def _assert_timeout_visible(
    adapter,
    requested_timeout: int,
) -> None:
    raw = adapter.info().raw
    returned_timeout = raw.get("timeout")
    end_at = raw.get("endAt") or raw.get("end_at")

    if returned_timeout is not None:
        assert int(returned_timeout) == requested_timeout

    assert end_at or returned_timeout is not None, (
        "sandbox info must expose either timeout or endAt to verify the explicit "
        f"connect timeout; raw={raw!r}"
    )

    if not end_at:
        return

    try:
        deadline = datetime.fromisoformat(str(end_at).replace("Z", "+00:00"))
    except ValueError as exc:
        raise AssertionError(f"invalid endAt returned by sandbox info: {end_at!r}") from exc
    if deadline.tzinfo is None:
        deadline = deadline.replace(tzinfo=timezone.utc)

    remaining_seconds = (deadline - datetime.now(timezone.utc)).total_seconds()
    assert requested_timeout - 15 <= remaining_seconds <= requested_timeout + 15, (
        f"endAt is not consistent with explicit connect timeout={requested_timeout}s: "
        f"remaining={remaining_seconds:.1f}s, endAt={end_at!r}"
    )


def test_connect_existing_sandbox_preserves_id(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id
    sdk_sandbox.write_file("/tmp/sdk-compat-connect.txt", "connect-marker")

    connected = connect_adapter(sdk_backend, sandbox_id, sdk_e2e_config)
    try:
        assert connected.sandbox_id == sandbox_id
        assert connected.read_file("/tmp/sdk-compat-connect.txt") == "connect-marker"
    finally:
        connected.close()


def test_connect_existing_sandbox_allows_commands(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sandbox_id = sdk_sandbox.sandbox_id

    connected = connect_adapter(sdk_backend, sandbox_id, sdk_e2e_config)
    try:
        result = connected.run_command(
            "printf connected",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "connected"
    finally:
        connected.close()


@pytest.mark.sandbox_create_options(timeout=120)
def test_connect_existing_running_sandbox_applies_explicit_timeout(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    connected = connect_adapter(
        sdk_backend,
        sdk_sandbox.sandbox_id,
        sdk_e2e_config,
        timeout=_EXPLICIT_TIMEOUT,
    )
    try:
        _assert_timeout_visible(
            connected,
            _EXPLICIT_TIMEOUT,
        )
    finally:
        connected.close()


@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.sandbox_create_options(
    timeout=120,
    lifecycle={"on_timeout": "pause", "auto_resume": False},
)
def test_connect_paused_sandbox_applies_explicit_timeout(
    sdk_sandbox,
    sdk_e2e_config,
):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    assert wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout) == "paused"

    connected = sdk_sandbox.resume_or_connect(timeout=_EXPLICIT_TIMEOUT)
    try:
        assert wait_until_running(connected, timeout=sdk_e2e_config.default_timeout) == "running"
        _assert_timeout_visible(
            connected,
            _EXPLICIT_TIMEOUT,
        )
    finally:
        connected.close()
