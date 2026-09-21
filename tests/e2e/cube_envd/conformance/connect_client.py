#!/usr/bin/env python3
"""Connect-JSON 客户端与规范化工具（一致性对照套件共用）。

只依赖标准库 + requests：capture.py 在 CI runner 上运行，对两个容器发起同样的
请求；因此这里不做任何"方便"的封装，请求与响应都保持裸形态（原始字节、原始头），
以便逐字段对比。
"""

from __future__ import annotations

import base64
import hashlib
import json
import re
import struct
from typing import Any, Iterator

import requests

CONNECT_STREAM_CONTENT_TYPE = "application/connect+json"
CONNECT_UNARY_CONTENT_TYPE = "application/json"
CONNECT_PROTOCOL_VERSION = "1"
UNARY_FLAG = 0x00
END_STREAM_FLAG = 0x02
MAX_FRAME_BYTES = 64 * 1024 * 1024

# 响应头里值得逐字节对比的部分。其余（Date/Server/Connection/Content-Length 等）
# 由运行时决定，不构成契约；Content-Length 通过 body_digest 间接对比。
KEPT_HEADERS = {
    "content-type",
    "content-encoding",
    "cache-control",
    "vary",
    "accept-ranges",
    "content-range",
    "content-disposition",
    "access-control-allow-origin",
    "access-control-allow-methods",
    "access-control-allow-headers",
    "access-control-expose-headers",
    "access-control-max-age",
    "allow",
    "last-modified",
    "x-e2b-legacy-sdk",
}

# 每次运行都不同、且不属于契约的字段值。只按 key 掩码，不做模糊匹配。
VOLATILE_JSON_KEYS = {
    "pid",
    "ts",
    "watcherId",
    "watcher_id",
    "modifiedTime",
    "modified_time",
    "startedAt",
    "started_at",
}
VOLATILE_JSON_KEY_RE = re.compile(r"^(pid|ts|watcherId|modifiedTime)$")
TIMESTAMP_RE = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z")
TMP_PATH_RE = re.compile(r"/tmp/cube-envd-conformance/[A-Za-z0-9._-]+")


def basic_auth(user: str) -> str:
    """构造 Basic 头：密码部分被参考实现忽略，保持一致写法。"""
    return "Basic " + base64.b64encode(f"{user}:".encode()).decode()


def encode_frame(flags: int, payload: bytes) -> bytes:
    """按 Connect 流式帧格式编码一条消息。"""
    return bytes([flags]) + len(payload).to_bytes(4, "big") + payload


def decode_frames(body: bytes) -> list[tuple[int, bytes]]:
    """拆解完整的流式响应体，返回 (flags, payload) 列表。"""
    frames: list[tuple[int, bytes]] = []
    offset = 0
    while offset < len(body):
        if len(body) - offset < 5:
            raise ValueError(f"truncated Connect frame header at offset {offset}")
        flags = body[offset]
        (length,) = struct.unpack(">I", body[offset + 1 : offset + 5])
        if length > MAX_FRAME_BYTES:
            raise ValueError(f"frame length {length} exceeds the protocol limit")
        start = offset + 5
        end = start + length
        if end > len(body):
            raise ValueError(f"truncated Connect frame payload at offset {offset}")
        frames.append((flags, body[start:end]))
        offset = end
    return frames


class FrameReader:
    """增量读取流式 Connect 响应，容忍任意分块边界。"""

    def __init__(self, response: requests.Response) -> None:
        self._response = response
        self._buffer = bytearray()

    def __iter__(self) -> Iterator[tuple[int, bytes]]:
        for chunk in self._response.iter_content(chunk_size=4096):
            self._buffer.extend(chunk)
            while True:
                if len(self._buffer) < 5:
                    break
                flags = self._buffer[0]
                (length,) = struct.unpack(">I", bytes(self._buffer[1:5]))
                if length > MAX_FRAME_BYTES:
                    raise ValueError(f"frame length {length} exceeds the protocol limit")
                if len(self._buffer) < 5 + length:
                    break
                payload = bytes(self._buffer[5 : 5 + length])
                del self._buffer[: 5 + length]
                yield flags, payload
                if flags & END_STREAM_FLAG:
                    return


