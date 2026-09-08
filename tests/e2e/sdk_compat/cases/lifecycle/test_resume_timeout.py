# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from datetime import datetime, timezone

import pytest
from cubesandbox import NEVER_TIMEOUT
from cubesandbox._exceptions import ApiError
from framework.capabilities import PAUSE_RESUME, SET_TIMEOUT
from framework.lifecycle import (
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
    pytest.mark.requires_capability(SET_TIMEOUT),
]


def _require_cubesandbox(sdk_backend: str) -> None:
    if sdk_backend != "cubesandbox":
        pytest.skip(
            "resume idle-timeout semantics are CubeSandbox-specific, "
            f"got {sdk_backend!r}"
        )


def _optional_end_at(raw: dict) -> datetime | None:
    value = raw.get("endAt") or raw.get("end_at")
    if not value:
        return None
    deadline = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    if deadline.tzinfo is None:
        deadline = deadline.replace(tzinfo=timezone.utc)
    return deadline


def _pause(sdk_sandbox, sdk_e2e_config) -> None:
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)


@pytest.mark.sandbox_create_options(timeout=180)
def test_resume_never_timeout(sdk_sandbox, sdk_backend, sdk_e2e_config):
    """resume(NEVER_TIMEOUT) marks the sandbox as never expiring."""
    _require_cubesandbox(sdk_backend)
    _pause(sdk_sandbox, sdk_e2e_config)

    resumed = sdk_sandbox.resume_idle_timeout(NEVER_TIMEOUT)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        assert _optional_end_at(resumed.info().raw) is None, (
            "resume(NEVER_TIMEOUT) should clear endAt"
        )
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
    finally:
        resumed.close()


def test_resume_invalid_timeout_rejected(sdk_sandbox, sdk_backend, sdk_e2e_config):
    """resume(timeout < -1) is rejected with 400 and leaves the sandbox paused."""
    _require_cubesandbox(sdk_backend)
    _pause(sdk_sandbox, sdk_e2e_config)

    with pytest.raises(ApiError) as exc:
        sdk_sandbox.resume_idle_timeout(-2)
    assert exc.value.status_code == 400

    state = wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert state == "paused", "rejected resume must not change sandbox state"

    # connect() omits idle TTL; this only gets the sandbox running again so
    # fixture teardown can kill it.
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        assert resumed.sandbox_id == sdk_sandbox.sandbox_id
    finally:
        resumed.close()


@pytest.mark.sandbox_create_options(timeout=180)
def test_resume_zero_preserves_timeout(sdk_sandbox, sdk_backend, sdk_e2e_config):
    """resume(timeout=0) is a no-op on the stored idle TTL: endAt must not move."""
    _require_cubesandbox(sdk_backend)
    before = _optional_end_at(sdk_sandbox.info().raw)
    assert before is not None, "create(timeout=180) should expose endAt"

    _pause(sdk_sandbox, sdk_e2e_config)
    resumed = sdk_sandbox.resume_idle_timeout(0)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        after = _optional_end_at(resumed.info().raw)
        assert after is not None, "resume(timeout=0) must preserve the stored TTL"

        # publishUpdateTimeout no-ops on 0; pause/resume only publishes state.
        # Stored EndAt is unchanged, so this must be ~0. A rebase to now+180
        # moves EndAt by the pause+resume duration (often >1s).
        drift = abs((after - before).total_seconds())
        assert drift < 1, (
            "resume(timeout=0) must not move endAt; "
            f"before={before.isoformat()} after={after.isoformat()} drift={drift:.1f}s"
        )
    finally:
        resumed.close()
