# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import pytest


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    """Clear CUBE_* and E2B_* env vars before each test so test order cannot leak state."""
    for key in list(monkeypatch._setitem):
        if key.startswith("CUBE_") or key.startswith("E2B_"):
            monkeypatch.delenv(key, raising=False)
