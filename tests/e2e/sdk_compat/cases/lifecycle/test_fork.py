# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.capabilities import FORK
from framework.cleanup import safe_kill

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(FORK),
]

_MARKER_PATH = "/tmp/sdk-compat-fork-{sandbox_id}.txt"


def test_fork_count_two_derives_working_copies(sdk_sandbox, sdk_e2e_config):
    marker_path = _MARKER_PATH.format(sandbox_id=sdk_sandbox.sandbox_id)
    sdk_sandbox.write_file(marker_path, "fork-marker")
    forks: list = []
    cleanup_errors: list[str] = []
    try:
        results = sdk_sandbox.fork(count=2)

        assert len(results) == 2, f"expected 2 fork results, got {len(results)}"
        failures = [err for _, err in results if err is not None]
        # A healthy backend must start both forks; per-fork failures here
        # are backend regressions, not expected partial-success behavior.
        assert not failures, f"fork reported per-fork failures: {failures!r}"
        forks = [adapter for adapter, _ in results if adapter is not None]
        assert len(forks) == 2

        for fork in forks:
            assert fork.read_file(marker_path) == "fork-marker", (
                f"fork {fork.sandbox_id} did not inherit the source filesystem"
            )
    finally:
        for fork in forks:
            cleanup_errors.extend(safe_kill(fork, sdk_e2e_config))
        cleanup_errors.extend(safe_kill(sdk_sandbox, sdk_e2e_config))
    assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
