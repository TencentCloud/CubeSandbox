# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import pytest

from framework.capabilities import AUTH_SIMPLE_KEY, capabilities_for_backend


# These cases drive the CubeAPI control plane directly (not the sdk_sandbox
# fixture), so the per-module requires_capability marker would not fire on its
# own — this autouse fixture is the real gate. It skips when the backend lacks
# simple-key auth (e.g. e2b uses its own key format) or when the live server was
# not started with CUBE_API_KEY, in which case the 401 rejection paths cannot be
# exercised.
@pytest.fixture(autouse=True)
def _require_simple_key_auth(sdk_backend: str) -> str:
    if AUTH_SIMPLE_KEY not in capabilities_for_backend(sdk_backend):
        pytest.skip(
            f"backend {sdk_backend!r} does not support capability {AUTH_SIMPLE_KEY!r}"
        )
    api_key = os.environ.get("CUBE_API_KEY", "").strip()
    if not api_key:
        pytest.skip(
            "CUBE_API_KEY is unset; start CubeAPI with a key to exercise "
            "simple-key auth accept/reject paths"
        )
    return api_key
