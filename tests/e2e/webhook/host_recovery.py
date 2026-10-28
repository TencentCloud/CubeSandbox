#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""One-shot P6 boot witness. Prepare the reviewed manifest and units separately.

Does not reboot/power off the host. Never edits Redis tasks or original configs.
Only explicitly inventoried P6 containers/units are started. Keep the manifest
and pre-fault evidence off-host before an externally controlled hard reset.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk_compat"))
from framework.webhook import delivery_records, records
from framework.webhook_resources import setting, run_root, container_info, checked_unit, resource_label, endpoint
from urllib.parse import urlsplit


def command(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=45)
    print(json.dumps({"at": time.time(), "command": args, "returncode": result.returncode,
                      "stdout": result.stdout, "stderr": result.stderr}), flush=True)
    result.check_returncode()
    return result.stdout


def save(path, value):
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.flush()
        os.fsync(stream.fileno())


def wait(predicate, description, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.5)
    raise RuntimeError("timed out: " + description)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run", type=Path)
    parser.add_argument("--verify-plan", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    root = run_root()
    assert args.run.resolve() == root
    directory = root / "reboot"
    manifest = json.loads((directory / "armed.json").read_text())
    assert manifest["original_cubelet_restored"] is True
    assert manifest["run_root"] == str(root) and manifest["resource_label"] == resource_label()
    assert manifest["receiver_url"] == endpoint("RECEIVER_URL")
    assert manifest["redis_db"] == os.environ.get("WEBHOOK_P6_REDIS_DB", "6")
    assert urlsplit(endpoint("RECEIVER_URL")).scheme == "http", "boot witness starts a plain HTTP loopback receiver"
    for key, variable in (("webhook_redis", "REDIS_CONTAINER"), ("business_redis", "BUSINESS_REDIS_CONTAINER")):
        assert manifest[key] == setting(variable)
        container_info(manifest[key])
    for key, variable in (("master_unit", "MASTER_UNIT"), ("ops_unit", "OPS_UNIT")):
        assert manifest[key] == setting(variable)
        checked_unit(manifest[key])
    assert manifest["tasks"] and len({row["key"] for row in manifest["tasks"]}) == len(manifest["tasks"])
    boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    if args.verify_plan:
        print(json.dumps({"plan_verified": True, "current_boot": boot,
                          "expected_previous_boot": manifest["boot_id"], "tasks": len(manifest["tasks"])}), flush=True)
        return
    assert boot != manifest["boot_id"], "this is not a new host boot"
    output = directory / boot
    output.mkdir(mode=0o700)
    save(output / "started.json", {"boot_id": boot, "previous_boot_id": manifest["boot_id"], "at": time.time()})
    # Reapply the dedicated loopback-only firewall restriction before starting services.
    for name in ("MASTER_URL", "OPS_URL"):
        url = urlsplit(endpoint(name))
        assert url.hostname == "127.0.0.1", "boot witness requires loopback-only control services"
        port = url.port or (443 if url.scheme == "https" else 80)
        rule = ["INPUT", "!", "-i", "lo", "-p", "tcp", "--dport", str(port),
                "-m", "comment", "--comment", "webhook-1903-p6-boot-local-only", "-j", "REJECT"]
        if subprocess.run(["iptables", "-C", *rule], capture_output=True).returncode:
            command("iptables", "-A", *rule)
    for container in (manifest["webhook_redis"], manifest["business_redis"]):
        command("docker", "start", container)
    password = manifest["webhook_password"]
    def durable():
        return delivery_records(manifest["webhook_redis"], password)
    def loaded():
        try:
            return bool(durable())
        except (subprocess.CalledProcessError, json.JSONDecodeError):
            return False
    wait(loaded, "webhook AOF replay")
    actual = {row["key"]: row for row in durable()}
    for saved in manifest["tasks"]:
        assert actual[saved["key"]] == saved, "task changed before Master startup"
    save(output / "before-master.json", durable())
    assert urlsplit(endpoint("RECEIVER_URL")).hostname == "127.0.0.1"
    receiver = output / "requests.jsonl"
    process_log = (output / "receiver-process.log").open("x")
    process = subprocess.Popen([sys.executable, str(Path(__file__).with_name("receiver.py")),
        "--port", str(urlsplit(endpoint("RECEIVER_URL")).port or 80), "--fsync", "--output", str(receiver), "--limit", "2000"],
        stdout=process_log, stderr=subprocess.STDOUT)
    wait(lambda: "listening" in (output / "receiver-process.log").read_text() and process.poll() is None,
         "new receiver process", seconds=5)
    # Original services are operator-owned; this witness never starts them.
    # Dedicated services must declare their actual database/network dependencies.
    command("systemctl", "start", manifest["ops_unit"])
    command("systemctl", "start", manifest["master_unit"])
    expected = {row["key"]: row for row in manifest["tasks"]}
    def delivered():
        rows = {row["key"]: row for row in durable()}
        return all(rows[key]["record"]["state"] == "succeeded" for key in expected)
    wait(delivered, "full Master delivers actual pre-boot lifecycle tasks")
    final = {row["key"]: row for row in durable()}
    for key, saved in expected.items():
        task = final[key]
        assert task["record"]["input"] == saved["record"]["input"]
        assert task["record"]["attempts"] == saved["record"]["attempts"] + 1
        attempts = records(receiver, saved["event"]["sandbox_id"], manifest["subscription"])
        matching = [row for row, event in attempts if event["id"] == saved["event"]["id"]]
        assert len(matching) == 1 and matching[0]["response_status"] == 204
        assert matching[0]["body_base64"] == manifest["last_http"][saved["event"]["id"]]["body_base64"]
        assert matching[0]["delivery_id"] != manifest["last_http"][saved["event"]["id"]]["delivery_id"]
    save(output / "result.json", {"passed": True, "boot_id": boot, "previous_boot_id": manifest["boot_id"],
        "tasks": [final[key] for key in expected], "at": time.time(), "fault_layer": manifest["fault_layer"]})
    (directory / "armed.json").rename(output / "manifest.json")
    print(json.dumps({"passed": True, "evidence": str(output)}), flush=True)


if __name__ == "__main__":
    main()
