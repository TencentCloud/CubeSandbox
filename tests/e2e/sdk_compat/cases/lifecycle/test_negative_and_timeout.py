# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time
from dataclasses import replace
from datetime import datetime, timezone

import pytest
from adapters import connect_adapter, create_adapter
from framework.capabilities import LIFECYCLE, PAUSE_RESUME, SET_TIMEOUT

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]


def _end_at(raw: dict) -> datetime:
    value = raw.get("endAt") or raw.get("end_at")
    assert value, f"set_timeout response is not observable in sandbox info: {raw!r}"
    deadline = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    if deadline.tzinfo is None:
        deadline = deadline.replace(tzinfo=timezone.utc)
    return deadline


@pytest.mark.requires_capability(SET_TIMEOUT)
def test_set_timeout_updates_end_at(sdk_sandbox):
    started = time.monotonic()
    sdk_sandbox.set_timeout(180)
    first_deadline = _end_at(sdk_sandbox.info().raw)
    sdk_sandbox.set_timeout(240)
    second_deadline = _end_at(sdk_sandbox.info().raw)
    elapsed = time.monotonic() - started

    delta = (second_deadline - first_deadline).total_seconds()
    assert abs(delta - 60) <= elapsed + 5, (
        "set_timeout did not update endAt by the expected amount: "
        f"first={first_deadline.isoformat()}, second={second_deadline.isoformat()}, "
        f"delta={delta:.1f}s, elapsed={elapsed:.1f}s"
    )


def test_connect_missing_sandbox_fails(sdk_backend, sdk_e2e_config):
    with pytest.raises(Exception, match="(?i)(not found|404|does not exist)"):
        connect_adapter(
            sdk_backend,
            "sdk-compat-sandbox-does-not-exist",
            sdk_e2e_config,
        )


def test_create_with_missing_template_fails(sdk_backend, sdk_e2e_config):
    missing_config = replace(
        sdk_e2e_config,
        cube_template_id="tpl-sdk-compat-does-not-exist",
    )
    with pytest.raises(
        Exception,
        match=r"(?i)(template[^\n]*(not found|does not exist)|(?:not found|404)[^\n]*template)",
    ):
        create_adapter(sdk_backend, missing_config)


@pytest.mark.requires_capability(PAUSE_RESUME)
def test_pause_deleted_sandbox_fails(sdk_sandbox):
    sdk_sandbox.kill()
    with pytest.raises(Exception, match="(?i)(not found|404|deleted|does not exist)"):
        sdk_sandbox.pause()
