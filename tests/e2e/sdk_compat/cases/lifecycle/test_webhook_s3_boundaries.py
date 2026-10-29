# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Real S3 local-success boundaries; establishment faults are external RPC errors."""
from dataclasses import replace
import json
import os
from pathlib import Path
import subprocess
import socket
import time
from uuid import uuid4

import pytest
import requests

from adapters.cubesandbox_adapter import CubeSandboxAdapter
from cases.lifecycle.test_webhook_recovery import p6_master, sandbox_and_barrier
from framework.webhook_resources import (
    setting, owned_path, checked_unit, resource_label, endpoint
)
from framework.lifecycle import wait_until
from framework.webhook import new_events, records

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.fixture
def s3_status(p6_master):
    p6 = p6_master
    session = requests.Session()
    session.trust_env = False
    def get(path, **params):
        response = session.get(p6.env["master_url"] + path, params=params, timeout=10)
        value = response.json()
        with (p6.output / "status.jsonl").open("a") as stream:
            stream.write(json.dumps({"at": time.time(), "path": path, "body": value}) + "\n")
        response.raise_for_status()
        assert value["ret"]["ret_code"] == 200, value
        return value
    yield get
    session.close()


@pytest.mark.parametrize("mode", ["error", "empty"])
@pytest.mark.parametrize("operation", ["pause", "checkpoint"])
def test_s3_export_establishment_failure_preserves_local_success(request, sdk_backend, sdk_e2e_config,
                                                                 p6_master, s3_status, mode, operation):
    path = os.environ.get("WEBHOOK_P6_STORAGE_PROXY_MODE")
    if sdk_backend != "cubesandbox" or not path:
        pytest.skip("dedicated real S3 node with external RPC fault fixture required")
    control = Path(path)
    owned_path(control)
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    snapshot = None
    restored = None
    before = {event["id"] for _,event in p6.observed(sandbox)}
    control.write_text(mode)
    try:
        if operation == "pause":
            sandbox.pause()
            detail = s3_status("/cube/sandbox/info", sandbox_id=sandbox.sandbox_id)["data"][0]
            snapshot_id = detail["annotations"]["cube.master.pause.snapshot.id"]
            inventory = s3_status("/cube/sandbox/inventory", host_id=detail["host_id"])["data"]
            state = next(row for row in inventory if row["sandbox_id"] == sandbox.sandbox_id)
            assert state["backend"] == "s3", state
            assert state["pause_snapshot_id"] == snapshot_id
        else:
            snapshot = sandbox.create_snapshot()
            state = s3_status("/cube/snapshot/" + snapshot)["snapshot"]
            assert state["backend"] == "s3" and state["status"] == "READY", state
            snapshot_id = snapshot
        label = "paused" if operation == "pause" else "checkpointed"
        wait_until(lambda: bool(new_events(p6.observed(sandbox), label, before)), timeout=15,
                   description="local-success event despite failed export establishment")
        # Restore actual local data, rather than inferring validity from READY alone.
        control.write_text("forward")
        if operation == "pause":
            sandbox.resume_or_connect(timeout=300)
        else:
            restored = CubeSandboxAdapter.create(replace(sdk_e2e_config, cube_template_id=snapshot),
                create_options={"distribution_scope": [state["origin_node_id"]], "timeout": 300})
        target = restored or sandbox
        wait_until(lambda: bool(new_events(records(p6.env["receiver_log"], target.sandbox_id,
                   p6.env["subscription_id"]), "resumed")), timeout=15, description="actual same-node local restore")
        for _ in range(3):
            sandbox.info()
            time.sleep(1)
        assert len(new_events(p6.observed(sandbox), label, before)) == 1
        p6.save("export-local-boundary", {"mode": mode, "operation": operation, "state": state,
            "snapshot_id": snapshot_id, "restored_sandbox": target.sandbox_id,
            "http": [row for row,event in p6.observed(sandbox)]})
    finally:
        control.write_text("forward")
        if restored:
            restored.kill()
        if snapshot:
            sandbox.delete_snapshot(snapshot)


@pytest.mark.parametrize("operation", ["pause", "checkpoint"])
def test_s3_local_snapshot_failure_has_no_success_event(request, sdk_backend, p6_master, operation):
    path = os.environ.get("WEBHOOK_P6_STORAGE_PROXY_MODE")
    if sdk_backend != "cubesandbox" or not path:
        pytest.skip("dedicated native S3 snapshot fault fixture required")
    from cubesandbox._exceptions import ApiError
    from cases.lifecycle.test_webhook_completion_boundaries import positive_barrier
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    control = Path(path)
    owned_path(control)
    before = {event["id"] for _, event in p6.observed(sandbox)}
    try:
        control.write_text("snapshot-error")
        with pytest.raises(ApiError) as failure:
            sandbox.pause() if operation == "pause" else sandbox.create_snapshot()
        assert "P6 dedicated storage operation rejected" in str(failure.value)
        p6.save("local-snapshot-failed", {"operation": operation, "business_error": str(failure.value)})
    finally:
        control.write_text("forward")
    positive_barrier(p6, sandbox, before)
    for label in ["paused", "checkpointed", "killed"]:
        assert not new_events(p6.observed(sandbox), label, before)


