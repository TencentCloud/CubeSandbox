# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""P6 deployment-only configuration/recovery experiments; never a default run."""
import copy
import json
import os
from pathlib import Path
import subprocess
import time
from types import SimpleNamespace
from urllib.parse import urlsplit

import pytest
import requests
import yaml

from framework.webhook_resources import (
    setting, run_root, owned_path, container_info, checked_unit, process_binary, resource_label, endpoint
)
from framework.lifecycle import wait_until
from framework.webhook import delivery_records, new_events, records

pytestmark = [pytest.mark.e2e, pytest.mark.sdk_compat, pytest.mark.lifecycle]
TERMINAL = {"succeeded", "failed", "cancelled"}


@pytest.fixture
def p6_master(request):
    required = ("WEBHOOK_P6_MASTER_UNIT", "WEBHOOK_P6_MASTER_CONFIG", "WEBHOOK_P6_MASTER_URL",
                "WEBHOOK_P6_REDIS_CONTAINER", "WEBHOOK_P6_REDIS_PASSWORD", "WEBHOOK_P6_RECEIVER_LOG",
                "WEBHOOK_P6_RECEIVER_MODE", "WEBHOOK_P6_SUBSCRIPTION_ID")
    if os.environ.get("WEBHOOK_P6_CONTROL") != "1" or not all(os.environ.get(k) for k in required):
        pytest.skip("dedicated full Master control experiment not requested")
    env = {k.removeprefix("WEBHOOK_P6_").lower(): os.environ[k] for k in required}
    unit, path = env["master_unit"], Path(env["master_config"])
    checked_unit(unit)
    owned_path(path)
    original = path.read_text()
    base = yaml.safe_load(original)
    assert base["instance_db_config"]["db_name"].startswith("whp6_")
    actual = subprocess.check_output(["systemctl", "show", unit, "-p", "Environment", "--value"], text=True)
    assert f"CUBE_MASTER_CONFIG_PATH={path}" in actual
    container_info(env["redis_container"])
    label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}',
                                     env["redis_container"]], text=True).strip()
    assert label == resource_label()
    output = run_root() / "evidence" / ("control-" + request.node.name + "-" + str(time.time_ns()))
    output.mkdir(mode=0o700, parents=True)
    mode = owned_path(env["receiver_mode"])
    owned_path(env["receiver_log"])
    mode.write_text("204")

    def save(name, data):
        (output / (name + ".json")).write_text(json.dumps(data, indent=2))

    def command(action):
        started = time.monotonic()
        result = subprocess.run(["sudo", "systemctl", action, unit], capture_output=True, text=True)
        row = {"action": action, "elapsed": time.monotonic() - started, "returncode": result.returncode,
               "stdout": result.stdout, "stderr": result.stderr, "at": time.time()}
        with (output / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps(row) + "\n")
        assert result.returncode == 0, row
        return row

    current_pid = int(subprocess.check_output(["systemctl", "show", unit, "-p", "MainPID", "--value"], text=True))
    process_binary(current_pid, "MASTER_BINARY")
    command("reset-failed")

    def pid():
        return int(subprocess.check_output(["systemctl", "show", unit, "-p", "MainPID", "--value"], text=True))

    session = requests.Session()
    session.trust_env = False

    def healthy():
        try:
            return session.get(env["master_url"] + "/notify/health", timeout=1).status_code == 200
        except requests.RequestException:
            return False

    def write(config):
        temporary = path.with_suffix(".p6-tmp")
        temporary.write_text(yaml.safe_dump(config, sort_keys=False))
        temporary.chmod(0o600)
        temporary.replace(path)

    def restart(config=None):
        before = pid()
        if config is not None:
            write(config)
        row = command("restart")
        wait_until(healthy, timeout=20, description="complete Master health after restart")
        row.update(previous_pid=before, current_pid=pid())
        assert row["current_pid"] != before and row["current_pid"] > 0
        save("restart-" + str(time.time_ns()), row)

    def durable():
        return delivery_records(env["redis_container"], env["redis_password"])

    def observed(sandbox):
        return records(env["receiver_log"], sandbox.sandbox_id, env["subscription_id"])

    restored = False

    def restore():
        nonlocal restored
        if restored:
            return
        if not p6.restore_allowed:
            mode.write_text("204")
            save("restore-deferred", {"reason": "queue/index audit required before restarting a protected Runtime"})
            return
        mode.write_text("204")
        path.write_text(original)
        restart()
        restored = True

    p6 = SimpleNamespace(base=base, write=write, restart=restart, stop=lambda: command("stop"),
                         start=lambda: command("start"), healthy=healthy, pid=pid, durable=durable,
                         observed=observed, save=save, mode=mode, restore=restore, restore_allowed=True, env=env, output=output)
    yield p6
    restore()
    session.close()


