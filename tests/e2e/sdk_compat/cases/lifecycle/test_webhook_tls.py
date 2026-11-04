# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Full Master delivery with real TLS and an isolated test CA."""
import copy
import os
from pathlib import Path

import pytest

from cases.lifecycle.test_webhook_delivery import webhook_master, sandbox_with_receiver
from framework.webhook_resources import owned_path, endpoint
from framework.lifecycle import wait_until
from framework.webhook import assert_payload, new_events

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.mark.parametrize("policy", ["trusted", "untrusted", "wrong-host"])
def test_full_master_preserves_tls_certificate_checks(request, sdk_backend, webhook_master, policy):
    ca = os.environ.get("WEBHOOK_TEST_TLS_CA")
    if sdk_backend != "cubesandbox" or not ca:
        pytest.skip("dedicated HTTPS endpoints and isolated Master CA required")
    owned_path(ca)
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    before = {event["id"] for _, event in master.observed(sandbox)}
    config = copy.deepcopy(master.base)
    config["webhooks"].update(max_attempts=3, request_timeout="500ms")
    config["webhooks"]["subscriptions"][0]["url"] = endpoint("TLS_" + policy.upper().replace("-", "_") + "_URL")
    master.restart(config)
    environment = Path(f"/proc/{master.pid()}/environ").read_bytes().split(b"\0")
    assert f"SSL_CERT_FILE={ca}".encode() in environment
    sandbox.set_timeout(431)
    terminal = "succeeded" if policy == "trusted" else "failed"
    wait_until(lambda: master.metric("terminal", terminal) == 1,
               timeout=25, description=f"real HTTPS {policy} delivery terminates")
    events = new_events(master.observed(sandbox), "updated", before, "set_timeout")
    if policy == "trusted":
        assert len(events) == 1 and master.metric("http", "success") == 1
        assert_payload(events[0], "updated", template="")
        attempts = [row for row, event in master.observed(sandbox) if event["id"] == events[0]["id"]]
        assert len(attempts) == 1 and attempts[0]["response_status"] == 204
    else:
        assert not events, "invalid TLS must not reach the HTTP handler"
        assert master.metric("send", "transport_error") == 3
        attempts = []
    master.save("tls-policy", {"policy": policy, "http": attempts, "terminal": terminal})
    master.restart(master.base)
    sandbox.set_timeout(432)
    wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before | {event["id"] for event in events}, "set_timeout")),
               timeout=15, description="positive publication after TLS policy case")
