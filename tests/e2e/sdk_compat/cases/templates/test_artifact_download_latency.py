# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Cold-start latency signal for the rootfs artifact download path.

CubeMaster picks the artifact download source per node (S3 presigned URL,
direct CubeTemplateCenter HTTP, or the legacy CubeMaster-proxied HTTP route)
without exposing which one it picked -- that decision lives entirely in
internal node-image annotations (see
``CubeMaster/pkg/templatecenter/artifact_download_source.go``) and is
unit-tested there. There is no SDK-level or CubeAPI-level field that reveals
the selected source, so this suite cannot assert "this run used TC direct"
from here.

What *is* observable end-to-end is wall-clock latency: a Cubelet that must
fetch a cold (not yet locally cached) artifact takes measurably longer than
one serving from a warm local copy, and a misconfigured direct source
(unreachable TC address, expired capability, wrong SHA) turns into either a
hard failure or a much slower fallback. This case creates a sandbox, times
how long it takes to become usable, and records that latency into the
existing JSONL event stream (see ``framework/reporting.py``) under the event
name ``artifact_cold_start_latency``.

Operators validating a CubeMaster config change (e.g. enabling direct TC/S3
passthrough vs. staying on the legacy Master-proxied path) should run this
case against both configurations and diff the recorded ``total_seconds`` --
that comparison is the actual direct-vs-non-direct data validation; this
file only guarantees both paths produce a *working* sandbox inside a sane
time budget on every run.
"""

from __future__ import annotations

import time

import pytest

from adapters import create_adapter_with_capacity_retry
from framework.cleanup import safe_kill
from framework.lifecycle import wait_until_data_plane_ready

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.templates,
    pytest.mark.p1,
]


def test_artifact_cold_start_latency_is_bounded_and_recorded(
    sdk_backend, sdk_e2e_config, sdk_e2e_reporter, request
):
    """Create -> data-plane-ready must stay inside a sane budget.

    The budget is deliberately generous (create_timeout + default_timeout):
    this is a regression guard against "the selected download source is
    reachable but pathologically slow or silently retries", not a strict
    performance SLA. Tightening it per-environment is a job for whoever
    diffs the recorded latency across configurations, not for this assertion.
    """
    budget = sdk_e2e_config.create_timeout + sdk_e2e_config.default_timeout

    t0 = time.monotonic()
    sandbox = create_adapter_with_capacity_retry(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_suite": "sdk_compat", "test_role": "artifact_cold_start_latency"},
    )
    t1 = time.monotonic()
    try:
        wait_until_data_plane_ready(
            sandbox,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        t2 = time.monotonic()

        create_seconds = t1 - t0
        data_plane_seconds = t2 - t1
        total_seconds = t2 - t0

        sdk_e2e_reporter.record(
            "artifact_cold_start_latency",
            backend=sdk_backend,
            sandbox_id=sandbox.sandbox_id,
            nodeid=request.node.nodeid,
            create_seconds=round(create_seconds, 3),
            data_plane_seconds=round(data_plane_seconds, 3),
            total_seconds=round(total_seconds, 3),
        )

        assert total_seconds <= budget, (
            f"sandbox {sandbox.sandbox_id} took {total_seconds:.1f}s to become "
            f"usable (create={create_seconds:.1f}s, data_plane={data_plane_seconds:.1f}s), "
            f"budget={budget}s -- check the artifact download source CubeMaster "
            f"selected for this node (S3 / TC / legacy Master)"
        )
    finally:
        safe_kill(sandbox, sdk_e2e_config)
