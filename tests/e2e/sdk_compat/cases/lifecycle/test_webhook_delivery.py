# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Opt-in delivery/configuration checks against one inventoried full Master."""
import copy
import json
import os
import subprocess
import time
from types import SimpleNamespace

import pytest
import requests
import yaml

from framework.webhook_resources import (
    setting, run_root, owned_path, checked_unit, process_binary, endpoint
)
from framework.lifecycle import wait_until
from framework.webhook import assert_payload, new_events, records

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]


@pytest.fixture
def webhook_master(request):
    required = ("MASTER_UNIT", "MASTER_CONFIG", "MASTER_URL", "MASTER_BINARY",
                "RECEIVER_LOG", "RECEIVER_MODE", "SUBSCRIPTION_ID")
    if os.environ.get("WEBHOOK_TEST_CONTROL") != "1" or not all(
            os.environ.get("WEBHOOK_TEST_" + key) for key in required):
        pytest.skip("dedicated full Master control experiment not requested")
    env = {key.lower(): setting(key) for key in required}
    unit = checked_unit(env["master_unit"])
    path = owned_path(env["master_config"])
    original = path.read_text()
    base = yaml.safe_load(original)
    assert base["instance_db_config"]["db_name"].startswith("wh_test_")
    actual = subprocess.check_output(["systemctl", "show", unit, "-p", "Environment", "--value"], text=True)
    assert f"CUBE_MASTER_CONFIG_PATH={path}" in actual
    output = run_root() / "evidence" / ("control-" + request.node.name + "-" + str(time.time_ns()))
    output.mkdir(mode=0o700, parents=True)
    mode = owned_path(env["receiver_mode"])
    owned_path(env["receiver_log"])
    original_mode = mode.read_text()

    def save(name, data):
        (output / (name + ".json")).write_text(json.dumps(data, indent=2))

    def pid():
        return int(subprocess.check_output(["systemctl", "show", unit, "-p", "MainPID", "--value"], text=True))

    process_binary(pid(), "MASTER_BINARY")
    session = requests.Session()
    session.trust_env = False

    def command(action):
        checked_unit(unit)
        current = pid()
        if current > 1:
            process_binary(current, "MASTER_BINARY")
        started = time.monotonic()
        result = subprocess.run(["sudo", "systemctl", action, unit], capture_output=True, text=True)
        row = {"action": action, "elapsed": time.monotonic() - started, "returncode": result.returncode,
               "stdout": result.stdout, "stderr": result.stderr, "at": time.time()}
        with (output / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps(row) + "\n")
        result.check_returncode()
        return row

    def healthy():
        try:
            return session.get(env["master_url"] + "/notify/health", timeout=1).status_code == 200
        except requests.RequestException:
            return False

    def write(config):
        temporary = path.with_suffix(".webhook-test-tmp")
        with temporary.open("x") as stream:
            temporary.chmod(0o600)
            stream.write(yaml.safe_dump(config, sort_keys=False))
        temporary.replace(path)

    def restart(config=None):
        before = pid()
        if config is not None:
            write(config)
        row = command("restart")
        wait_until(healthy, timeout=20, description="complete Master health after restart")
        row.update(previous_pid=before, current_pid=pid())
        assert row["current_pid"] != before and row["current_pid"] > 1
        process_binary(pid(), "MASTER_BINARY")
        save("restart-" + str(time.time_ns()), row)

    def metric(operation, result):
        response = session.get(env["master_url"] + "/metrics", timeout=3)
        response.raise_for_status()
        text = response.text
        (output / ("metrics-" + str(time.time_ns()) + ".txt")).write_text(text)
        return sum(float(line.rsplit(" ", 1)[1]) for line in text.splitlines()
                   if line.startswith("cubemaster_webhook_operations_total{")
                   and f'operation="{operation}"' in line and f'result="{result}"' in line)

    def observed(sandbox):
        return records(env["receiver_log"], sandbox.sandbox_id, env["subscription_id"])

    restored = False

    def restore():
        nonlocal restored
        if restored:
            return
        mode.write_text("204")
        path.write_text(original)
        restart()
        mode.write_text(original_mode)
        save("restored", {"healthy": healthy(), "pid": pid(), "config_restored": path.read_text() == original,
                          "receiver_mode": mode.read_text()})
        restored = True

    master = SimpleNamespace(base=base, write=write, restart=restart, stop=lambda: command("stop"),
                             start=lambda: command("start"), healthy=healthy, pid=pid, metric=metric,
                             observed=observed, save=save, mode=mode, restore=restore, env=env, output=output)
    mode.write_text("204")
    try:
        yield master
    finally:
        try:
            restore()
        finally:
            session.close()


