#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Integration script for Python SDK Volume API — full scenario coverage plus
# real data-plane checks (not just single assert-and-pass smoke tests).
#
# Design principles:
#   * Show real data: print volume_id / name / token from VolumeInfo after create;
#     compare list/get responses instead of only printing "pass".
#   * Inspect inside sandboxes: after mount, run df / mount / ls -ld to verify the
#     mount point, filesystem type, and directory permissions.
#   * Verify read/write: echo→cat→stat→rm on the mount point, then cross-check via
#     SDK files.write/read/stat.
#   * Negative cases: mount non-existent volume, get_info/destroy missing volume,
#     invalid volume name — all must be rejected correctly.
#
#   Block 0 — HTTP response contract (raw REST, no SDK wrapper):
#     Assert status code + response fields per API table:
#       GET    /volumes            -> 200  [{volumeID, name}]
#       POST   /volumes            -> 201  {volumeID, name, token}
#       GET    /volumes/{volumeID} -> 200  {volumeID, name, token}
#       DELETE /volumes/{volumeID} -> 204  (empty body)
#     Also verify GET -> 404 after delete; raw status/body go into the report.
#
#   Block 1 — Per-driver full lifecycle (control plane + data plane):
#     Run for default / cos / each driver in CUBE_VOLUME_DRIVERS:
#       1) create()     -> print Volume fields
#       2) list()       -> assert and print the new entry
#       3) get_info(id) -> assert and print (includes token)
#       4) sandbox      -> inspect mount + read/write (CLI + SDK)
#       5) destroy()    -> delete volume
#       6) list()       -> assert entry is gone
#     Drivers not deployed here (cfs / s3 / nfs) are skipped when listed.
#
#   Block 2 — Multi-sandbox data-plane scenarios (one working driver):
#     B   one volume, two sandboxes  -> both inspect mount; A writes, B reads
#     C   one volume, sequential use -> A writes, kill A, B reads (persistence)
#
#   Block 3 — Exception / negative validation:
#     E1  mount non-existent volume     -> must be rejected
#     E2  get_info missing volume       -> VolumeNotFoundError / ApiError
#     E3  destroy missing volume        -> error or idempotent (record as-is)
#     E4  invalid volume name           -> SDK ValueError
#     E5  unexpected/unconfigured driver -> backend rejects; show raw request/response
#
# Each scenario cleans up (kill sandbox then delete volume). One failure does not
# stop the rest. Final output is a grouped report with real data.
#
# Run on the CubeProxy host so data-plane traffic uses loopback and avoids
# corporate gateways that block remote :80 with 403:
#
#   export CUBE_API_URL=http://127.0.0.1:3000
#   export CUBE_TEMPLATE_ID=<template-id>            # required
#   export CUBE_PROXY_NODE_IP=127.0.0.1              # loopback, bypass gateway
#   export CUBE_VOLUME_DRIVERS=cos                   # comma-separated drivers
#   export CUBE_VOLUME_MOUNT_PATH=/workspace         # in-sandbox mount path
#   export CUBE_VOLUME_UNEXPECTED_DRIVER=unknown     # optional: E5 driver name
#   export CUBE_API_KEY=<key>                        # optional: X-API-Key header
#
# Usage:
#
#   pip install 'cubesandbox>=0.6.0' requests
#
#   export CUBE_API_URL=http://127.0.0.1:3000
#   export CUBE_TEMPLATE_ID=<template-id>            # required
#   export CUBE_PROXY_NODE_IP=127.0.0.1
#   export CUBE_VOLUME_DRIVERS=cos                   # use cos-rpc for RPC plugin
#   export CUBE_VOLUME_MOUNT_PATH=/workspace
#   export CUBE_VOLUME_UNEXPECTED_DRIVER=unknown     # optional
#   export CUBE_API_KEY=<key>                        # optional
#
#   cd examples/volume/cos
#   python3 verify_volume.py
#
# Optional: point at SDK source tree instead of installed package:
#   export CUBESANDBOX_SRC=/path/to/parent-of-cubesandbox-package
#   python3 verify_volume.py

import os
import re
import sys
import time
import uuid
from dataclasses import dataclass

import requests

# Prefer installed cubesandbox; optional CUBESANDBOX_SRC for SDK source tree.
_src = os.environ.get("CUBESANDBOX_SRC")
if _src:
    sys.path.insert(0, _src)

from cubesandbox import Sandbox, ApiError  # noqa: E402

try:
    from cubesandbox import Volume, VolumeNotFoundError  # noqa: E402
    _USE_SDK_VOLUME = True