def test_unready_remote_snapshot_with_origin_service_unavailable_is_not_resumed(request, sdk_backend,
                                                                               sdk_e2e_config, p6_master,
                                                                               s3_status):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_CROSS_UNREADY") != "1":
        pytest.skip("dedicated independent NodeB and origin control required")
    mode = Path(os.environ["WEBHOOK_P6_S3_HTTP_MODE"])
    owned_path(mode)
    assert mode.read_text().strip() == "forward"
    from cubesandbox._exceptions import ApiError
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    session = requests.Session()
    session.trust_env = False
    for container in [setting("MINIO_CONTAINER"), setting("NODE_B_CONTAINER")]:
        info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
        assert info["Config"]["Labels"]["purpose"] == resource_label() and info["State"]["Running"]
    target = session.get(endpoint("OPS_URL") + "/internal/v1/nodes/" + setting("RESTORE_TARGET"), timeout=5).json()
    assert target["Healthy"] is True and not target.get("LocalTemplates"), target
    p6.save("independent-target", target)
    snapshot = None
    origin_stopped = False
    marker = uuid4().hex
    def node_reachable():
        try:
            with socket.create_connection((setting("NODE_HOST"), int(setting("NODE_PORT"))), timeout=.5):
                return True
        except OSError:
            return False
    try:
        # Hold only remote manifest publication; local S3 reads must remain
        # available for the genuine local snapshot to finish.
        mode.write_text("hold-manifest")
        snapshot = sandbox.create_snapshot()
        state = s3_status("/cube/snapshot/" + snapshot)["snapshot"]
        assert state["backend"] == "s3" and state["status"] == "READY" and state["remote_status"] == "inprogress", state
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "checkpointed")), timeout=15,
                   description="local checkpoint success before unavailable-origin restore")
        response = session.delete(endpoint("OPS_URL") + "/internal/v1/nodes/" + setting("RESTORE_TARGET") + "/isolation", timeout=5)
        response.raise_for_status()
        def target_schedulable():
            response = session.get(p6.env["master_url"] + "/internal/node",
                                   params={"host_id": target["InstanceID"]}, timeout=5)
            rows = response.json().get("data", [])
            return rows and rows[0]["Healthy"] and not rows[0]["SchedulingDisabled"]
        wait_until(target_schedulable, timeout=15, description="Master observes real target unisolated")
        subprocess.run(["sudo", "systemctl", "stop", checked_unit(setting("NODE_UNIT"))], check=True)
        origin_stopped = True
        assert not node_reachable(), "origin Cubelet still reachable"
        with pytest.raises(ApiError) as failure:
            CubeSandboxAdapter.create(replace(sdk_e2e_config, cube_template_id=snapshot),
                metadata={"p6_restore_failure_marker": marker},
                create_options={"distribution_scope":[target["InstanceID"]],"timeout":300})
        # FromSnap intentionally pins non-ready S3 to its origin before the
        # scheduler. Its existing API may report NoResource for that unavailable
        # origin; do not invent a more precise classification protocol.
        assert ("returned error code 130597:" in str(failure.value).lower() or "ready" in str(failure.value).lower()
                or "remote" in str(failure.value).lower()), str(failure.value)
        p6.save("unready-cross-node", {"snapshot": state, "marker": marker,
            "origin_service_reachable": False, "target": target, "business_error": str(failure.value)})
    finally:
        mode.write_text("forward")
        if origin_stopped:
            subprocess.run(["sudo", "systemctl", "start", checked_unit(setting("NODE_UNIT"))], check=True)
            wait_until(node_reachable, timeout=60, description="origin service restored")
        session.put(endpoint("OPS_URL") + "/internal/v1/nodes/" + setting("RESTORE_TARGET") + "/isolation", timeout=5).raise_for_status()
        if snapshot:
            # A listening socket precedes recovery of Master's gRPC connection.
            # Retry only that transient transport failure while restoring the fixture.
            cleanup_errors = []
            def delete_after_origin_reconnect():
                try:
                    sandbox.delete_snapshot(snapshot)
                    return True
                except ApiError as error:
                    if "rpc error: code = Unavailable" not in str(error):
                        raise
                    cleanup_errors.append({"at": time.time(), "error": str(error)})
                    p6.save("origin-reconnect-cleanup", cleanup_errors)
                    return False
            wait_until(delete_after_origin_reconnect, timeout=30,
                       description="snapshot cleanup after origin gRPC reconnect")
        session.close()
    sandbox.set_timeout(377)
    wait_until(lambda: bool(new_events(p6.observed(sandbox),"updated",field="set_timeout")),
               timeout=15, description="positive publication barrier after rejected cross-node restore")
    all_http = Path(p6.env["receiver_log"]).read_text().splitlines()
    import base64
    for line in all_http:
        event = json.loads(base64.b64decode(json.loads(line)["body_base64"]))
        assert event.get("event_data",{}).get("sandbox_metadata",{}).get("p6_restore_failure_marker") != marker
    assert not any(row["event"].get("event_data",{}).get("sandbox_metadata",{}).get("p6_restore_failure_marker") == marker
                   for row in p6.durable()), "failed restore persisted a success notification"


