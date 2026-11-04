# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Real SDK operations with narrow failures in the dedicated cloned database."""
from contextlib import contextmanager
import copy
import json
import os
from pathlib import Path
import re
import subprocess
import time
from uuid import uuid4

import pytest
import requests

from cases.lifecycle.test_webhook_delivery import webhook_master, sandbox_with_receiver
from framework.webhook_resources import (
    setting, owned_path, container_info, resource_label
)
from framework.lifecycle import wait_until
from framework.webhook import assert_payload, new_events

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.fixture
def database_fault(webhook_master):
    if os.environ.get("WEBHOOK_TEST_DATABASE_FAULTS") != "1":
        pytest.skip("dedicated database boundary experiment not requested")
    master = webhook_master
    database = master.base["instance_db_config"]["db_name"]
    assert re.fullmatch(r"wh_test_[0-9_]+", database)
    assert master.base["instance_db_config"]["user"] == "webhook_test_1903"
    container = setting("MYSQL_CONTAINER")
    info = container_info(container)
    bindings = info["NetworkSettings"]["Ports"]["3306/tcp"]
    assert any(f'{b["HostIp"]}:{b["HostPort"]}' == master.base["instance_db_config"]["addr"]
               for b in bindings)
    def sql(statement):
        admin = statement.startswith(("DELIMITER", "DROP TRIGGER"))
        env = {**os.environ, "MYSQL_PWD": master.base["instance_db_config"]["pwd"]}
        if admin:
            # Binlogged MySQL requires SUPER for trigger DDL. Do not change
            # shared log_bin_trust_function_creators or grant global privileges.
            info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
            settings = dict(item.split("=", 1) for item in info["Config"]["Env"] if "=" in item)
            env["MYSQL_PWD"] = settings["MYSQL_ROOT_PASSWORD"]
        result = subprocess.run(["docker", "exec", "-i", "-e", "MYSQL_PWD",
            container, "mysql", "-uroot" if admin else "-uwebhook_test_1903",
            "--batch", "--raw", "--skip-column-names", database],
            input=statement, capture_output=True, text=True, timeout=15, env=env)
        with (master.output / "database.jsonl").open("a") as stream:
            stream.write(json.dumps({"sql": statement, "at": time.time(), "returncode": result.returncode,
                "stdout": result.stdout, "stderr": result.stderr}) + "\n")
        result.check_returncode()
        return result.stdout

    @contextmanager
    def trigger(table, condition, event="UPDATE"):
        assert table in {"t_cube_pause_snapshot", "t_cube_snapshot", "t_cube_template_image_job", "t_cube_sandbox_spec"}
        assert event in {"UPDATE", "INSERT", "DELETE"}
        name = "webhook_test_" + uuid4().hex
        message = "webhook dedicated final boundary " + name
        sql(f"""DELIMITER //
CREATE TRIGGER {name} BEFORE {event} ON {table} FOR EACH ROW
BEGIN IF {condition} THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='{message}'; END IF; END//
DELIMITER ;
""")
        try:
            yield message, sql
        finally:
            sql(f"DROP TRIGGER {name};")
    return trigger


def positive_barrier(master, sandbox, before):
    sandbox.set_timeout(373)
    wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before, "set_timeout")),
               timeout=15, description="actual successful publication after failed business boundary")


@pytest.fixture
def business_proxy(webhook_master):
    path = os.environ.get("WEBHOOK_TEST_BUSINESS_PROXY_DIR")
    if not path:
        pytest.skip("dedicated business RESP proxy required")
    directory = Path(path)
    owned_path(directory)
    address = (directory / "ready").read_text().strip()
    assert address.startswith("127.0.0.1:")
    master = webhook_master
    config = copy.deepcopy(master.base)
    config["redis"]["nodes"] = address
    master.restart(config)
    control = directory / "control.json"
    control.write_text("{}")
    def arm(operation, **match):
        sequence = time.time_ns()
        temporary = control.with_suffix(".tmp")
        temporary.write_text(json.dumps({"id": sequence, "op": operation, "when": "before",
                                        "error": True, "repeat": 64, **match}))
        temporary.replace(control)
        return sequence, directory / f"fault-{sequence}.json"
    yield arm, directory
    control.write_text("{}")


def node_listing(master):
    result = subprocess.check_output(["sudo", str(owned_path(setting("NODE_CLI"))), "--address",
                                     str(owned_path(setting("NODE_SOCKET"))), "cubebox", "ls", "--no-trunc"], text=True)
    with (master.output / "node-list.jsonl").open("a") as stream:
        stream.write(json.dumps({"at": time.time(), "listing": result}) + "\n")
    return result