except ImportError:
    _USE_SDK_VOLUME = False

    # Fallback until cubesandbox 0.6.0 Volume API is installed (e.g. from PyPI).
    VOLUME_NAME_RE = re.compile(r"^[a-zA-Z0-9_-]+$")
    MAX_VOLUME_NAME_LEN = 128

    class VolumeNotFoundError(ApiError):
        """Raised when GET/DELETE /volumes/{id} returns 404."""

    @dataclass
    class VolumeInfo:
        volume_id: str
        name: str
        token: str = ""

    class Volume:
        @staticmethod
        def _api_base() -> str:
            return os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000").rstrip("/")

        @staticmethod
        def _validate_name(name: str) -> None:
            if not name or len(name) > MAX_VOLUME_NAME_LEN or not VOLUME_NAME_RE.match(name):
                raise ValueError(
                    "volume name must match ^[a-zA-Z0-9_-]+$ and be at most %d characters"
                    % MAX_VOLUME_NAME_LEN
                )

        @staticmethod
        def _raise_api(resp: requests.Response) -> None:
            try:
                msg = resp.json().get("message") or resp.json().get("detail") or resp.text
            except Exception:
                msg = resp.text or ("HTTP %d" % resp.status_code)
            if resp.status_code == 404:
                raise VolumeNotFoundError(msg, resp.status_code)
            raise ApiError(msg, resp.status_code)

        @classmethod
        def create(cls, name: str, driver: str | None = None) -> VolumeInfo:
            cls._validate_name(name)
            body: dict = {"name": name}
            if driver:
                body["driver"] = driver
            resp = requests.post(
                cls._api_base() + "/volumes",
                json=body,
                headers={"Content-Type": "application/json", **auth_headers()},
                timeout=30,
            )
            if not resp.ok:
                cls._raise_api(resp)
            data = resp.json()
            return VolumeInfo(
                volume_id=data["volumeID"],
                name=data["name"],
                token=data.get("token", ""),
            )

        @classmethod
        def list(cls) -> list[VolumeInfo]:
            resp = requests.get(
                cls._api_base() + "/volumes",
                headers=auth_headers(),
                timeout=30,
            )
            if not resp.ok:
                cls._raise_api(resp)
            items = resp.json()
            if isinstance(items, dict):
                items = items.get("volumes") or items.get("items") or []
            return [
                VolumeInfo(volume_id=it["volumeID"], name=it["name"])
                for it in items
                if isinstance(it, dict) and "volumeID" in it
            ]

        @classmethod
        def get_info(cls, volume_id: str) -> VolumeInfo:
            resp = requests.get(
                cls._api_base() + "/volumes/" + volume_id,
                headers=auth_headers(),
                timeout=30,
            )
            if not resp.ok:
                cls._raise_api(resp)
            data = resp.json()
            return VolumeInfo(
                volume_id=data["volumeID"],
                name=data["name"],
                token=data.get("token", ""),
            )

        @classmethod
        def destroy(cls, volume_id: str) -> None:
            resp = requests.delete(
                cls._api_base() + "/volumes/" + volume_id,
                headers=auth_headers(),
                timeout=30,
            )
            if resp.status_code in (200, 204):
                return
            if not resp.ok:
                cls._raise_api(resp)


def _volume_mount_name(vol) -> str:
    """Resolve volumeID for wire volumeMounts[].name (Volume instance or id string)."""
    return vol.volume_id if hasattr(vol, "volume_id") else vol


def create_sandbox(template_id: str, volume_mounts=None, **kwargs):
    """Create sandbox; volume_mounts is e2b-style {mount_path: Volume | volume_id}."""
    if volume_mounts:
        if _USE_SDK_VOLUME:
            return Sandbox.create(
                template=template_id, volume_mounts=volume_mounts, **kwargs
            )
        kwargs["volumeMounts"] = [
            {"name": _volume_mount_name(v), "path": p}
            for p, v in volume_mounts.items()
        ]
    return Sandbox.create(template=template_id, **kwargs)

# Drivers not deployed in the default COS demo environment; skip if listed in
# CUBE_VOLUME_DRIVERS instead of failing block 1.
SKIPPED_DRIVERS = frozenset({"cfs", "s3", "nfs"})

PASS = 0
FAIL = 0
SKIP = 0

# Report rows: (status, scene, check label, detail)
# Status: PASS / FAIL / SKIP / INFO (INFO shows data only, not counted pass/fail)
RESULTS = []
_CURRENT_SCENE = "-"


def _record(status, label, detail=""):
    RESULTS.append((status, _CURRENT_SCENE, label, detail))


def scene(name):
    """Tag subsequent checks with a scene name for grouped reporting."""
    global _CURRENT_SCENE
    _CURRENT_SCENE = name


def green(s):
    print("\033[32m  通过: %s\033[0m" % s)


def red(s):
    print("\033[31m  失败: %s\033[0m" % s)


def gray(s):
    print("\033[90m  跳过: %s\033[0m" % s)


def blue(s):
    print("\033[34m  信息: %s\033[0m" % s)


def yellow(s):
    print("\033[33m%s\033[0m" % s)


def header(s):
    print()
    print("\033[36m%s\033[0m" % s)


