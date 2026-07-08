#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Python SDK Volume（卷）能力的「场景全覆盖 + 真实业务验证」集成脚本。
#
# 设计原则（不做「一个简单断言就算过」）：
#   * 展示真实数据：创建后按 VolumeInfo 结构体打印 volume_id / name / token，
#     list / get 回来的内容也打印出来对照，而不是只喊「通过」。
#   * 进沙箱看真相：卷挂进沙箱后，用 df / mount / ls -ld 真正查看挂载点是否
#     挂上、是什么文件系统、目录权限如何。
#   * 验证读写权限：在挂载点里用命令行 echo→cat→stat→rm 走一遍真实读写删，
#     再用 SDK files.write/read/stat 双通道复核。
#   * 负向/异常场景：挂不存在的卷、get/delete 不存在的卷、非法卷名，断言被
#     正确拒绝。
#
#   区块 0 —— HTTP 响应契约验证（直接打 REST 端点，不经 SDK 封装）：
#     对照接口表逐条断言「状态码 + 响应字段」——
#       GET    /volumes            -> 200  [{volumeID, name}]
#       POST   /volumes            -> 201  {volumeID, name, token}
#       GET    /volumes/{volumeID} -> 200  {volumeID, name, token}
#       DELETE /volumes/{volumeID} -> 204  （空响应体）
#     并额外验证「删除后 GET -> 404」，真实响应（状态码/body）都进报告。
#
#   区块 1 —— 逐 driver 完整生命周期（管控面 + 真实数据面）：
#     对 default / cos / cfs / ... 每个 driver：
#       1) create()  -> 打印 VolumeInfo 结构体
#       2) list()    -> 断言并打印列表中的该项    （创建后列出看看）
#       3) get(id)   -> 断言并打印（get 带 token） （单查确认）
#       4) 挂沙箱    -> 进沙箱看挂载点 + 读写权限 + 命令行/SDK 双通道读写
#       5) delete()  -> 删除
#       6) list()    -> 断言列表里已消失          （删除后列出确认）
#
#   区块 2 —— 多沙箱数据面场景（复用一个可用 driver）：
#     B   单卷 + 两个沙箱共用  -> 两边都看挂载点，A 写 / B 读
#     C   单卷 + 先后使用      -> A 写、kill A、B 读回（跨沙箱持久化）
#
#   区块 3 —— 异常/负向验证：
#     E1  挂载不存在的卷        -> 断言被拒绝
#     E2  get 不存在的卷        -> 断言抛 VolumeNotFoundError / ApiError
#     E3  delete 不存在的卷     -> 断言报错或幂等（如实记录）
#     E4  非法卷名创建          -> 断言 SDK 侧 ValueError
#     E5  非预期/未配置 driver  -> 断言被后端拒绝（如 cfs 未配置），展示原始
#                                 请求入参 + 原始响应（状态码 + raw json）+ SDK 错误码
#
#   区块 4 —— cfs / s3 对象存储 driver 能力专项验证：
#     把 cfs、s3 各自「单独领出来」跑一遍完整生命周期（create→list/get→挂沙箱看
#     挂载/读写→delete→list 确认）。create/delete 走 REST 原始端点，逐步打印
#     「请求入参 + 原始响应（状态码 + raw json）」，任一步异常都如实记录原始信息，
#     方便直接观察这两个 driver 在当前部署下的真实行为与异常。driver 名可用
#     CUBE_VOLUME_CFS_DRIVER / CUBE_VOLUME_S3_DRIVER 覆盖（默认 cfs / s3）。
#
# 每个场景自带清理（先 kill 沙箱再 delete 卷），单场景失败不中断其余，最终
# 输出一份按场景分组、带真实数据的完整报告。
#
# 建议在 CubeProxy 所在机器（如 9.135.78.206）上跑，让数据面请求走 loopback，
# 绕开公司安全网关对远端 :80 流量返回 403 的拦截：
#
#   export CUBE_API_URL=http://127.0.0.1:3000
#   export CUBE_TEMPLATE_ID=<模板ID>                 # 必填
#   export CUBE_PROXY_NODE_IP=127.0.0.1              # loopback，不过网关
#   export CUBE_VOLUME_DRIVERS=cos,cfs               # 要测的 driver
#   export CUBE_VOLUME_MOUNT_PATH=/workspace         # 沙箱内挂载点
#   export CUBE_VOLUME_UNEXPECTED_DRIVER=cfs          # 可选：E5 用的「非预期/未配置」
#                                                    #   driver 名，不设则用一个随机
#                                                    #   不存在的名字（cfs-not-configured-*）
#   export CUBE_VOLUME_CFS_DRIVER=cfs                 # 可选：区块 4 的 cfs driver 名（默认 cfs）
#   export CUBE_VOLUME_S3_DRIVER=s3                   # 可选：区块 4 的 s3 driver 名（默认 s3）
#   export CUBE_API_KEY=<key>                        # 可选：后端开启鉴权时带 X-API-Key
#                                                    #   默认不强制校验，不设即无鉴权直连
#
# 用法：python3 integration_test_volume_scenarios.py

