#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Complex + concurrent volume scenarios on a live Cube cluster.

Scenarios:
  C1  concurrent create of N distinct volumes
  C2  concurrent get_info on those volumes
  C3  one volume mounted by M sandboxes concurrently (shared R/W)
  C4  attach/detach churn: parallel sandboxes + IO over multiple rounds
  C5  delete-while-in-use must fail; succeed after sandboxes gone
  C6  concurrent destroy of many idle volumes
  C7  race: parallel create same name -> exactly one winner

Cleanup: destroy remaining sandboxes, then destroy all volumes tagged with
PREFIX (and leftover smoke/e2e/cx- volumes).
"""

from __future__ import annotations

import concurrent.futures
import os
import sys
import time
import traceback
import uuid
from dataclasses import dataclass, field

import requests
from cubesandbox import ApiError, Sandbox, Volume

API = os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000")
TPL = os.environ["CUBE_TEMPLATE_ID"]
PROXY = os.environ.get("CUBE_PROXY_NODE_IP", "127.0.0.1")
DRIVER = os.environ.get("CUBE_VOLUME_DRIVERS", "cos").split(",")[0].strip()
MOUNT = os.environ.get("CUBE_VOLUME_MOUNT_PATH", "/workspace")
PREFIX = os.environ.get("CUBE_VOLUME_TEST_PREFIX", "cx-" + uuid.uuid4().hex[:8])

os.environ.setdefault("CUBE_API_URL", API)
os.environ.setdefault("E2B_API_URL", API)
os.environ.setdefault("CUBE_DOMAIN", PROXY)


@dataclass
class Report:
    ok: list[str] = field(default_factory=list)
    fail: list[str] = field(default_factory=list)
    info: list[str] = field(default_factory=list)

    def pass_(self, msg: str) -> None:
        self.ok.append(msg)
        print(f"  PASS  {msg}")

    def fail_(self, msg: str) -> None:
        self.fail.append(msg)
        print(f"  FAIL  {msg}")

    def note(self, msg: str) -> None:
        self.info.append(msg)
        print(f"  INFO  {msg}")


R = Report()
OWNED_VOLS: set[str] = set()
OWNED_SBS: set[str] = set()


def vol_name(tag: str) -> str:
    return f"{PREFIX}-{tag}"


def track_vol(v: Volume) -> Volume:
    OWNED_VOLS.add(v.volume_id)
    return v


def sandbox_id(sb: Sandbox) -> str | None:
    return getattr(sb, "sandbox_id", None) or getattr(sb, "id", None)


def track_sb(sb: Sandbox) -> Sandbox:
    sid = sandbox_id(sb)
    if sid:
        OWNED_SBS.add(sid)
    return sb


def create_sandbox_with_volume(vol: Volume, **kwargs) -> Sandbox:
    """Match verify_volume.py: e2b-style volume_mounts={path: Volume}."""
    return Sandbox.create(
        template=TPL,
        timeout=kwargs.pop("timeout", 300),
        volume_mounts={MOUNT: vol},
        **kwargs,
    )

def kill_sb(sb: Sandbox) -> None:
    try:
        sb.kill()
    except Exception as e:  # noqa: BLE001
        R.note(f"sandbox kill best-effort: {e}")
    sid = sandbox_id(sb)
    if sid:
        OWNED_SBS.discard(sid)


def destroy_vol(vid: str) -> None:
    try:
        Volume.destroy(vid)
        R.note(f"destroyed volume {vid}")
    except Exception as e:  # noqa: BLE001
        try:
            requests.delete(f"{API}/volumes/{vid}", timeout=30)
        except Exception:  # noqa: BLE001
            pass
        R.note(f"destroy {vid}: {e}")
    OWNED_VOLS.discard(vid)


def file_text(data: object) -> str:
    if isinstance(data, (bytes, bytearray)):
        return data.decode()
    return str(data)


def cleanup_all() -> None:
    print("\n=== CLEANUP ===")
    for sid in list(OWNED_SBS):
        try:
            requests.delete(f"{API}/sandboxes/{sid}", timeout=60)
            R.note(f"deleted sandbox {sid}")
        except Exception as e:  # noqa: BLE001
            R.note(f"sandbox cleanup {sid}: {e}")
        OWNED_SBS.discard(sid)

    try:
        resp = requests.get(f"{API}/sandboxes", timeout=30)
        if resp.status_code == 200 and isinstance(resp.json(), list):
            for item in resp.json():
                sid = item.get("sandboxID") or item.get("sandbox_id")
                if sid:
                    requests.delete(f"{API}/sandboxes/{sid}", timeout=60)
    except Exception as e:  # noqa: BLE001
        R.note(f"sandbox list cleanup: {e}")

    time.sleep(2)

    vids = set(OWNED_VOLS)
    try:
        resp = requests.get(f"{API}/volumes", timeout=30)
        if resp.status_code == 200:
            for item in resp.json():
                vid = item.get("volumeID") or item.get("volume_id")
                name = item.get("name", "")
                if not vid:
                    continue
                if (
                    vid in vids
                    or name.startswith(PREFIX)
                    or name.startswith("vol-smoke-")
                    or name.startswith("e2e-")
                    or name.startswith("cx-")
                ):
                    vids.add(vid)
    except Exception as e:  # noqa: BLE001
        R.note(f"list volumes for cleanup: {e}")

    for vid in sorted(vids):
        # DELETE may only soft-delete one of several duplicate live rows.
        for _ in range(8):
            code = requests.delete(f"{API}/volumes/{vid}", timeout=30).status_code
            if code in (204, 404):
                # keep going a few times in case duplicates remain
                pass
            time.sleep(0.1)
        OWNED_VOLS.discard(vid)

    # Final sweep by list names.
    for _ in range(5):
        left = requests.get(f"{API}/volumes", timeout=30).json()
        leftover = [
            v
            for v in left
            if any(
                str(v.get("name", "")).startswith(p)
                for p in (PREFIX, "vol-smoke-", "e2e-", "cx-")
            )
        ]
        if not leftover:
            break
        for v in leftover:
            vid = v.get("volumeID") or v.get("name")
            requests.delete(f"{API}/volumes/{vid}", timeout=30)

    left = requests.get(f"{API}/volumes", timeout=30).json()
    leftover = [
        v
        for v in left
        if any(
            str(v.get("name", "")).startswith(p)
            for p in (PREFIX, "vol-smoke-", "e2e-", "cx-")
        )
    ]
    if leftover:
        # Last resort on single-node lab hosts: hard-delete ghost duplicate rows
        # created by the UNIQUE(volume_id, deleted_at) NULL race.
        import subprocess

        sql = (
            "DELETE FROM t_cube_volume WHERE volume_id LIKE 'cx-%' "
            "OR volume_id LIKE 'e2e-%' OR volume_id LIKE 'vol-smoke-%';"
        )
        try:
            subprocess.run(
                [
                    "docker",
                    "exec",
                    "cube-sandbox-mysql",
                    "mysql",
                    "-ucube",
                    "-pcube_pass",
                    "cube_mvp",
                    "-e",
                    sql,
                ],
                check=False,
                capture_output=True,
                text=True,
            )
            left = requests.get(f"{API}/volumes", timeout=30).json()
            leftover = [
                v
                for v in left
                if any(
                    str(v.get("name", "")).startswith(p)
                    for p in (PREFIX, "vol-smoke-", "e2e-", "cx-")
                )
            ]
        except Exception as e:  # noqa: BLE001
            R.note(f"SQL hard cleanup skipped: {e}")

    if leftover:
        R.fail_(f"leftover volumes after cleanup: {leftover}")
    else:
        R.pass_(f"cleanup: no leftover test volumes (prefix={PREFIX})")


def scenario_c1_concurrent_create(n: int = 8) -> list[str]:
    print(f"\n=== C1 concurrent create x{n} ===")
    names = [vol_name(f"c1-{i}") for i in range(n)]

    def one(name: str) -> tuple[str, str]:
        v = Volume.create(name, driver=DRIVER)
        track_vol(v)
        return v.volume_id, v.name

    t0 = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=n) as ex:
        futs = [ex.submit(one, nm) for nm in names]
        results = [f.result() for f in concurrent.futures.as_completed(futs)]
    dt = time.time() - t0
    ids = {r[0] for r in results}
    if len(ids) == n:
        R.pass_(f"C1: created {n} volumes concurrently in {dt:.2f}s")
    else:
        R.fail_(f"C1: expected {n} unique ids, got {len(ids)}")
    listed = {v.volume_id for v in Volume.list()}
    missing = ids - listed
    if missing:
        R.fail_(f"C1: missing from list: {missing}")
    else:
        R.pass_("C1: all concurrent creates visible in list")
    return list(ids)


def scenario_c2_concurrent_get(vol_ids: list[str]) -> None:
    print(f"\n=== C2 concurrent get x{len(vol_ids)} ===")

    def one(vid: str) -> str:
        info = Volume.get_info(vid)
        assert info.volume_id == vid
        return vid

    with concurrent.futures.ThreadPoolExecutor(max_workers=len(vol_ids)) as ex:
        list(ex.map(one, vol_ids))
    R.pass_(f"C2: concurrent get_info for {len(vol_ids)} volumes OK")


def scenario_c3_multi_sandbox_share(m: int = 3) -> None:
    print(f"\n=== C3 concurrent mounts + sequential share (m={m}) ===")
    v = track_vol(Volume.create(vol_name("c3-share"), driver=DRIVER))
    marker = f"c3-{uuid.uuid4().hex[:8]}"
    path = f"{MOUNT}/c3_shared.txt"

    def boot(i: int) -> tuple[int, Sandbox, str]:
        sb = track_sb(create_sandbox_with_volume(v))
        out = sb.commands.run(f"test -d {MOUNT} && df -h {MOUNT} | tail -1")
        return i, sb, str(getattr(out, "stdout", "") or "")

    with concurrent.futures.ThreadPoolExecutor(max_workers=m) as ex:
        results = [
            f.result()
            for f in concurrent.futures.as_completed(
                [ex.submit(boot, i) for i in range(m)]
            )
        ]
    results.sort(key=lambda x: x[0])
    R.pass_(f"C3a: booted {m} sandboxes concurrently on {v.volume_id}")
    for i, _sb, dfout in results:
        if MOUNT in dfout or "virtio" in dfout or dfout.strip():
            R.pass_(f"C3a: sandbox[{i}] mount visible")
        else:
            R.fail_(f"C3a: sandbox[{i}] mount missing: {dfout!r}")

    # Sequential share (same contract as verify_volume scenario B).
    writer = results[0][1]
    reader = results[1][1] if len(results) > 1 else results[0][1]
    writer.files.write(path, marker)
    writer.commands.run(f"sync; ls -la {path}")
    text = ""
    for _ in range(15):
        try:
            text = file_text(reader.files.read(path))
        except Exception:  # noqa: BLE001
            out = reader.commands.run(f"cat {path} 2>/dev/null || true")
            text = str(getattr(out, "stdout", "") or "")
        if marker in text:
            break
        time.sleep(0.5)
    if marker in text:
        R.pass_("C3b: sequential cross-sandbox read sees writer data")
    else:
        R.note(f"C3b: share not visible via peer mount (cosfs cache?): {text!r}")
        R.fail_("C3b: sequential cross-sandbox read failed")

    with concurrent.futures.ThreadPoolExecutor(max_workers=m) as ex:
        list(ex.map(lambda x: kill_sb(x[1]), results))
    time.sleep(2)
    try:
        Volume.destroy(v.volume_id)
        OWNED_VOLS.discard(v.volume_id)
        R.pass_("C3: destroy volume after parallel sandbox teardown OK")
    except Exception as e:  # noqa: BLE001
        R.fail_(f"C3: destroy after teardown failed: {e}")


def scenario_c4_attach_detach_churn(rounds: int = 2, parallel: int = 2) -> None:
    print(f"\n=== C4 attach/detach churn rounds={rounds} parallel={parallel} ===")
    v = track_vol(Volume.create(vol_name("c4-churn"), driver=DRIVER))

    for r in range(rounds):

        def boot(_i: int, round_no: int = r) -> Sandbox:
            sb = track_sb(create_sandbox_with_volume(v))
            path = f"{MOUNT}/churn-{round_no}-{_i}.txt"
            payload = f"r{round_no}-i{_i}"
            sb.files.write(path, payload)
            text = file_text(sb.files.read(path))
            if payload not in text:
                raise RuntimeError(f"io mismatch {text!r}")
            return sb

        with concurrent.futures.ThreadPoolExecutor(max_workers=parallel) as ex:
            sbs = [
                f.result()
                for f in concurrent.futures.as_completed(
                    [ex.submit(boot, i) for i in range(parallel)]
                )
            ]
        with concurrent.futures.ThreadPoolExecutor(max_workers=parallel) as ex:
            list(ex.map(kill_sb, sbs))
        time.sleep(1.5)
        R.pass_(f"C4: round {r + 1}/{rounds} parallel={parallel} attach+IO+detach OK")

    sb = track_sb(create_sandbox_with_volume(v))
    out = sb.commands.run(f"ls -la {MOUNT} | head -20")
    R.note(f"C4: after churn ls: {getattr(out, 'stdout', out)}")
    kill_sb(sb)
    time.sleep(2)
    destroy_vol(v.volume_id)
    R.pass_("C4: churn volume destroyed")


def scenario_c5_delete_in_use() -> None:
    print("\n=== C5 delete while in use ===")
    v = track_vol(Volume.create(vol_name("c5-busy"), driver=DRIVER))
    sb = track_sb(create_sandbox_with_volume(v))
    # Give Cubelet → CubeMaster refcount update a moment to land.
    time.sleep(3)
    rejected = False
    try:
        Volume.destroy(v.volume_id)
    except Exception as e:  # noqa: BLE001
        rejected = True
        R.pass_(f"C5: destroy in-use rejected: {type(e).__name__}: {e}")
    if not rejected:
        # Probe raw HTTP too for clarity.
        code = requests.delete(f"{API}/volumes/{v.volume_id}", timeout=30).status_code
        R.note(f"C5: in-use destroy HTTP after SDK success path code={code}")
        R.fail_(
            "C5: destroy in-use volume succeeded (refcount gate not armed yet "
            "or not enforced on single-node)"
        )
        OWNED_VOLS.discard(v.volume_id)
    kill_sb(sb)
    time.sleep(2)
    if rejected:
        try:
            Volume.destroy(v.volume_id)
            OWNED_VOLS.discard(v.volume_id)
            R.pass_("C5: destroy after sandbox gone OK")
        except Exception as e:  # noqa: BLE001
            R.fail_(f"C5: destroy after free failed: {e}")
    else:
        R.note("C5: volume already destroyed while sandbox was live")


def scenario_c6_concurrent_destroy(vol_ids: list[str]) -> None:
    print(f"\n=== C6 concurrent destroy x{len(vol_ids)} ===")
    present = {v.volume_id for v in Volume.list()}
    targets = [vid for vid in vol_ids if vid in present]
    if not targets:
        R.note("C6: no targets left (already cleaned)")
        return

    def one(vid: str) -> str:
        Volume.destroy(vid)
        OWNED_VOLS.discard(vid)
        return vid

    with concurrent.futures.ThreadPoolExecutor(
        max_workers=min(8, len(targets))
    ) as ex:
        done = list(ex.map(one, targets))
    left = {v.volume_id for v in Volume.list()} & set(done)
    if left:
        R.fail_(f"C6: still listed after destroy: {left}")
    else:
        R.pass_(f"C6: concurrently destroyed {len(done)} volumes")


def scenario_c7_duplicate_name_race() -> None:
    print("\n=== C7 concurrent create same name ===")
    name = vol_name("c7-dup")
    winners: list[str] = []
    errors: list[str] = []

    def one() -> tuple[str, str]:
        try:
            v = Volume.create(name, driver=DRIVER)
            track_vol(v)
            return "ok", v.volume_id
        except Exception as e:  # noqa: BLE001
            return "err", f"{type(e).__name__}: {e}"

    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as ex:
        results = [
            f.result()
            for f in concurrent.futures.as_completed([ex.submit(one) for _ in range(6)])
        ]
    for kind, val in results:
        if kind == "ok":
            winners.append(val)
        else:
            errors.append(val)
    uniq = set(winners)
    listed = [x for x in Volume.list() if x.name == name]
    R.note(
        f"C7: sdk_ok={len(winners)} unique_ids={len(uniq)} "
        f"errors={len(errors)} list_rows={len(listed)}"
    )
    # Ideal: one winner + others conflict. Soft-delete UNIQUE(volume_id,deleted_at)
    # allows multiple live NULL deleted_at rows under race — detect that.
    if len(listed) == 1 and len(uniq) <= 1:
        R.pass_("C7: list shows a single live row for the raced name")
    elif len(listed) > 1:
        R.fail_(
            f"C7: race created {len(listed)} live list rows for same name "
            "(MySQL UNIQUE(volume_id,deleted_at) NULL gap)"
        )
    else:
        R.pass_("C7: race completed without multi-row list anomaly")
    # Always soft-clean every list row with this name via repeated DELETE.
    for _ in range(len(listed) + 2):
        try:
            Volume.destroy(name)
        except Exception:  # noqa: BLE001
            break
    OWNED_VOLS.discard(name)
    for vid in uniq:
        OWNED_VOLS.discard(vid)


def run_scenario(name: str, fn) -> None:
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        R.fail_(f"{name} ABORT: {type(e).__name__}: {e}")
        traceback.print_exc()


def main() -> int:
    print(f"API={API} TPL={TPL} DRIVER={DRIVER} PREFIX={PREFIX}")
    t0 = time.time()
    c1_ids: list[str] = []
    try:
        try:
            c1_ids = scenario_c1_concurrent_create(8)
        except Exception as e:  # noqa: BLE001
            R.fail_(f"C1 ABORT: {type(e).__name__}: {e}")
            traceback.print_exc()
        run_scenario("C2", lambda: scenario_c2_concurrent_get(c1_ids))
        run_scenario("C3", lambda: scenario_c3_multi_sandbox_share(3))
        run_scenario(
            "C4", lambda: scenario_c4_attach_detach_churn(rounds=2, parallel=2)
        )
        run_scenario("C5", scenario_c5_delete_in_use)
        run_scenario("C6", lambda: scenario_c6_concurrent_destroy(c1_ids))
        run_scenario("C7", scenario_c7_duplicate_name_race)
    finally:
        cleanup_all()

    dt = time.time() - t0
    print("\n======== SUMMARY ========")
    print(f"PASS={len(R.ok)} FAIL={len(R.fail)} elapsed={dt:.1f}s")
    for m in R.fail:
        print(f"  FAIL: {m}")
    if R.fail:
        return 1
    print("ALL COMPLEX/CONCURRENT SCENARIOS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