def _compact(s, limit=400):
    """Collapse multiline command output to one line for the report."""
    if not s:
        return ""
    one = " ¦ ".join(line.rstrip() for line in s.strip().splitlines() if line.strip())
    return one if len(one) <= limit else one[: limit - 3] + "..."


def info(label, detail=""):
    """Log real data as INFO (shown in report, not counted as pass/fail)."""
    blue("%s%s" % (label, ("  ->  " + detail) if detail else ""))
    _record("INFO", label, detail)


def assert_true(label, ok, detail=""):
    global PASS, FAIL
    if ok:
        PASS += 1
        green(label)
        _record("PASS", label)
    else:
        FAIL += 1
        red("%s (%s)" % (label, detail))
        _record("FAIL", label, detail)
    return ok


def assert_eq(label, expected, actual):
    return assert_true(
        label, expected == actual, "期望=%r 实际=%r" % (expected, actual)
    )


def note_fail(label, detail=""):
    """Record a non-assertion failure (exception, cleanup error, etc.)."""
    global FAIL
    FAIL += 1
    red("%s (%s)" % (label, detail) if detail else label)
    _record("FAIL", label, detail)


def skip(label, reason):
    global SKIP
    SKIP += 1
    gray("%s (%s)" % (label, reason))
    _record("SKIP", label, reason)


def uniq(prefix):
    return "%s-%s" % (prefix, uuid.uuid4().hex[:12])


# Auth: CubeAPI does not require auth by default. Send X-API-Key only when
# CUBE_API_KEY is set so this works with both secured and open deployments.
API_KEY = os.environ.get("CUBE_API_KEY")


def auth_headers():
    return {"X-API-Key": API_KEY} if API_KEY else {}


def err_desc(exc):
    """Format SDK exception as 'Type status_code=N: message'."""
    code = getattr(exc, "status_code", None)
    if code is not None:
        return "%s status_code=%s: %s" % (type(exc).__name__, code, exc)
    return "%s: %s" % (type(exc).__name__, exc)


def list_volume_ids():
    """Return the set of all current volume_id values from list()."""
    return {v.volume_id for v in Volume.list()}


def safe_delete_volume(volume_id):
    if not volume_id:
        return
    try:
        Volume.destroy(volume_id)
        green("清理：卷 %s 已销毁" % volume_id)
        _record("PASS", "清理：销毁卷 %s" % volume_id)
    except Exception as exc:  # noqa: BLE001
        note_fail("清理：卷 %s 删除失败" % volume_id, str(exc))


def safe_kill(sb, label=""):
    if sb is None:
        return
    try:
        sb.kill()
        green("清理：沙箱 %s 已销毁" % (label or sb.sandbox_id))
        _record("PASS", "清理：销毁沙箱 %s" % (label or sb.sandbox_id))
    except Exception as exc:  # noqa: BLE001
        note_fail("清理：沙箱 %s 销毁失败" % label, str(exc))


def try_create(name, driver=None):
    """Create a volume; return (VolumeInfo, None) or (None, failure reason)."""
    try:
        if driver:
            return Volume.create(name, driver=driver), None
        return Volume.create(name), None
    except ApiError as exc:
        return None, "ApiError(status_code=%s): %s" % (exc.status_code, exc)
    except Exception as exc:  # noqa: BLE001
        return None, err_desc(exc)


def run_cmd(sb, cmd, label, tag, expect_zero=True):
    """Run a command in sandbox; assert exit code and log output. Returns result or None."""
    try:
        res = sb.commands.run(cmd, timeout=60)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：命令执行异常" % tag, "%s | cmd=%s" % (exc, cmd))
        return None
    out = _compact(res.stdout) or _compact(res.stderr) or "(空输出)"
    if expect_zero:
        assert_true("%s：%s (exit=%d)" % (tag, label, res.exit_code),
                    res.exit_code == 0, "stderr=%s" % _compact(res.stderr))
    info("%s：%s 输出" % (tag, label), out)
    return res


# ---------------------------------------------------------------------------
# Helpers: confirm volume exists after create; confirm gone after delete
# ---------------------------------------------------------------------------
def verify_present(vid, name, label):
    """After create: list must include volume and get_info must match; print real data."""
    # After create: list must show the new volume (do not trust create() alone)
    try:
        listed = {v.volume_id: v for v in Volume.list()}
        ok = assert_true("%s：list 中能看到刚创建的卷" % label, vid in listed,
                         "vid=%s 不在列表中" % vid)
        if ok:
            v = listed[vid]
            info("%s：list 中该卷" % label,
                 "volume_id=%r name=%r token=%r" % (v.volume_id, v.name, v.token))
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：list 失败" % label, str(exc))
    # get_info returns token (list does not)
    try:
        got = Volume.get_info(vid)
        info("%s：get_info 返回 VolumeInfo" % label,
             "volume_id=%r name=%r token=%r" % (got.volume_id, got.name, got.token))
        assert_eq("%s：get 返回相同 volume_id" % label, vid, got.volume_id)
        assert_eq("%s：get 回显 name 一致" % label, name, got.name)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：get 失败" % label, str(exc))