def sandbox_and_barrier(request, p6):
    sandbox = request.getfixturevalue("sdk_sandbox")
    request.addfinalizer(p6.restore)
    wait_until(lambda: all(x["record"]["state"] in TERMINAL for x in p6.durable()),
               timeout=35, description="saved tasks quiescent")
    return sandbox


def pending_update(p6, sandbox):
    before = {event["id"] for _, event in p6.observed(sandbox)}
    p6.mode.write_text("503")
    sandbox.set_timeout(300)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before)),
               timeout=15, description="real receiver accepted failing delivery")
    event_id = new_events(p6.observed(sandbox), "updated", before)[0]["id"]

    def task():
        return next((x for x in p6.durable() if x["event"]["id"] == event_id), None)

    wait_until(lambda: task() is not None and task()["record"]["state"] == "pending",
               timeout=10, description="actual persisted retry")
    p6.stop()
    saved = task()
    assert saved["record"]["state"] not in TERMINAL
    p6.save("saved-before-restart", saved)
    p6.mode.write_text("204")
    return event_id, task, saved


def test_global_off_preserves_backlog_and_reenable(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    event_id, task, saved = pending_update(p6, sandbox)
    off = copy.deepcopy(p6.base)
    off["webhooks"]["enabled"] = False
    p6.write(off)
    p6.start()
    wait_until(p6.healthy, timeout=20, description="disabled Master still serves business")
    sandbox.set_timeout(301)
    time.sleep(2)
    assert task() == saved, "disabled module consumed or changed saved work"
    assert {event["id"] for _, event in p6.observed(sandbox) if "set_timeout" in event.get("event_data", {})} == {event_id}
    p6.restart(p6.base)
    wait_until(lambda: task()["record"]["state"] == "succeeded", timeout=25, description="backlog resumed")
    final = task()
    assert final["record"]["input"] == saved["record"]["input"]
    assert final["record"]["attempts"] > saved["record"]["attempts"]
    p6.save("recovered", final)


@pytest.mark.parametrize("change", ["removed", "disabled", "url", "events"])
def test_revoked_saved_task_is_cancelled(request, sdk_backend, p6_master, change):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    event_id, task, saved = pending_update(p6, sandbox)
    changed = copy.deepcopy(p6.base)
    sub = changed["webhooks"]["subscriptions"][0]
    if change == "removed":
        changed["webhooks"]["subscriptions"] = []
    elif change == "disabled":
        sub["enabled"] = False
    elif change == "url":
        sub["url"] += "/changed"
    else:
        sub["events"] = ["sandbox.lifecycle.created"]
    count = len([row for row, event in p6.observed(sandbox) if event["id"] == event_id])
    p6.write(changed)
    p6.start()
    wait_until(p6.healthy, timeout=20, description="changed subscription Master health")
    wait_until(lambda: task()["record"]["state"] == "cancelled", timeout=25, description="old target cancelled")
    time.sleep(1)
    assert len([row for row, event in p6.observed(sandbox) if event["id"] == event_id]) == count
    assert task()["record"]["input"] == saved["record"]["input"]
    p6.save("cancelled", task())


@pytest.mark.parametrize("invalid", ["workers", "url", "default-off", "subscription-default-off"])
def test_module_configuration_isolates_business(request, sdk_backend, p6_master, invalid):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    changed = copy.deepcopy(p6.base)
    block = changed["webhooks"]
    if invalid == "workers":
        block["workers"] = "nope"
    elif invalid == "url":
        block["subscriptions"][0]["url"] = "not-a-url"
    elif invalid == "default-off":
        block.pop("enabled")
    else:
        block["subscriptions"][0].pop("enabled")
    p6.restart(changed)
    before = p6.durable()
    sandbox.set_timeout(300)
    time.sleep(2)
    assert p6.durable() == before
    assert not new_events(p6.observed(sandbox), "updated")
    p6.save("store-unchanged", before)


def test_running_file_edit_keeps_startup_subscription(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    changed = copy.deepcopy(p6.base)
    changed["webhooks"]["enabled"] = False
    changed["webhooks"]["subscriptions"][0]["url"] += "/not-started"
    process = p6.pid()
    p6.write(changed)
    time.sleep(2)
    sandbox.set_timeout(300)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated")), timeout=15,
               description="startup subscription still active after watcher reload")
    assert p6.pid() == process
    p6.save("startup-snapshot", [row for row, event in p6.observed(sandbox)
                                 if event["type"] == "sandbox.lifecycle.updated"])


@pytest.mark.parametrize("signal", ["STOP", "KILL"])
def test_two_complete_masters_take_over_real_inflight(request, sdk_backend, p6_master, signal):
    """Actual process death/suspension after HTTP intake, not a synthetic Claim."""
    if sdk_backend != "cubesandbox":
        pytest.skip("CubeSandbox deployment control experiment")
    secondary = os.environ.get("WEBHOOK_P6_SECOND_MASTER_UNIT")
    log = os.environ.get("WEBHOOK_P6_CONCURRENT_RECEIVER_LOG")
    mode_path = os.environ.get("WEBHOOK_P6_CONCURRENT_RECEIVER_MODE")
    if not all((secondary, log, mode_path)):
        pytest.skip("second dedicated full Master and bounded concurrent receiver required")
    checked_unit(secondary)
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    mode = owned_path(mode_path)
    mode.write_text("204")
    units = [p6.env["master_unit"], secondary]

    def systemctl(*args):
        result = subprocess.run(["sudo", "systemctl", *args], capture_output=True, text=True)
        p6.save("systemctl-" + str(time.time_ns()), {"args": args, "returncode": result.returncode,
                                                   "stdout": result.stdout, "stderr": result.stderr})
        assert result.returncode == 0, result.stderr

    def pid(unit):
        return int(subprocess.check_output(["systemctl", "show", unit, "-p", "MainPID", "--value"], text=True))

    def observed():
        return records(log, sandbox.sandbox_id, p6.env["subscription_id"])

    owner = None
    try:
        config = copy.deepcopy(p6.base)
        config["webhooks"]["subscriptions"][0]["url"] = endpoint("CONCURRENT_RECEIVER_URL")
        p6.restart(config)
        systemctl("start", secondary)
        session = requests.Session()
        session.trust_env = False
        def secondary_healthy():
            try:
                return session.get(endpoint("SECOND_MASTER_URL") + "/notify/health", timeout=2).status_code == 200
            except requests.RequestException:
                return False
        wait_until(secondary_healthy, timeout=20, description="second complete Master health")
        pids = {unit: pid(unit) for unit in units}
        assert len(set(pids.values())) == 2 and all(pids.values())
        mode.write_text("slow")
        sandbox.set_timeout(300)
        wait_until(lambda: bool(new_events(observed(), "updated")), timeout=10,
                   description="HTTP intake from first lease holder")
        event_id = new_events(observed(), "updated")[0]["id"]

        def task():
            return next((x for x in p6.durable() if x["event"]["id"] == event_id), None)

        before = task()
        assert before["record"]["state"] == "inflight" and before["record"]["attempts"] == 1
        sockets = subprocess.check_output(["sudo", "ss", "-tnp", "state", "established", "(", "dport", "=", str(urlsplit(endpoint("CONCURRENT_RECEIVER_URL")).port or 80), ")"], text=True)
        holders = [unit for unit, value in pids.items() if f"pid={value}," in sockets]
        assert len(holders) == 1, sockets
        owner = holders[0]
        p6.save("claim-holder", {"pids": pids, "owner": owner, "sockets": sockets, "task": before})
        systemctl("kill", "--kill-who=main", "--signal=" + signal, owner)
        mode.write_text("204")
        wait_until(lambda: task()["record"]["state"] == "succeeded", timeout=22,
                   description="other complete Master reclaims expired persisted lease")
        recovered = task()
        assert recovered["record"]["attempts"] == 2
        assert recovered["record"]["input"] == before["record"]["input"]
        rows = [row for row, event in observed() if event["id"] == event_id]
        assert len(rows) == 2
        assert rows[1]["active_requests"] >= 2, "did not observe old and new HTTP requests overlap"
        assert rows[1]["response_status"] == 204
        if signal == "STOP":
            systemctl("kill", "--kill-who=main", "--signal=CONT", owner)
            time.sleep(2)
            assert task() == recovered, "old holder changed a completed newer Receipt"
            address = p6.env["master_url"] if owner == units[0] else endpoint("SECOND_MASTER_URL")
            metrics = session.get(address + "/metrics", timeout=3).text
            (p6.output / "old-holder-metrics.txt").write_text(metrics)
            assert any('operation="retry"' in line and 'result="stale"' in line and not line.endswith(" 0")
                       for line in metrics.splitlines()), "old Receipt rejection not observed"
        else:
            systemctl("reset-failed", owner)
            systemctl("start", owner)
        p6.save("recovered", {"task": recovered, "http": rows})
        # All producer/consumer processes restart; the terminal task remains stable.
        for unit in units:
            systemctl("restart", unit)
        time.sleep(2)
        assert task() == recovered
        assert len([row for row, event in observed() if event["id"] == event_id]) == 2
        session.close()
    finally:
        if owner and signal == "STOP":
            subprocess.run(["sudo", "systemctl", "kill", "--kill-who=main", "--signal=CONT", owner], capture_output=True)
        mode.write_text("204")
        systemctl("stop", secondary)


def test_deployment_api_isolation_and_latency(request, sdk_backend, p6_master):
    """Measured deployment observations; deliberately has no fixed latency SLO."""
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_PERFORMANCE") != "1":
        pytest.skip("dedicated deployment performance experiment not requested")
    import math
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    session = requests.Session()
    session.trust_env = False
    rows = []
    paused = False
    container = p6.env["redis_container"]

    def resources(phase):
        process = p6.pid()
        response = session.get(p6.env["master_url"] + "/metrics", timeout=3)
        (p6.output / (phase + "-metrics.txt")).write_text(response.text)
        (p6.output / (phase + "-process.txt")).write_text(Path(f"/proc/{process}/status").read_text())
        sockets = subprocess.check_output(["sudo", "ss", "-tanp"], text=True)
        (p6.output / (phase + "-sockets.txt")).write_text("\n".join(line for line in sockets.splitlines() if f"pid={process}," in line))
        response = session.get(endpoint("MASTER_DEBUG_URL") + "/debug/pprof/goroutine?debug=2", timeout=3)
        (p6.output / (phase + "-goroutines.txt")).write_text(response.text)

    def sample(phase, count, interval=0):
        for number in range(5):
            started = time.monotonic()
            sandbox.set_timeout(600 + number)
            with (p6.output / "warmup.jsonl").open("a") as stream:
                stream.write(json.dumps({"phase": phase, "index": number, "success": True,
                    "elapsed_ms": (time.monotonic()-started)*1000, "fault_active": paused}) + "\n")
        resources(phase + "-before")
        began = time.monotonic()
        for number in range(count):
            started = time.monotonic()
            sandbox.set_timeout(300 + number)
            row = {"phase": phase, "index": number, "elapsed_ms": (time.monotonic() - started) * 1000,
                   "completed_at": time.time(), "success": True, "fault_active": paused}
            rows.append(row)
            with (p6.output / "api-latency.jsonl").open("a") as stream:
                stream.write(json.dumps(row) + "\n")
            if interval:
                time.sleep(max(0, began + (number + 1) * interval - time.monotonic()))
        resources(phase + "-after")
        values = sorted(row["elapsed_ms"] for row in rows if row["phase"] == phase)
        p6.save(phase + "-distribution", {"samples": len(values), "warmup": 5, "concurrency": 1,
            "wall_seconds": time.monotonic() - began, "p50_ms": values[math.ceil(len(values)*.50)-1],
            "p95_ms": values[math.ceil(len(values)*.95)-1], "p99_ms": values[math.ceil(len(values)*.99)-1],
            "max_ms": max(values)})

    try:
        off = copy.deepcopy(p6.base)
        off["webhooks"]["enabled"] = False
        p6.restart(off)
        sample("disabled", 60)
        p6.restart(p6.base)
        sample("healthy", 60)
        wait_until(lambda: all(x["record"]["state"] in TERMINAL for x in p6.durable()), timeout=35,
                   description="healthy load drained")
        # HTTP requests remain unresolved while successful business operations return.
        slow = copy.deepcopy(p6.base)
        slow["webhooks"]["subscriptions"][0]["url"] = endpoint("CONCURRENT_RECEIVER_URL")
        p6.restart(slow)
        concurrent_mode = owned_path(os.environ["WEBHOOK_P6_CONCURRENT_RECEIVER_MODE"])
        concurrent_mode.write_text("slow")
        try:
            sample("slow-http", 60)
        finally:
            concurrent_mode.write_text("204")
        p6.restart(p6.base)
        wait_until(lambda: all(x["record"]["state"] in TERMINAL for x in p6.durable()), timeout=35,
                   description="slow HTTP backlog settled")
        result = subprocess.run(["docker", "pause", container], capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        paused = True
        p6.save("redis-pause", {"at": time.time(), "container": container, "result": result.stdout})
        # Longer than Writer's 30s retry window. Business Redis is a separate live service.
        sample("webhook-redis-unresponsive", 40, interval=1)
        result = subprocess.run(["docker", "unpause", container], capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        paused = False
        p6.save("redis-unpause", {"at": time.time(), "result": result.stdout})
        wait_until(lambda: all(x["record"]["state"] in TERMINAL for x in p6.durable()), timeout=40,
                   description="saved work drains after notification Redis recovers")
        bounded = copy.deepcopy(p6.base)
        bounded["webhooks"]["ingress_capacity"] = 4
        bounded["webhooks"]["store"]["max_records"] = len(p6.durable()) + 8
        p6.restart(bounded)
        before = p6.durable()
        sample("capacity", 80)
        time.sleep(32)
        final = p6.durable()
        assert len(final) <= bounded["webhooks"]["store"]["max_records"]
        assert {x["key"]: x["record"]["input"] for x in before} == {
            x["key"]: x["record"]["input"] for x in final if x["key"] in {v["key"] for v in before}}
        resources("capacity-settled")
        p6.save("capacity", {"before": len(before), "after": len(final), "max_records": bounded["webhooks"]["store"]["max_records"]})
        assert any(row["fault_active"] for row in rows)
    finally:
        if paused:
            subprocess.run(["docker", "unpause", container], capture_output=True, check=True)
        session.close()


def test_real_event_fanout_and_filter(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox" or not os.environ.get("WEBHOOK_P6_CONCURRENT_RECEIVER_LOG"):
        pytest.skip("two dedicated receivers required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    config = copy.deepcopy(p6.base)
    second = {"id": "p6-secondary", "enabled": True, "url": endpoint("CONCURRENT_RECEIVER_URL"),
              "events": ["sandbox.lifecycle.updated"]}
    config["webhooks"]["subscriptions"].append(second)
    p6.restart(config)
    sandbox.set_timeout(300)
    log = os.environ["WEBHOOK_P6_CONCURRENT_RECEIVER_LOG"]
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated")) and
               bool(new_events(records(log, sandbox.sandbox_id, second["id"]), "updated")),
               timeout=15, description="one business event reaches two independently persisted subscriptions")
    primary = new_events(p6.observed(sandbox), "updated")[0]
    secondary = new_events(records(log, sandbox.sandbox_id, second["id"]), "updated")[0]
    assert primary == secondary
    tasks = [x for x in p6.durable() if x["event"]["id"] == primary["id"]]
    assert len(tasks) == 2 and len({x["key"] for x in tasks}) == 2
    assert all(x["record"]["attempts"] == 1 for x in tasks)
    sandbox.kill()
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "killed")), timeout=15,
               description="primary subscription receives unfiltered killed event")
    time.sleep(1)
    assert not new_events(records(log, sandbox.sandbox_id, second["id"]), "killed")
    p6.save("fanout-and-filter", {"event": primary, "tasks": tasks,
        "secondary_http": [row for row, event in records(log, sandbox.sandbox_id, second["id"])]})


@pytest.mark.parametrize("revoke", [False, True])
def test_sigterm_with_real_http_inflight(request, sdk_backend, p6_master, revoke):
    if sdk_backend != "cubesandbox" or not os.environ.get("WEBHOOK_P6_CONCURRENT_RECEIVER_LOG"):
        pytest.skip("dedicated concurrent receiver required")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    mode = owned_path(os.environ["WEBHOOK_P6_CONCURRENT_RECEIVER_MODE"])
    config = copy.deepcopy(p6.base)
    config["webhooks"]["subscriptions"][0]["url"] = endpoint("CONCURRENT_RECEIVER_URL")
    p6.restart(config)
    mode.write_text("slow")
    try:
        sandbox.set_timeout(300)
        def observed():
            return records(os.environ["WEBHOOK_P6_CONCURRENT_RECEIVER_LOG"], sandbox.sandbox_id,
                           p6.env["subscription_id"])
        wait_until(lambda: bool(new_events(observed(), "updated")), timeout=10, description="real inflight HTTP")
        event_id = new_events(observed(), "updated")[0]["id"]
        def task():
            return next((x for x in p6.durable() if x["event"]["id"] == event_id), None)
        saved = task()
        assert saved["record"]["state"] == "inflight" and saved["record"]["attempts"] == 1
        started = time.monotonic()
        p6.stop()
        elapsed = time.monotonic() - started
        assert elapsed < 15, "complete Master did not honor normal shutdown budget"
        exit_status = subprocess.check_output(["systemctl", "show", p6.env["master_unit"],
                                               "-p", "ExecMainStatus", "--value"], text=True).strip()
        # Existing App/debug shutdown can exit nonzero. The webhook contract is
        # its own bounded drain and durable progress, not the App's exit code.
        assert task()["record"]["state"] == "pending", "inflight webhook result was not persisted before App cancellation"
        p6.save("after-sigterm", {"elapsed_seconds": elapsed, "app_exit_status": exit_status,
                                  "before": saved, "after": task()})
        mode.write_text("204")
        if revoke:
            config["webhooks"]["subscriptions"] = []
        p6.write(config)
        p6.start()
        wait_until(p6.healthy, timeout=20, description="complete Master restarted")
        expected = "cancelled" if revoke else "succeeded"
        wait_until(lambda: task()["record"]["state"] == expected, timeout=25, description=expected)
        assert task()["record"]["input"] == saved["record"]["input"]
        rows = [row for row, event in observed() if event["id"] == event_id]
        assert len(rows) == (1 if revoke else 2)
        p6.save("after-restart", {"task": task(), "http": rows})
    finally:
        mode.write_text("204")


@pytest.mark.parametrize("restart", ["restart", "kill"])
def test_real_task_survives_notification_redis_restart(request, sdk_backend, p6_master, restart):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_REDIS_RESTART") != "1":
        pytest.skip("dedicated full Master Redis restart experiment not requested")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    before = {event["id"] for _, event in p6.observed(sandbox)}
    p6.mode.write_text("503")
    sandbox.set_timeout(300)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before)), timeout=15, description="real failed first HTTP")
    event_id = new_events(p6.observed(sandbox), "updated", before)[0]["id"]
    def task():
        return next((x for x in p6.durable() if x["event"]["id"] == event_id), None)
    wait_until(lambda: task() is not None and task()["record"]["state"] == "pending", timeout=5, description="persisted retry")
    process = p6.pid()
    subprocess.run(["sudo", "kill", "-STOP", str(process)], check=True)
    try:
        saved = task()
        p6.mode.write_text("204")
        container = p6.env["redis_container"]
        commands = [["docker", "restart", container]] if restart == "restart" else [
            ["docker", "kill", "--signal=KILL", container], ["docker", "start", container]]
        for number, command in enumerate(commands):
            result = subprocess.run(command, capture_output=True, text=True)
            p6.save("redis-command-" + str(number), {"command": command, "returncode": result.returncode,
                                                    "stdout": result.stdout, "stderr": result.stderr, "at": time.time()})
            assert result.returncode == 0
        def restored():
            try:
                return task() == saved
            except (subprocess.CalledProcessError, json.JSONDecodeError):
                return False
        wait_until(restored, timeout=10, description="same AOF volume restores exact task record")
        p6.save("restored-before-client-resume", task())
    finally:
        subprocess.run(["sudo", "kill", "-CONT", str(process)], check=True)
        p6.mode.write_text("204")
    wait_until(lambda: task()["record"]["state"] == "succeeded", timeout=25, description="full Master reconnected and delivered")
    assert task()["record"]["input"] == saved["record"]["input"]
    assert task()["record"]["attempts"] == saved["record"]["attempts"] + 1
    rows = [row for row, event in p6.observed(sandbox) if event["id"] == event_id]
    assert len(rows) == task()["record"]["attempts"]
    p6.save("recovered", {"task": task(), "http": rows})


