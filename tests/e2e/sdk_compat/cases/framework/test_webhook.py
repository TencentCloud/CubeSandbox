# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
import base64
import copy
import json
import subprocess
from types import SimpleNamespace
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
        delivery = str(uuid4())
        rows.append(dict(sandbox_id="sb", webhook_id="sub", delivery_id=delivery,
                         event_id=payload["id"], type=payload["type"], content_type="application/json",
                         headers=[["e2b-webhook-id", "sub"], ["e2b-delivery-id", delivery],
                                  ["Content-Type", "application/json"]],
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
    value["sandbox_template_id"] = ""
    assert_payload(value, "updated", template="", network=network)
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


def test_metadata_provenance_and_destroy_missing_fields():
    created = event("created")
    created["event_data"] = {"sandbox_metadata": {"user_token": "fixture-only"}}
    assert_payload(created, "created", metadata={"user_token": "fixture-only"})
    with pytest.raises(AssertionError, match="trusted current create"):
        assert_payload(created, "created")
    updated = event()
    updated["sandbox_template_id"] = ""
    updated["event_data"]["sandbox_metadata"] = {"user_token": "fixture-only"}
    with pytest.raises(AssertionError, match="trusted current create"):
        assert_payload(updated, "updated")
    killed = event("killed")
    killed["sandbox_template_id"] = ""
    killed["event_data"] = {"kill_reason": "unknown", "execution": {"started_at": ""}}
    assert_payload(killed, "killed", reason="unknown")
    killed["event_data"]["execution"]["vcpu_count"] = 1
    with pytest.raises(AssertionError, match="invented destroy"):
        assert_payload(killed, "killed")


def test_master_fixture_restores_config_and_mode_after_failure(tmp_path, monkeypatch):
    from cases.lifecycle.test_webhook_delivery import webhook_master
    import yaml

    config = tmp_path / "master.yaml"
    original = "# preserve original formatting\ninstance_db_config:\n  db_name: wh_test_1903\nwebhooks:\n  enabled: true\n"
    config.write_text(original)
    mode = tmp_path / "mode"
    mode.write_text("503")
    binary = tmp_path / "bin/master"
    for key, value in {
        "CONTROL": "1", "RUN_ROOT": str(tmp_path), "MASTER_CONFIG": str(config),
        "MASTER_UNIT": "webhook-1903-unit.service", "MASTER_URL": "http://127.0.0.1:1903",
        "MASTER_BINARY": str(binary), "RECEIVER_LOG": str(tmp_path / "requests.jsonl"),
        "RECEIVER_MODE": str(mode), "SUBSCRIPTION_ID": "fixture-test",
    }.items():
        monkeypatch.setenv("WEBHOOK_TEST_" + key, value)
    pid = 1903

    def show(args, **_):
        if args[0] == "systemctl":
            return {"ExecStart": "{ path=" + str(binary) + " ; }", "MainPID": str(pid),
                    "Environment": "CUBE_MASTER_CONFIG_PATH=" + str(config)}[args[args.index("-p") + 1]]
        assert args[:2] == ["sudo", "readlink"]
        return str(binary)

    def command(args, **_):
        nonlocal pid
        assert args == ["sudo", "systemctl", "restart", "webhook-1903-unit.service"]
        pid += 1
        return subprocess.CompletedProcess(args, 0, "", "")

    closed = []
    monkeypatch.setattr("subprocess.check_output", show)
    monkeypatch.setattr("subprocess.run", command)
    monkeypatch.setattr("requests.Session", lambda: SimpleNamespace(
        get=lambda *a, **k: SimpleNamespace(status_code=200), close=lambda: closed.append(True)))
    fixture = webhook_master.__wrapped__(SimpleNamespace(node=SimpleNamespace(name="restore-check")))
    master = next(fixture)
    changed = yaml.safe_load(original)
    changed["webhooks"]["enabled"] = False
    master.restart(changed)
    assert "enabled: false" in config.read_text()
    master.mode.write_text("slow")
    with pytest.raises(RuntimeError, match="test failure"):
        fixture.throw(RuntimeError("test failure"))
    assert config.read_text() == original and mode.read_text() == "503"
    assert json.loads((master.output / "restored.json").read_text())["config_restored"]
    assert closed == [True]