def test_create_proxy_failure_rolls_back_without_created_or_killed(sdk_backend, sdk_e2e_config,
                                                                  webhook_master, business_proxy):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    from cubesandbox._exceptions import ApiError
    master = webhook_master
    arm, directory = business_proxy
    sequence, first = arm("HSET", key_prefix="cube:v1:shared:sandbox:proxy:")
    try:
        with pytest.raises(ApiError) as failure:
            CubeSandboxAdapter.create(sdk_e2e_config, metadata={"test_suite": "master-create-rollback"})
        assert "setProxyToRedis" in str(failure.value)
        wait_until(first.is_file, timeout=5, description="actual create proxy write rejected")
        hits = [json.loads(path.read_text()) for path in directory.glob(f"fault-{sequence}*.json")]
        ids = {hit["matched_key"].removeprefix("cube:v1:shared:sandbox:proxy:") for hit in hits}
        assert ids and all(re.fullmatch(r"[a-zA-Z0-9-]+", item) for item in ids)
    finally:
        (directory / "control.json").write_text("{}")
    wait_until(lambda: all(item not in node_listing(master) for item in ids), timeout=40,
               description="actual failed-create rollback removes node sandbox")
    other = CubeSandboxAdapter.create(sdk_e2e_config)
    try:
        positive_barrier(master, other, ())
    finally:
        other.kill()
    from framework.webhook import records
    for sandbox_id in ids:
        assert not records(master.env["receiver_log"], sandbox_id, master.env["subscription_id"])
    master.save("create-rollback", {"business_error": str(failure.value), "sandbox_ids": sorted(ids), "faults": hits})


def test_async_destroy_waits_for_master_proxy_cleanup(request, sdk_backend, webhook_master, business_proxy):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    arm, directory = business_proxy
    before = {event["id"] for _,event in master.observed(sandbox)}
    sequence, first = arm("DEL", key="cube:v1:shared:sandbox:proxy:" + sandbox.sandbox_id)
    session = requests.Session()
    session.trust_env = False
    try:
        response = session.delete(master.env["master_url"] + "/cube/sandbox",
                                  json={"sandbox_id": sandbox.sandbox_id, "sync": False}, timeout=10)
        assert response.json()["ret"]["ret_code"] == 200
        wait_until(first.is_file, timeout=15, description="destroy reached Master cleanup after node success")
        wait_until(lambda: sandbox.sandbox_id not in node_listing(master), timeout=15,
                   description="node sandbox absent while Master cleanup is rejected")
        assert not new_events(master.observed(sandbox), "killed", before)
        master.save("before-cleanup-recovery", {"accepted": response.json(), "fault": json.loads(first.read_text()),
                                            "http": [row for row,event in master.observed(sandbox)]})
    finally:
        (directory / "control.json").write_text("{}")
        session.close()
    wait_until(lambda: bool(new_events(master.observed(sandbox), "killed", before)), timeout=35,
               description="same async task completes cleanup and publishes killed")
    assert len(new_events(master.observed(sandbox), "killed", before)) == 1


def test_paused_delete_binding_warn_only_still_publishes_killed(request, sdk_backend, webhook_master, database_fault):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    sandbox.pause()
    wait_until(lambda: bool(new_events(master.observed(sandbox), "paused")), timeout=15, description="actual pause")
    before = {event["id"] for _,event in master.observed(sandbox)}
    session = requests.Session()
    session.trust_env = False
    def destroy():
        response = session.delete(master.env["master_url"] + "/cube/sandbox",
            json={"sandbox_id": sandbox.sandbox_id, "sync": True}, timeout=20)
        master.save("delete-" + str(time.time_ns()), response.json())
        return response.json()
    try:
        with database_fault("t_cube_pause_snapshot", f"OLD.sandbox_id='{sandbox.sandbox_id}'", "DELETE") as (message,sql):
            failed = destroy()
            # Binding removal remains warn-only; existing handler success
            # publishes killed without an additional evidence requirement.
            assert failed["ret"]["ret_code"] == 200
            assert sql(f"SELECT COUNT(*) FROM t_cube_pause_snapshot WHERE sandbox_id='{sandbox.sandbox_id}';").strip() == "1"
            from adapters.cubesandbox_adapter import CubeSandboxAdapter
            from framework.config import SdkE2EConfig
            other = CubeSandboxAdapter.create(SdkE2EConfig.from_env())
            try:
                positive_barrier(master, other, ())
            finally:
                other.kill()
            wait_until(lambda: bool(new_events(master.observed(sandbox), "killed", before)), timeout=15,
                       description="warn-only binding failure preserves successful killed notification")
            first = new_events(master.observed(sandbox), "killed", before)
            assert len(first) == 1
            assert_payload(first[0], "killed", template="", reason="request")
            before = {event["id"] for _, event in master.observed(sandbox)}
        assert destroy()["ret"]["ret_code"] == 200
        wait_until(lambda: bool(new_events(master.observed(sandbox), "killed", before)), timeout=15,
                   description="paused delete final cleanup succeeds")
        assert len(new_events(master.observed(sandbox), "killed", before)) == 1
    finally:
        session.close()


