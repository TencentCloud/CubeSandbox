# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Local checkpoint and same-node restore on an explicitly selected XFS/S3 template."""
from dataclasses import replace
import os

import pytest
import requests

from adapters.cubesandbox_adapter import CubeSandboxAdapter
from framework.webhook_resources import endpoint
from framework.lifecycle import wait_until
from framework.webhook import assert_payload, new_events, records

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


def test_local_checkpoint_and_user_snapshot_restore(request, sdk_backend, sdk_e2e_config, sdk_e2e_reporter):
    backend = os.environ.get("WEBHOOK_TEST_STORAGE_BACKEND")
    log = os.environ.get("WEBHOOK_TEST_RECEIVER_LOG")
    subscription = os.environ.get("WEBHOOK_TEST_SUBSCRIPTION_ID")
    if sdk_backend != "cubesandbox" or backend not in {"xfs", "s3"} or not log or not subscription:
        pytest.skip("explicit XFS/S3 READY template, receiver and Master endpoint required")
    sandbox = request.getfixturevalue("sdk_sandbox")
    snapshot = restored = None
    session = requests.Session()
    session.trust_env = False
    try:
        snapshot = sandbox.create_snapshot()
        response = session.get(endpoint("MASTER_URL") + "/cube/snapshot/" + snapshot, timeout=10)
        response.raise_for_status()
        result = response.json()
        assert result["ret"]["ret_code"] == 200, result
        state = result["snapshot"]
        assert state["backend"] == backend and state["status"] == "READY", state
        wait_until(lambda: bool(new_events(records(log, sandbox.sandbox_id, subscription), "checkpointed")),
                   timeout=15, description="checkpoint event after local READY")
        events = new_events(records(log, sandbox.sandbox_id, subscription), "checkpointed")
        assert len(events) == 1
        assert_payload(events[0], "checkpointed", template=sdk_e2e_config.cube_template_id)
        # Keep placement local; no remote-upload gate or independent-node recovery.
        restored = CubeSandboxAdapter.create(replace(sdk_e2e_config, cube_template_id=snapshot),
            metadata={"test_suite": "webhook-snapshot-restore"},
            create_options={"distribution_scope": [state["origin_node_id"]], "timeout": 300})
        placement = session.get(endpoint("MASTER_URL") + "/cube/sandbox/info",
                                params={"sandbox_id": restored.sandbox_id}, timeout=10).json()
        assert placement["ret"]["ret_code"] == 200, placement
        assert placement["data"][0]["host_id"] == state["origin_node_id"], placement
        wait_until(lambda: bool(new_events(records(log, restored.sandbox_id, subscription), "resumed")),
                   timeout=15, description="real user snapshot create publishes resumed")
        rows = records(log, restored.sandbox_id, subscription)
        resumed = new_events(rows, "resumed")
        assert len(resumed) == 1 and not new_events(rows, "created")
        assert_payload(resumed[0], "resumed", template=snapshot,
                       metadata={"test_suite": "webhook-snapshot-restore"})
        assert not new_events(rows, "updated"), "internal timeout refresh published updated"
        sandbox.set_timeout(409)
        wait_until(lambda: bool(new_events(records(log, sandbox.sandbox_id, subscription), "updated")),
                   timeout=15, description="positive publication after snapshot observations")
        assert len(new_events(records(log, sandbox.sandbox_id, subscription), "checkpointed")) == 1
        sdk_e2e_reporter.record("webhook_local_storage", backend=backend, snapshot=state,
            sandbox_id=sandbox.sandbox_id, restored_id=restored.sandbox_id, placement=placement,
            checkpoint_http=[row for row, _ in records(log, sandbox.sandbox_id, subscription)],
            restore_http=[row for row, _ in rows])
    finally:
        try:
            if restored:
                try:
                    restored.kill()
                finally:
                    restored.close()
        finally:
            try:
                if snapshot:
                    sandbox.delete_snapshot(snapshot)
            finally:
                session.close()
