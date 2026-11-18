# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest


# Make every package-local autouse setup fixture depend on the global volume
# gate. Pytest orders fixtures by dependency, not declaration/discovery order;
# this guarantees an unavailable plugin skips before host prerequisites run.
@pytest.fixture(autouse=True)
def _volume_plugin_ready(volume_plugin_gate: None) -> None:
    pass
