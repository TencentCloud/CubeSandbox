# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Real SDK operations with narrow failures in the dedicated cloned database."""
from contextlib import contextmanager
import copy
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import time
from uuid import uuid4

import pytest
import requests
import yaml

from cases.lifecycle.test_webhook_recovery import p6_master, sandbox_and_barrier, pending_update
from framework.webhook_resources import (
    setting, owned_path, container_info, checked_unit, resource_label, endpoint
)
from framework.lifecycle import wait_until
from framework.webhook import assert_payload, new_events

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.fixture
def database_fault(p6_master):
    if os.environ.get("WEBHOOK_P6_DATABASE_FAULTS") != "1":
        pytest.skip("dedicated database boundary experiment not requested")
    p6 = p6_master
    database = p6.base["instance_db_config"]["db_name"]
    assert re.fullmatch(r"whp6_[0-9_]+", database)
    assert p6.base["instance_db_config"]["user"] == "p6_1903"
    container = setting("MYSQL_CONTAINER")
    info = container_info(container)
    bindings = info["NetworkSettings"]["Ports"]["3306/tcp"]
    assert any(f'{b["HostIp"]}:{b["HostPort"]}' == p6.base["instance_db_config"]["addr"]
               for b in bindings)
    def sql(statement):
        admin = statement.startswith(("DELIMITER", "DROP TRIGGER"))
        env = {**os.environ, "MYSQL_PWD": p6.base["instance_db_config"]["pwd"]}
        if admin:
            # Binlogged MySQL requires SUPER for trigger DDL. Do not change
            # shared log_bin_trust_function_creators or grant global privileges.
            info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
            settings = dict(item.split("=", 1) for item in info["Config"]["Env"] if "=" in item)
            env["MYSQL_PWD"] = settings["MYSQL_ROOT_PASSWORD"]
        result = subprocess.run(["docker", "exec", "-i", "-e", "MYSQL_PWD",
            container, "mysql", "-uroot" if admin else "-up6_1903",
            "--batch", "--raw", "--skip-column-names", database],
            input=statement, capture_output=True, text=True, timeout=15, env=env)
        with (p6.output / "database.jsonl").open("a") as stream:
            stream.write(json.dumps({"sql": statement, "at": time.time(), "returncode": result.returncode,
                "stdout": result.stdout, "stderr": result.stderr}) + "\n")
        result.check_returncode()
        return result.stdout

    @contextmanager
    def trigger(table, condition, event="UPDATE"):
        assert table in {"t_cube_pause_snapshot", "t_cube_snapshot", "t_cube_template_image_job", "t_cube_sandbox_spec"}
        assert event in {"UPDATE", "INSERT", "DELETE"}
        name = "p6_r2_" + uuid4().hex
        message = "P6 dedicated final boundary " + name
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


def positive_barrier(p6, sandbox, before):
    sandbox.set_timeout(373)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before, "set_timeout")),
               timeout=15, description="actual successful publication after failed business boundary")


@pytest.fixture
def business_proxy(p6_master):
    path = os.environ.get("WEBHOOK_P6_BUSINESS_PROXY_DIR")
    if not path:
        pytest.skip("dedicated business RESP proxy required")
    directory = Path(path)
    owned_path(directory)
    address = (directory / "ready").read_text().strip()
    assert address.startswith("127.0.0.1:")
    p6 = p6_master
    config = copy.deepcopy(p6.base)
    config["redis"]["nodes"] = address
    p6.restart(config)
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


def node_listing(p6):
    result = subprocess.check_output(["sudo", str(owned_path(setting("NODE_CLI"))), "--address",
                                     str(owned_path(setting("NODE_SOCKET"))), "cubebox", "ls", "--no-trunc"], text=True)
    with (p6.output / "node-list.jsonl").open("a") as stream:
        stream.write(json.dumps({"at": time.time(), "listing": result}) + "\n")
    return result


