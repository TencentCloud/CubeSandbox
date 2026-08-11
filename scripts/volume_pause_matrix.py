#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Volume + lifecycle path matrix with Master/Node refcount checks (same-node)."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import uuid
from dataclasses import dataclass

API = os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000")
TPL = os.environ.get("CUBE_TEMPLATE_ID", "tpl-e6c7b3ceae22485499efc1f3")
DRIVER = os.environ.get("SDK_E2E_VOLUME_DRIVER", "localdir")
MOUNT = "/workspace"
PROXY_IP = os.environ.get("CUBE_PROXY_NODE_IP", "127.0.0.1")
PROXY_PORT = int(os.environ.get("CUBE_PROXY_PORT_HTTP", "80"))
RC_DB = "/data/cubelet/state/io.cubelet.internal.v1.storage/db/volume_refcount.db"
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
DUMP_GO = os.environ.get("CUBE_DUMP_VRC_GO", os.path.join(_SCRIPT_DIR, "dump_vrc.go"))
SDK_PATH = os.environ.get("CUBE_SDK_PATH", "/root/CubeSandbox/sdk/python")

sys.path.insert(0, SDK_PATH)

from cubesandbox import Config, Sandbox, Volume  # noqa: E402


@dataclass
class Check:
    path: str
    step: str
    ok: bool
    detail: str


RESULTS: list[Check] = []
FAILED = 0


def log(msg: str) -> None:
    print(msg, flush=True)


def record(path: str, step: str, ok: bool, detail: str = "") -> None:
    global FAILED
    RESULTS.append(Check(path, step, ok, detail))
    mark = "PASS" if ok else "FAIL"
    if not ok:
        FAILED += 1
    log(f"  [{mark}] {step}: {detail}")


def cubelet_pid() -> str:
    out = subprocess.check_output(
        ["pgrep", "-f", "/Cubelet/bin/cubelet --config"], text=True
    ).strip().splitlines()
    if not out:
        raise RuntimeError("cubelet pid not found")
    return out[0]


def master_rc(volume_id: str) -> int:
    sql = f'SELECT refcount FROM t_cube_volume WHERE volume_id="{volume_id}";'
    out = subprocess.check_output(
        [
            "docker",
            "exec",
            "cube-sandbox-mysql",
            "mysql",
            "-N",
            "-ucube",
            "-pcube_pass",
            "cube_mvp",
            "-e",
            sql,
        ],
        text=True,
        stderr=subprocess.DEVNULL,
    ).strip()
    if not out:
        return -1
    return int(out.splitlines()[0])