def verify_absent(vid, label):
    """After delete: volume must no longer appear in list()."""
    try:
        ids = list_volume_ids()
        assert_true("%s：删除后 list 中已消失" % label, vid not in ids,
                    "vid=%s 删除后仍在列表中" % vid)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：删除后 list 失败" % label, str(exc))


def inspect_mount(sb, mount_path, tag):
    """Inspect mount point: exists, filesystem, permissions (data-plane view)."""
    mp = mount_path.rstrip("/")
    # Mount point directory must exist
    run_cmd(sb, "test -d %s && echo EXIST" % mp, "挂载点目录存在", tag)
    # df: filesystem type and capacity at mount point
    run_cmd(sb, "df -hT %s 2>&1 || df -h %s" % (mp, mp), "df 挂载点文件系统/容量", tag)
    # mount / mountinfo: source, type, options
    run_cmd(sb,
            "findmnt -no SOURCE,FSTYPE,OPTIONS %s 2>/dev/null "
            "|| mount | grep -w %s "
            "|| grep -w %s /proc/self/mountinfo" % (mp, mp, mp),
            "挂载来源/类型/选项", tag)
    # Directory owner and permission bits
    run_cmd(sb, "ls -ld %s" % mp, "挂载点权限(ls -ld)", tag)


def verify_rw_permission(sb, mount_path, tag):
    """Verify read/write on mount: CLI echo→cat→stat→rm, then SDK cross-check."""
    mp = mount_path.rstrip("/")
    probe = "%s/rwtest_%s.txt" % (mp, uuid.uuid4().hex[:8])
    payload = "cmd-%s" % uuid.uuid4().hex[:8]

    # CLI: write -> read -> stat -> delete (covers read/write/delete)
    cmd = (
        "set -e; "
        "echo %s > %s; "
        "echo '[read]'; cat %s; "
        "echo '[stat]'; stat -c '%%A %%U:%%G %%s bytes' %s; "
        "rm -f %s; echo '[removed]'"
    ) % (payload, probe, probe, probe, probe)
    res = run_cmd(sb, cmd, "命令行 写→读→查权限→删", tag)
    if res is not None:
        assert_true("%s：命令行读回内容与写入一致" % tag,
                    payload in res.stdout, "stdout=%s" % _compact(res.stdout))

    # SDK: files.write / read / stat cross-check
    sdk_target = "%s/sdk_probe_%s.txt" % (mp, uuid.uuid4().hex[:8])
    sdk_payload = "sdk-%s" % uuid.uuid4().hex[:8]
    try:
        sb.files.write(sdk_target, sdk_payload)
        assert_eq("%s：SDK 写入后读回一致" % tag, sdk_payload, sb.files.read(sdk_target))
        try:
            st = sb.files.stat(sdk_target)
            info("%s：files.stat 元数据" % tag, _compact(str(st)))
        except Exception as exc:  # noqa: BLE001
            info("%s：files.stat 不可用" % tag, str(exc))
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：SDK 读写异常" % tag, str(exc))


# ---------------------------------------------------------------------------
# Block 0: HTTP response contract (raw REST; status code + response fields)
# ---------------------------------------------------------------------------
def _short_body(resp, limit=300):
    """Collapse HTTP body to one line for the report."""
    one = " ".join((resp.text or "").split())
    return one if len(one) <= limit else one[: limit - 3] + "..."


