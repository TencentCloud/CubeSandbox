# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.capabilities import IS_RUNNING, PAUSE_RESUME
from framework.exceptions import UnsupportedCapability
from framework.lifecycle import wait_until, wait_until_paused

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(IS_RUNNING),
]


def _probe(adapter) -> bool:
    """Return adapter.is_running(), or skip the test if the backend lacks it."""
    try:
        return bool(adapter.is_running())
    except UnsupportedCapability:
        pytest.skip(f"backend {adapter.backend!r} does not expose is_running()")


def test_is_running_true_for_live_sandbox(sdk_sandbox):
    # envd readiness can lag the control-plane running state, so poll until True.
    def _ready() -> bool:
        try:
            return _probe(sdk_sandbox) is True
        except Exception:
            # Transient probe error (5xx / 403) — keep polling.
            return False

    wait_until(
        _ready,
        timeout=90,
        interval=1,
        description="is_running() to report True for a live sandbox",
    )


def test_is_running_false_after_kill(sdk_sandbox):
    # kill() removes the sandbox; get_info() 404s so is_running() returns False.
    sdk_sandbox.kill()

    def _not_running() -> bool:
        try:
            return _probe(sdk_sandbox) is False
        except Exception:
            # Transient probe error — keep polling until it settles to False.
            return False

    wait_until(
        _not_running,
        timeout=60,
        interval=1,
        description="is_running() to report False after kill",
    )


@pytest.mark.requires_capability(PAUSE_RESUME)
def test_is_running_false_after_pause(sdk_sandbox, sdk_e2e_config):
    # A paused sandbox is answered from the control-plane state without a
    # data-plane probe, so is_running() returns False and never auto-resumes it.
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert _probe(sdk_sandbox) is False