def test_create_proxy_failure_rolls_back_without_created_or_killed(sdk_backend, sdk_e2e_config,
                                                                  p6_master, business_proxy):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    from cubesandbox._exceptions import ApiError
    p6 = p6_master
    arm, directory = business_proxy
    sequence, first = arm("HSET", key_prefix="cube:v1:shared:sandbox:proxy:")
    try:
        with pytest.raises(ApiError) as failure:
            CubeSandboxAdapter.create(sdk_e2e_config, metadata={"test_suite": "p6-create-rollback"})
        assert "setProxyToRedis" in str(failure.value)
        wait_until(first.is_file, timeout=5, description="actual create proxy write rejected")
        hits = [json.loads(path.read_text()) for path in directory.glob(f"fault-{sequence}*.json")]
        ids = {hit["matched_key"].removeprefix("cube:v1:shared:sandbox:proxy:") for hit in hits}
        assert ids and all(re.fullmatch(r"[a-zA-Z0-9-]+", item) for item in ids)
    finally:
        (directory / "control.json").write_text("{}")
    wait_until(lambda: all(item not in node_listing(p6) for item in ids), timeout=40,
               description="actual failed-create rollback removes node sandbox")
    other = CubeSandboxAdapter.create(sdk_e2e_config)
    try:
        positive_barrier(p6, other, ())
    finally:
        other.kill()
    from framework.webhook import records
    for sandbox_id in ids:
        assert not records(p6.env["receiver_log"], sandbox_id, p6.env["subscription_id"])
    p6.save("create-rollback", {"business_error": str(failure.value), "sandbox_ids": sorted(ids), "faults": hits})


def test_async_destroy_waits_for_master_proxy_cleanup(request, sdk_backend, p6_master, business_proxy):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    arm, directory = business_proxy
    before = {event["id"] for _,event in p6.observed(sandbox)}
    sequence, first = arm("DEL", key="cube:v1:shared:sandbox:proxy:" + sandbox.sandbox_id)
    session = requests.Session()
    session.trust_env = False
    try:
        response = session.delete(p6.env["master_url"] + "/cube/sandbox",
                                  json={"sandbox_id": sandbox.sandbox_id, "sync": False}, timeout=10)
        assert response.json()["ret"]["ret_code"] == 200
        wait_until(first.is_file, timeout=15, description="destroy reached Master cleanup after node success")
        wait_until(lambda: sandbox.sandbox_id not in node_listing(p6), timeout=15,
                   description="node sandbox absent while Master cleanup is rejected")
        assert not new_events(p6.observed(sandbox), "killed", before)
        p6.save("before-cleanup-recovery", {"accepted": response.json(), "fault": json.loads(first.read_text()),
                                            "http": [row for row,event in p6.observed(sandbox)]})
    finally:
        (directory / "control.json").write_text("{}")
        session.close()
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "killed", before)), timeout=35,
               description="same async task completes cleanup and publishes killed")
    assert len(new_events(p6.observed(sandbox), "killed", before)) == 1


def test_paused_delete_final_binding_failure_has_no_killed(request, sdk_backend, p6_master, database_fault):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    sandbox.pause()
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "paused")), timeout=15, description="actual pause")
    before = {event["id"] for _,event in p6.observed(sandbox)}
    session = requests.Session()
    session.trust_env = False
    def destroy():
        response = session.delete(p6.env["master_url"] + "/cube/sandbox",
            json={"sandbox_id": sandbox.sandbox_id, "sync": True}, timeout=20)
        p6.save("delete-" + str(time.time_ns()), response.json())
        return response.json()
    try:
        with database_fault("t_cube_pause_snapshot", f"OLD.sandbox_id='{sandbox.sandbox_id}'", "DELETE") as (message,sql):
            failed = destroy()
            # Binding removal is historically warn-only. Notification wiring
            # must preserve that API result, while withholding killed until
            # the necessary Master cleanup has actually completed.
            assert failed["ret"]["ret_code"] == 200
            assert sql(f"SELECT COUNT(*) FROM t_cube_pause_snapshot WHERE sandbox_id='{sandbox.sandbox_id}';").strip() == "1"
            from adapters.cubesandbox_adapter import CubeSandboxAdapter
            from framework.config import SdkE2EConfig
            other = CubeSandboxAdapter.create(SdkE2EConfig.from_env())
            try:
                positive_barrier(p6, other, ())
            finally:
                other.kill()
            assert not new_events(p6.observed(sandbox), "killed", before)
        assert destroy()["ret"]["ret_code"] == 200
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "killed", before)), timeout=15,
                   description="paused delete final cleanup succeeds")
        assert len(new_events(p6.observed(sandbox), "killed", before)) == 1
    finally:
        session.close()