class Client:
    """对单个 envd 端点的最小 HTTP 客户端。

    `user` 是请求要执行的本地用户：默认取运行capture 的当前用户，这样本地（非 root）
    与容器内（root）采集到的行为一致——两个实现都会在"切换用户"这一步失败，如果
    请求声明的用户与 daemon 自身用户不同的话。

    所有请求显式声明 `Accept-Encoding: identity`：响应压缩是独立的一维行为，由
    `compression_probe` 场景单独对照，避免它污染其余记录的头部对比。
    """

    def __init__(
        self,
        endpoint: str,
        timeout: float = 30.0,
        user: str | None = None,
        host_header: str | None = None,
        token: str | None = None,
        token_header: str = "e2b-traffic-access-token",
    ) -> None:
        self.endpoint = endpoint.rstrip("/")
        self.timeout = timeout
        self.user = user
        # 活体模式：请求打到 CubeProxy，靠虚拟 Host（`49983-<sandboxID>.<domain>`）与
        # traffic token 路由到沙箱；本地模式两者都为 None，直连 daemon。
        self.host_header = host_header
        self.token = token
        self.token_header = token_header
        self._session = requests.Session()

    def request(
        self,
        method: str,
        path: str,
        *,
        headers: dict[str, str] | None = None,
        body: bytes | None = None,
        stream: bool = False,
        timeout: float | None = None,
    ) -> requests.Response:
        """发送一个请求；`stream=True` 时不缓冲响应体。"""
        merged = {"Accept-Encoding": "identity", "Connection": "close"}
        if self.host_header:
            merged["Host"] = self.host_header
        if self.token:
            merged[self.token_header] = self.token
        if headers:
            merged.update(headers)
        return self._session.request(
            method,
            f"{self.endpoint}{path}",
            headers=merged,
            data=body,
            stream=stream,
            timeout=timeout or self.timeout,
        )

    def unary(
        self,
        path: str,
        payload: dict[str, Any],
        *,
        user: str | None = None,
        extra_headers: dict[str, str] | None = None,
    ) -> requests.Response:
        """发送一元 Connect 请求（`application/json` + 协议版本头）。"""
        headers = {
            "Content-Type": CONNECT_UNARY_CONTENT_TYPE,
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
        }
        selected = user or self.user
        if selected:
            headers["Authorization"] = basic_auth(selected)
        if extra_headers:
            headers.update(extra_headers)
        return self.request("POST", path, headers=headers, body=json.dumps(payload).encode())

    def streaming(
        self,
        path: str,
        payload: dict[str, Any],
        *,
        user: str | None = None,
        extra_headers: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> requests.Response:
        """发送单帧流式 Connect 请求并返回未缓冲的响应。"""
        headers = {
            "Content-Type": CONNECT_STREAM_CONTENT_TYPE,
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
        }
        selected = user or self.user
        if selected:
            headers["Authorization"] = basic_auth(selected)
        if extra_headers:
            headers.update(extra_headers)
        body = encode_frame(UNARY_FLAG, json.dumps(payload).encode())
        return self.request(
            "POST", path, headers=headers, body=body, stream=True, timeout=timeout
        )


# 值本身每次采集都不同、但"是否存在"仍属契约的头部。
PRESENCE_ONLY_HEADERS = {"last-modified"}


def stable_headers(response: requests.Response) -> dict[str, str]:
    """抽取值得对比的响应头（小写键，多值以 `, ` 连接）。

    `Last-Modified` 只比较存在性：它的值来自文件 mtime，两次采集（基线/候选各一次）
    必然不同，比较取值只会产生噪声。
    """
    kept: dict[str, str] = {}
    for name, value in response.headers.items():
        lowered = name.lower()
        if lowered in PRESENCE_ONLY_HEADERS:
            kept[lowered] = "<present>"
        elif lowered in KEPT_HEADERS:
            kept[lowered] = value
    return kept


def body_digest(body: bytes) -> str:
    """返回响应体的 sha256 前 16 位十六进制。"""
    return hashlib.sha256(body).hexdigest()[:16]


def mask_volatile(value: Any) -> Any:
    """递归掩码每次运行都会变化的字段取值。

    规范化是"回归可能藏身之处"，因此规则保持最小且显式：只掩码已知的动态字段
    （pid/时间戳/watcher id/临时路径），其余一律逐字节比较。
    """
    if isinstance(value, dict):
        masked = {}
        for key, item in value.items():
            if VOLATILE_JSON_KEY_RE.match(key):
                masked[key] = "<volatile>"
            else:
                masked[key] = mask_volatile(item)
        return masked
    if isinstance(value, list):
        return [mask_volatile(item) for item in value]
    if isinstance(value, str):
        text = TIMESTAMP_RE.sub("<time>", value)
        text = TMP_PATH_RE.sub("<tmp>", text)
        return text
    return value


def _stream_of(event: dict[str, Any]) -> str:
    """取出数据事件所属的流名（stdout/stderr），非数据事件返回空串。"""
    payload = event.get("event")
    if isinstance(payload, dict):
        data = payload.get("event", {}).get("data") if "event" in payload else None
        if isinstance(data, dict) and len(data) == 1:
            return next(iter(data))
    return ""


def _normalize_concurrent_data_order(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """把**连续**数据事件按流名排序，消除 stdout/stderr 的相对顺序噪声。

    stdout 与 stderr 是两条独立管道：`/bin/sh` 在 stdout 接管道时会缓冲输出，而
    stderr 不缓冲，所以同一命令里 stderr 往往先到——两个实现谁的先到取决于各自的
    读取调度，不属于协议契约。这里只对"连续的一段数据事件"排序：事件集合、每个
    事件的内容、以及数据事件与 start/end/keepalive 之间的相对位置都仍然逐条比对。
    """
    normalized: list[dict[str, Any]] = []
    run: list[dict[str, Any]] = []
    for event in events:
        if _stream_of(event):
            run.append(event)
            continue
        normalized.extend(sorted(run, key=_stream_of))
        run = []
        normalized.append(event)
    normalized.extend(sorted(run, key=_stream_of))
    return normalized


def describe_stream(frames: list[tuple[int, bytes]]) -> dict[str, Any]:
    """把流式响应压成可对比的事件序列描述。

    只保留**事件种类**与**事件内容**（掩码后），不保留字节切分方式：分块边界由
    运行时与缓冲决定，不是契约；并发数据事件的相对顺序同样按上面的理由归一化。
    """
    events: list[dict[str, Any]] = []
    for flags, payload in frames:
        entry: dict[str, Any] = {"flags": flags}
        try:
            decoded = json.loads(payload)
        except json.JSONDecodeError:
            entry["raw"] = payload.decode("utf-8", "replace")
            events.append(entry)
            continue
        if isinstance(decoded, dict) and "error" in decoded:
            entry["error"] = mask_volatile(decoded["error"])
        else:
            entry["event"] = mask_volatile(decoded)
        events.append(entry)
    events = _normalize_concurrent_data_order(events)
    return {"event_sequence": [sorted(event.get("event", event).keys()) for event in events],
            "events": events}


def read_stream(response: requests.Response, *, limit: int = 64) -> dict[str, Any]:
    """读取流式响应的前 `limit` 帧并返回其事件描述。

    读取超时被当作"采集窗口结束"（流本身仍然是打开的），这样长时间静默的流
    （例如无人改动目录的 WatchDir）不会把整轮采集拖成失败。
    """
    frames: list[tuple[int, bytes]] = []
    try:
        for frame in FrameReader(response):
            frames.append(frame)
            if len(frames) >= limit:
                break
    except requests.exceptions.RequestException:
        pass
    finally:
        response.close()
    return describe_stream(frames)
