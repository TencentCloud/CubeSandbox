# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Opt-in full-platform Redis reply faults, using the existing Go RESP fixture.

The operator starts TestP6PlatformRedisProxy and connects a dedicated Master to
its ready address. Every task here is produced by a real SDK timeout operation.
"""
import json
import os
from pathlib import Path
import subprocess
import time

import pytest

from framework.webhook_resources import (
    owned_path, container_info, resource_label
)
from framework.lifecycle import wait_until
from framework.webhook import delivery_records, new_events, records, utc_time

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]
TERMINAL = {"succeeded", "failed", "cancelled"}


def write_control(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value))
    temporary.replace(path)


@pytest.fixture
def p6_reply_faults():
    names = ("WEBHOOK_P6_PROXY_DIR", "WEBHOOK_P6_REDIS_CONTAINER",
             "WEBHOOK_P6_REDIS_PASSWORD", "WEBHOOK_P6_RECEIVER_LOG",
             "WEBHOOK_P6_RECEIVER_MODE", "WEBHOOK_P6_SUBSCRIPTION_ID")
    if os.environ.get("WEBHOOK_P6_REPLY_FAULTS") != "1" or not all(os.environ.get(n) for n in names):
        pytest.skip("dedicated full-platform reply fault environment not requested")
    values = {name: os.environ[name] for name in names}
    container = values["WEBHOOK_P6_REDIS_CONTAINER"]
    container_info(container)
    label = subprocess.check_output([
        "docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container
    ], text=True).strip()
    assert label == resource_label()
    directory = owned_path(values["WEBHOOK_P6_PROXY_DIR"])
    assert (directory / "ready").is_file()

    def durable():
        return delivery_records(container, values["WEBHOOK_P6_REDIS_PASSWORD"])

    mode = owned_path(values["WEBHOOK_P6_RECEIVER_MODE"])
    control = directory / "control.json"
    mode.write_text("204")
    write_control(control, {})
    yield directory, control, mode, values, durable
    write_control(control, {})
    mode.write_text("204")


@pytest.mark.parametrize("operation,when,expected_attempts,expected_http", [
    ("put", "before", 1, 1), ("put", "after", 1, 1),
    ("claim", "before", 1, 1), ("claim", "after", 2, 1),
    ("complete", "before", 2, 2), ("complete", "after", 1, 1),
    ("retry", "before", 2, 2), ("retry", "after", 2, 2),
])
def test_real_timeout_redis_reply_fault(request, sdk_backend, sdk_e2e_config,
                                      p6_reply_faults, operation, when,
                                      expected_attempts, expected_http):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment fault experiment")
    directory, control, mode, values, durable = p6_reply_faults
    sandbox = request.getfixturevalue("sdk_sandbox")
    request.addfinalizer(lambda: write_control(control, {}))
    request.addfinalizer(lambda: mode.write_text("204"))
    # No unrelated saved task may consume the one-shot operation fault.
    wait_until(lambda: all(x["record"]["state"] in TERMINAL for x in durable()),
               timeout=35, description="delivery quiescence before arming fault")
    log = values["WEBHOOK_P6_RECEIVER_LOG"]
    subscription = values["WEBHOOK_P6_SUBSCRIPTION_ID"]

    def observed():
        return records(log, sandbox.sandbox_id, subscription)

    before = {event["id"] for _, event in observed()}
    sequence = time.time_ns()
    fired_path = directory / f"fault-{sequence}.json"
    mode.write_text("503" if operation == "retry" else "204")
    write_control(control, {"id": sequence, "op": operation, "when": when})
    sandbox.set_timeout(300)
    wait_until(fired_path.is_file, timeout=15, description="actual Redis fault fired")
    fired = json.loads(fired_path.read_text())
    assert fired["upstream_executed"] == (when == "after")
    assert fired["client_reply"] is False

    def tasks():
        return [x for x in durable() if x["event"]["sandbox_id"] == sandbox.sandbox_id
                and x["event"]["type"] == "sandbox.lifecycle.updated"
                and x["event"]["id"] not in before]

    at_fault = tasks()
    (directory / f"state-{sequence}.json").write_text(json.dumps(at_fault, indent=2))
    mode.write_text("204")
    if operation == "claim" and when == "after":
        assert fired["claim_values"] > 2, "lost a real task/Receipt reply, not an idle poll"
        assert len(at_fault) == 1
        assert at_fault[0]["record"]["state"] == "inflight"
        assert at_fault[0]["record"]["attempts"] == 1
        time.sleep(0.5)
        assert not new_events(observed(), "updated", before), "sent without a valid Receipt"

    wait_until(lambda: bool(tasks()) and tasks()[0]["record"]["state"] == "succeeded",
               timeout=35, description="saved lifecycle delivery recovered")
    final = tasks()
    assert len(final) == 1
    task = final[0]
    assert task["record"]["attempts"] == expected_attempts
    assert task["record"]["max"] == 3
    if operation == "put":
        assert task["record"]["input"] == fired["immutable_json"]
    if operation == "complete" and when == "after":
        time.sleep(2)
    rows = [(row, event) for row, event in observed() if event["id"] == task["event"]["id"]]
    assert len(rows) == expected_http
    if operation == "retry":
        assert [row["response_status"] for row, _ in rows] == [503, 204]
    else:
        assert all(row["response_status"] == 204 for row, _ in rows)
    if (operation == "claim" and when == "after") or (when == "before" and operation in {"retry", "complete"}):
        recovered = utc_time(rows[-1][0]["received_at"].replace("+00:00", "Z"))
        assert (recovered - utc_time(fired["at"])).total_seconds() >= 12, "resent from memory before lease recovery"
    (directory / f"result-{sequence}.json").write_text(json.dumps({
        "fault": fired, "task": task, "http": [row for row, _ in rows],
    }, indent=2))
