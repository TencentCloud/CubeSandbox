# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Shared SDK, control-node readiness and scheduler helpers for JFS benchmarks."""

from __future__ import annotations

import json
import os
import re
import subprocess
import time

from pathlib import Path
import shlex
from contextlib import contextmanager

def _nodes_from_env() -> dict[str, str]:
    nodes = {}
    for item in re.split(r"[\s,]+", os.environ.get("CUBE_NODES", "").strip()):
        if not item:
            continue
        if "=" not in item:
            raise ValueError(f"Invalid CUBE_NODES entry: {item!r}")
        name, ip = item.split("=", 1)
        nodes[name] = ip
    return nodes


NODE_IPS = _nodes_from_env()
CONTROL = os.environ.get("CUBE_CONTROL_NODE", "c1")


class _SDK:
    def __getattr__(self, name):
        from cubesandbox import Sandbox as cls

        return getattr(cls, name)


Sandbox = _SDK()


def preflight(*nodes):
    if not NODE_IPS or not os.environ.get("CUBE_API_URL"):
        raise ValueError(
            "Set CUBE_NODES and CUBE_API_URL; see tests/perf/README.md"
        )
    for node in nodes:
        if node not in NODE_IPS:
            raise ValueError(f"Node {node!r} missing from config.nodes")
    # Fail before creating anything if the SDK is missing.
    __import__("cubesandbox")


def identifier(value):
    if not re.fullmatch(r"[A-Za-z0-9_.:-]+", value):
        raise ValueError(f"Invalid resource identifier: {value!r}")
    return value


def save_result(path, result):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    tmp.replace(path)


def snap_row(snap):
    rows = mysql(
        f"select status, backend, remote_status from t_cube_snapshot where snapshot_id='{identifier(snap)}'"
    )
    row = (rows[0] if rows else []) + [""] * 3
    return dict(zip(("status", "backend", "remote_status"), row))


def wait_until(fn, done, limit=300):
    start = time.monotonic()
    value = None
    while time.monotonic() - start < limit:
        value = fn()
        if done(value):
            return value, time.monotonic() - start
        time.sleep(0.3)
    raise TimeoutError(f"remote readiness timed out: {value}")


def wait_ready(get):
    row, elapsed = wait_until(
        get, lambda r: r.get("remote_status") in ("ready", "failed")
    )
    if row.get("remote_status") != "ready":
        raise RuntimeError(f"remote package failed: {row}")
    return round(elapsed * 1000, 1)


@contextmanager
def target_only(target, allowed):
    if not allowed:
        raise ValueError(
            "Cross-node resume requires --allow-node-isolation on a dedicated test cluster"
        )
    before = isolated()
    if target in before:
        raise ValueError(f"Target {target} is already isolated")
    changed = []
    try:
        for node in NODE_IPS:
            if node != target and node not in before:
                # Record before issuing the command so partial failure is restored too.
                changed.append(node)
                isolate([node], True)
        time.sleep(3)
        yield
    finally:
        errors = []
        for node in changed:
            try:
                isolate([node], False)
            except Exception as exc:
                errors.append(f"{node}: {exc}")
        if errors:
            raise RuntimeError(
                "Failed to restore node scheduling: " + "; ".join(errors)
            )


def control(cmd: str, stdin: str | None = None) -> str:
    """Execute a control-plane command locally on CUBE_CONTROL_NODE."""
    r = subprocess.run(
        cmd, shell=True, capture_output=True, text=True, timeout=120, input=stdin
    )
    if r.returncode != 0:
        raise RuntimeError(f"control command {cmd!r}: {r.stderr.strip()}")
    return r.stdout.strip()


def mysql(sql: str) -> list[list[str]]:
    out = control(
        "docker exec -i "
        + shlex.quote(os.environ.get("CUBE_MYSQL_CONTAINER", "cube-sandbox-mysql"))
        + " sh -c 'mysql -u$MYSQL_USER -p$MYSQL_PASSWORD $MYSQL_DATABASE -N -B' 2>/dev/null",
        stdin=sql + ";\n",
    )
    return [l.split("\t") for l in out.splitlines() if l]


def pause_row(sid: str) -> dict:
    sid = identifier(sid)
    rows = mysql(
        "select snapshot_id,node_ip,remote_status,export_uuids from t_cube_pause_snapshot "
        f"where sandbox_id='{sid}' order by id desc limit 1"
    )
    if not rows:
        return {}
    r = rows[0] + [""] * 4
    return {
        "snapshot_id": r[0],
        "node_ip": r[1],
        "remote_status": r[2],
        "export_uuids": r[3],
    }


def isolate(hosts: list[str], on: bool) -> None:
    verb = "isolate" if on else "unisolate"
    for h in hosts:
        control(f"cubeopscli node {verb} {shlex.quote(NODE_IPS[h])}")


def isolated() -> set[str]:
    out = control("cubeopscli node list")
    ips = {
        l.split()[1]
        for l in out.splitlines()[1:]
        if len(l.split()) > 4 and l.split()[-2] == "true"
    }
    return {h for h, ip in NODE_IPS.items() if ip in ips}


def create_on(template: str, origin: str) -> Sandbox:
    return Sandbox.create(template, timeout=3600, distribution_scope=[NODE_IPS[origin]])