def test_final_claim_crash_before_http_consumes_budget_without_sending(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    # A TLS handshake gate proves the Sender was entered after a valid Claim,
    # while withholding ServerHello makes an HTTP request impossible. No TLS
    # verification is disabled, and no certificate or fake receipt is supplied.
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen(2)
        listener.settimeout(10)
        config = copy.deepcopy(p6.base)
        config["webhooks"]["max_attempts"] = 1
        config["webhooks"]["subscriptions"][0]["url"] = f"https://127.0.0.1:{listener.getsockname()[1]}/events"
        p6.restart(config)
        before = {row["key"] for row in p6.durable()}
        sandbox.set_timeout(319)
        peer, _ = listener.accept()
        with peer:
            peer.settimeout(2)
            hello = peer.recv(8192)
            assert hello[:2] == b"\x16\x03", "Sender did not reach a TLS ClientHello"
            tasks = [row for row in p6.durable() if row["key"] not in before]
            assert len(tasks) == 1
            saved = tasks[0]
            assert saved["record"]["state"] == "inflight" and saved["record"]["attempts"] == 1
            pid = p6.pid()
            executable = subprocess.check_output(["sudo", "readlink", f"/proc/{pid}/exe"], text=True).strip()
            assert Path(executable).resolve() == owned_path(setting("MASTER_BINARY"))
            subprocess.run(["sudo", "kill", "-KILL", str(pid)], check=True)
        p6.save("claimed-before-http-crash", {"task": saved, "tls_client_hello_hex": hello.hex(), "pid": pid})
        p6.start()
        wait_until(p6.healthy, timeout=20, description="full Master restarted after final Claim crash")
        def task():
            return next(row for row in p6.durable() if row["key"] == saved["key"])
        wait_until(lambda: task()["record"]["state"] == "failed", timeout=30,
                   description="expired final Claim terminates without a new attempt")
        assert task()["record"]["attempts"] == 1 and task()["record"]["input"] == saved["record"]["input"]
        listener.settimeout(1)
        with pytest.raises(socket.timeout):
            listener.accept()
        assert not new_events(p6.observed(sandbox), "updated")
        p6.save("final-claim-exhausted", task())


@pytest.mark.parametrize("boundary", ["pause_complete", "snapshot_ready", "job_ready"])
def test_final_database_write_failure_does_not_publish_success(request, sdk_backend, p6_master,
                                                              database_fault, boundary):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    from cubesandbox._exceptions import ApiError
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    assert re.fullmatch(r"[a-zA-Z0-9-]+", sandbox.sandbox_id)
    before = {event["id"] for _, event in p6.observed(sandbox)}
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
        p6.save("final-write-failed", {"boundary": boundary, "business_error": str(failure.value),
                                     "stored_state": state})
        assert '"status": "READY"' not in state
    # Pause's node is already torn down; use a second real sandbox as the Writer barrier.
    if boundary == "pause_complete":
        from adapters.cubesandbox_adapter import CubeSandboxAdapter
        from framework.config import SdkE2EConfig
        other = CubeSandboxAdapter.create(SdkE2EConfig.from_env())
        try:
            positive_barrier(p6, other, ())
        finally:
            other.kill()
    else:
        positive_barrier(p6, sandbox, before)
    assert not new_events(p6.observed(sandbox), "paused", before)
    assert not new_events(p6.observed(sandbox), "checkpointed", before)
    assert not new_events(p6.observed(sandbox), "killed", before)


def test_resume_final_proxy_purge_failure_has_no_resumed(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_PROXY_FAULTS") != "1":
        pytest.skip("dedicated real proxy fault not requested")
    from cubesandbox._exceptions import ApiError
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    container = setting("PROXY_CONTAINER")
    container_info(container)
    label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container], text=True)
    assert label.strip() == setting("RESOURCE_LABEL")
    assert label.strip() == resource_label()
    sandbox.pause()
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "paused")), timeout=15,
               description="actual pause before final purge fault")
    before = {event["id"] for _, event in p6.observed(sandbox)}
    subprocess.run(["docker", "stop", container], check=True, capture_output=True)
    try:
        with pytest.raises(ApiError) as failure:
            sandbox.resume_or_connect(timeout=300)
        assert "pre-pause route" in str(failure.value)
        p6.save("purge-error", {"business_error": str(failure.value)})
    finally:
        subprocess.run(["docker", "start", container], check=True, capture_output=True)
    positive_barrier(p6, sandbox, before)
    assert not new_events(p6.observed(sandbox), "resumed", before)
    assert not new_events(p6.observed(sandbox), "updated", before, "network_egress")
    assert len(new_events(p6.observed(sandbox), "updated", before, "set_timeout")) == 1


@pytest.mark.parametrize("mode,reason", [("require-signature", "signature_headers_missing"),
                                         ("strict-execution", "execution_id_not_uuid")])