def scenario_http_contract(api_url):
    """Hit REST endpoints directly; assert status code + response fields per API table.

    Complements block 1 (SDK): validates wire contract (volumeID/name/token fields,
    201/204/404 codes, empty 204 body) that SDK wrappers hide.
    """
    header("=== 区块 0：HTTP 响应契约验证（状态码 + 响应字段） ===")
    scene("区块0-HTTP响应契约")
    base = api_url.rstrip("/")
    s = requests.Session()
    vid = None
    vid_deleted = None
    if API_KEY:
        info("区块 0：鉴权", "已设置 CUBE_API_KEY，所有请求带 X-API-Key 头")
    else:
        info("区块 0：鉴权", "未设置 CUBE_API_KEY，按无鉴权部署直连（后端默认不强制校验）")

    # POST /volumes -> 201 {volumeID, name, token}
    name = uniq("e2e-http")
    try:
        r = s.post(base + "/volumes", json={"name": name},
                   headers={"Content-Type": "application/json", **auth_headers()},
                   timeout=30)
        info("POST /volumes 响应", "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
        assert_eq("POST /volumes 状态码=201", 201, r.status_code)
        body = r.json() if r.content else {}
        assert_true("POST 响应含 volumeID 字段", "volumeID" in body, "keys=%s" % list(body))
        assert_true("POST 响应含 name 字段", "name" in body, "keys=%s" % list(body))
        assert_true("POST 响应含 token 字段", "token" in body, "keys=%s" % list(body))
        assert_eq("POST 响应 name 回显一致", name, body.get("name"))
        vid = body.get("volumeID")
    except Exception as exc:  # noqa: BLE001
        note_fail("POST /volumes 请求异常", str(exc))
        return
    if not vid:
        skip("区块 0 后续检查", "POST 未返回 volumeID，无法继续契约验证")
        return

    # GET /volumes -> 200 [{volumeID, name}]
    try:
        r = s.get(base + "/volumes", headers=auth_headers(), timeout=30)
        info("GET /volumes 响应", "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
        assert_eq("GET /volumes 状态码=200", 200, r.status_code)
        arr = r.json() if r.content else []
        if isinstance(arr, dict):  # tolerate {"volumes":[...]} wrapper
            arr = arr.get("volumes") or arr.get("items") or []
        assert_true("GET /volumes 返回数组", isinstance(arr, list),
                    "type=%s" % type(arr).__name__)
        items = [it for it in arr if isinstance(it, dict)]
        if items:
            assert_true("GET /volumes 列表项含 volumeID", "volumeID" in items[0],
                        "keys=%s" % list(items[0]))
            assert_true("GET /volumes 列表项含 name", "name" in items[0],
                        "keys=%s" % list(items[0]))
        assert_true("GET /volumes 能列出刚创建的卷",
                    any(it.get("volumeID") == vid for it in items),
                    "vid=%s 不在列表中" % vid)
    except Exception as exc:  # noqa: BLE001
        note_fail("GET /volumes 请求异常", str(exc))

    # GET /volumes/{volumeID} -> 200 {volumeID, name, token}
    try:
        r = s.get(base + "/volumes/%s" % vid, headers=auth_headers(), timeout=30)
        info("GET /volumes/{id} 响应", "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
        assert_eq("GET /volumes/{id} 状态码=200", 200, r.status_code)
        body = r.json() if r.content else {}
        assert_eq("GET /volumes/{id} 回显 volumeID 一致", vid, body.get("volumeID"))
        assert_true("GET /volumes/{id} 含 name 字段", "name" in body, "keys=%s" % list(body))
        assert_true("GET /volumes/{id} 含 token 字段", "token" in body, "keys=%s" % list(body))
    except Exception as exc:  # noqa: BLE001
        note_fail("GET /volumes/{id} 请求异常", str(exc))

    # DELETE /volumes/{volumeID} -> 204 (empty body)
    try:
        r = s.delete(base + "/volumes/%s" % vid, headers=auth_headers(), timeout=30)
        info("DELETE /volumes/{id} 响应",
             "HTTP %d  body=%r" % (r.status_code, (r.text or "")[:80]))
        assert_eq("DELETE /volumes/{id} 状态码=204", 204, r.status_code)
        assert_true("DELETE 响应体为空（204 No Content）",
                    not (r.text or "").strip(), "body=%r" % (r.text or "")[:80])
        vid_deleted, vid = vid, None
    except Exception as exc:  # noqa: BLE001
        note_fail("DELETE /volumes/{id} 请求异常", str(exc))

    # After delete, GET must return 404 (proves delete + 404 contract)
    if vid_deleted:
        try:
            r = s.get(base + "/volumes/%s" % vid_deleted, headers=auth_headers(), timeout=30)
            info("删除后 GET /volumes/{id} 响应",
                 "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
            assert_eq("删除后 GET /volumes/{id} 状态码=404", 404, r.status_code)
        except Exception as exc:  # noqa: BLE001
            note_fail("删除后 GET /volumes/{id} 请求异常", str(exc))
    elif vid:
        # DELETE failed: best-effort cleanup to avoid leaking volumes
        safe_delete_volume(vid)


# ---------------------------------------------------------------------------
# Block 1: per-driver full lifecycle (control plane + data plane)
# ---------------------------------------------------------------------------
def volume_lifecycle(label, driver, template_id, mount_path):
    """create -> print fields -> list/get verify -> sandbox mount/RW ->
    delete -> list verify absent. Returns (success, driver)."""
    header("=== %s ===" % label)
    scene(label)

    name = uniq("e2e-%s" % (driver or "default"))
    vol, reason = try_create(name, driver)
    if not vol:
        skip(label, reason or "卷创建失败")
        return False, driver
    # Print structured create result (not just "pass")
    info("%s：create 返回 VolumeInfo" % label,
         "volume_id=%r name=%r token=%r" % (vol.volume_id, vol.name, vol.token))
    if not assert_true("%s：create 返回 volume_id" % label, bool(vol.volume_id)):
        return False, driver
    assert_eq("%s：create 回显 name" % label, name, vol.name)

    vid = vol.volume_id
    verify_present(vid, name, label)  # list + get after create; print real data

    # Data plane: mount into sandbox, inspect mount point and read/write
    sb = None
    try:
        sb = create_sandbox(
            template_id,
            volume_mounts={mount_path: vol},
        )
        if assert_true("%s：挂卷沙箱创建成功" % label, bool(sb.sandbox_id)):
            info("%s：sandbox_id" % label, sb.sandbox_id)
            inspect_mount(sb, mount_path, label)
            verify_rw_permission(sb, mount_path, label)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：挂载/数据面异常" % label, str(exc))
    finally:
        safe_kill(sb, label)

    safe_delete_volume(vid)   # destroy volume
    verify_absent(vid, label)  # list must no longer show it
    return True, driver


# ---------------------------------------------------------------------------
# Block 2: multi-sandbox data-plane scenarios
# ---------------------------------------------------------------------------
def scenario_b_shared(template_id, driver, mount_path):
    """One volume, two concurrent sandboxes: both inspect mount; A writes, B reads.

    Note: whether B sees A's write immediately depends on driver semantics — network
    FS (e.g. CFS) supports shared RW; object-store mounts (cos) may cache. Failure
    here signals the driver does not offer live shared visibility.
    """
    header("=== 场景 B：单卷 + 两个沙箱共用挂载（数据面） ===")
    scene("场景B-多沙箱共用")
    mp = mount_path.rstrip("/")
    vid = None
    sb_a = None
    sb_b = None
    try:
        vol, reason = try_create(uniq("e2e-B-%s" % (driver or "default")), driver)
        if not vol:
            skip("场景 B", reason or "卷创建失败")
            return
        vid = vol.volume_id
        info("场景 B：共用卷", "volume_id=%r name=%r" % (vol.volume_id, vol.name))
        target = "%s/b_shared.txt" % mp
        payload = "scenario-B-%s" % uuid.uuid4().hex[:8]

        sb_a = create_sandbox(template_id, volume_mounts={mount_path: vol})
        sb_b = create_sandbox(template_id, volume_mounts={mount_path: vol})
        assert_true("B：沙箱 A 创建成功", bool(sb_a.sandbox_id))
        assert_true("B：沙箱 B 创建成功", bool(sb_b.sandbox_id))
        info("B：两个沙箱", "A=%s  B=%s" % (sb_a.sandbox_id, sb_b.sandbox_id))

        # Both sandboxes inspect mount point (same volume)
        run_cmd(sb_a, "df -h %s | tail -1" % mp, "A 侧挂载点", "B-A")
        run_cmd(sb_b, "df -h %s | tail -1" % mp, "B 侧挂载点", "B-B")

        # A writes, B reads
        sb_a.files.write(target, payload)
        info("B：沙箱 A 已写入", "%s -> %r" % (target, payload))
        try:
            seen = sb_b.files.read(target)
            assert_eq("B：沙箱 B 读到 A 在共享卷上的写入", payload, seen)
        except Exception as exc:  # noqa: BLE001
            note_fail("B：沙箱 B 读取共享文件失败",
                      "%s（该 driver 可能不支持实时共享可见性）" % exc)
    except Exception as exc:  # noqa: BLE001
        note_fail("场景 B 异常", str(exc))
    finally:
        # Kill both sandboxes before delete (destroy does not auto-detach)
        safe_kill(sb_a, "B-A")
        safe_kill(sb_b, "B-B")
        safe_delete_volume(vid)


def scenario_c_persist(template_id, driver, mount_path):
    """One volume, sequential sandboxes: A writes, kill A, B reads back, then delete.

    True cross-sandbox persistence: bytes must land on backend storage, not sandbox A overlay.
    """
    header("=== 场景 C：跨沙箱持久化（A 写、kill、B 读回，数据面） ===")
    scene("场景C-跨沙箱持久化")
    mp = mount_path.rstrip("/")
    vid = None
    sb_a = None
    sb_b = None
    try:
        vol, reason = try_create(uniq("e2e-C-%s" % (driver or "default")), driver)
        if not vol:
            skip("场景 C", reason or "卷创建失败")
            return
        vid = vol.volume_id
        info("场景 C：持久化卷", "volume_id=%r name=%r" % (vol.volume_id, vol.name))
        target = "%s/c_persist.txt" % mp
        payload = "scenario-C-%s" % uuid.uuid4().hex[:8]

        sb_a = create_sandbox(template_id, volume_mounts={mount_path: vol})
        assert_true("C：沙箱 A 创建成功", bool(sb_a.sandbox_id))
        sb_a.files.write(target, payload)
        info("C：沙箱 A 已写入", "%s -> %r" % (target, payload))
        sb_a.kill()
        sb_a = None
        green("C：沙箱 A 已销毁（重新挂载前先 detach）")
        _record("PASS", "C：沙箱 A 已销毁")

        sb_b = create_sandbox(template_id, volume_mounts={mount_path: vol})
        assert_true("C：沙箱 B 创建成功", bool(sb_b.sandbox_id))
        assert_eq("C：文件跨沙箱持久化", payload, sb_b.files.read(target))
    except Exception as exc:  # noqa: BLE001
        note_fail("场景 C 异常", str(exc))
    finally:
        safe_kill(sb_a, "C-A")
        safe_kill(sb_b, "C-B")
        safe_delete_volume(vid)


# ---------------------------------------------------------------------------
# Block 3: exception / negative validation
# ---------------------------------------------------------------------------
def scenario_exceptions(api_url, template_id, mount_path):
    header("=== 区块 3：异常 / 负向验证 ===")
    scene("区块3-异常验证")

    # E1: mount non-existent volume — must be rejected
    fake_vid = uniq("nonexistent-vol")
    sb = None
    try:
        sb = create_sandbox(
            template_id,
            volume_mounts={mount_path: fake_vid},
        )
        note_fail("E1：挂载不存在的卷应失败，但沙箱竟创建成功",
                  "fake_vid=%s sandbox=%s" % (fake_vid, sb.sandbox_id))
    except Exception as exc:  # noqa: BLE001
        assert_true("E1：挂载不存在的卷被正确拒绝", True)
        info("E1：拒绝异常", err_desc(exc))
    finally:
        safe_kill(sb, "E1")

    # E2: get_info missing volume — expect VolumeNotFoundError / ApiError
    try:
        got = Volume.get_info(uniq("nope-get"))
        note_fail("E2：get 不存在的卷应报错，但成功返回", "got=%r" % got)
    except VolumeNotFoundError as exc:
        assert_true("E2：get 不存在的卷抛 VolumeNotFoundError(status_code=%s)" % exc.status_code, True)
        info("E2：异常", err_desc(exc))
    except ApiError as exc:
        assert_true("E2：get 不存在的卷抛 ApiError(status_code=%s)" % exc.status_code, True)
        info("E2：异常", err_desc(exc))
    except Exception as exc:  # noqa: BLE001
        note_fail("E2：get 不存在的卷抛了非预期异常", err_desc(exc))

    # E3: destroy missing volume — error or backend idempotency both OK
    try:
        Volume.destroy(uniq("nope-del"))
        assert_true("E3：destroy 不存在的卷未崩溃（幂等语义，可接受）", True)
        info("E3：说明", "后端未报错，按幂等处理")
    except (VolumeNotFoundError, ApiError) as exc:
        assert_true("E3：delete 不存在的卷抛 %s(status_code=%s)（可接受）"
                    % (type(exc).__name__, exc.status_code), True)
        info("E3：异常", err_desc(exc))
    except Exception as exc:  # noqa: BLE001
        note_fail("E3：delete 不存在的卷抛了非预期异常", err_desc(exc))

    # E4: invalid volume name — expect SDK ValueError (client-side guard)
    try:
        Volume.create("bad name!!")
        note_fail("E4：非法卷名应被拒绝，但创建成功了")
    except ValueError as exc:
        assert_true("E4：非法卷名被 SDK 拒绝(ValueError)", True)
        info("E4：异常", str(exc))
    except Exception as exc:  # noqa: BLE001
        assert_true("E4：非法卷名被拒绝(%s)" % type(exc).__name__, True)
        info("E4：异常", err_desc(exc))

    # E5: unexpected / unconfigured driver — backend must reject
    unexpected_driver = os.environ.get("CUBE_VOLUME_UNEXPECTED_DRIVER") \
        or uniq("unknown-driver")
    base = api_url.rstrip("/")

    # E5a) raw HTTP: request body + raw response
    leaked_vid = None
    req_body = {"name": uniq("e2e-baddrv"), "driver": unexpected_driver}
    info("E5：请求入参", "POST /volumes  body=%s" % req_body)
    try:
        s = requests.Session()
        r = s.post(base + "/volumes", json=req_body,
                   headers={"Content-Type": "application/json", **auth_headers()},
                   timeout=30)
        info("E5：HTTP 原始响应",
             "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
        rejected = r.status_code >= 400
        assert_true("E5：非预期 driver=%r 被后端拒绝（非 2xx）" % unexpected_driver,
                    rejected,
                    "status_code=%d body=%s" % (r.status_code, _short_body(r)))
        if not rejected:
            # Backend accepted unknown driver: record volumeID for cleanup
            try:
                body = r.json() if r.content else {}
                leaked_vid = body.get("volumeID")
            except Exception:  # noqa: BLE001
                leaked_vid = None
    except Exception as exc:  # noqa: BLE001
        note_fail("E5：HTTP 请求异常", str(exc))
    finally:
        if leaked_vid:
            info("E5：后端未拒绝非预期 driver，清理残留卷", leaked_vid)
            safe_delete_volume(leaked_vid)

    # E5b) SDK path: create(driver=...) should raise ApiError with status code
    try:
        vol = Volume.create(uniq("e2e-baddrv-sdk"), driver=unexpected_driver)
        note_fail("E5：SDK 用非预期 driver 创建应报错，但成功了",
                  "volume_id=%s" % vol.volume_id)
        safe_delete_volume(vol.volume_id)
    except ApiError as exc:
        assert_true("E5：SDK create(driver=...) 抛 ApiError(status_code=%s)" % exc.status_code, True)
        info("E5：SDK 异常", err_desc(exc))
    except Exception as exc:  # noqa: BLE001
        assert_true("E5：SDK 用非预期 driver 被拒绝(%s)" % type(exc).__name__, True)
        info("E5：SDK 异常", err_desc(exc))


def print_report(elapsed):
    """Print structured report: per-scene details (with real data) + summary."""
    header("======== 验证报告 ========")

    order = []
    grouped = {}
    for status, sc, label, detail in RESULTS:
        if sc not in grouped:
            grouped[sc] = []
            order.append(sc)
        grouped[sc].append((status, label, detail))

    icon = {
        "PASS": "\033[32m[通过]\033[0m",
        "FAIL": "\033[31m[失败]\033[0m",
        "SKIP": "\033[90m[跳过]\033[0m",
        "INFO": "\033[34m[信息]\033[0m",
    }
    for sc in order:
        p = sum(1 for s, _, _ in grouped[sc] if s == "PASS")
        f = sum(1 for s, _, _ in grouped[sc] if s == "FAIL")
        k = sum(1 for s, _, _ in grouped[sc] if s == "SKIP")
        print("\n\033[36m# %s\033[0m  （通过 %d / 失败 %d / 跳过 %d）" % (sc, p, f, k))
        for status, label, detail in grouped[sc]:
            line = "  %s %s" % (icon[status], label)
            if detail:
                line += "  —— %s" % detail
            print(line)

    total = PASS + FAIL
    print()
    print("========================================")
    print(" 汇总：断言 %d 项（另有信息项若干），耗时 %.1fs" % (total + SKIP, elapsed))
    if FAIL == 0:
        green("全部通过：%d/%d 项断言（跳过 %d 项）" % (PASS, total, SKIP))
    else:
        red("存在失败：失败 %d/%d 项断言" % (FAIL, total))
        yellow(" 通过：  %d/%d" % (PASS, total))
        yellow(" 跳过：  %d 项" % SKIP)
    print("========================================")


def main():
    api_url = os.environ.get("CUBE_API_URL", "http://127.0.0.1:3000")
    template_id = os.environ.get("CUBE_TEMPLATE_ID")
    mount_path = os.environ.get("CUBE_VOLUME_MOUNT_PATH", "/workspace")
    raw_drivers = [d.strip() for d in os.environ.get("CUBE_VOLUME_DRIVERS", "cos").split(",") if d.strip()]
    drivers = []
    for drv in raw_drivers:
        if drv in SKIPPED_DRIVERS:
            print("\033[90m  跳过: driver=%r 未在本环境部署，已从 CUBE_VOLUME_DRIVERS 排除\033[0m" % drv)
            continue
        drivers.append(drv)
    inter_sandbox_delay = float(os.environ.get("CUBE_VOLUME_INTER_SANDBOX_DELAY_SECS", "3"))

    if not template_id:
        sys.exit("必须设置 CUBE_TEMPLATE_ID（挂载场景需要用到）")

    print("========================================")
    print(" Python SDK Volume —— 场景 & 真实业务验证")
    print(" CubeAPI：  %s" % api_url)
    print(" Driver：   %s" % (", ".join(drivers) or "<仅默认>"))
    print(" 挂载点：   %s" % mount_path)
    print(" 鉴权：     %s" % ("X-API-Key（已设置 CUBE_API_KEY）" if API_KEY else "无（后端默认不强制校验）"))
    print("========================================")

    started = time.time()

    # Block 0: HTTP contract (status + fields), no SDK wrapper
    scenario_http_contract(api_url)

    # Block 1: per-driver lifecycle — default (no driver) + each explicit driver
    header("=== 区块 1：逐 driver 完整生命周期（创建→列出→单查→进沙箱看挂载/读写→删除→列出） ===")
    working_drivers = []
    volume_lifecycle("default（默认插件，兼容 e2b）", None, template_id, mount_path)
    for drv in drivers:
        ok, _ = volume_lifecycle("driver=%s" % drv, drv, template_id, mount_path)
        if ok:
            working_drivers.append(drv)

    # Pick one driver for block 2: first working explicit driver, else default plugin
    mount_driver = working_drivers[0] if working_drivers else None
    yellow("\n区块 2 多沙箱场景将使用 driver=%r" % (mount_driver or "<默认>"))

    # Block 2: multi-sandbox data-plane scenarios
    scenario_b_shared(template_id, mount_driver, mount_path)
    if inter_sandbox_delay > 0:
        info("区块 2→C", "等待 %.1fs 让 TAP 网络资源回收" % inter_sandbox_delay)
        time.sleep(inter_sandbox_delay)
    scenario_c_persist(template_id, mount_driver, mount_path)

    # Block 3: exception / negative validation
    scenario_exceptions(api_url, template_id, mount_path)

    # Print final report
    print_report(time.time() - started)
    sys.exit(FAIL)


if __name__ == "__main__":
    main()

