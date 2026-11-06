# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Full Master delivery with real TLS and an isolated test CA."""
import copy
import os
from pathlib import Path

import pytest

from cases.lifecycle.test_webhook_recovery import p6_master, sandbox_and_barrier
from framework.webhook_resources import (
    owned_path, endpoint
)
from framework.lifecycle import wait_until
from framework.webhook import new_events

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.mark.parametrize("policy", ["trusted", "untrusted", "wrong-host"])
def test_full_master_preserves_tls_certificate_checks(request, sdk_backend, p6_master, policy):
    ca = os.environ.get("WEBHOOK_P6_TLS_CA")
    if sdk_backend != "cubesandbox" or not ca:
        pytest.skip("dedicated HTTPS endpoints and isolated Master CA required")
    owned_path(Path(ca))
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    before = {row["key"] for row in p6.durable()}
    config = copy.deepcopy(p6.base)
    config["webhooks"]["subscriptions"][0]["url"] = endpoint("TLS_" + policy.upper().replace("-", "_") + "_URL")
    p6.restart(config)
    environment = Path(f"/proc/{p6.pid()}/environ").read_bytes().split(b"\0")
    assert f"SSL_CERT_FILE={ca}".encode() in environment
    sandbox.set_timeout(431)
    def task():
        matches = [row for row in p6.durable() if row["key"] not in before
                   and row["event"]["sandbox_id"] == sandbox.sandbox_id
                   and "set_timeout" in row["event"].get("event_data", {})]
        return matches[0] if len(matches) == 1 else None
    expected = "succeeded" if policy == "trusted" else "failed"
    wait_until(lambda: task() and task()["record"]["state"] == expected,
               timeout=25, description=f"real HTTPS {policy} delivery becomes {expected}")
    saved = task()
    attempts = [row for row,event in p6.observed(sandbox) if event["id"] == saved["event"]["id"]]
    if policy == "trusted":
        assert len(attempts) == saved["record"]["attempts"] == 1
        assert attempts[0]["response_status"] == 204
    else:
        assert not attempts, "invalid TLS must not reach the HTTP handler"
        assert saved["record"]["attempts"] == saved["record"]["max"]
        assert saved["record"]["failure"]["Code"] == "transport_error"
    p6.save("tls-policy", {"policy":policy, "task":saved, "http":attempts})
    p6.restart(p6.base)
    sandbox.set_timeout(432)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", {saved["event"]["id"]}, "set_timeout")),
               timeout=15, description="positive business publication after TLS policy case")
