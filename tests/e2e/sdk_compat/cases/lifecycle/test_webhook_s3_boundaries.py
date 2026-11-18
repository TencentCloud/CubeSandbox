# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Local S3 snapshot rejection; no upload or independent-node recovery control."""
import os

import pytest

from cases.lifecycle.test_webhook_delivery import webhook_master, sandbox_with_receiver
from framework.webhook_resources import owned_path
from framework.webhook import new_events
from cases.lifecycle.test_webhook_completion_boundaries import positive_barrier

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.mark.parametrize("operation", ["pause", "checkpoint"])
def test_s3_local_snapshot_failure_has_no_success_event(request, sdk_backend, webhook_master, operation):
    path = os.environ.get("WEBHOOK_TEST_STORAGE_PROXY_MODE")
    if (sdk_backend != "cubesandbox" or not path
            or os.environ.get("WEBHOOK_TEST_STORAGE_BACKEND") != "s3"):
        pytest.skip("dedicated S3 template with local snapshot rejection fixture required")
    from cubesandbox._exceptions import ApiError
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    control = owned_path(path)
    assert control.read_text().strip() == "forward"
    before = {event["id"] for _, event in master.observed(sandbox)}
    try:
        control.write_text("snapshot-error")
        with pytest.raises(ApiError) as failure:
            sandbox.pause() if operation == "pause" else sandbox.create_snapshot()
        assert "webhook dedicated storage operation rejected" in str(failure.value)
        master.save("local-snapshot-failed", {"operation": operation, "business_error": str(failure.value)})
    finally:
        control.write_text("forward")
        master.save("storage-mode-restored", {"path": str(control), "mode": control.read_text()})
    positive_barrier(master, sandbox, before)
    for label in ["paused", "checkpointed", "killed"]:
        assert not new_events(master.observed(sandbox), label, before)
