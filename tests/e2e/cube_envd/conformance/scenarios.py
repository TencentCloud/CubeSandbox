#!/usr/bin/env python3
"""对照套件的场景定义。

一个"场景"= 对同一端点执行的一组请求；每个请求产出一条"记录"（record），记录里
只保留契约相关的部分（状态码、白名单响应头、响应体摘要/掩码后的 JSON、流式事件的
种类与内容）。Go 基线与 cube-envd 用完全相同的场景与记录键，diff 才有意义。

新增场景的要求：
1. 每个场景必须对应契约里的一句话（README 的 API 表、声明差异表，或 issue #1227 的
   验收条件），不要为了"多测点"而堆请求；
2. 记录里的动态取值必须走 `mask_volatile`，不要在场景里做模糊比较；
3. 场景之间不能共享状态（同一进程内可能连续跑两个实现）。
"""

from __future__ import annotations

import json
import time
import uuid
from typing import Any, Callable

import requests

from connect_client import (
    CONNECT_PROTOCOL_VERSION,
    Client,
    basic_auth,
    body_digest,
    encode_frame,
    mask_volatile,
    read_stream,
    stable_headers,
)

# 场景使用的临时目录前缀；`mask_volatile` 会把它替换成 `<tmp>`，因此两个实现
# 各自用自己的临时目录也能逐字节对比。
TMP_PREFIX = "/tmp/cube-envd-conformance"


def _record(response: requests.Response, *, body: bytes | None = None) -> dict[str, Any]:
    """把一次 HTTP 响应压成一条可对比记录。"""
    payload = body if body is not None else response.content
    record: dict[str, Any] = {
        "status": response.status_code,
        "headers": stable_headers(response),
    }
    content_type = response.headers.get("Content-Type", "")
    if "json" in content_type and payload:
        try:
            # JSON 响应按**掩码后的结构**比较：摘要和长度会被 modifiedTime、
            # pid 这类每次运行都不同的取值带偏，属于噪声而不是契约。
            record["json"] = mask_volatile(json.loads(payload))
            return record
        except json.JSONDecodeError:
            record["body_text"] = payload.decode("utf-8", "replace")
    else:
        # 非 JSON 响应按字节摘要比较；若它是文本，先把临时路径/时间戳掩码掉再摘要——
        # 否则上传响应里的随机临时目录会让每次采集的摘要都不同（噪声，不是差异）。
        try:
            text = payload.decode("utf-8")
        except UnicodeDecodeError:
            record["body_digest"] = body_digest(payload)
        else:
            record["body_digest"] = body_digest(mask_volatile(text).encode())
        record["body_len"] = len(payload)
    return record


def _json_record(response: requests.Response) -> dict[str, Any]:
    """一元 RPC 的统一记录（状态码 + 头部 + 掩码 JSON）。"""
    return _record(response)


def _temp_dir(name: str) -> str:
    """返回场景独占的临时目录路径（尚未创建）。"""
    return f"{TMP_PREFIX}/{name}-{uuid.uuid4().hex[:8]}"


def rest_health(client: Client) -> dict[str, dict[str, Any]]:
    """`GET /health`：模板就绪探针与 SDK 就绪检查的契约。"""
    response = client.request("GET", "/health")
    return {"get_health": _record(response)}


def rest_init_envs(client: Client) -> dict[str, dict[str, Any]]:
    """`POST /init` + `GET /envs`：创建时环境变量的注入与回读。"""
    init = client.unary(
        "/init", {"envVars": {"CONFORMANCE": "1", "CUBE_ENVD_TEST": "yes"}}
    )
    envs = client.request("GET", "/envs")
    return {"post_init": _json_record(init), "get_envs": _json_record(envs)}


def proc_stdout_stderr_exit(client: Client) -> dict[str, dict[str, Any]]:
    """`process.Process/Start`（管道）：stdout/stderr/退出码的事件序列。"""
    response = client.streaming(
        "/process.Process/Start",
        {
            "process": {
                "cmd": "/bin/sh",
                "args": ["-c", "printf out; printf err 1>&2; exit 3"],
                "envs": {},
            },
            "stdin": False,
        },
    )
    stream = read_stream(response)
    return {
        "start_stream": {
            "status": response.status_code,
            "headers": stable_headers(response),
        },
        "start_events": stream,
    }


def proc_signal_exit(client: Client) -> dict[str, dict[str, Any]]:
    """`Start` + `SendSignal`：信号终止时 EndEvent 的形状（声明差异之一）。"""
    response = client.streaming(
        "/process.Process/Start",
        {
            "process": {
                "cmd": "/bin/sleep",
                "args": ["30"],
                "envs": {},
            },
            "tag": "conformance-signal",
            "stdin": False,
        },
    )

    from connect_client import FrameReader  # 局部导入：只有本场景需要逐帧推进

    frames: list[tuple[int, bytes]] = []
    signal_status: int | None = None
    try:
        for frame in FrameReader(response):
            frames.append(frame)
            if signal_status is None:
                # 收到首帧（start 事件）之后再发信号，避免与 spawn 竞态。
                signal = client.unary(
                    "/process.Process/SendSignal",
                    {"process": {"tag": "conformance-signal"}, "signal": "SIGNAL_SIGTERM"},
                )
                signal_status = signal.status_code
            if frame[0] & 0x02:
                break
    finally:
        response.close()

    from connect_client import describe_stream

    return {
        "send_signal": {"status": signal_status},
        "start_events": describe_stream(frames),
    }