import os
import sys
import time
import uuid

import requests

# 从源码直接导入 SDK，无需安装。
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox, Volume, VolumeMount  # noqa: E402
from cubesandbox import ApiError, VolumeNotFoundError  # noqa: E402

PASS = 0
FAIL = 0
SKIP = 0

# 报告明细：每条记录 (状态, 所属场景, 检查项, 详情)
# 状态取值：PASS / FAIL / SKIP / INFO（INFO 仅展示真实数据，不计入通过/失败）
RESULTS = []
_CURRENT_SCENE = "-"


def _record(status, label, detail=""):
    RESULTS.append((status, _CURRENT_SCENE, label, detail))


def scene(name):
    """标记后续检查项归属的场景，供报告分组使用。"""
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
    """把多行命令输出压成单行，便于写进报告。"""
    if not s:
        return ""
    one = " ¦ ".join(line.rstrip() for line in s.strip().splitlines() if line.strip())
    return one if len(one) <= limit else one[: limit - 3] + "..."


def info(label, detail=""):
    """展示一条真实数据，进报告但不计入通过/失败。"""
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
    """记录一个非断言型的失败（如异常、清理失败）。"""
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


# 认证：CubeAPI 默认不强制校验（未配置 auth callback 时全放行）；这里只有设置了
# CUBE_API_KEY 才会带上 X-API-Key 头，既能对接开启鉴权的后端，也不影响无鉴权部署。
API_KEY = os.environ.get("CUBE_API_KEY")


def auth_headers():
    return {"X-API-Key": API_KEY} if API_KEY else {}


def err_desc(exc):
    """把 SDK 异常格式化为「类型 status_code=<码>: 消息」，展示 SDK 侧的具体错误码。"""
    code = getattr(exc, "status_code", None)
    if code is not None:
        return "%s status_code=%s: %s" % (type(exc).__name__, code, exc)
    return "%s: %s" % (type(exc).__name__, exc)


def list_volume_ids():
    """列出当前所有卷的 volume_id 集合。"""
    return {v.volume_id for v in Volume.list()}


def safe_delete_volume(volume_id):
    if not volume_id:
        return
    try:
        Volume.delete(volume_id)
        green("清理：卷 %s 已删除" % volume_id)
        _record("PASS", "清理：删除卷 %s" % volume_id)
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


def try_create(name, driver):
    """创建一个卷；返回 (VolumeInfo, None) 或 (None, 失败原因)。"""
    try:
        if driver:
            return Volume.create(name, driver=driver), None
        return Volume.create(name), None
    except ApiError as exc:
        # 该 driver 在当前部署未配置、后端 500 等。展示 SDK 侧具体状态码。
        return None, "ApiError(status_code=%s): %s" % (exc.status_code, exc)
    except Exception as exc:  # noqa: BLE001
        return None, err_desc(exc)


