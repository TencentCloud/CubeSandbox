#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Seed actual SDK killed events before the separately reviewed P6 host reboot.

This stops ONLY the inventoried dedicated Master after confirming saved tasks.
It does not reboot, arm boot recovery, edit queue records or leave live VMs.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk_compat"))
sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "sdk/python"))
from adapters.cubesandbox_adapter import CubeSandboxAdapter
from framework.config import SdkE2EConfig
from framework.webhook_resources import setting, run_root, owned_path, container_info, checked_unit, process_binary, endpoint
from framework.webhook import delivery_records, new_events, records


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    root = run_root()
    assert args.run.resolve() == root
    unit = checked_unit(setting("MASTER_UNIT"))
    checked_unit(setting("OPS_UNIT"))
    container = setting("REDIS_CONTAINER")
    container_info(container)
    container_info(setting("BUSINESS_REDIS_CONTAINER"))
    pid = int(subprocess.check_output(["systemctl", "show", unit, "-p", "MainPID", "--value"], text=True))
    process_binary(pid, "MASTER_BINARY")
    config = SdkE2EConfig.from_env()
    assert config.cube_api_url and config.cube_template_id
    password = setting("REDIS_PASSWORD")
    subscription = setting("SUBSCRIPTION_ID")
    directory = root / "reboot"
    directory.mkdir(mode=0o700, exist_ok=True)
    assert not (directory / "seed.json").exists(), "do not overwrite prior evidence"
    mode = owned_path(setting("RECEIVER_MODE"))
    log = owned_path(setting("RECEIVER_LOG"))
    mode.write_text("204")
    sandboxes, killed = [], set()
    stopped = False
    success = False
    try:
        for _ in range(3):
            sandboxes.append(CubeSandboxAdapter.create(config,
                metadata={"test_suite": "p6-host-reboot"}, create_options={"timeout": 600}))
        # Drain actual creates before injecting receiver failure.
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if all(row["record"]["state"] in {"succeeded", "failed", "cancelled"}
                   for row in delivery_records(container, password)):
                break
            time.sleep(.1)
        else:
            raise RuntimeError("preexisting deliveries did not quiesce")
        mode.write_text("503")
        for sandbox in sandboxes:
            sandbox.kill()
            killed.add(sandbox.sandbox_id)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            events = [new_events(records(log, sandbox.sandbox_id, subscription), "killed") for sandbox in sandboxes]
            if all(len(rows) == 1 for rows in events):
                break
            time.sleep(.01)
        else:
            raise RuntimeError("actual killed deliveries did not reach receiver")
        subprocess.run(["sudo", "kill", "-STOP", str(pid)], check=True)
        stopped = True
        ids = {rows[0]["id"] for rows in events}
        tasks = [row for row in delivery_records(container, password) if row["event"]["id"] in ids]
        assert len(tasks) == 3
        assert all(row["record"]["state"] in {"pending", "inflight"} and
                   0 < row["record"]["attempts"] < row["record"]["max"] for row in tasks)
        last_http = {}
        for sandbox in sandboxes:
            for row, event in records(log, sandbox.sandbox_id, subscription):
                if event["id"] in ids:
                    assert row["mode"] == "503"
                    last_http[event["id"]] = row
        manifest = {"boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "at": time.time(), "fault_layer": "normal host reboot; dedicated Master pre-stopped after persisted SDK kills",
            "operation": "actual Sandbox.kill; confirmed business cleanup", "master_pid": pid,
            "webhook_redis": container, "webhook_password": password,
            "business_redis": setting("BUSINESS_REDIS_CONTAINER"), "master_unit": unit,
            "ops_unit": setting("OPS_UNIT"), "tasks": tasks, "last_http": last_http,
            "original_cubelet_restored": False, "subscription": subscription,
            "run_root": str(root), "resource_label": setting("RESOURCE_LABEL"),
            "receiver_url": endpoint("RECEIVER_URL"), "redis_db": os.environ.get("WEBHOOK_P6_REDIS_DB", "6")}
        with (directory / "seed.json").open("x") as stream:
            json.dump(manifest, stream, indent=2)
            stream.flush()
            os.fsync(stream.fileno())
        subprocess.run(["sudo", "kill", "-KILL", str(pid)], check=True)
        stopped = False
        success = True
        print(json.dumps({"seeded": True, "file": str(directory / "seed.json"),
            "tasks": [{"event_id": row["event"]["id"], "state": row["record"]["state"],
                       "attempts": row["record"]["attempts"]} for row in tasks]}), flush=True)
    finally:
        mode.write_text("204")
        if stopped:
            subprocess.run(["sudo", "kill", "-CONT", str(pid)], check=True)
        if not success:
            subprocess.run(["sudo", "systemctl", "start", unit], check=True)
            for sandbox in sandboxes:
                if sandbox.sandbox_id not in killed:
                    sandbox.kill()


if __name__ == "__main__":
    main()
