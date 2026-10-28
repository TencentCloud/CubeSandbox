# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Opt-in P6 positive path; requires the dedicated receiver's local JSONL file.

This does not attest remote durability, failure boundaries or cross-node restore.
Run separately on XFS and S3. No handcrafted webhook events are submitted.
"""
import os
import time
from pathlib import Path
from datetime import datetime, timedelta, timezone

import pytest

from framework.lifecycle import wait_until, wait_until_paused, wait_until_running
from framework.webhook import assert_payload, new_events, records, utc_time

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


def test_six_lifecycle_webhooks(request, sdk_backend, sdk_e2e_config):
    path = os.environ.get("WEBHOOK_P6_RECEIVER_LOG")
    subscription = os.environ.get("WEBHOOK_P6_SUBSCRIPTION_ID")
    if not path or not subscription:
        pytest.skip("P6 receiver log and subscription ID must be explicitly configured")
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox P6 acceptance")
    log = Path(path)
    assert log.is_file(), "start receiver before creating the sandbox"
    sandbox = request.getfixturevalue("sdk_sandbox")
    current = sandbox
    snapshot = None
    killed = False
    seen = {}
    template = sdk_e2e_config.cube_template_id
    execution = {key: int(os.environ[env]) for key, env in (
        ("vcpu_count", "WEBHOOK_P6_EXPECT_VCPU"), ("memory_mb", "WEBHOOK_P6_EXPECT_MEMORY_MB")
    ) if env in os.environ} or None

    def observed():
        return records(log, sandbox.sandbox_id, subscription)

    def ids():
        return {event["id"] for _, event in observed()}

    def wait_event(label, *, before=(), field=None, **expected):
        deadline = time.monotonic() + sdk_e2e_config.default_timeout
        while time.monotonic() < deadline:
            matches = new_events(observed(), label, before, field)
            if matches:
                assert len(matches) == 1, "one operation produced multiple event identities"
                assert_payload(matches[0], label, **expected)
                seen[label + ("-network" if field == "network_egress" else "")] = matches[0]["id"]
                return matches[0]
            time.sleep(0.1)
        pytest.fail(f"no {label} webhook for {sandbox.sandbox_id}")

    try:
        wait_event("created", template=template, metadata={"test_suite": "sdk_compat"})
        before = ids()
        earliest = datetime.now(timezone.utc) + timedelta(seconds=300)
        current.set_timeout(300)
        latest = datetime.now(timezone.utc) + timedelta(seconds=300)
        update = wait_event("updated", before=before, field="set_timeout", template="")
        actual = utc_time(update["event_data"]["set_timeout"])
        assert earliest.replace(microsecond=0) <= actual <= latest.replace(microsecond=0)
        before = ids()
        current.update_network(network={"allow_internet_access": False})
        wait_event("updated", before=before, field="network_egress", network={
            "allowed_cidrs": None, "denied_cidrs": ["0.0.0.0/0"], "allowed_domains": None},
            template=template, metadata={"test_suite": "sdk_compat"})
        snapshot = current.create_snapshot()
        wait_event("checkpointed", template=template, metadata={"test_suite": "sdk_compat"})
        current.pause(timeout=sdk_e2e_config.default_timeout)
        wait_until_paused(current, timeout=sdk_e2e_config.default_timeout)
        wait_event("paused", template=template, execution=execution, metadata={"test_suite": "sdk_compat"})
        current = current.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
        wait_until_running(current, timeout=sdk_e2e_config.default_timeout)
        wait_event("resumed", template=template, metadata={"test_suite": "sdk_compat"})
        current.kill()
        killed = True
        wait_event("killed", reason="request", template=template, execution=execution,
                   metadata={"test_suite": "sdk_compat"})
        assert len(set(seen.values())) == 7
        if os.environ.get("WEBHOOK_P6_EXPECT_RETRY") == "1":
            deadline = time.monotonic() + sdk_e2e_config.default_timeout
            while time.monotonic() < deadline:
                rows = observed()
                statuses = {event_id: [row["response_status"] for row, event in rows
                                       if event["id"] == event_id] for event_id in seen.values()}
                if all(values == [503, 204] for values in statuses.values()):
                    break
                time.sleep(0.1)
            else:
                pytest.fail(f"retry-once did not complete for all operations: {statuses}")
    finally:
        # The existing sdk_sandbox fixture also owns sandbox cleanup. Release the
        # runtime reference before deleting this test's checkpoint on failure.
        try:
            if not killed:
                current.kill()
        finally:
            if current is not sandbox:
                current.close()
            if snapshot:
                sandbox.delete_snapshot(snapshot)


def test_never_timeout_and_repeated_network(request, sdk_backend, sdk_e2e_config):
    path = os.environ.get("WEBHOOK_P6_RECEIVER_LOG")
    subscription = os.environ.get("WEBHOOK_P6_SUBSCRIPTION_ID")
    if not path or not subscription or sdk_backend != "cubesandbox":
        pytest.skip("dedicated CubeSandbox P6 receiver required")
    sandbox = request.getfixturevalue("sdk_sandbox")

    def observed():
        return records(path, sandbox.sandbox_id, subscription)

    sandbox.set_timeout(-1)
    # A bounded observation, followed by two positive Writer/Worker barriers.
    time.sleep(2)
    assert not new_events(observed(), "updated", field="set_timeout")
    expected = {"allowed_cidrs": None, "denied_cidrs": ["0.0.0.0/0"], "allowed_domains": None}
    for _ in range(2):
        before = {event["id"] for _, event in observed()}
        sandbox.update_network(network={"allow_internet_access": False})
        wait_until(lambda: bool(new_events(observed(), "updated", before, "network_egress")),
                   timeout=sdk_e2e_config.default_timeout, description="network webhook")
        matches = new_events(observed(), "updated", before, "network_egress")
        assert len(matches) == 1
        assert_payload(matches[0], "updated", network=expected)
    assert len(new_events(observed(), "updated", field="network_egress")) == 2
    assert not new_events(observed(), "updated", field="set_timeout")


def test_zero_timeout_deadline(request, sdk_backend, sdk_e2e_config):
    path = os.environ.get("WEBHOOK_P6_RECEIVER_LOG")
    subscription = os.environ.get("WEBHOOK_P6_SUBSCRIPTION_ID")
    if not path or not subscription or sdk_backend != "cubesandbox":
        pytest.skip("dedicated CubeSandbox P6 receiver required")
    sandbox = request.getfixturevalue("sdk_sandbox")
    earliest = datetime.now(timezone.utc)
    sandbox.set_timeout(0)
    latest = datetime.now(timezone.utc)

    def updated():
        return new_events(records(path, sandbox.sandbox_id, subscription), "updated", field="set_timeout")

    wait_until(lambda: bool(updated()), timeout=sdk_e2e_config.default_timeout,
               description="immediate timeout webhook")
    assert len(updated()) == 1
    assert_payload(updated()[0], "updated")
    actual = utc_time(updated()[0]["event_data"]["set_timeout"])
    assert earliest.replace(microsecond=0) <= actual <= latest.replace(microsecond=0)