def sandbox_with_receiver(request, master):
    sandbox = request.getfixturevalue("sdk_sandbox")
    request.addfinalizer(master.restore)
    wait_until(lambda: bool(new_events(master.observed(sandbox), "created")), timeout=15,
               description="real created HTTP delivery before fault")
    return sandbox


@pytest.mark.parametrize("invalid", ["workers", "url", "default-off", "subscription-default-off", "legacy-store"])
def test_module_configuration_isolates_business(request, sdk_backend, webhook_master, invalid):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    changed = copy.deepcopy(master.base)
    block = changed["webhooks"]
    if invalid == "workers":
        block["workers"] = "nope"
    elif invalid == "url":
        block["subscriptions"][0]["url"] = "not-a-url"
    elif invalid == "default-off":
        block.pop("enabled")
    elif invalid == "legacy-store":
        block["store"] = {"backend": "redis"}
    else:
        block["subscriptions"][0].pop("enabled")
    master.restart(changed)
    before = {event["id"] for _, event in master.observed(sandbox)}
    sandbox.set_timeout(300)
    sandbox.update_network({"allow_out": [], "deny_out": []})
    time.sleep(2)
    assert not new_events(master.observed(sandbox), "updated", before)
    assert master.metric("capture", "accepted") == 0
    master.save("isolated-business", {"configuration": invalid, "sandbox_id": sandbox.sandbox_id,
                                     "http": [row for row, _ in master.observed(sandbox)]})
    master.restart(master.base)
    sandbox.set_timeout(301)
    wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before, "set_timeout")),
               timeout=15, description="enabled config delivers a new operation")


def test_running_file_edit_keeps_startup_subscription(request, sdk_backend, webhook_master):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    changed = copy.deepcopy(master.base)
    changed["webhooks"]["enabled"] = False
    changed["webhooks"]["subscriptions"][0]["url"] += "/not-started"
    process = master.pid()
    master.write(changed)
    time.sleep(2)
    sandbox.set_timeout(300)
    wait_until(lambda: bool(new_events(master.observed(sandbox), "updated")), timeout=15,
               description="startup subscription still active after watcher reload")
    assert master.pid() == process
    master.save("startup-snapshot", [row for row, event in master.observed(sandbox)
                                     if event["type"] == "sandbox.lifecycle.updated"])


def test_real_event_fanout_and_filter(request, sdk_backend, webhook_master):
    if sdk_backend != "cubesandbox" or not os.environ.get("WEBHOOK_TEST_CONCURRENT_RECEIVER_LOG"):
        pytest.skip("two dedicated receivers required")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    config = copy.deepcopy(master.base)
    second = {"id": "secondary-receiver", "enabled": True, "url": endpoint("CONCURRENT_RECEIVER_URL"),
              "events": ["sandbox.lifecycle.updated"]}
    config["webhooks"]["subscriptions"].append(second)
    log = owned_path(setting("CONCURRENT_RECEIVER_LOG"))
    master.restart(config)
    sandbox.set_timeout(300)
    wait_until(lambda: bool(new_events(master.observed(sandbox), "updated")) and
               bool(new_events(records(log, sandbox.sandbox_id, second["id"]), "updated")),
               timeout=15, description="one business event reaches both subscriptions")
    primary = new_events(master.observed(sandbox), "updated")[0]
    secondary = new_events(records(log, sandbox.sandbox_id, second["id"]), "updated")[0]
    assert primary == secondary
    assert_payload(primary, "updated", template="")
    sandbox.kill()
    wait_until(lambda: bool(new_events(master.observed(sandbox), "killed")), timeout=15,
               description="primary subscription receives unfiltered killed event")
    time.sleep(2)
    assert not new_events(records(log, sandbox.sandbox_id, second["id"]), "killed")
    master.save("fanout-and-filter", {"event": primary,
        "primary_http": [row for row, _ in master.observed(sandbox)],
        "secondary_http": [row for row, _ in records(log, sandbox.sandbox_id, second["id"])]})