def fs_unary_roundtrip(client: Client) -> dict[str, dict[str, Any]]:
    """`filesystem.Filesystem` 一元 RPC：stat / listDir / makeDir / move / remove。"""
    root = _temp_dir("fs")
    nested = f"{root}/nested"
    moved = f"{root}/moved"

    make_dir = client.unary("/filesystem.Filesystem/MakeDir", {"path": nested})
    stat = client.unary("/filesystem.Filesystem/Stat", {"path": nested})
    list_dir = client.unary("/filesystem.Filesystem/ListDir", {"path": root, "depth": 2})
    move = client.unary(
        "/filesystem.Filesystem/Move", {"source": nested, "destination": moved}
    )
    remove = client.unary("/filesystem.Filesystem/Remove", {"path": moved})
    remove_again = client.unary("/filesystem.Filesystem/Remove", {"path": moved})

    # 错误路径同样属于契约：Go 与 Rust 的错误文案系统性不同（errno 大小写、
    # syscall 形状），必须逐字节比对，否则 SDK 侧按文本匹配的逻辑会失效。
    stat_missing = client.unary(
        "/filesystem.Filesystem/Stat", {"path": f"{root}/missing-entry"}
    )
    list_dir_on_file = client.unary(
        "/filesystem.Filesystem/ListDir", {"path": f"{root}/missing-entry", "depth": 1}
    )
    move_missing = client.unary(
        "/filesystem.Filesystem/Move",
        {"source": f"{root}/absent-src", "destination": f"{root}/absent-dst"},
    )

    return {
        "make_dir": _json_record(make_dir),
        "stat": _json_record(stat),
        "list_dir": _json_record(list_dir),
        "move": _json_record(move),
        "remove": _json_record(remove),
        # 参考实现的 Remove 是幂等的；重复删除必须仍然成功。
        "remove_missing": _json_record(remove_again),
        "stat_missing": _json_record(stat_missing),
        "list_dir_on_missing": _json_record(list_dir_on_file),
        "move_missing_source": _json_record(move_missing),
    }


def rest_files_roundtrip(client: Client) -> dict[str, dict[str, Any]]:
    """`POST /files` + `GET /files`：上传/下载的内容与头部契约。"""
    root = _temp_dir("files")
    target = f"{root}/payload.bin"
    payload = b"conformance-payload\x00\x01\x02"

    # /files 用查询参数选择用户（不经 Authorization）；本地非 root 运行时必须是
    # 当前用户，否则参考实现会在 chown 到 root 时 EPERM，采到的就不是协议差异了。
    suffix = f"username={client.user}" if client.user else ""
    upload = client.request(
        "POST",
        f"/files?path={target}&{suffix}",
        headers={"Content-Type": "application/octet-stream"},
        body=payload,
    )
    download = client.request("GET", f"/files?path={target}&{suffix}")
    missing = client.request("GET", f"/files?path={root}/missing.bin&{suffix}")
    # Range 请求：基线的 /files 走 Go 的 http.ServeContent，天然支持 206 + Content-Range；
    # cube-envd 的 MVP 未实现该分支，返回完整 200（已在差异清单里声明）。
    ranged = client.request(
        "GET",
        f"/files?path={target}&{suffix}",
        headers={"Range": "bytes=0-3"},
    )

    return {
        "upload": _json_record(upload),
        "download": _record(download),
        "download_missing": _json_record(missing),
        "download_range": _record(ranged),
    }


def rest_unimplemented(client: Client) -> dict[str, dict[str, Any]]:
    """声明范围之外的 REST/一元面必须返回稳定错误，而不是 404 或 panic。"""
    compose = client.request("POST", "/files/compose", body=b"{}")
    metrics = client.request("GET", "/metrics")
    create_watcher = client.unary(
        "/filesystem.Filesystem/CreateWatcher", {"path": _temp_dir("watcher")}
    )
    return {
        "files_compose": _json_record(compose),
        "metrics": _record(metrics),
        "create_watcher": _json_record(create_watcher),
    }


def cors_preflight(client: Client) -> dict[str, dict[str, Any]]:
    """浏览器直连 SDK 依赖的 CORS 行为（上游用 rs/cors 包住整个 server）。"""
    preflight = client.request(
        "OPTIONS",
        "/process.Process/Start",
        headers={
            "Origin": "https://conformance.test",
            "Access-Control-Request-Method": "POST",
            "Access-Control-Request-Headers": "content-type",
        },
    )
    actual = client.request(
        "GET", "/health", headers={"Origin": "https://conformance.test"}
    )
    return {
        "preflight": _record(preflight),
        "actual_request": _record(actual),
    }