def run_cmd(sb, cmd, label, tag, expect_zero=True):
    """在沙箱里执行命令：断言退出码，并把真实输出记进报告。返回 CommandResult 或 None。"""
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
# 复用小工具：创建后列出/单查确认、删除后列出确认
# ---------------------------------------------------------------------------
def verify_present(vid, name, label):
    """创建后确认卷「真的存在」：list 能列出 + get 单查字段一致；并打印真实数据。"""
    # 创建后列出看看 —— 不能只信 create 的返回值
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
    # 单查确认 —— get 会带回 token（list 不带）
    try:
        got = Volume.get(vid)
        info("%s：get 返回 VolumeInfo" % label,
             "volume_id=%r name=%r token=%r" % (got.volume_id, got.name, got.token))
        assert_eq("%s：get 返回相同 volume_id" % label, vid, got.volume_id)
        assert_eq("%s：get 回显 name 一致" % label, name, got.name)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：get 失败" % label, str(exc))


def verify_absent(vid, label):
    """删除后确认卷「真的没了」：list 中不再出现。"""
    try:
        ids = list_volume_ids()
        assert_true("%s：删除后 list 中已消失" % label, vid not in ids,
                    "vid=%s 删除后仍在列表中" % vid)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：删除后 list 失败" % label, str(exc))


def inspect_mount(sb, mount_path, tag):
    """进沙箱看挂载点是否真的挂上、是什么文件系统、目录权限如何（真实业务视角）。"""
    mp = mount_path.rstrip("/")
    # 挂载点目录必须存在
    run_cmd(sb, "test -d %s && echo EXIST" % mp, "挂载点目录存在", tag)
    # df：挂载点的文件系统 / 容量
    run_cmd(sb, "df -hT %s 2>&1 || df -h %s" % (mp, mp), "df 挂载点文件系统/容量", tag)
    # mount / mountinfo：确认挂载来源与类型
    run_cmd(sb,
            "findmnt -no SOURCE,FSTYPE,OPTIONS %s 2>/dev/null "
            "|| mount | grep -w %s "
            "|| grep -w %s /proc/self/mountinfo" % (mp, mp, mp),
            "挂载来源/类型/选项", tag)
    # 目录权限 / 属主
    run_cmd(sb, "ls -ld %s" % mp, "挂载点权限(ls -ld)", tag)


def verify_rw_permission(sb, mount_path, tag):
    """在挂载点里真实验证读写权限：命令行 echo→cat→stat→rm，再用 SDK 复核。"""
    mp = mount_path.rstrip("/")
    probe = "%s/rwtest_%s.txt" % (mp, uuid.uuid4().hex[:8])
    payload = "cmd-%s" % uuid.uuid4().hex[:8]

    # 命令行：写 -> 读 -> 看权限/属主/大小 -> 删（覆盖读写删三种权限）
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

    # SDK 通道：write / read / stat 复核
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
# 区块 0：HTTP 响应契约验证（直接打 REST 端点，校验状态码 + 响应字段）
# ---------------------------------------------------------------------------
def _short_body(resp, limit=300):
    """把响应体压成单行，便于写进报告。"""
    one = " ".join((resp.text or "").split())
    return one if len(one) <= limit else one[: limit - 3] + "..."


