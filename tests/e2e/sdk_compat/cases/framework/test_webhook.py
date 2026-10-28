# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
import base64
import copy
import json
from uuid import uuid4

import pytest
from framework.webhook import assert_payload, new_events, records, utc_time

pytestmark = pytest.mark.framework


def event(label="updated"):
    return dict(id=str(uuid4()), version="v2", type="sandbox.lifecycle." + label,
                timestamp="2026-10-24T00:00:00Z", sandbox_id="sb", sandbox_template_id="tpl",
                sandbox_execution_id="", sandbox_build_id="", sandbox_team_id="",
                event_category="", event_label="", event_data={"network_egress": {
                    "allowed_cidrs": None, "denied_cidrs": ["0.0.0.0/0"], "allowed_domains": None}})


def test_operation_ids_and_retry_bytes(tmp_path):
    one, two = event(), event()
    path = tmp_path / "requests.jsonl"
    rows = []
    for payload in [one, one, two]:
        rows.append(dict(sandbox_id="sb", webhook_id="sub", delivery_id=str(uuid4()),
                         event_id=payload["id"], type=payload["type"], content_type="application/json",
                         body_base64=base64.b64encode(json.dumps(payload).encode()).decode()))
    path.write_text("".join(json.dumps(row) + "\n" for row in rows) + '{"partial":')
    read = records(path, "sb", "sub")
    assert len(read) == 3
    assert [e["id"] for e in new_events(read, "updated", {one["id"]})] == [two["id"]]
    changed = copy.deepcopy(one)
    changed["timestamp"] = "2026-10-24T00:00:01Z"
    rows[1]["body_base64"] = base64.b64encode(json.dumps(changed).encode()).decode()
    path.write_text("".join(json.dumps(row) + "\n" for row in rows))
    with pytest.raises(AssertionError, match="retry changed"):
        records(path, "sb", "sub")


def test_payload_values_and_missing_fields():
    assert utc_time("2026-10-24T10:23:34.069084369Z").microsecond == 69084
    assert utc_time("2026-10-24T10:23:34.1Z").microsecond == 100000
    value = event()
    network = value["event_data"]["network_egress"]
    assert_payload(value, "updated", template="tpl", network=network)
    with pytest.raises(AssertionError):
        assert_payload(value, "updated", network={**network, "denied_cidrs": []})
    value["sandbox_team_id"] = "fake-team"
    with pytest.raises(AssertionError, match="invented"):
        assert_payload(value, "updated")
    paused = event("paused")
    paused["event_data"] = {"pause_mode": "full", "execution": {"started_at": "", "vcpu_count": 1, "memory_mb": 512}}
    assert_payload(paused, "paused", execution={"vcpu_count": 1, "memory_mb": 512})
    paused["event_data"]["execution"]["execution_time"] = 0
    with pytest.raises(AssertionError):
        assert_payload(paused, "paused")