def protocol_errors(client: Client) -> dict[str, dict[str, Any]]:
    """协议层的输入校验：缺/错 Content-Type 与畸形 JSON 的错误形状。"""
    missing_type = client.request(
        "POST", "/process.Process/List", body=json.dumps({}).encode()
    )
    wrong_type = client.request(
        "POST",
        "/process.Process/List",
        headers={"Content-Type": "text/plain"},
        body=json.dumps({}).encode(),
    )
    malformed = client.request(
        "POST",
        "/process.Process/List",
        headers={
            "Content-Type": "application/json",
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
        },
        body=b"{",
    )
    return {
        "missing_content_type": _json_record(missing_type),
        "wrong_content_type": _json_record(wrong_type),
        "malformed_json": _json_record(malformed),
    }


def fs_watch_dir(client: Client) -> dict[str, dict[str, Any]]:
    """`WatchDir`（服务端流）：启动事件与一次文件创建的事件形状。"""
    from connect_client import FrameReader, describe_stream

    root = _temp_dir("watch")
    client.unary("/filesystem.Filesystem/MakeDir", {"path": root})

    suffix = f"username={client.user}" if client.user else ""
    # 采集窗口固定 10 s：无人改动目录时流会一直静默，超时即视为采集结束。
    response = client.streaming(
        "/filesystem.Filesystem/WatchDir",
        {"path": root, "recursive": False},
        timeout=10.0,
    )
    frames: list[tuple[int, bytes]] = []
    created = False
    created_status: int | None = None
    deadline = time.monotonic() + 10.0
    try:
        for frame in FrameReader(response):
            frames.append(frame)
            if not created:
                created_status = client.request(
                    "POST",
                    f"/files?path={root}/created.txt&{suffix}",
                    headers={"Content-Type": "application/octet-stream"},
                    body=b"watch-me",
                ).status_code
                created = True
            if len(frames) >= 6 or time.monotonic() > deadline:
                break
    except requests.exceptions.RequestException:
        pass
    finally:
        response.close()

    return {
        "watch_stream": {
            "status": response.status_code,
            "headers": stable_headers(response),
        },
        "watch_events": describe_stream(frames),
        "file_create_status": {"status": created_status},
    }


def limits_probe(client: Client) -> dict[str, dict[str, Any]]:
    """协议上限探针：一元 body 上限与流式单帧上限。

    两个实现的差异点正在"阈值"上，因此场景刻意构造跨阈值载荷：
    - 一元 body 超过 4 MiB：基线与 cube-envd 都必须拒绝（同为 4 MiB）；
    - 流式单帧约 5 MiB：基线按 connect-go 默认 4 MiB 拒绝，cube-envd 按 SDK 口径
      64 MiB 接受（见 declared_differences.toml 的 limits_probe 条目）。
    """
    oversized_unary = client.unary(
        "/process.Process/List", {"padding": "x" * (4 * 1024 * 1024)}
    )

    # 65 MiB：越过 cube-envd 的 64 MiB 帧上限，而参考实现在 v1.18.1 上用的是
    # connect-go 默认（无单帧上限）。5 MiB 之类的小载荷两个实现都会接受，
    # 观测不到差异，只会把"命令启动失败"的文案差异误当成上限差异。
    padding = "x" * (65 * 1024 * 1024)
    response = client.streaming(
        "/process.Process/Start",
        {
            "process": {"cmd": "/bin/true", "args": [], "envs": {"PAD": padding}},
            "stdin": False,
        },
    )
    stream = read_stream(response, limit=3)
    return {
        "oversized_unary": _json_record(oversized_unary),
        "large_stream_frame": {
            "status": response.status_code,
            "headers": stable_headers(response),
            "stream": stream,
        },
    }


def compression_probe(client: Client) -> dict[str, dict[str, Any]]:
    """响应压缩：请求显式声明 `Accept-Encoding: gzip` 时的编码协商。

    其余场景一律声明 `identity`，把这一维差异隔离在本场景内。
    """
    gzip_header = {"Accept-Encoding": "gzip"}
    health = client.request("GET", "/health", headers=gzip_header)
    listing = client.unary(
        "/process.Process/List", {}, extra_headers=gzip_header
    )
    return {"health_gzip": _record(health), "unary_gzip": _json_record(listing)}


SCENARIOS: dict[str, Callable[[Client], dict[str, dict[str, Any]]]] = {
    "rest_health": rest_health,
    "rest_init_envs": rest_init_envs,
    "proc_stdout_stderr_exit": proc_stdout_stderr_exit,
    "proc_signal_exit": proc_signal_exit,
    "fs_unary_roundtrip": fs_unary_roundtrip,
    "rest_files_roundtrip": rest_files_roundtrip,
    "rest_unimplemented": rest_unimplemented,
    "cors_preflight": cors_preflight,
    "protocol_errors": protocol_errors,
    "fs_watch_dir": fs_watch_dir,
    "limits_probe": limits_probe,
    "compression_probe": compression_probe,
}