def node_rc(volume_id: str) -> tuple[int, set[str]]:
    pid = cubelet_pid()
    tmp = f"/tmp/vrc-{uuid.uuid4().hex}.db"
    subprocess.check_call(
        ["nsenter", "-t", pid, "-m", "--", "cp", RC_DB, tmp],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        out = subprocess.check_output(
            ["go", "run", DUMP_GO, tmp],
            cwd="/root/CubeSandbox/Cubelet",
            text=True,
            stderr=subprocess.STDOUT,
        )
    finally:
        try:
            os.remove(tmp)
        except OSError:
            pass
    for line in out.splitlines():
        if not line.strip() or line.startswith("ERR"):
            continue
        if "\t" not in line:
            continue
        key, payload = line.split("\t", 1)
        if volume_id not in key:
            continue
        data = json.loads(payload)
        count = int(data.get("count", 0))
        sids = set((data.get("sandbox_ids") or {}).keys())
        return count, sids
    return 0, set()


def expect_rc(
    path: str,
    step: str,
    volume_id: str,
    want_master: int,
    want_node: int,
    want_sids: set[str] | None = None,
    retries: int = 20,
    sleep_s: float = 0.5,
) -> None:
    last = ""
    for _ in range(retries):
        m = master_rc(volume_id)
        n, sids = node_rc(volume_id)
        last = f"master={m} node={n} sids={sorted(sids)}"
        ok = m == want_master and n == want_node
        if want_sids is not None:
            ok = ok and sids == want_sids
        if ok:
            record(path, step, True, last)
            return
        time.sleep(sleep_s)
    want = f"want master={want_master} node={want_node}"
    if want_sids is not None:
        want += f" sids={sorted(want_sids)}"
    record(path, step, False, f"{want}; got {last}")


def sdk_config() -> Config:
    return Config(
        api_url=API,
        template_id=TPL,
        proxy_node_ip=PROXY_IP,
        proxy_port=PROXY_PORT,
    )


def create_volume(name: str) -> Volume:
    return Volume.create(name, driver=DRIVER, config=sdk_config())


def delete_volume(volume_id: str) -> int:
    import requests

    r = requests.delete(f"{API}/volumes/{volume_id}", timeout=30)
    return r.status_code


def create_sb(volume: Volume, *, label: str) -> Sandbox:
    return Sandbox.create(
        timeout=300,
        metadata={"matrix": label},
        config=sdk_config(),
        volume_mounts={MOUNT: volume},
    )


def kill_sb(sb: Sandbox | None) -> None:
    if sb is None:
        return
    try:
        sb.kill()
    except Exception as e:
        log(f"  warn kill {getattr(sb, 'sandbox_id', '?')}: {e}")


def wait_state(sb: Sandbox, want: str, timeout: float = 60) -> str:
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        info = sb.get_info()
        last = str(info.get("state") or info.get("State") or "")
        if last.lower() == want.lower():
            return last
        time.sleep(0.5)
    raise TimeoutError(f"state want={want} got={last}")


def guest_write(sb: Sandbox, rel: str, content: str) -> None:
    sb.files.write(f"{MOUNT}/{rel}", content)


def guest_read(sb: Sandbox, rel: str) -> str:
    return sb.files.read(f"{MOUNT}/{rel}")


def assert_guest(path: str, step: str, sb: Sandbox, rel: str, expect: str) -> None:
    try:
        got = guest_read(sb, rel).strip()
        ok = got == expect
        record(path, step, ok, f"read {MOUNT}/{rel}={got!r} expect={expect!r}")
    except Exception as e:
        record(path, step, False, f"read failed: {e}")


def http_resume(sid: str) -> tuple[int, str]:
    import requests

    r = requests.post(
        f"{API}/sandboxes/{sid}/resume",
        headers={"Content-Type": "application/json"},
        json={},
        timeout=120,
    )
    return r.status_code, r.text[:300]


def path_a_basic_lifecycle() -> None:
    name = "A.basic create→use→delete"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-a-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb = None
    try:
        expect_rc(name, "after create volume", vid, 0, 0, set())
        sb = create_sb(vol, label="A")
        expect_rc(name, "after create sandbox", vid, 1, 1, {sb.sandbox_id})
        guest_write(sb, "a.txt", "hello-a")
        assert_guest(name, "volume R/W while running", sb, "a.txt", "hello-a")
        code = delete_volume(vid)
        record(name, "delete volume while bound → 409", code == 409, f"http={code}")
        kill_sb(sb)
        sb = None
        expect_rc(name, "after delete sandbox", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume unbound → 204", code == 204, f"http={code}")
        vid = ""
    finally:
        kill_sb(sb)
        if vid:
            delete_volume(vid)


def path_b_pause_resume() -> None:
    name = "B.pause→resume same sandbox"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-b-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb = None
    try:
        sb = create_sb(vol, label="B")
        sid = sb.sandbox_id
        expect_rc(name, "running", vid, 1, 1, {sid})
        guest_write(sb, "b.txt", "before-pause")
        sb.pause()
        wait_state(sb, "paused")
        expect_rc(name, "after pause (detached)", vid, 0, 0, set())
        code, body = http_resume(sid)
        record(name, "resume HTTP", code in (200, 201, 204), f"http={code} body={body}")
        sb2 = Sandbox.connect(sid, config=sdk_config())
        wait_state(sb2, "running", timeout=120)
        expect_rc(name, "after resume (reattach)", vid, 1, 1, {sid})
        assert_guest(name, "volume data after resume", sb2, "b.txt", "before-pause")
        guest_write(sb2, "b2.txt", "after-resume")
        assert_guest(name, "volume write after resume", sb2, "b2.txt", "after-resume")
        kill_sb(sb2)
        sb = None
        expect_rc(name, "after delete", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume", code == 204, f"http={code}")
        vid = ""
    finally:
        kill_sb(sb)
        if vid:
            delete_volume(vid)


def path_c_pause_then_delete() -> None:
    name = "C.pause→delete paused (no resume)"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-c-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb = None
    try:
        sb = create_sb(vol, label="C")
        sid = sb.sandbox_id
        expect_rc(name, "running", vid, 1, 1, {sid})
        guest_write(sb, "c.txt", "will-delete-paused")
        sb.pause()
        wait_state(sb, "paused")
        expect_rc(name, "after pause", vid, 0, 0, set())
        kill_sb(sb)
        sb = None
        expect_rc(name, "after delete paused", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume", code == 204, f"http={code}")
        vid = ""
    finally:
        kill_sb(sb)
        if vid:
            delete_volume(vid)


def path_d_share_pause_one() -> None:
    name = "D.two sandboxes share vol; pause one"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-d-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb1 = sb2 = None
    try:
        sb1 = create_sb(vol, label="D1")
        expect_rc(name, "after SB1", vid, 1, 1, {sb1.sandbox_id})
        sb2 = create_sb(vol, label="D2")
        expect_rc(name, "after SB2 (same node)", vid, 1, 2, {sb1.sandbox_id, sb2.sandbox_id})
        guest_write(sb1, "shared.txt", "from-sb1")
        assert_guest(name, "SB2 sees SB1 write", sb2, "shared.txt", "from-sb1")
        sb1.pause()
        wait_state(sb1, "paused")
        expect_rc(name, "after pause SB1", vid, 1, 1, {sb2.sandbox_id})
        assert_guest(name, "SB2 still R/W while SB1 paused", sb2, "shared.txt", "from-sb1")
        guest_write(sb2, "shared.txt", "from-sb2-during-pause")
        code, body = http_resume(sb1.sandbox_id)
        record(name, "resume SB1 HTTP", code in (200, 201, 204), f"http={code} body={body}")
        sb1 = Sandbox.connect(sb1.sandbox_id, config=sdk_config())
        wait_state(sb1, "running", timeout=120)
        expect_rc(name, "after resume SB1", vid, 1, 2, {sb1.sandbox_id, sb2.sandbox_id})
        assert_guest(name, "SB1 sees SB2 write after resume", sb1, "shared.txt", "from-sb2-during-pause")
        kill_sb(sb1)
        sb1 = None
        expect_rc(name, "after delete SB1", vid, 1, 1, {sb2.sandbox_id})
        kill_sb(sb2)
        sb2 = None
        expect_rc(name, "after delete SB2", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume", code == 204, f"http={code}")
        vid = ""
    finally:
        kill_sb(sb1)
        kill_sb(sb2)
        if vid:
            delete_volume(vid)


def path_e_share_pause_delete_paused() -> None:
    name = "E.share vol; pause SB1 then delete paused"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-e-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb1 = sb2 = None
    try:
        sb1 = create_sb(vol, label="E1")
        sb2 = create_sb(vol, label="E2")
        expect_rc(name, "both running", vid, 1, 2, {sb1.sandbox_id, sb2.sandbox_id})
        sb1.pause()
        wait_state(sb1, "paused")
        expect_rc(name, "after pause SB1", vid, 1, 1, {sb2.sandbox_id})
        kill_sb(sb1)
        sb1 = None
        expect_rc(name, "after delete paused SB1 (no double-dec)", vid, 1, 1, {sb2.sandbox_id})
        assert_guest(name, "SB2 still works", sb2, ".localdir-ready", "localdir-ready")
        kill_sb(sb2)
        sb2 = None
        expect_rc(name, "after delete SB2", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume", code == 204, f"http={code}")
        vid = ""
    finally:
        kill_sb(sb1)
        kill_sb(sb2)
        if vid:
            delete_volume(vid)


def path_f_delete_volume_while_paused_then_resume() -> None:
    name = "F.delete volume while paused then resume"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-f-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb = None
    try:
        sb = create_sb(vol, label="F")
        sid = sb.sandbox_id
        guest_write(sb, "f.txt", "data")
        sb.pause()
        wait_state(sb, "paused")
        expect_rc(name, "after pause", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume while paused → 204", code == 204, f"http={code}")
        vid = ""
        import requests

        code, body = http_resume(sid)
        ok_fail = code >= 400
        record(
            name,
            "resume after volume deleted should fail",
            ok_fail,
            f"http={code} body={body}",
        )
        try:
            Sandbox.connect(sid, config=sdk_config()).kill()
        except Exception:
            requests.delete(f"{API}/sandboxes/{sid}", timeout=60)
        sb = None
    finally:
        kill_sb(sb)
        if vid:
            delete_volume(vid)


def path_g_double_pause_resume() -> None:
    name = "G.pause→resume→pause→resume"
    log(f"\n=== {name} ===")
    vol = create_volume(f"vol-g-{uuid.uuid4().hex[:8]}")
    vid = vol.volume_id
    sb = None
    try:
        sb = create_sb(vol, label="G")
        sid = sb.sandbox_id
        guest_write(sb, "g.txt", "v1")
        for i in (1, 2):
            sb.pause()
            wait_state(sb, "paused")
            expect_rc(name, f"pause#{i}", vid, 0, 0, set())
            code, body = http_resume(sid)
            record(name, f"resume#{i} HTTP", code in (200, 201, 204), f"http={code} body={body}")
            sb = Sandbox.connect(sid, config=sdk_config())
            wait_state(sb, "running", timeout=120)
            expect_rc(name, f"resume#{i}", vid, 1, 1, {sid})
            expect_data = "v1" if i == 1 else "v2"
            assert_guest(name, f"data after resume#{i}", sb, "g.txt", expect_data)
            guest_write(sb, "g.txt", f"v{i + 1}")
        assert_guest(name, "final data", sb, "g.txt", "v3")
        kill_sb(sb)
        sb = None
        expect_rc(name, "after delete", vid, 0, 0, set())
        code = delete_volume(vid)
        record(name, "delete volume", code == 204, f"http={code}")
        vid = ""
    finally:
        kill_sb(sb)
        if vid:
            delete_volume(vid)


def main() -> int:
    log(f"API={API} TPL={TPL} DRIVER={DRIVER}")
    if not os.path.exists(DUMP_GO):
        raise SystemExit(f"missing {DUMP_GO}")
    paths = [
        path_a_basic_lifecycle,
        path_b_pause_resume,
        path_c_pause_then_delete,
        path_d_share_pause_one,
        path_e_share_pause_delete_paused,
        path_f_delete_volume_while_paused_then_resume,
        path_g_double_pause_resume,
    ]
    for fn in paths:
        try:
            fn()
        except Exception as e:
            record(fn.__name__, "PATH EXCEPTION", False, repr(e))
            log(f"  !! exception: {e}")
    log("\n======== SUMMARY ========")
    for c in RESULTS:
        mark = "PASS" if c.ok else "FAIL"
        log(f"{mark:4} | {c.path} | {c.step} | {c.detail}")
    log(f"\nTotal={len(RESULTS)} Failed={FAILED}")
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())