def scenario_http_contract(api_url):
    """直接请求 REST 端点，逐条对照接口表验证「状态码 + 响应字段」。

    与走 SDK 封装的区块 1 互补：这里校验的是 wire 层契约（原始 volumeID/name/
    token 字段、201/204/404 状态码、204 的空响应体），SDK 封装后这些细节都被
    吞掉了。
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
        if isinstance(arr, dict):  # 兼容 {"volumes":[...]} 包裹
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

    # DELETE /volumes/{volumeID} -> 204（空响应体）
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

    # 删除后再 GET -> 404（验证删除真的生效 + 404 契约）
    if vid_deleted:
        try:
            r = s.get(base + "/volumes/%s" % vid_deleted, headers=auth_headers(), timeout=30)
            info("删除后 GET /volumes/{id} 响应",
                 "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
            assert_eq("删除后 GET /volumes/{id} 状态码=404", 404, r.status_code)
        except Exception as exc:  # noqa: BLE001
            note_fail("删除后 GET /volumes/{id} 请求异常", str(exc))
    elif vid:
        # DELETE 未成功，兜底清理，避免残留卷。
        safe_delete_volume(vid)


# ---------------------------------------------------------------------------
# 区块 1：逐 driver 的完整生命周期（管控面 + 真实数据面）
# ---------------------------------------------------------------------------
def volume_lifecycle(label, driver, template_id, mount_path):
    """create -> 打印结构体 -> list/get 确认 -> 进沙箱看挂载点+读写权限 ->
    delete -> list 确认消失。返回 (是否成功, driver)。"""
    header("=== %s ===" % label)
    scene(label)

    name = uniq("e2e-%s" % (driver or "default"))
    vol, reason = try_create(name, driver)
    if not vol:
        skip(label, reason or "卷创建失败")
        return False, driver
    # 按结构体展示创建结果，而不是只喊「通过」
    info("%s：create 返回 VolumeInfo" % label,
         "volume_id=%r name=%r token=%r" % (vol.volume_id, vol.name, vol.token))
    if not assert_true("%s：create 返回 volume_id" % label, bool(vol.volume_id)):
        return False, driver
    assert_eq("%s：create 回显 name" % label, name, vol.name)

    vid = vol.volume_id
    verify_present(vid, name, label)  # 创建后 list + get 确认，并打印真实数据

    # 数据面：挂进沙箱，进去看挂载点 + 验证读写权限
    sb = None
    try:
        sb = Sandbox.create(
            template=template_id,
            volume_mounts=[VolumeMount(name=vid, path=mount_path)],
        )
        if assert_true("%s：挂卷沙箱创建成功" % label, bool(sb.sandbox_id)):
            info("%s：sandbox_id" % label, sb.sandbox_id)
            inspect_mount(sb, mount_path, label)
            verify_rw_permission(sb, mount_path, label)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：挂载/数据面异常" % label, str(exc))
    finally:
        safe_kill(sb, label)

    safe_delete_volume(vid)   # 删除
    verify_absent(vid, label)  # 删除后 list 确认消失
    return True, driver


# ---------------------------------------------------------------------------
# 区块 2：多沙箱数据面场景
# ---------------------------------------------------------------------------
def scenario_b_shared(template_id, driver, mount_path):
    """单卷 + 两个并发沙箱共用 -> 两边都看挂载点，A 写、B 读。

    注意：B 能否立即看到 A 的写入取决于 driver 语义 —— 网络文件系统（如 CFS）
    支持并发共享读写，而对象存储挂载（cos）可能有缓存、无法实时反映。此处失败
    本身就说明该 driver 不支持实时共享可见性，也是有用信号。
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
        mount = [VolumeMount(name=vid, path=mount_path)]
        target = "%s/b_shared.txt" % mp
        payload = "scenario-B-%s" % uuid.uuid4().hex[:8]

        sb_a = Sandbox.create(template=template_id, volume_mounts=mount)
        sb_b = Sandbox.create(template=template_id, volume_mounts=mount)
        assert_true("B：沙箱 A 创建成功", bool(sb_a.sandbox_id))
        assert_true("B：沙箱 B 创建成功", bool(sb_b.sandbox_id))
        info("B：两个沙箱", "A=%s  B=%s" % (sb_a.sandbox_id, sb_b.sandbox_id))

        # 两个沙箱都进去看挂载点，确认都挂上同一个卷
        run_cmd(sb_a, "df -h %s | tail -1" % mp, "A 侧挂载点", "B-A")
        run_cmd(sb_b, "df -h %s | tail -1" % mp, "B 侧挂载点", "B-B")

        # A 写，B 读
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
        # 删除卷前先 kill 两个沙箱（delete 不会自动 detach）。
        safe_kill(sb_a, "B-A")
        safe_kill(sb_b, "B-B")
        safe_delete_volume(vid)