def test_receiver_policy_rejects_real_payload_and_exhausts_budget(request, sdk_backend, p6_master, mode, reason):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    before = {event["id"] for _, event in p6.observed(sandbox)}
    p6.mode.write_text(mode)
    try:
        sandbox.set_timeout(321)
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before)), timeout=15,
                   description="real payload rejected by receiver policy")
        event_id = new_events(p6.observed(sandbox), "updated", before)[0]["id"]
        def task():
            return next(x for x in p6.durable() if x["event"]["id"] == event_id)
        wait_until(lambda: task()["record"]["state"] == "failed", timeout=25,
                   description="bounded rejection retries exhausted")
        attempts = [row for row,event in p6.observed(sandbox) if event["id"] == event_id]
        assert len(attempts) == task()["record"]["max"] == task()["record"]["attempts"]
        assert all(row["rejection_reason"] == reason for row in attempts)
        p6.save("receiver-policy", {"mode": mode, "task": task(), "http": attempts})
    finally:
        p6.mode.write_text("204")
    positive_barrier(p6, sandbox, {event_id})
    assert len([row for row,event in p6.observed(sandbox) if event["id"] == event_id]) == len(attempts)


@pytest.mark.parametrize("policy,allowed,denied,domains", [
    ({"allow_internet_access": True}, None, None, None),
    ({"allow_internet_access": False}, None, ["0.0.0.0/0"], None),
    ({"allow_out": ["203.0.113.0/24"], "deny_out": ["198.51.100.0/24"]},
     ["203.0.113.0/24"], ["198.51.100.0/24"], None),
    ({"allow_internet_access": False, "allow_out": ["203.0.113.0/24", "example.org"],
      "rules": [{"name": "p6-mixed", "match": {"scheme": "https", "host": "example.org"},
                 "action": {"allow": False}}]}, ["203.0.113.0/24"], ["0.0.0.0/0"], ["example.org"]),
])
def test_network_projection_while_spec_persistence_fails(request, sdk_backend, p6_master,
                                                        database_fault, policy, allowed, denied, domains):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    before = {event["id"] for _,event in p6.observed(sandbox)}
    with database_fault("t_cube_sandbox_spec", f"NEW.sandbox_id='{sandbox.sandbox_id}'") as (_,sql):
        stored = sql(f"SELECT request_json FROM t_cube_sandbox_spec WHERE sandbox_id='{sandbox.sandbox_id}';")
        sandbox.update_network(policy)
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before, "network_egress")),
                   timeout=15, description="node-applied policy not suppressed by best-effort spec failure")
        assert sql(f"SELECT request_json FROM t_cube_sandbox_spec WHERE sandbox_id='{sandbox.sandbox_id}';") == stored
    matches = new_events(p6.observed(sandbox), "updated", before, "network_egress")
    assert len(matches) == 1
    assert_payload(matches[0], "updated", network={"allowed_cidrs": allowed, "denied_cidrs": denied,
                                                   "allowed_domains": domains})
    p6.save("network-with-spec-failure", {"requested": policy, "event": matches[0]})


@pytest.mark.parametrize("kind", ["empty", "user-keys", "oversized-entries", "oversized-bytes"])
def test_real_metadata_boundaries_preserve_business_and_known_fields(sdk_backend, sdk_e2e_config, p6_master, kind):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment required")
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    p6 = p6_master
    metadata = {
        "empty": {}, "user-keys": {"user_token":"fixture-only", "api_token":"not-a-secret", "empty":""},
        "oversized-entries": {f"key{i}":"value" for i in range(65)},
        "oversized-bytes": {"large":"x"*17000},
    }[kind]
    session = requests.Session()
    session.trust_env = False
    def metrics():
        response = session.get(p6.env["master_url"] + "/metrics", timeout=5)
        response.raise_for_status()
        return response.text
    def oversized():
        return sum(float(line.rsplit(" ",1)[1]) for line in metrics().splitlines()
                   if 'operation="capture"' in line and 'result="oversized"' in line)
    initial = oversized()
    sandbox = CubeSandboxAdapter.create(sdk_e2e_config, metadata=metadata)
    try:
        positive_barrier(p6, sandbox, ())
        created = new_events(p6.observed(sandbox), "created")
        if kind.startswith("oversized"):
            wait_until(lambda: oversized() > initial, timeout=5, description="bounded capture rejection observed")
            assert not created, "oversized capture should not leak a partial created event"
        else:
            wait_until(lambda: bool(new_events(p6.observed(sandbox), "created")), timeout=15,
                       description="real created event for bounded metadata")
            sandbox.update_network({"allow_out": ["203.0.113.0/24"]})
            wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", field="network_egress")),
                       timeout=15, description="same metadata provenance on subsequent network event")
            for _,event in p6.observed(sandbox):
                if event["type"].endswith(".created") or "network_egress" in event.get("event_data", {}):
                    actual = event.get("event_data", {}).get("sandbox_metadata")
                    if kind == "empty":
                        assert actual in (None, {})
                    else:
                        assert actual == metadata
                assert event["sandbox_execution_id"] == event["sandbox_team_id"] == event["sandbox_build_id"] == ""
        p6.save("metadata-boundary", {"kind": kind, "sandbox_id":sandbox.sandbox_id,
            "http":[row for row,event in p6.observed(sandbox)], "capture_oversized_before":initial,
            "capture_oversized_after":oversized()})
    finally:
        sandbox.kill()
        session.close()


