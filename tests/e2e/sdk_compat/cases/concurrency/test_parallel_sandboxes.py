# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from adapters import create_adapter
from framework.capabilities import FILESYSTEM
from framework.config import SdkE2EConfig

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.p3,
    pytest.mark.slow,
    pytest.mark.requires_capability(FILESYSTEM),
]


def test_two_sandboxes_run_independently(
    sdk_sandbox, sdk_backend, sdk_e2e_config: SdkE2EConfig
):
    second = create_adapter(
        sdk_backend, sdk_e2e_config, metadata={"test": "concurrency"}
    )
    try:
        sdk_sandbox.write_file("/tmp/first.txt", "first")
        second.write_file("/tmp/second.txt", "second")

        assert sdk_sandbox.read_file("/tmp/first.txt") == "first"
        assert second.read_file("/tmp/second.txt") == "second"

        # Cross-sandbox isolation: files written to one must not leak to the other.
        assert sdk_sandbox.exists("/tmp/second.txt") is False
    finally:
        try:
            second.kill()
        except Exception:  # noqa: BLE001 - best-effort teardown
            pass
        try:
            second.close()
        except Exception:  # noqa: BLE001 - best-effort teardown
            pass