@pytest.mark.parametrize("failure_point", ["manifest", "registry"])
def test_native_s3_upload_failure_does_not_retract_or_duplicate_checkpoint(request, sdk_backend,
                                                                         p6_master, s3_status,
                                                                         failure_point):
    path = os.environ.get("WEBHOOK_P6_S3_HTTP_MODE")
    if sdk_backend != "cubesandbox" or not path:
        pytest.skip("dedicated S3 HTTP rejection proxy not requested")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    before = {event["id"] for _,event in p6.observed(sandbox)}
    mode = Path(path)
    owned_path(mode)
    assert mode.read_text().strip() == "forward"
    http_log = mode.with_name("access.log")
    start_line = len(http_log.read_text().splitlines())
    import boto3
    from botocore.config import Config
    from botocore.exceptions import ClientError
    info = json.loads(subprocess.check_output(["docker", "inspect", setting("MINIO_CONTAINER")], text=True))[0]
    assert info["Config"]["Labels"]["purpose"] == resource_label()
    settings = dict(item.split("=",1) for item in info["Config"]["Env"] if "=" in item)
    client = boto3.client("s3", endpoint_url=endpoint("S3_HTTP_URL"), region_name="us-east-1",
        aws_access_key_id=settings["MINIO_ROOT_USER"], aws_secret_access_key=settings["MINIO_ROOT_PASSWORD"],
        config=Config(proxies={}, retries={"max_attempts": 0}))
    snapshot = None
    try:
        mode.write_text("deny-manifest" if failure_point == "manifest" else "deny-exports")
        with pytest.raises(ClientError) as rejected:
            client.put_object(Bucket=setting("S3_BUCKET"), Key="p6-deny-probe", Body=b"p6-test")
        assert rejected.value.response["ResponseMetadata"]["HTTPStatusCode"] == 403
        p6.save("http-fault-probe", rejected.value.response)
        snapshot = sandbox.create_snapshot()
        initial = s3_status("/cube/snapshot/" + snapshot)["snapshot"]
        assert initial["status"] == "READY" and initial["backend"] == "s3"
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "checkpointed", before)), timeout=15,
                   description="local checkpoint notification before asynchronous export result")
        rejected_uploads = []
        def native_rejected():
            rejected_uploads.clear()
            for line in http_log.read_text().splitlines()[start_line:]:
                fields = line.split()
                if len(fields) != 5 or fields[1] != "PUT" or fields[3] != "403":
                    continue
                uri = fields[2]
                if ((failure_point == "manifest" and "/exports/" in uri and uri.endswith(".json"))
                        or (failure_point == "registry" and uri.endswith("/meta/exports.json"))):
                    rejected_uploads.append(line)
            return bool(rejected_uploads)
        wait_until(native_rejected, timeout=30, description="actual native export publication rejected, beyond the probe")
        # Record the dependency's real status without requiring a storage fix.
        # Local success must retain one notification regardless of that status.
        for _ in range(3):
            s3_status("/cube/snapshot/" + snapshot)
            time.sleep(1)
        sandbox.set_timeout(401)
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before, "set_timeout")),
                   timeout=15, description="positive publisher barrier after asynchronous S3 failure")
        assert len(new_events(p6.observed(sandbox), "checkpointed", before)) == 1
        p6.save("native-upload-observation", {"failure_point": failure_point, "initial": initial,
            "final": s3_status("/cube/snapshot/" + snapshot), "snapshot": snapshot,
            "rejected_native_requests": list(rejected_uploads),
            "events": new_events(p6.observed(sandbox), "checkpointed", before)})
    finally:
        mode.write_text("forward")
        if snapshot:
            sandbox.delete_snapshot(snapshot)