@pytest.mark.parametrize("boundary", ["pause_complete", "snapshot_ready", "job_ready"])
def test_final_database_write_failure_does_not_publish_success(request, sdk_backend, webhook_master,
                                                              database_fault, boundary):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    from cubesandbox._exceptions import ApiError
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    assert re.fullmatch(r"[a-zA-Z0-9-]+", sandbox.sandbox_id)
    before = {event["id"] for _, event in master.observed(sandbox)}
    table, identity = {
        "pause_complete": ("t_cube_pause_snapshot", "sandbox_id"),
        "snapshot_ready": ("t_cube_snapshot", "origin_sandbox_id"),
        "job_ready": ("t_cube_template_image_job", "sandbox_id"),
    }[boundary]
    condition = f"NEW.{identity}='{sandbox.sandbox_id}' AND NEW.status='READY'"
    with database_fault(table, condition) as (message, sql):
        with pytest.raises(ApiError) as failure:
            sandbox.pause() if boundary == "pause_complete" else sandbox.create_snapshot()
        assert message in str(failure.value), "did not reach the precise final write"
        state = sql(f"SELECT JSON_OBJECT('status',status,'snapshot_id',"
                    f"{'template_id' if boundary == 'job_ready' else 'snapshot_id'}) FROM {table} "
                    f"WHERE {identity}='{sandbox.sandbox_id}';")
        master.save("final-write-failed", {"boundary": boundary, "business_error": str(failure.value),
                                     "stored_state": state})
        assert '"status": "READY"' not in state
    # Pause's node is already torn down; use a second real sandbox as the Writer barrier.
    if boundary == "pause_complete":
        from adapters.cubesandbox_adapter import CubeSandboxAdapter
        from framework.config import SdkE2EConfig
        other = CubeSandboxAdapter.create(SdkE2EConfig.from_env())
        try:
            positive_barrier(master, other, ())
        finally:
            other.kill()
    else:
        positive_barrier(master, sandbox, before)
    assert not new_events(master.observed(sandbox), "paused", before)
    assert not new_events(master.observed(sandbox), "checkpointed", before)
    assert not new_events(master.observed(sandbox), "killed", before)


def test_resume_final_proxy_purge_failure_has_no_resumed(request, sdk_backend, webhook_master):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_TEST_PROXY_FAULTS") != "1":
        pytest.skip("dedicated real proxy fault not requested")
    from cubesandbox._exceptions import ApiError
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    container = setting("PROXY_CONTAINER")
    container_info(container)
    label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container], text=True)
    assert label.strip() == setting("RESOURCE_LABEL")
    assert label.strip() == resource_label()
    sandbox.pause()
    wait_until(lambda: bool(new_events(master.observed(sandbox), "paused")), timeout=15,
               description="actual pause before final purge fault")
    before = {event["id"] for _, event in master.observed(sandbox)}
    subprocess.run(["docker", "stop", container], check=True, capture_output=True)
    try:
        with pytest.raises(ApiError) as failure:
            sandbox.resume_or_connect(timeout=300)
        assert "pre-pause route" in str(failure.value)
        master.save("purge-error", {"business_error": str(failure.value)})
    finally:
        subprocess.run(["docker", "start", container], check=True, capture_output=True)
    positive_barrier(master, sandbox, before)
    assert not new_events(master.observed(sandbox), "resumed", before)
    assert not new_events(master.observed(sandbox), "updated", before, "network_egress")
    assert len(new_events(master.observed(sandbox), "updated", before, "set_timeout")) == 1


@pytest.mark.parametrize("mode,reason", [("require-signature", "signature_headers_missing"),
                                         ("strict-execution", "execution_id_not_uuid")])
def test_receiver_policy_rejects_real_payload_and_exhausts_budget(request, sdk_backend, webhook_master, mode, reason):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    before = {event["id"] for _, event in master.observed(sandbox)}
    config = copy.deepcopy(master.base)
    config["webhooks"].update(max_attempts=3, request_timeout="500ms")
    master.restart(config)
    failed = master.metric("terminal", "failed")
    master.mode.write_text(mode)
    try:
        sandbox.set_timeout(321)
        wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before)), timeout=15,
                   description="real payload rejected by receiver policy")
        event_id = new_events(master.observed(sandbox), "updated", before)[0]["id"]
        wait_until(lambda: master.metric("terminal", "failed") > failed, timeout=25,
                   description="bounded rejection retries exhausted")
        attempts = [row for row,event in master.observed(sandbox) if event["id"] == event_id]
        assert len(attempts) == 3
        assert all(row["rejection_reason"] == reason for row in attempts)
        master.save("receiver-policy", {"mode": mode, "event_id": event_id, "http": attempts})
    finally:
        master.mode.write_text("204")
    positive_barrier(master, sandbox, {event_id})
    assert len([row for row,event in master.observed(sandbox) if event["id"] == event_id]) == len(attempts)


