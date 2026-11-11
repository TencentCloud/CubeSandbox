# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.capabilities import VOLUME_PLUGIN, require_capabilities


# This autouse fixture is the real capability gate for the volume cases: they
# build their API client / adapter directly instead of the sdk_sandbox fixture,
# so the per-test requires_capability marker (enforced only inside sdk_sandbox)
# would not fire on its own.
@pytest.fixture(autouse=True)
def _require_volume_plugin_capability(sdk_backend: str) -> None:
    require_capabilities(sdk_backend, VOLUME_PLUGIN)
