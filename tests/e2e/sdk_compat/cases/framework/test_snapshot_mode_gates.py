# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Pure-logic tests for the filesystem-only snapshot E2E capability gate.

fs-only snapshots are a CubeSandbox backend capability: E2B has neither the
``mode`` field nor the endpoint, so the marker must keep those runs out. The
mapping is easy to break silently — a case filed under the wrong capability
would never run against CubeSandbox and nobody would notice — so it is pinned
here instead of only in a live run.

Marked ``framework`` so they run on every gate without a live environment
(see ``conftest.pytest_collection_modifyitems``).
"""

from __future__ import annotations

import pytest

from framework.capabilities import (
    CUBESANDBOX_CAPABILITIES,
    E2B_CAPABILITIES,
    FILESYSTEM_ONLY_SNAPSHOT,
    capabilities_for_backend,
)

pytestmark = pytest.mark.framework


def test_filesystem_only_capability_is_cubesandbox_only() -> None:
    # E2B exposes neither fs-only snapshots nor a ``mode`` field, so the marker
    # must skip e2b runs rather than attempt the REST path.
    assert FILESYSTEM_ONLY_SNAPSHOT in CUBESANDBOX_CAPABILITIES
    assert FILESYSTEM_ONLY_SNAPSHOT not in E2B_CAPABILITIES
    assert FILESYSTEM_ONLY_SNAPSHOT in capabilities_for_backend("cubesandbox")
    assert FILESYSTEM_ONLY_SNAPSHOT not in capabilities_for_backend("e2b")