def test_mixed_master_disable_window_and_coordinated_revocation(request, sdk_backend, p6_master):
    unit = os.environ.get("WEBHOOK_P6_SECOND_MASTER_UNIT")
    path = os.environ.get("WEBHOOK_P6_SECOND_MASTER_CONFIG")
    if sdk_backend != "cubesandbox" or not unit or not path:
        pytest.skip("second full dedicated Master configuration required")
    checked_unit(unit)
    config_path = Path(path)
    owned_path(config_path)
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    original = config_path.read_text()
    second = yaml.safe_load(original)
    assert second["instance_db_config"]["db_name"] == p6.base["instance_db_config"]["db_name"]
    second["webhooks"] = copy.deepcopy(p6.base["webhooks"])
    session = requests.Session()
    session.trust_env = False
    def command(action):
        result = subprocess.run(["sudo", "systemctl", action, unit], capture_output=True, text=True)
        with (p6.output / "secondary-commands.jsonl").open("a") as stream:
            stream.write(json.dumps({"action":action,"at":time.time(),"returncode":result.returncode,
                                     "stdout":result.stdout,"stderr":result.stderr})+"\n")
        result.check_returncode()
    def write(value):
        config_path.write_text(yaml.safe_dump(value, sort_keys=False))
    def healthy():
        try:
            return session.get(endpoint("SECOND_MASTER_URL") + "/notify/health",timeout=1).status_code==200
        except requests.RequestException:
            return False
    command("stop")
    try:
        event_id,task,saved = pending_update(p6,sandbox)
        off = copy.deepcopy(p6.base)
        off["webhooks"]["enabled"] = False
        p6.restart(off)
        # Disabling the new instance cannot revoke the old instance's snapshot.
        write(second)
        command("start")
        wait_until(healthy,timeout=20,description="second full Master with old enabled snapshot")
        wait_until(lambda:task()["record"]["state"]=="succeeded",timeout=25,
                   description="old Master still delivers during mixed disable rollout")
        assert task()["record"]["input"]==saved["record"]["input"]
        attempts=[row for row,event in p6.observed(sandbox) if event["id"]==event_id]
        assert len(attempts)==2 and [row["response_status"] for row in attempts]==[503,204]
        p6.save("mixed-window",{"saved":saved,"final":task(),"http":attempts})
        command("stop")
        p6.restart(p6.base)
        event_id,task,saved = pending_update(p6,sandbox)
        # All old consumers are stopped before changing subscription policy.
        disabled_second=copy.deepcopy(second)
        disabled_second["webhooks"]["enabled"]=False
        write(disabled_second)
        p6.restart(off)
        command("start")
        wait_until(healthy,timeout=20,description="both full Masters globally disabled")
        time.sleep(2)
        assert task()==saved
        command("stop")
        revoked=copy.deepcopy(p6.base)
        revoked["webhooks"]["subscriptions"]=[]
        revoked_second=copy.deepcopy(second)
        revoked_second["webhooks"]["subscriptions"]=[]
        write(revoked_second)
        p6.restart(revoked)
        command("start")
        wait_until(healthy,timeout=20,description="both full Masters use revoked subscriptions")
        wait_until(lambda:task()["record"]["state"]=="cancelled",timeout=20,
                   description="saved task cancelled after coordinated revocation")
        attempts=[row for row,event in p6.observed(sandbox) if event["id"]==event_id]
        assert len(attempts)==1 and attempts[0]["response_status"]==503
        p6.save("coordinated-revocation",{"saved":saved,"final":task(),"http":attempts})
    finally:
        command("stop")
        config_path.write_text(original)
        session.close()