def test_complete_master_follows_real_sentinel_election(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_SENTINEL_PLATFORM") != "1":
        pytest.skip("dedicated real Sentinel deployment experiment not requested")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    primary = setting("SENTINEL_CURRENT_PRIMARY")
    replica = setting("SENTINEL_CURRENT_REPLICA")
    sentinel = setting("SENTINEL_CONTAINER")
    password = setting("SENTINEL_REDIS_PASSWORD")
    sentinel_password = setting("SENTINEL_PASSWORD")
    endpoints = {}
    for container in (primary, replica, sentinel):
        container_info(container)
        label = subprocess.check_output(["docker", "inspect", "--format", '{{index .Config.Labels "purpose"}}', container], text=True)
        assert label.strip() == setting("RESOURCE_LABEL")
        assert label.strip() == resource_label()
        info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
        networks = list(info["NetworkSettings"]["Networks"].values())
        assert len(networks) == 1 and networks[0]["IPAddress"]
        endpoints[container] = [networks[0]["IPAddress"], "6379"]
    def redis(container, *args, sentinel_auth=False):
        env = {**os.environ, "REDISCLI_AUTH": sentinel_password if sentinel_auth else password}
        command = ["docker", "exec", "-e", "REDISCLI_AUTH", container, "redis-cli", "--json"]
        if sentinel_auth:
            command += ["-p", "26379"]
        return json.loads(subprocess.check_output(command + list(args), env=env, text=True))
    assert redis(primary, "ROLE")[0] == "master"
    wait_until(lambda: redis(replica, "ROLE")[0] == "slave" and redis(replica, "ROLE")[3] == "connected",
               timeout=15, description="real replica connected before primary-loss experiment")
    previous = redis(sentinel, "SENTINEL", "get-master-addr-by-name", setting("SENTINEL_MASTER_NAME"), sentinel_auth=True)
    assert previous == endpoints[primary]
    config = copy.deepcopy(p6.base)
    config["webhooks"]["store"]["redis"] = {"master_name": setting("SENTINEL_MASTER_NAME"), "sentinel_nodes":
        setting("SENTINEL_NODES"),
        "sentinel_password": sentinel_password, "password": password, "db_no": int(os.environ.get("WEBHOOK_P6_REDIS_DB", "6"))}
    p6.restart(config)
    p6.mode.write_text("503")
    sandbox.set_timeout(300)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated")), timeout=15, description="real lifecycle task on Sentinel primary")
    event_id = new_events(p6.observed(sandbox), "updated")[0]["id"]
    def task(container):
        return next((x for x in delivery_records(container, password) if x["event"]["id"] == event_id), None)
    wait_until(lambda: task(primary) is not None and task(primary)["record"]["state"] == "pending", timeout=5,
               description="confirmed first retry on actual elected primary")
    process = p6.pid()
    subprocess.run(["sudo", "kill", "-STOP", str(process)], check=True)
    try:
        saved = task(primary)
        wait_until(lambda: task(replica) == saved, timeout=10, description="particular task and budget replicated before kill")
        p6.save("before-election", {"task": saved, "sentinel_master": previous, "replica": task(replica)})
        p6.mode.write_text("204")
        result = subprocess.run(["docker", "kill", "--signal=KILL", primary], capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        p6.save("primary-killed", {"at": time.time(), "container": primary, "stdout": result.stdout})
        wait_until(lambda: redis(sentinel, "SENTINEL", "get-master-addr-by-name", setting("SENTINEL_MASTER_NAME"), sentinel_auth=True) ==
                   endpoints[replica] and redis(replica, "ROLE")[0] == "master", timeout=20,
                   description="three real Sentinels elect the former replica")
        assert task(replica) == saved
        p6.save("elected-before-client-resume", {"task": task(replica), "master": redis(sentinel,
            "SENTINEL", "get-master-addr-by-name", setting("SENTINEL_MASTER_NAME"), sentinel_auth=True), "role": redis(replica, "ROLE")})
    finally:
        subprocess.run(["sudo", "kill", "-CONT", str(process)], check=True)
        p6.mode.write_text("204")
    wait_until(lambda: task(replica)["record"]["state"] == "succeeded", timeout=25,
               description="same full Master resolves new elected endpoint and delivers saved task")
    final = task(replica)
    assert final["record"]["input"] == saved["record"]["input"]
    assert final["record"]["attempts"] == saved["record"]["attempts"] + 1
    rows = [row for row, event in p6.observed(sandbox) if event["id"] == event_id]
    assert len(rows) == final["record"]["attempts"]
    p6.save("recovered", {"task": final, "http": rows})


@pytest.mark.parametrize("mode", ["400", "401", "503", "drop"])
def test_real_receiver_failure_exhausts_budget_without_replay(request, sdk_backend, p6_master, mode):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_HTTP_FAILURES") != "1":
        pytest.skip("dedicated receiver exhaustion experiment not requested")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    before = {event["id"] for _, event in p6.observed(sandbox)}
    p6.mode.write_text(mode)
    try:
        sandbox.set_timeout(300)
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before)), timeout=15, description="actual rejected/drop HTTP intake")
        event_id = new_events(p6.observed(sandbox), "updated", before)[0]["id"]
        def task():
            return next((row for row in p6.durable() if row["event"]["id"] == event_id), None)
        wait_until(lambda: task()["record"]["state"] == "failed", timeout=25, description="finite persisted attempt budget exhausted")
        assert task()["record"]["attempts"] == task()["record"]["max"] == 3
        rows = [row for row, event in p6.observed(sandbox) if event["id"] == event_id]
        assert len(rows) == 3 and all(row["mode"] == mode for row in rows)
        p6.mode.write_text("204")
        sandbox.update_network({"allow_out": [], "deny_out": []})
        wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", before, "network_egress")),
                   timeout=15, description="new business event delivered after receiver recovery")
        assert task()["record"]["state"] == "failed"
        assert len([row for row, event in p6.observed(sandbox) if event["id"] == event_id]) == 3
        p6.save("exhaustion", {"mode": mode, "task": task(), "http": rows})
    finally:
        p6.mode.write_text("204")


