# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Opt-in real S3 snapshot status observations, using the SDK/API and receiver."""
import json
import os
from pathlib import Path
import subprocess
import time

import pytest
import requests

from framework.webhook_resources import (
    owned_path, container_info, resource_label, endpoint
)
from framework.lifecycle import wait_until
from framework.webhook import new_events, records

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.mark.parametrize("unavailable", [False, True])
def test_s3_checkpoint_local_boundary_and_remote_status(request, sdk_backend, unavailable):
    names = ("WEBHOOK_P6_MASTER_URL", "WEBHOOK_P6_RECEIVER_LOG", "WEBHOOK_P6_SUBSCRIPTION_ID",
             "WEBHOOK_P6_STORAGE_EVIDENCE", "WEBHOOK_P6_MINIO_CONTAINER", "WEBHOOK_P6_S3_HTTP_MODE")
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_S3_STATUS") != "1" or not all(os.environ.get(k) for k in names):
        pytest.skip("dedicated real S3 platform status experiment not requested")
    container = os.environ["WEBHOOK_P6_MINIO_CONTAINER"]
    container_info(container)
    label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container], text=True)
    assert label.strip() == resource_label()
    output = owned_path(os.environ["WEBHOOK_P6_STORAGE_EVIDENCE"]) / ("outage-" if unavailable else "healthy-")
    output = output.with_name(output.name + str(time.time_ns()))
    output.mkdir(mode=0o700)
    session = requests.Session()
    session.trust_env = False
    base = os.environ["WEBHOOK_P6_MASTER_URL"]
    mode = Path(os.environ["WEBHOOK_P6_S3_HTTP_MODE"])
    owned_path(mode)
    assert mode.read_text().strip() == "forward"
    http_log = mode.with_name("access.log")
    start_line = len(http_log.read_text().splitlines())
    sandbox = request.getfixturevalue("sdk_sandbox")
    snapshot = None
    stopped = False

    def observed():
        return records(os.environ["WEBHOOK_P6_RECEIVER_LOG"], sandbox.sandbox_id,
                       os.environ["WEBHOOK_P6_SUBSCRIPTION_ID"])

    def command(action):
        result = subprocess.run(["docker", action, container], capture_output=True, text=True)
        (output / (action + ".json")).write_text(json.dumps({"command": ["docker", action, container],
            "returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr, "at": time.time()}))
        assert result.returncode == 0, result.stderr

    def info():
        response = session.get(base + "/cube/snapshot/" + snapshot, timeout=10)
        response.raise_for_status()
        value = response.json()
        with (output / "status.jsonl").open("a") as stream:
            stream.write(json.dumps({"at": time.time(), "response": value}) + "\n")
        assert value["ret"]["ret_code"] == 200, value
        return value["snapshot"]

    try:
        if unavailable:
            # Gate only asynchronous publication. Stopping the whole object
            # service before capture can also break the VM's local snapshot.
            mode.write_text("hold-manifest")
        before = {event["id"] for _, event in observed()}
        started = time.monotonic()
        snapshot = sandbox.create_snapshot()
        (output / "created.json").write_text(json.dumps({"sandbox_id": sandbox.sandbox_id,
            "snapshot_id": snapshot, "unavailable": unavailable, "elapsed": time.monotonic() - started}))
        wait_until(lambda: bool(new_events(observed(), "checkpointed", before)), timeout=15,
                   description="checkpoint notification after final local business success")
        initial = info()
        assert initial["backend"] == "s3" and initial["status"] == "READY", initial
        (output / "local-success.json").write_text(json.dumps({"snapshot": initial,
            "events": new_events(observed(), "checkpointed", before)}, indent=2))
        if unavailable:
            assert initial["remote_status"] == "inprogress", initial
            command("stop")
            stopped = True
            mode.write_text("forward")
            rejected_uploads = []
            def native_unavailable():
                rejected_uploads.clear()
                for line in http_log.read_text().splitlines()[start_line:]:
                    fields = line.split()
                    if (len(fields) == 5 and fields[1] == "PUT"
                            and ("/exports/" in fields[2] or "/data/" in fields[2])
                            and fields[3] in {"502", "503", "504"}):
                        rejected_uploads.append(line)
                return bool(rejected_uploads)
            wait_until(native_unavailable, timeout=20, description="native upload hits the stopped real S3 service")
            assert subprocess.check_output(["docker", "inspect", "--format", "{{.State.Running}}", container], text=True).strip() == "false"
            (output / "unavailable-upload.json").write_text(json.dumps(rejected_uploads, indent=2))
            for _ in range(3):
                info()  # Observe uncertainty/failure without changing storage classification.
                time.sleep(1)
            command("start")
            stopped = False
            def storage_healthy():
                try:
                    return session.get(endpoint("S3_HTTP_URL") + "/minio/health/live", timeout=1).status_code == 200
                except requests.RequestException:
                    return False
            wait_until(storage_healthy, timeout=20, description="same S3 service restored")
        # Query and upload terminal reconciliation must not publish a new checkpoint identity.
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if info().get("remote_status") in {"ready", "failed"}:
                break
            time.sleep(1)
        terminal = info()
        (output / "dependency-status.json").write_text(json.dumps(terminal, indent=2))
        for _ in range(3):
            info()
            time.sleep(1)
        sandbox.set_timeout(409)
        wait_until(lambda: bool(new_events(observed(), "updated", before, "set_timeout")), timeout=15,
                   description="positive publisher barrier after S3 observation/recovery")
        assert len(new_events(observed(), "checkpointed", before)) == 1
        (output / "http.json").write_text(json.dumps([row for row, event in observed()], indent=2))
        # The manifest is deliberately retained for an independently executed cross-node restore.
        if (not unavailable and os.environ.get("WEBHOOK_P6_KEEP_SNAPSHOT") == "1"
                and terminal["remote_status"] == "ready"):
            (output.parent / "cross-node-source.json").write_text(json.dumps({"snapshot_id": snapshot,
                "sandbox_id": sandbox.sandbox_id, "info": terminal}, indent=2))
            snapshot = None
    finally:
        mode.write_text("forward")
        if stopped:
            command("start")
        if snapshot:
            sandbox.delete_snapshot(snapshot)
        session.close()


def test_user_snapshot_start_on_explicit_real_node(sdk_backend, sdk_e2e_config):
    manifest = os.environ.get("WEBHOOK_P6_SNAPSHOT_MANIFEST")
    target = os.environ.get("WEBHOOK_P6_RESTORE_TARGET")
    output = os.environ.get("WEBHOOK_P6_RESTORE_EVIDENCE")
    if sdk_backend != "cubesandbox" or not all((manifest, target, output)):
        pytest.skip("retained real snapshot, explicit node and evidence path required")
    from dataclasses import replace
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    from framework.webhook import assert_payload
    source = json.loads(owned_path(manifest).read_text())
    assert source["info"]["backend"] in {"xfs", "s3"}
    if source["info"]["backend"] == "s3":
        assert source["info"]["remote_status"] == "ready"
    else:
        assert target == source["info"]["origin_node_id"]
    directory = owned_path(output)
    directory.mkdir(mode=0o700)
    sandbox = None
    started = time.monotonic()
    try:
        try:
            sandbox = CubeSandboxAdapter.create(replace(sdk_e2e_config, cube_template_id=source["snapshot_id"]),
                metadata={"test_suite": "sdk_compat", "p6_restore_target": target},
                create_options={"distribution_scope": [target], "timeout": 300})
        except Exception as error:
            if os.environ.get("WEBHOOK_P6_RESTORE_EXPECT_ERROR") != "missing-shared-dir":
                raise
            from cubesandbox._exceptions import ApiError
            import re
            assert isinstance(error, ApiError) and "ActivateVirtioFs" in str(error), str(error)
            match = re.search(r"failed to run container ([0-9a-f]{32}):", str(error))
            assert match, str(error)
            failed_id = match.group(1)
            time.sleep(2)
            observed = records(os.environ["WEBHOOK_P6_RECEIVER_LOG"], failed_id,
                               os.environ["WEBHOOK_P6_SUBSCRIPTION_ID"])
            assert not observed, "failed restore/rollback published a lifecycle success"
            (directory / "result.json").write_text(json.dumps({"expected_failure": True,
                "sandbox_id": failed_id, "error": str(error), "http": observed}, indent=2))
            return
        assert not os.environ.get("WEBHOOK_P6_RESTORE_EXPECT_ERROR"), "incompatible layout unexpectedly restored"
        info = sandbox.info().raw
        (directory / "business-info.json").write_text(json.dumps(info, indent=2))
        # Master authoritative placement information, rather than a requested pin alone.
        session = requests.Session()
        session.trust_env = False
        response = session.get(os.environ["WEBHOOK_P6_MASTER_URL"] + "/cube/sandbox/info",
            params={"sandbox_id": sandbox.sandbox_id}, timeout=10)
        (directory / "master-info.json").write_text(response.text)
        assert response.json()["ret"]["ret_code"] == 200, response.text
        assert response.json()["data"][0]["host_id"] == target, response.text
        def observed():
            return records(os.environ["WEBHOOK_P6_RECEIVER_LOG"], sandbox.sandbox_id,
                           os.environ["WEBHOOK_P6_SUBSCRIPTION_ID"])
        wait_until(lambda: bool(new_events(observed(), "resumed")), timeout=15, description="real user snapshot resumed event")
        events = new_events(observed(), "resumed")
        assert len(events) == 1 and not new_events(observed(), "created")
        assert_payload(events[0], "resumed", template=source["snapshot_id"],
                       metadata={"test_suite": "sdk_compat", "p6_restore_target": target})
        assert events[0]["event_data"]["resume_mode"] == "restore"
        assert not new_events(observed(), "updated"), "internal timeout refresh leaked updated"
        (directory / "result.json").write_text(json.dumps({"source": source, "target": target,
            "sandbox_id": sandbox.sandbox_id, "elapsed": time.monotonic()-started, "event": events[0]}, indent=2))
        session.close()
    finally:
        if sandbox:
            sandbox.kill()