def test_sigterm_drops_inflight_backlog_on_restart(request, sdk_backend, webhook_master):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    config = copy.deepcopy(master.base)
    config["webhooks"]["request_timeout"] = "30s"
    master.restart(config)
    master.mode.write_text("slow")
    try:
        sandbox.set_timeout(300)
        wait_until(lambda: bool(new_events(master.observed(sandbox), "updated")), timeout=10,
                   description="real slow HTTP intake before SIGTERM")
        event_id = new_events(master.observed(sandbox), "updated")[0]["id"]
        master.stop()
        elapsed = json.loads((master.output / "commands.jsonl").read_text().splitlines()[-1])["elapsed"]
        assert elapsed < 15, "complete Master did not honor its normal shutdown budget"
        # The component test measures webhook's own two-second budget. This
        # observation includes the existing App/systemd shutdown policy.
        exit_status = subprocess.check_output(["systemctl", "show", master.env["master_unit"],
                                               "-p", "ExecMainStatus", "--value"], text=True).strip()
        stopped_count = len([row for row, event in master.observed(sandbox) if event["id"] == event_id])
        master.mode.write_text("204")
        master.start()
        wait_until(master.healthy, timeout=20, description="complete Master restarted")
        process_binary(master.pid(), "MASTER_BINARY")
        sandbox.set_timeout(301)
        wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", {event_id})), timeout=15,
                   description="fresh business event delivered after restart")
        time.sleep(2)
        assert len([row for row, event in master.observed(sandbox) if event["id"] == event_id]) == stopped_count
        master.save("restart-loss", {"event_id": event_id, "elapsed_seconds": elapsed,
            "app_exit_status": exit_status, "old_attempts": stopped_count,
            "http": [row for row, _ in master.observed(sandbox)]})
    finally:
        master.mode.write_text("204")


@pytest.mark.parametrize("mode", ["400", "401", "503", "drop", "slow"])
def test_real_receiver_failure_exhausts_budget_without_replay(request, sdk_backend, webhook_master, mode):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_TEST_HTTP_FAILURES") != "1":
        pytest.skip("dedicated receiver exhaustion experiment not requested")
    master = webhook_master
    sandbox = sandbox_with_receiver(request, master)
    config = copy.deepcopy(master.base)
    config["webhooks"].update(max_attempts=3, request_timeout="500ms")
    master.restart(config)
    before = {event["id"] for _, event in master.observed(sandbox)}
    failed = master.metric("terminal", "failed")
    master.mode.write_text(mode)
    try:
        sandbox.set_timeout(300)
        wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before)), timeout=15,
                   description="actual rejected/drop/timeout HTTP intake")
        event_id = new_events(master.observed(sandbox), "updated", before)[0]["id"]
        wait_until(lambda: master.metric("terminal", "failed") > failed, timeout=40,
                   description="finite HTTP budget exhausted")
        wait_until(lambda: len([row for row, event in master.observed(sandbox) if event["id"] == event_id]) == 3,
                   timeout=40, description="receiver records all attempts, including queued slow handlers")
        rows = [row for row, event in master.observed(sandbox) if event["id"] == event_id]
        assert len(rows) == 3 and all(row["mode"] == mode for row in rows)
        master.save("exhaustion", {"mode": mode, "event_id": event_id, "http": rows})
    finally:
        master.mode.write_text("204")
    sandbox.update_network({"allow_out": [], "deny_out": []})
    wait_until(lambda: bool(new_events(master.observed(sandbox), "updated", before, "network_egress")),
               timeout=30, description="new event delivered after receiver recovery")
    assert len([row for row, event in master.observed(sandbox) if event["id"] == event_id]) == 3