def test_dedicated_redis_maxmemory_isolates_real_business(request, sdk_backend, p6_master):
    if sdk_backend != "cubesandbox" or os.environ.get("WEBHOOK_P6_OOM") != "1":
        pytest.skip("dedicated noeviction Redis maxmemory experiment not requested")
    p6 = p6_master
    sandbox = sandbox_and_barrier(request, p6)
    container = p6.env["redis_container"]
    env = {**os.environ, "REDISCLI_AUTH": p6.env["redis_password"]}
    prefix = "p6:oom:" + str(time.time_ns()) + ":"
    keys = []
    def redis(*args, payload=None):
        command = ["docker", "exec", "-i", "-e", "REDISCLI_AUTH", container, "redis-cli", "-n", "15"]
        if payload is not None:
            command += ["-x"]
        result = subprocess.run(command + list(args), input=payload, capture_output=True, env=env)
        row = {"command": args, "input_bytes": len(payload) if payload else 0,
               "returncode": result.returncode, "stdout": result.stdout.decode(), "stderr": result.stderr.decode(), "at": time.time()}
        with (p6.output / "redis-commands.jsonl").open("a") as stream:
            stream.write(json.dumps(row) + "\n")
        assert result.returncode == 0, row
        return result.stdout.decode().strip()
    original = redis("CONFIG", "GET", "maxmemory").splitlines()[-1]
    assert redis("CONFIG", "GET", "maxmemory-policy").splitlines()[-1] == "noeviction"
    before = p6.durable()
    ids = {event["id"] for _, event in p6.observed(sandbox)}
    p6.restore_allowed = False
    try:
        assert redis("CONFIG", "SET", "maxmemory", "16777216") == "OK"
        rejected = False
        for number in range(64):
            key = prefix + str(number)
            keys.append(key)
            result = redis("SET", key, payload=b"p6-dedicated-oom-".ljust(1024*1024, b"x"))
            if "OOM" in result:
                rejected = True
                break
            assert result == "OK", result
        assert rejected, "did not establish actual Redis maxmemory rejection"
        # A large SET can be refused while small webhook writes still fit.
        # Lower only this dedicated noeviction instance below its current data
        # usage; prove a one-byte SET is rejected as well.
        assert redis("CONFIG", "SET", "maxmemory", "1048576") == "OK"
        probe = prefix + "small-probe"
        keys.append(probe)
        assert "OOM" in redis("SET", probe, "x")
        p6.save("memory-during", {"info": redis("INFO", "memory")})
        sandbox.set_timeout(300)
        time.sleep(3)
        assert not new_events(p6.observed(sandbox), "updated", ids), "notification Store was not actually constrained"
        p6.save("old-tasks-during", p6.durable())
    finally:
        assert redis("CONFIG", "SET", "maxmemory", original) == "OK"
        if keys:
            redis("DEL", *keys)
    session = requests.Session()
    session.trust_env = False
    metrics = session.get(p6.env["master_url"] + "/metrics", timeout=3).text
    (p6.output / "protected-runtime-metrics.txt").write_text(metrics)
    assert any('result="corrupt"' in line and not line.endswith(" 0")
               for line in metrics.splitlines()), "OOM protection was not observed"
    process = p6.pid()
    sandbox.set_timeout(301)
    time.sleep(1)
    assert p6.pid() == process and not new_events(p6.observed(sandbox), "updated", ids)
    # Read-only diagnosis before an explicit restart: no queue rebuild or deletion.
    final = {row["key"]: row for row in p6.durable()}
    assert final == {row["key"]: row for row in before}, "preserve unexpected partial write evidence"
    indexes = {}
    for kind in ("due", "terminal"):
        raw = subprocess.check_output(["docker", "exec", "-e", "REDISCLI_AUTH", container,
            "redis-cli", "--json", "-n", os.environ.get("WEBHOOK_P6_REDIS_DB", "6"), "ZRANGE", "cube:v1:master:webhook:" + kind + ":{delivery}",
            "0", "-1", "WITHSCORES"], env=env, text=True)
        data = json.loads(raw)
        indexes[kind] = dict(data) if data and isinstance(data[0], list) else dict(zip(data[::2], data[1::2]))
    expected_active = {key for key, row in final.items() if row["record"]["state"] not in TERMINAL}
    expected_terminal = set(final) - expected_active
    assert set(indexes["due"]) == expected_active
    assert set(indexes["terminal"]) == expected_terminal
    for key, score in indexes["terminal"].items():
        assert int(float(score)) >= final[key]["record"]["finished"]
    p6.save("read-only-index-audit", {"records": list(final.values()), "indexes": indexes})
    p6.restore_allowed = True
    p6.restart(p6.base)
    sandbox.set_timeout(302)
    wait_until(lambda: bool(new_events(p6.observed(sandbox), "updated", ids)), timeout=15,
               description="new business operation delivered after audited restart")
    assert len(new_events(p6.observed(sandbox), "updated", ids)) == 1
    session.close()
    p6.save("memory-recovered", {"info": redis("INFO", "memory"), "maxmemory": redis("CONFIG", "GET", "maxmemory")})
