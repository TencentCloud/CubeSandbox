# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.capabilities import FILESYSTEM
from framework.lifecycle import managed_control_sandbox

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.concurrency,
    pytest.mark.p2,
    pytest.mark.requires_capability(FILESYSTEM),
]

_SHARED_PATH = "/tmp/sdk-compat-sandbox-isolation.txt"


def test_two_sandboxes_keep_files_isolated(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    with managed_control_sandbox(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_role": "isolation_peer"},
    ) as peer:
        assert peer.sandbox_id != sdk_sandbox.sandbox_id

        sdk_sandbox.write_file(_SHARED_PATH, "primary")
        peer.write_file(_SHARED_PATH, "peer")

        assert sdk_sandbox.read_file(_SHARED_PATH) == "primary"
        assert peer.read_file(_SHARED_PATH) == "peer"
