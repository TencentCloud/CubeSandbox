# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time

import pytest
from framework.capabilities import LIFECYCLE

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p1,
    pytest.mark.requires_capability(LIFECYCLE),
]


def test_info_after_kill_raises(sdk_sandbox):
    sdk_sandbox.kill()

    # Any exception is acceptable (ApiError, SandboxNotFoundError, HTTP error, ...).
    with pytest.raises(Exception):
        sdk_sandbox.info()


def test_kill_already_killed_is_idempotent_or_raises(sdk_sandbox):
    sdk_sandbox.kill()

    # The second kill may succeed (idempotent) or raise; it just must not hang.
    start = time.monotonic()
    try:
        sdk_sandbox.kill()
    except Exception:
        pass
    elapsed = time.monotonic() - start
    assert elapsed < 30.0, f"second kill took {elapsed:.1f}s, expected prompt return"