@pytest.mark.parametrize("policy,allowed,denied,domains", [
    ({"allow_internet_access": True}, None, None, None),
    ({"allow_internet_access": False}, None, ["0.0.0.0/0"], None),
    ({"allow_out": ["203.0.113.0/24"], "deny_out": ["198.51.100.0/24"]},
     ["203.0.113.0/24"], ["198.51.100.0/24"], None),
    ({"allow_internet_access": False, "allow_out": ["203.0.113.0/24", "example.org"],
      "rules": [{"name": "master-mixed", "match": {"scheme": "https", "host": "example.org"},
                 "action": {"allow": False}}]}, ["203.0.113.0/24"], ["0.0.0.0/0"], ["example.org"]),
])
def test_network_projection_while_spec_persistence_fails(request, sdk_backend, webhook_master,
                                                        database_fault, policy, allowed, denied, domains):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    before = {event["id"] for _,event in master.observed(sandbox)}
    with database_fault("t_cube_sandbox_spec", f"NEW.sandbox_id='{sandbox.sandbox_id}'") as (_,sql):
        stored = sql(f"SELECT request_json FROM t_cube_sandbox_spec WHERE sandbox_id='{sandbox.sandbox_id}';")
        sandbox.update_network(policy)
        wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before, "network_egress")),
                   timeout=15, description="node-applied policy not suppressed by best-effort spec failure")
        assert sql(f"SELECT request_json FROM t_cube_sandbox_spec WHERE sandbox_id='{sandbox.sandbox_id}';") == stored
    matches = new_events(master.observed(sandbox), "updated", before, "network_egress")
    assert len(matches) == 1
    assert_payload(matches[0], "updated", network={"allowed_cidrs": allowed, "denied_cidrs": denied,
                                                   "allowed_domains": domains})
    master.save("network-with-spec-failure", {"requested": policy, "event": matches[0]})


@pytest.mark.parametrize("kind", ["empty", "user-keys", "oversized-entries", "oversized-bytes"])
def test_real_metadata_boundaries_preserve_business_and_known_fields(sdk_backend, sdk_e2e_config, webhook_master, kind):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    master = webhook_master
    metadata = {
        "empty": {}, "user-keys": {"user_token":"fixture-only", "api_token":"not-a-secret", "empty":""},
        "oversized-entries": {f"key{i}":"value" for i in range(65)},
        "oversized-bytes": {"large":"x"*17000},
    }[kind]
    session = requests.Session()
    session.trust_env = False
    def metrics():
        response = session.get(master.env["master_url"] + "/metrics", timeout=5)
        response.raise_for_status()
        return response.text
    def oversized():
        return sum(float(line.rsplit(" ",1)[1]) for line in metrics().splitlines()
                   if 'operation="capture"' in line and 'result="oversized"' in line)
    initial = oversized()
    sandbox = CubeSandboxAdapter.create(sdk_e2e_config, metadata=metadata)
    try:
        positive_barrier(master, sandbox, ())
        created = new_events(master.observed(sandbox), "created")
        if kind.startswith("oversized"):
            wait_until(lambda: oversized() > initial, timeout=5, description="bounded capture rejection observed")
            assert not created, "oversized capture should not leak a partial created event"
        else:
            wait_until(lambda: bool(new_events(master.observed(sandbox), "created")), timeout=15,
                       description="real created event for bounded metadata")
            sandbox.update_network({"allow_out": ["203.0.113.0/24"]})
            wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", field="network_egress")),
                       timeout=15, description="network event without persisted user metadata")
            created = new_events(master.observed(sandbox), "created")
            assert len(created) == 1
            if kind == "empty":
                assert "sandbox_metadata" not in created[0].get("event_data", {})
            else:
                assert_payload(created[0], "created", metadata=metadata)
                assert created[0]["event_data"]["sandbox_metadata"] == metadata
            network_events = new_events(master.observed(sandbox), "updated", field="network_egress")
            assert len(network_events) == 1
            assert_payload(network_events[0], "updated", template="")
            for _, event in master.observed(sandbox):
                assert event["sandbox_execution_id"] == event["sandbox_team_id"] == event["sandbox_build_id"] == ""
        master.save("metadata-boundary", {"kind": kind, "sandbox_id":sandbox.sandbox_id,
            "http":[row for row,event in master.observed(sandbox)], "capture_oversized_before":initial,
            "capture_oversized_after":oversized()})
    finally:
        sandbox.kill()
        session.close()
