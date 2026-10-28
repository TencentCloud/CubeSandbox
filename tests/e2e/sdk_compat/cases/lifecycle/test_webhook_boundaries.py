# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Opt-in negative boundaries on the dedicated real node and full Master."""
import copy
import json
import os
from pathlib import Path
import subprocess
import time

import pytest
import requests

from cases.lifecycle.test_webhook_recovery import p6_master, sandbox_and_barrier
from framework.webhook_resources import (
    setting, owned_path, container_info, checked_unit, process_binary, resource_label
)
from framework.lifecycle import wait_until
from framework.webhook import assert_payload, new_events

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


def node_pid():
    unit = checked_unit(setting("NODE_UNIT"))
    pid = int(subprocess.check_output(["systemctl", "show", unit, "-p", "MainPID", "--value"], text=True))
    process_binary(pid, "NODE_BINARY")
    return pid


def test_node_network_failure_suppresses_updated(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_NODE_FAULTS") != "1":
        pytest.skip("dedicated node failure experiment not requested")
    from cubesandbox._exceptions import ApiError
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    container = setting("EGRESS_CONTAINER")
    container_info(container)
    label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container], text=True)
    assert label.strip() == resource_label()
    before = {event["id"] for _, event in p6.observed(sandbox)}
    network = {"allow_out": ["203.0.113.0/24"], "deny_out": [], "rules": [{"name": "p6-deny", "match": {"scheme": "https", "sni": "example.org", "host": "example.org"}, "action": {"allow": False}}]}
    subprocess.run(["docker", "stop", container], check=True, capture_output=True)
    try:
        with pytest.raises(ApiError) as failure:
            sandbox.update_network(network)
        p6.save("business-failure", {"error": str(failure.value), "at": time.time()})
        time.sleep(1)
        assert not new_events(p6.observed(sandbox), "updated", before)
    finally:
        subprocess.run(["docker", "start", container], check=True, capture_output=True)
    sandbox.update_network(network)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before)), timeout=15,
               description="positive network barrier after node dependency recovers")
    assert len(new_events(p6.observed(sandbox), "updated", before)) == 1


@pytest.mark.parametrize("operation", ["pause", "checkpoint"])
def test_local_snapshot_failure_has_no_success_event(request, sdk_backend, p6_master, operation):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_NODE_FAULTS") != "1":
        pytest.skip("dedicated local snapshot failure experiment not requested")
    from cubesandbox._exceptions import ApiError
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    pid = node_pid()
    target = str(owned_path(setting("NODE_XFS_ROOT")) /
                 ("pause-snapshots" if operation == "pause" else "snapshots"))
    prefix = ["sudo", "nsenter", "-t", str(pid), "-m", "--"]
    mounts = subprocess.check_output(prefix + ["findmnt", "-T", target, "-o", "TARGET,SOURCE,OPTIONS"], text=True)
    owned_path(Path(target))
    (p6.output / "mount-before.txt").write_text(mounts)
    before = {event["id"] for _, event in p6.observed(sandbox)}
    subprocess.run(prefix + ["mount", "--bind", target, target], check=True)
    try:
        subprocess.run(prefix + ["mount", "-o", "remount,bind,ro", target], check=True)
        with pytest.raises(ApiError) as failure:
            sandbox.pause() if operation == "pause" else sandbox.create_snapshot()
        p6.save("business-failure", {"operation": operation, "error": str(failure.value), "at": time.time()})
    finally:
        subprocess.run(prefix + ["umount", target], check=True)
        (p6.output / "mount-restored.txt").write_text(subprocess.check_output(prefix +
            ["findmnt", "-T", target, "-o", "TARGET,SOURCE,OPTIONS"], text=True))
    sandbox.update_network({"allow_out": [], "deny_out": []})
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before)), timeout=15,
               description="positive publication barrier after failed snapshot")
    assert not new_events(p6.observed(sandbox), "paused", before)
    assert not new_events(p6.observed(sandbox), "checkpointed", before)
    assert not new_events(p6.observed(sandbox), "killed", before)