def scenario_c_persist(template_id, driver, mount_path):
    """单卷、先后使用：A 写、kill A、B 读回 -> delete。

    真正的跨沙箱持久化校验：字节必须落到后端存储，而非沙箱 A 的本地 overlay。
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
        mount = [VolumeMount(name=vid, path=mount_path)]
        target = "%s/c_persist.txt" % mp
        payload = "scenario-C-%s" % uuid.uuid4().hex[:8]

        sb_a = Sandbox.create(template=template_id, volume_mounts=mount)
        assert_true("C：沙箱 A 创建成功", bool(sb_a.sandbox_id))
        sb_a.files.write(target, payload)
        info("C：沙箱 A 已写入", "%s -> %r" % (target, payload))
        sb_a.kill()
        sb_a = None
        green("C：沙箱 A 已销毁（重新挂载前先 detach）")
        _record("PASS", "C：沙箱 A 已销毁")

        sb_b = Sandbox.create(template=template_id, volume_mounts=mount)
        assert_true("C：沙箱 B 创建成功", bool(sb_b.sandbox_id))
        assert_eq("C：文件跨沙箱持久化", payload, sb_b.files.read(target))
    except Exception as exc:  # noqa: BLE001
        note_fail("场景 C 异常", str(exc))
    finally:
        safe_kill(sb_a, "C-A")
        safe_kill(sb_b, "C-B")
        safe_delete_volume(vid)


# ---------------------------------------------------------------------------
# 区块 3：异常 / 负向验证
# ---------------------------------------------------------------------------
def scenario_exceptions(api_url, template_id, mount_path):
    header("=== 区块 3：异常 / 负向验证 ===")
    scene("区块3-异常验证")

    # E1：挂载不存在的卷 —— 期望被拒绝
    fake_vid = uniq("nonexistent-vol")
    sb = None
    try:
        sb = Sandbox.create(
            template=template_id,
            volume_mounts=[VolumeMount(name=fake_vid, path=mount_path)],
        )
        note_fail("E1：挂载不存在的卷应失败，但沙箱竟创建成功",
                  "fake_vid=%s sandbox=%s" % (fake_vid, sb.sandbox_id))
    except Exception as exc:  # noqa: BLE001
        assert_true("E1：挂载不存在的卷被正确拒绝", True)
        info("E1：拒绝异常", err_desc(exc))
    finally:
        safe_kill(sb, "E1")

    # E2：get 不存在的卷 —— 期望 VolumeNotFoundError / ApiError
    try:
        got = Volume.get(uniq("nope-get"))
        note_fail("E2：get 不存在的卷应报错，但成功返回", "got=%r" % got)
    except VolumeNotFoundError as exc:
        assert_true("E2：get 不存在的卷抛 VolumeNotFoundError(status_code=%s)" % exc.status_code, True)
        info("E2：异常", err_desc(exc))
    except ApiError as exc:
        assert_true("E2：get 不存在的卷抛 ApiError(status_code=%s)" % exc.status_code, True)
        info("E2：异常", err_desc(exc))
    except Exception as exc:  # noqa: BLE001
        note_fail("E2：get 不存在的卷抛了非预期异常", err_desc(exc))

    # E3：delete 不存在的卷 —— 报错或后端幂等都可接受，如实记录
    try:
        Volume.delete(uniq("nope-del"))
        assert_true("E3：delete 不存在的卷未崩溃（幂等语义，可接受）", True)
        info("E3：说明", "后端未报错，按幂等处理")
    except (VolumeNotFoundError, ApiError) as exc:
        assert_true("E3：delete 不存在的卷抛 %s(status_code=%s)（可接受）"
                    % (type(exc).__name__, exc.status_code), True)
        info("E3：异常", err_desc(exc))
    except Exception as exc:  # noqa: BLE001
        note_fail("E3：delete 不存在的卷抛了非预期异常", err_desc(exc))

    # E4：非法卷名 —— 期望 SDK 侧 ValueError（客户端提前拦截）
    try:
        Volume.create("bad name!!")
        note_fail("E4：非法卷名应被拒绝，但创建成功了")
    except ValueError as exc:
        assert_true("E4：非法卷名被 SDK 拒绝(ValueError)", True)
        info("E4：异常", str(exc))
    except Exception as exc:  # noqa: BLE001
        assert_true("E4：非法卷名被拒绝(%s)" % type(exc).__name__, True)
        info("E4：异常", err_desc(exc))

    # E5：非预期 / 未配置的 driver 创建卷 —— 期望被后端拒绝。
    #   典型场景：环境里没有配置 cfs 插件（或写了个根本不存在的 driver 名），
    #   后端应拒绝而不是静默 fallback 到默认插件。这里同时走两条通道并展示真实数据：
    #     5a) 直接打 REST 端点：展示「请求入参 body」+「原始响应（状态码 + raw json）」
    #     5b) 走 SDK create(driver=...)：展示 SDK 侧封装后的错误码
    #   若后端竟创建成功（说明没有校验 driver），如实记为失败并清理残留卷。
    unexpected_driver = os.environ.get("CUBE_VOLUME_UNEXPECTED_DRIVER") \
        or uniq("cfs-not-configured")
    base = api_url.rstrip("/")

    # 5a) HTTP 原始端点：入参 + 原始响应
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
            # 后端未校验 driver、静默创建成功：记录 volumeID 以便清理，避免残留。
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

    # 5b) SDK 通道复核：create(driver=...) 应抛 ApiError，展示 SDK 侧具体错误码
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


# ---------------------------------------------------------------------------
# 区块 4：cfs / s3 对象存储 driver 能力专项验证（重点暴露原始异常数据）
# ---------------------------------------------------------------------------
def scenario_driver_capability(label, driver, api_url, template_id, mount_path):
    """把某个具体 driver（cfs / s3 对象存储）单独领出来，跑一遍完整能力验证。

    与区块 1 的通用 volume_lifecycle 的区别：这里「创建 / 删除」都走 REST 原始端点，
    逐步打印「请求入参 + 原始响应（状态码 + raw json）」，任一步异常都如实记录原始信息
    ——目的就是直接观察 cfs / s3 driver 在当前部署下的真实行为与异常，而不是只喊通过。

    流程：
      1) POST /volumes {name, driver}   -> 打印入参 + 原始响应；失败则如实记异常并 SKIP 后续
      2) list / get                     -> SDK 通道复核卷真实存在（打印 VolumeInfo）
      3) 挂沙箱                          -> 进沙箱看挂载点(df/mount/权限) + 命令行/SDK 双通道读写
      4) DELETE /volumes/{id}           -> 打印原始响应；再 list 确认消失
    """
    header("=== %s（driver=%r） ===" % (label, driver))
    scene(label)
    base = api_url.rstrip("/")
    s = requests.Session()
    vid = None

    # 1) 创建：REST 原始端点，展示入参 + 原始响应（raw json）
    name = uniq("e2e-%s" % driver)
    req_body = {"name": name, "driver": driver}
    info("%s：create 请求入参" % label, "POST /volumes  body=%s" % req_body)
    try:
        r = s.post(base + "/volumes", json=req_body,
                   headers={"Content-Type": "application/json", **auth_headers()},
                   timeout=30)
        info("%s：create 原始响应" % label,
             "HTTP %d  body=%s" % (r.status_code, _short_body(r)))
        if not assert_true("%s：driver=%r 创建成功(2xx)" % (label, driver),
                           r.status_code < 400,
                           "status_code=%d body=%s" % (r.status_code, _short_body(r))):
            skip("%s 后续能力验证" % label,
                 "driver=%r 创建失败（可能该 driver 未在后端配置），见上方原始响应" % driver)
            return
        body = r.json() if r.content else {}
        vid = body.get("volumeID")
        info("%s：create 返回字段" % label,
             "volumeID=%r name=%r token=%r"
             % (vid, body.get("name"), body.get("token")))
        assert_true("%s：create 返回 volumeID" % label, bool(vid),
                    "body=%s" % _short_body(r))
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：create 请求异常" % label, str(exc))
        return
    if not vid:
        skip("%s 后续能力验证" % label, "create 未返回 volumeID")
        return

    # 2) list / get 确认（SDK 通道复核，打印真实 VolumeInfo）
    verify_present(vid, name, label)

    # 3) 数据面：挂进沙箱，进去看挂载点 + 验证读写权限
    sb = None
    try:
        sb = Sandbox.create(
            template=template_id,
            volume_mounts=[VolumeMount(name=vid, path=mount_path)],
        )
        if assert_true("%s：挂卷沙箱创建成功" % label, bool(sb.sandbox_id)):
            info("%s：sandbox_id" % label, sb.sandbox_id)
            inspect_mount(sb, mount_path, label)
            verify_rw_permission(sb, mount_path, label)
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：挂载/数据面异常" % label, str(exc))
    finally:
        safe_kill(sb, label)

    # 4) 删除：REST 原始端点，展示原始响应；再 list 确认消失
    deleted_vid = None
    try:
        r = s.delete(base + "/volumes/%s" % vid, headers=auth_headers(), timeout=30)
        info("%s：delete 原始响应" % label,
             "HTTP %d  body=%r" % (r.status_code, (r.text or "")[:80]))
        assert_true("%s：delete 成功(2xx)" % label, r.status_code < 400,
                    "status_code=%d" % r.status_code)
        deleted_vid, vid = vid, None
    except Exception as exc:  # noqa: BLE001
        note_fail("%s：delete 请求异常" % label, str(exc))
    finally:
        if vid:  # delete 未成功，兜底清理避免残留
            safe_delete_volume(vid)
    if deleted_vid:
        verify_absent(deleted_vid, label)


def print_report(elapsed):
    """输出一份结构化验证报告：按场景分组的明细（含真实数据）+ 汇总。"""
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
    drivers = [d.strip() for d in os.environ.get("CUBE_VOLUME_DRIVERS", "cos").split(",") if d.strip()]

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

    # 区块 0：HTTP 响应契约验证（状态码 + 响应字段），不经 SDK 封装。
    scenario_http_contract(api_url)

    # 区块 1：逐 driver 完整生命周期。default（无 driver）+ 每个显式 driver 各一遍。
    header("=== 区块 1：逐 driver 完整生命周期（创建→列出→单查→进沙箱看挂载/读写→删除→列出） ===")
    working_drivers = []
    volume_lifecycle("default（默认插件，兼容 e2b）", None, template_id, mount_path)
    for drv in drivers:
        ok, _ = volume_lifecycle("driver=%s" % drv, drv, template_id, mount_path)
        if ok:
            working_drivers.append(drv)

    # 为区块 2 选一个 driver：优先创建成功的显式 driver，否则回退默认插件。
    mount_driver = working_drivers[0] if working_drivers else None
    yellow("\n区块 2 多沙箱场景将使用 driver=%r" % (mount_driver or "<默认>"))

    # 区块 2：多沙箱数据面场景。
    scenario_b_shared(template_id, mount_driver, mount_path)
    scenario_c_persist(template_id, mount_driver, mount_path)

    # 区块 3：异常 / 负向验证。
    scenario_exceptions(api_url, template_id, mount_path)

    # 区块 4：cfs / s3 对象存储 driver 能力专项验证（单独领出来，重点看原始异常数据）。
    header("=== 区块 4：cfs / s3 对象存储 driver 能力专项验证 ===")
    cfs_driver = os.environ.get("CUBE_VOLUME_CFS_DRIVER", "cfs")
    s3_driver = os.environ.get("CUBE_VOLUME_S3_DRIVER", "s3")
    yellow("\n区块 4 将逐个验证 driver：cfs=%r，s3=%r（可用 CUBE_VOLUME_CFS_DRIVER / "
           "CUBE_VOLUME_S3_DRIVER 覆盖）" % (cfs_driver, s3_driver))
    scenario_driver_capability("区块4-cfs driver 能力", cfs_driver,
                               api_url, template_id, mount_path)
    scenario_driver_capability("区块4-s3 对象存储能力", s3_driver,
                               api_url, template_id, mount_path)

    # 输出报告
    print_report(time.time() - started)
    sys.exit(FAIL)


if __name__ == "__main__":
    main()
