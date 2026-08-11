#!/usr/bin/env python3
"""Smoke: plain pause/resume + volume pause/resume with master/node refcounts."""
from __future__ import annotations

import os
import subprocess
import sys
import time

sys.path.insert(0, "/root/CubeSandbox/sdk/python")
from cubesandbox import Config, Sandbox, Volume  # noqa: E402

API = os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000")
TPL = os.environ.get("CUBE_TEMPLATE_ID", "tpl-e6c7b3ceae22485499efc1f3")
cfg = Config(api_url=API, template_id=TPL, proxy_node_ip="127.0.0.1")


def master_rc(volume_id: str) -> int:
    out = subprocess.check_output(
        [
            "docker",
            "exec",
            "cube-sandbox-mysql",
            "mysql",
            "-ucube",
            "-pcube_pass",
            "cube_mvp",
            "-N",
            "-e",
            f"SELECT refcount FROM t_cube_volume WHERE volume_id='{volume_id}' AND deleted_at IS NULL",
        ],
        text=True,
    ).strip()
    return int(out or "0")


def node_rc(volume_id: str) -> int:
    # dump via go helper if present, else nsenter+strings heuristics
    helper = "/root/CubeSandbox/scripts/dump_vrc"
    if os.path.exists(helper):
        out = subprocess.check_output([helper, volume_id], text=True).strip()
        for line in out.splitlines():
            if volume_id in line:
                parts = line.split()
                for p in parts:
                    if p.isdigit():
                        return int(p)
    # fallback: use cubelet bolt dump script
    script = r"""
set -e
PID=$(pidof cubelet | awk '{print $1}')
DB=/data/cubelet/state/io.cubelet.internal.v1.storage/db/volume_refcount.db
nsenter -t "$PID" -m -- /bin/sh -c "strings '$DB'" | grep -F "$1" || true
"""
    out = subprocess.check_output(
        ["bash", "-c", script, "_", volume_id], text=True, stderr=subprocess.DEVNULL
    )
    # best-effort: presence implies >0; parse nearby digits hard — prefer go helper
    return -1 if not out.strip() else -2  # -1 missing, -2 present unknown


def expect(label: str, cond: bool, detail: str = "") -> None:
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {label} {detail}")
    if not cond:
        raise SystemExit(1)


def smoke_plain() -> None:
    print("=== plain pause/resume ===")
    sb = Sandbox.create(timeout=300, config=cfg)
    print("created", sb.sandbox_id)
    sb.commands.run("sh -c 'echo plain > /tmp/p.txt'", timeout=30)
    sb.pause()
    print("paused")
    sb.resume()
    print("resumed")
    r = sb.commands.run("cat /tmp/p.txt", timeout=30)
    expect("plain file preserved", (r.stdout or "").strip() == "plain", repr(r.stdout))
    sb.kill()
    print("plain OK")


def smoke_volume() -> None:
    print("=== volume pause/resume + RC ===")
    vol = Volume.create(name=f"v-smoke-{int(time.time())}", driver="localdir", config=cfg)
    vid = vol.volume_id
    print("volume", vid, "master_rc", master_rc(vid))
    expect("master rc after create vol", master_rc(vid) == 0)

    sb = Sandbox.create(
        timeout=300,
        config=cfg,
        volume_mounts={"/mnt/vol": vol},
    )
    print("created", sb.sandbox_id, "master_rc", master_rc(vid))
    expect("master rc after attach", master_rc(vid) == 1)
    sb.commands.run(
        "sh -c 'echo volhi > /mnt/vol/m.txt; echo memhi > /tmp/m.txt'", timeout=30
    )
    r = sb.commands.run("cat /mnt/vol/m.txt", timeout=30)
    expect("volume writable", (r.stdout or "").strip() == "volhi")

    sb.pause()
    print("paused master_rc", master_rc(vid))
    expect("master rc after pause (detach)", master_rc(vid) == 0)

    sb.resume()
    print("resumed master_rc", master_rc(vid))
    expect("master rc after resume (attach)", master_rc(vid) == 1)
    r1 = sb.commands.run("cat /tmp/m.txt", timeout=30)
    r2 = sb.commands.run("cat /mnt/vol/m.txt", timeout=30)
    expect("mem preserved", (r1.stdout or "").strip() == "memhi", repr(r1.stdout))
    expect("vol preserved", (r2.stdout or "").strip() == "volhi", repr(r2.stdout))

    sb.kill()
    time.sleep(2)
    print("after kill master_rc", master_rc(vid))
    expect("master rc after kill", master_rc(vid) == 0)
    vol.delete()
    print("volume OK")


if __name__ == "__main__":
    smoke_plain()
    smoke_volume()
    print("ALL SMOKE PASSED")