@pytest.mark.parametrize("mode", ["sync", "async-blocked", "paused", "manager-timeout"])
def test_actual_destroy_boundaries(request, sdk_backend, p6_master, mode):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_NODE_FAULTS") != "1":
        pytest.skip("dedicated destroy boundary experiment not requested")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    session = requests.Session()
    session.trust_env = False
    before = {event["id"] for _, event in p6.observed(sandbox)}
    stopped = None
    try:
        if mode == "paused":
            sandbox.pause()
            wait_until(lambda: bool(new_events(p6.observed(sandbox), "paused", before)), timeout=15,
                       description="real pause final success")
            assert not new_events(p6.observed(sandbox), "killed", before)
        if mode == "manager-timeout":
            assert subprocess.check_output(["systemctl", "is-active", checked_unit(setting("LCM_UNIT"))], text=True).strip() == "active"
            sandbox.set_timeout(0)
        else:
            if mode == "async-blocked":
                sandbox.info()
                sandbox.set_timeout(300)
                stopped = node_pid()
                subprocess.run(["sudo", "kill", "-STOP", str(stopped)], check=True)
            body = {"sandbox_id": sandbox.sandbox_id, "sync": mode == "sync"}
            if mode == "sync":
                body["kill_reason"] = "p6-explicit"
            response = session.delete(p6.env["master_url"] + "/cube/sandbox", json=body, timeout=15)
            p6.save("delete-response", {"status": response.status_code, "body": response.json(), "at": time.time()})
            assert response.json()["ret"]["ret_code"] == 200
            if stopped:
                time.sleep(1)
                assert not new_events(p6.observed(sandbox), "killed", before), "accepted task was mistaken for completed destruction"
                subprocess.run(["sudo", "kill", "-CONT", str(stopped)], check=True)
                stopped = None
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "killed", before)), timeout=35,
                   description="confirmed final destroy notification")
        events = new_events(p6.observed(sandbox), "killed", before)
        assert len(events) == 1
        reason = "timeout" if mode == "manager-timeout" else ("p6-explicit" if mode == "sync" else "request")
        assert_payload(events[0], "killed", reason=reason)
        response = session.get(os.environ["CUBE_API_URL"] + "/sandboxes/" + sandbox.sandbox_id, timeout=10)
        assert response.status_code == 404, response.text
        p6.save("final", {"event": events[0], "business_info_http": response.status_code})
    finally:
        if stopped:
            subprocess.run(["sudo", "kill", "-CONT", str(stopped)], check=True)
        session.close()


@pytest.mark.parametrize("operation", ["HSET", "XADD"])
def test_authoritative_timeout_write_fault(request, sdk_backend, p6_master, operation):
    if sdk_backend != "cubesandbox" or not os.environ.get("WEBHOOK_P6_BUSINESS_PROXY_DIR"):
        pytest.skip("dedicated business Redis fault proxy required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    directory = owned_path(os.environ["WEBHOOK_P6_BUSINESS_PROXY_DIR"])
    proxy = (directory / "ready").read_text().strip()
    assert proxy.startswith("127.0.0.1:")
    control = directory / "control.json"
    config = copy.deepcopy(p6.base)
    config["redis"]["nodes"] = proxy
    p6.restart(config)
    key = "cube:v1:shared:sandbox:lifecycle:meta"
    container = setting("BUSINESS_REDIS_CONTAINER")
    container_info(container)
    label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container], text=True)
    assert label.strip() == resource_label()
    def meta():
        env = {**os.environ, "REDISCLI_AUTH": p6.base["redis"]["password"]}
        return json.loads(subprocess.check_output(["docker", "exec", "-e", "REDISCLI_AUTH",
            container, "redis-cli", "--json", "HGET", key, sandbox.sandbox_id], env=env, text=True))
    before_meta = meta()
    before = {event["id"] for _, event in p6.observed(sandbox)}
    sequence = time.time_ns()
    fault = {"id": sequence, "op": operation, "when": "before", "error": True,
             "key": key if operation == "HSET" else "cube:v1:shared:sandbox:lifecycle:events"}
    if operation == "HSET":
        fault["field"] = sandbox.sandbox_id
    temporary = control.with_suffix(".tmp")
    temporary.write_text(json.dumps(fault))
    temporary.replace(control)
    try:
        # Existing API success semantics are unchanged even if persistence fails.
        sandbox.set_timeout(317)
        fired = directory / f"fault-{sequence}.json"
        wait_until(fired.is_file, timeout=10, description="precisely matched authoritative write fault")
        evidence = json.loads(fired.read_text())
        assert evidence["upstream_executed"] is False and evidence["server_error"] is True
        control.write_text("{}")
        sandbox.update_network({"allow_out": [], "deny_out": []})
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before, "network_egress")),
                   timeout=15, description="positive Writer/receiver barrier")
        timeout_events = new_events(p6.observed(sandbox), "updated", before, "set_timeout")
        if operation == "HSET":
            assert meta() == before_meta
            assert not timeout_events
        else:
            assert meta() != before_meta
            assert len(timeout_events) == 1
        p6.save("authority", {"operation": operation, "fault": evidence, "before_meta": before_meta,
                              "after_meta": meta(), "timeout_events": timeout_events})
    finally:
        control.write_text("{}")
