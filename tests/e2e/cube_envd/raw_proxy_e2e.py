#!/usr/bin/env python3
"""通过 CubeProxy 验证 cube-envd 的原始 HTTP 和 Connect-JSON 数据面。"""

from __future__ import annotations

import argparse
import base64
import json
import struct
import time
import uuid
from dataclasses import dataclass
from pathlib import PurePosixPath
from typing import Any, Iterator

import requests


CONNECT_CONTENT_TYPE = "application/connect+json"
CONNECT_PROTOCOL_VERSION = "1"
END_STREAM_FLAG = 0x02
# 与 cube-envd 及三个自带 SDK 的 Connect 载荷上限一致（MAX_CONNECT_ENVELOPE_SIZE）：
# 越界探针用 MAX_FRAME_BYTES + 1 构造，常量若与 daemon 不一致就会退化成"JSON 解析失败"。
MAX_FRAME_BYTES = 64 * 1024 * 1024


class E2EAssertionError(AssertionError):
    """表示端到端响应不符合 cube-envd 契约。"""


def require(condition: bool, message: str) -> None:
    """在条件不成立时给出可定位的验收失败信息。"""
    if not condition:
        raise E2EAssertionError(message)


def encode_frame(flags: int, payload: bytes) -> bytes:
    """按 Connect-JSON 的五字节头格式编码一条消息。"""
    return bytes([flags]) + len(payload).to_bytes(4, "big") + payload


def decode_frames(body: bytes) -> list[tuple[int, bytes]]:
    """拆解一段完整的 Connect-JSON 响应体，保留标志位与原始载荷。"""
    frames: list[tuple[int, bytes]] = []
    offset = 0
    while offset < len(body):
        require(len(body) - offset >= 5, "truncated Connect frame header")
        flags = body[offset]
        length = int.from_bytes(body[offset + 1 : offset + 5], "big")
        end = offset + 5 + length
        require(end <= len(body), "truncated Connect frame payload")
        frames.append((flags, body[offset + 5 : end]))
        offset = end
    return frames


@dataclass
class Frame:
    """表示从 Connect-JSON 流中读取的一条完整帧。"""

    flags: int
    payload: dict[str, Any]


class FrameReader:
    """从 requests 的分块响应中按帧读取 Connect-JSON 数据。"""

    def __init__(self, response: requests.Response) -> None:
        """保存响应迭代器和未消费的帧缓冲区。"""
        self._response = response
        self._chunks = response.iter_content(chunk_size=8192)
        self._buffer = bytearray()

    def next(self) -> Frame:
        """读取一条完整帧，响应提前结束时稳定失败。"""
        self._fill(5)
        flags = self._buffer[0]
        size = int.from_bytes(self._buffer[1:5], "big")
        require(size <= MAX_FRAME_BYTES, f"response frame exceeds limit: {size}")
        self._fill(5 + size)
        payload = bytes(self._buffer[5 : 5 + size])
        del self._buffer[: 5 + size]
        try:
            decoded = json.loads(payload)
        except json.JSONDecodeError as exc:
            raise E2EAssertionError(f"invalid Connect JSON response: {exc}") from exc
        require(isinstance(decoded, dict), "Connect payload must be a JSON object")
        return Frame(flags=flags, payload=decoded)

    def _fill(self, length: int) -> None:
        """持续读取网络分块直到缓冲区达到指定长度。"""
        while len(self._buffer) < length:
            try:
                chunk = next(self._chunks)
            except StopIteration as exc:
                raise E2EAssertionError("Connect stream ended before a complete frame") from exc
            if chunk:
                self._buffer.extend(chunk)


class ProxyClient:
    """以 CubeProxy 虚拟 Host 访问单个 sandbox 的 HTTP 数据面。"""

    def __init__(self, sandbox_id: str, token: str, proxy_url: str, domain: str) -> None:
        """保存不含 token 的请求目标和 sandbox 路由信息。"""
        self.sandbox_id = sandbox_id
        self._token = token
        self._proxy_url = proxy_url.rstrip("/")
        self._host = f"49983-{sandbox_id}.{domain}"
        self._session = requests.Session()

    def request(
        self,
        method: str,
        path: str,
        *,
        headers: dict[str, str] | None = None,
        token_header: str | None = "e2b-traffic-access-token",
        stream: bool = False,
        **kwargs: Any,
    ) -> requests.Response:
        """通过本地 CubeProxy 地址发送带虚拟 Host 的请求。"""
        request_headers = {"Host": self._host, "Connection": "close"}
        if token_header:
            request_headers[token_header] = self._token
        if headers:
            request_headers.update(headers)
        return self._session.request(
            method,
            f"{self._proxy_url}{path}",
            headers=request_headers,
            stream=stream,
            timeout=45,
            **kwargs,
        )

    def unary(
        self,
        service: str,
        method: str,
        payload: dict[str, Any],
        *,
        user: str | None = None,
    ) -> dict[str, Any]:
        """发送普通 JSON Connect unary RPC 并要求成功 JSON 响应。"""
        headers = {
            "Content-Type": "application/json",
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
        }
        if user:
            headers["Authorization"] = basic_auth(user)
        response = self.request(
            "POST",
            f"/{service}/{method}",
            headers=headers,
            data=json.dumps(payload).encode(),
        )
        require(response.status_code == 200, response_summary(response))
        if not response.content:
            return {}
        body = response.json()
        require(isinstance(body, dict), f"{service}/{method} response is not an object")
        return body

    def stream(
        self,
        service: str,
        method: str,
        payload: dict[str, Any],
        *,
        user: str | None = None,
        timeout_ms: int | None = None,
        flags: int = 0,
    ) -> tuple[requests.Response, FrameReader]:
        """建立单帧 server-streaming Connect 请求并返回帧读取器。"""
        headers = {
            "Content-Type": CONNECT_CONTENT_TYPE,
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
        }
        if user:
            headers["Authorization"] = basic_auth(user)
        if timeout_ms is not None:
            headers["Connect-Timeout-Ms"] = str(timeout_ms)
        response = self.request(
            "POST",
            f"/{service}/{method}",
            headers=headers,
            data=encode_frame(flags, json.dumps(payload).encode()),
            stream=True,
        )
        if response.status_code != 200:
            raise E2EAssertionError(response_summary(response))
        return response, FrameReader(response)


def basic_auth(username: str) -> str:
    """构造仅用于选择本地用户的 Basic 认证头。"""
    encoded = base64.b64encode(f"{username}:".encode()).decode()
    return f"Basic {encoded}"


def response_summary(response: requests.Response) -> str:
    """生成不含认证头和请求内容的简短失败说明。"""
    try:
        payload = response.json()
    except ValueError:
        payload = response.text[:240]
    return f"HTTP {response.status_code}: {payload}"


def stream_frames(reader: FrameReader) -> list[Frame]:
    """读取服务端流直到规范要求的 end-stream 帧。"""
    frames: list[Frame] = []
    while True:
        frame = reader.next()
        frames.append(frame)
        if frame.flags & END_STREAM_FLAG:
            require(frame.payload == {}, f"stream ended with error: {frame.payload}")
            return frames


def process_output(frames: list[Frame], key: str) -> bytes:
    """聚合进程流中指定 stdout、stderr 或 pty 通道的字节。"""
    output = bytearray()
    for frame in frames:
        event = frame.payload.get("event", {})
        data = event.get("data", {})
        if key in data:
            output.extend(base64.b64decode(data[key]))
    return bytes(output)


def process_pid(frame: Frame) -> int:
    """从 Start 流的首帧提取 envd 注册的进程 PID。"""
    pid = frame.payload.get("event", {}).get("start", {}).get("pid")
    require(isinstance(pid, int) and pid > 0, f"missing Start PID: {frame.payload}")
    return pid


def run_short_process(
    client: ProxyClient,
    command: str,
    *,
    user: str,
    envs: dict[str, str] | None = None,
    cwd: str | None = None,
    tag: str | None = None,
) -> list[Frame]:
    """启动短命令并收集 stdout、stderr、结束事件和 end-stream。"""
    process: dict[str, Any] = {"cmd": "/bin/sh", "args": ["-c", command], "envs": envs or {}}
    if cwd:
        process["cwd"] = cwd
    payload: dict[str, Any] = {"process": process, "stdin": False}
    if tag:
        payload["tag"] = tag
    response, reader = client.stream("process.Process", "Start", payload, user=user)
    try:
        frames = stream_frames(reader)
    finally:
        response.close()
    require(any("end" in frame.payload.get("event", {}) for frame in frames), "process end event missing")
    return frames


def create_sandbox(api_url: str, template_id: str) -> tuple[str, str]:
    """经 CubeAPI 创建私有 sandbox，并返回 ID 和 traffic token。"""
    response = requests.post(
        f"{api_url.rstrip('/')}/sandboxes",
        json={"templateID": template_id, "network": {"allowPublicTraffic": False}},
        timeout=120,
    )
    require(response.status_code in (200, 201), response_summary(response))
    payload = response.json()
    sandbox_id = payload.get("sandboxID")
    token = payload.get("trafficAccessToken")
    require(isinstance(sandbox_id, str) and sandbox_id, "CubeAPI create response lacks sandboxID")
    require(isinstance(token, str) and token, "private sandbox response lacks trafficAccessToken")
    return sandbox_id, token


def delete_sandbox(api_url: str, sandbox_id: str) -> None:
    """删除本 harness 创建的 sandbox，避免远端测试资源泄漏。"""
    response = requests.delete(f"{api_url.rstrip('/')}/sandboxes/{sandbox_id}", timeout=90)
    require(response.status_code in (200, 202, 204, 404), response_summary(response))


def wait_for_health(client: ProxyClient) -> None:
    """等待 CubeProxy 完成路由注册并转发 envd 健康检查。"""
    last: str | None = None
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            response = client.request("GET", "/health")
            if response.status_code == 204:
                return
            last = response_summary(response)
        except requests.RequestException as exc:
            last = str(exc)
        time.sleep(1)
    raise E2EAssertionError(f"CubeProxy/envd health did not become ready: {last}")


def test_proxy_access_control(client: ProxyClient) -> None:
    """验证私有 sandbox 的 CubeProxy token 解析和两种兼容 header。"""
    absent = client.request("GET", "/health", token_header=None)
    require(absent.status_code == 403, f"missing traffic token: {response_summary(absent)}")
    wrong = client.request("GET", "/health", headers={"e2b-traffic-access-token": "wrong"})
    require(wrong.status_code == 403, f"wrong traffic token: {response_summary(wrong)}")
    alternate = client.request("GET", "/health", token_header="cube-traffic-access-token")
    require(alternate.status_code == 204, f"cube traffic token rejected: {response_summary(alternate)}")


def test_init_and_process(client: ProxyClient, user: str, prefix: str) -> None:
    """验证 init/envs、命令流、输入、信号、超时、重连与 PTY 控制。"""
    response = client.request("POST", "/init", headers={"Content-Type": "application/json"}, json={"envVars": {"OLD": "old", "E2E_GLOBAL": "first"}})
    require(response.status_code == 204, response_summary(response))
    response = client.request("POST", "/init", headers={"Content-Type": "application/json"}, json={"envVars": {"E2E_GLOBAL": "global"}})
    require(response.status_code == 204, response_summary(response))
    envs = client.request("GET", "/envs")
    # /init 是**逐键合并**，不是整份替换：参考实现用 defaults.EnvVars.Store 覆盖单键
    # （init.go:189-196），并在启动时种入 E2B_SANDBOX（main.go:154-159，非 FC 模式下为
    # "false"）。因此第二次 /init 只改它提到的键，OLD 必须还在。
    require(
        envs.status_code == 200
        and envs.json() == {"E2B_SANDBOX": "false", "E2E_GLOBAL": "global", "OLD": "old"},
        f"init must merge per key and keep the seeded E2B_SANDBOX: {response_summary(envs)}",
    )
    unsupported = client.request("POST", "/init", headers={"Content-Type": "application/json"}, json={"accessToken": "unsupported"})
    require(unsupported.status_code == 400, f"unsupported init field: {response_summary(unsupported)}")

    frames = run_short_process(
        client,
        "printf '%s/%s/%s' \"$E2E_GLOBAL\" \"$E2E_LOCAL\" \"$PWD\"; printf err >&2; exit 7",
        user=user,
        envs={"E2E_LOCAL": "local"},
        cwd="/tmp",
        tag=f"{prefix}-short",
    )
    actual_stdout = process_output(frames, "stdout")
    require(
        actual_stdout == b"global/local//tmp",
        f"process env or cwd mismatch: stdout={actual_stdout!r}, frames={[frame.payload for frame in frames]}",
    )
    actual_stderr = process_output(frames, "stderr")
    require(
        actual_stderr == b"err",
        f"stderr stream mismatch: stderr={actual_stderr!r}, frames={[frame.payload for frame in frames]}",
    )
    end_events = [frame.payload["event"]["end"] for frame in frames if "end" in frame.payload.get("event", {})]
    require(end_events and end_events[-1].get("exitCode") == 7, f"nonzero exit missing: {end_events}")

    input_tag = f"{prefix}-input"
    response, reader = client.stream(
        "process.Process",
        "Start",
        {"process": {"cmd": "/bin/sh", "args": ["-c", "read line; printf '%s' \"$line\""], "envs": {}}, "tag": input_tag, "stdin": True},
        user=user,
    )
    try:
        pid = process_pid(reader.next())
        listed = client.unary("process.Process", "List", {}, user=user)
        require(any(item.get("pid") == pid and item.get("tag") == input_tag for item in listed.get("processes", [])), "live process absent from List")
        client.unary("process.Process", "SendInput", {"process": {"pid": pid}, "input": {"stdin": base64.b64encode(b"direct-input\n").decode()}}, user=user)
        frames = stream_frames(reader)
    finally:
        response.close()
    require(process_output(frames, "stdout") == b"direct-input", "SendInput stdout mismatch")
    reconnect_response, reconnect_reader = client.stream("process.Process", "Connect", {"process": {"tag": input_tag}}, user=user)
    try:
        reconnect_frames = stream_frames(reconnect_reader)
    finally:
        reconnect_response.close()
    require(any("end" in frame.payload.get("event", {}) for frame in reconnect_frames), "terminal Connect lacks end event")

    stream_tag = f"{prefix}-stream-input"
    response, reader = client.stream(
        "process.Process",
        "Start",
        {"process": {"cmd": "/bin/sh", "args": ["-c", "read line; printf '%s' \"$line\""], "envs": {}}, "tag": stream_tag, "stdin": True},
        user=user,
    )
    try:
        stream_pid = process_pid(reader.next())
        body = encode_frame(0, json.dumps({"start": {"process": {"pid": stream_pid}}}).encode())
        body += encode_frame(0, json.dumps({"data": {"input": {"stdin": base64.b64encode(b"stream-input\n").decode()}}}).encode())
        stream_response = client.request(
            "POST",
            "/process.Process/StreamInput",
            headers={"Content-Type": CONNECT_CONTENT_TYPE, "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION, "Authorization": basic_auth(user)},
            data=body,
        )
        require(stream_response.status_code == 200, response_summary(stream_response))
        # StreamInput 是客户端流式 RPC：响应必须走流式编解码（connect+json 内容
        # 类型 + 数据信封 + end-stream 信封）。只断言 200 会漏掉裸 JSON 响应，
        # 而 connect-go 客户端会因流式响应 content-type 校验失败而直接报错。
        require(
            stream_response.headers.get("content-type", "").startswith(
                "application/connect+"
            ),
            response_summary(stream_response),
        )
        response_frames = decode_frames(stream_response.content)
        require(
            len(response_frames) == 2,
            f"StreamInput must answer with a data and an end-stream envelope: {response_frames!r}",
        )
        require(
            response_frames[0] == (0, b"{}"),
            f"StreamInput data envelope mismatch: {response_frames[0]!r}",
        )
        require(
            response_frames[1][0] == 2,
            f"StreamInput must end with an end-stream envelope: {response_frames[1]!r}",
        )
        frames = stream_frames(reader)
    finally:
        response.close()
    require(process_output(frames, "stdout") == b"stream-input", "StreamInput stdout mismatch")

    close_tag = f"{prefix}-close-stdin"
    response, reader = client.stream(
        "process.Process",
        "Start",
        {"process": {"cmd": "/bin/cat", "args": [], "envs": {}}, "tag": close_tag, "stdin": True},
        user=user,
    )
    try:
        close_pid = process_pid(reader.next())
        client.unary("process.Process", "CloseStdin", {"process": {"pid": close_pid}}, user=user)
        close_frames = stream_frames(reader)
    finally:
        response.close()
    require(any("end" in frame.payload.get("event", {}) for frame in close_frames), "CloseStdin did not finish cat")

    signal_tag = f"{prefix}-signal"
    response, reader = client.stream(
        "process.Process",
        "Start",
        {"process": {"cmd": "/bin/sleep", "args": ["30"], "envs": {}}, "tag": signal_tag, "stdin": False},
        user=user,
    )
    try:
        signal_pid = process_pid(reader.next())
        client.unary("process.Process", "SendSignal", {"process": {"pid": signal_pid}, "signal": "SIGNAL_SIGTERM"}, user=user)
        signal_frames = stream_frames(reader)
    finally:
        response.close()
    require(any("end" in frame.payload.get("event", {}) for frame in signal_frames), "SendSignal lacks end event")

    timeout_response, timeout_reader = client.stream(
        "process.Process",
        "Start",
        {"process": {"cmd": "/bin/sleep", "args": ["30"], "envs": {}}, "stdin": False},
        user=user,
        timeout_ms=200,
    )
    try:
        timeout_frames = stream_frames(timeout_reader)
    finally:
        timeout_response.close()
    timeout_end = next(
        (frame.payload["event"]["end"] for frame in timeout_frames if "end" in frame.payload.get("event", {})),
        None,
    )
    require(timeout_end is not None, "Connect-Timeout-Ms lacks end event")
    require(
        timeout_end.get("exitCode") == 143
        and "exited" not in timeout_end
        and timeout_end.get("error") == "terminated by signal 15",
        f"timeout EndEvent does not describe SIGTERM termination: {timeout_end}",
    )

    pty_response, pty_reader = client.stream(
        "process.Process",
        "Start",
        {"process": {"cmd": "/bin/sh", "args": ["-c", "read line; printf 'pty:%s' \"$line\""], "envs": {}}, "pty": {"size": {"cols": 80, "rows": 24}}},
        user=user,
    )
    try:
        pty_pid = process_pid(pty_reader.next())
        client.unary("process.Process", "Update", {"process": {"pid": pty_pid}, "pty": {"size": {"cols": 120, "rows": 40}}}, user=user)
        client.unary("process.Process", "SendInput", {"process": {"pid": pty_pid}, "input": {"pty": base64.b64encode(b"hello\n").decode()}}, user=user)
        pty_frames = stream_frames(pty_reader)
    finally:
        pty_response.close()
    require(b"pty:hello" in process_output(pty_frames, "pty"), "PTY output mismatch")


def test_filesystem(client: ProxyClient, user: str, prefix: str) -> None:
    """验证 raw/multipart 文件、目录 RPC、路径语义、符号链接和 WatchDir。"""
    root = PurePosixPath(f"/home/{user}/{prefix}-files")
    client.unary("filesystem.Filesystem", "MakeDir", {"path": str(root / "nested")}, user=user)
    binary = bytes(range(256)) + b"\\x00cube-envd-e2e"
    response = client.request(
        "POST",
        "/files",
        headers={"Content-Type": "application/octet-stream"},
        params={"path": str(root / "nested" / "binary.bin"), "username": user},
        data=binary,
    )
    require(response.status_code == 200, response_summary(response))
    download = client.request("GET", "/files", params={"path": str(root / "nested" / "binary.bin"), "username": user})
    require(download.status_code == 200 and download.content == binary, "binary file round trip mismatch")

    multipart = client.request(
        "POST",
        "/files",
        params={"username": user},
        files={"file": (str(root / "nested" / "from-form.txt"), b"from multipart")},
    )
    require(multipart.status_code == 200, response_summary(multipart))
    stat = client.unary("filesystem.Filesystem", "Stat", {"path": str(root / "nested" / "binary.bin")}, user=user)
    entry = stat.get("entry", {})
    require(entry.get("type") == "FILE_TYPE_FILE" and entry.get("owner") == user, f"file metadata mismatch: {entry}")
    listed = client.unary("filesystem.Filesystem", "ListDir", {"path": str(root), "depth": 2}, user=user)
    require(any(item.get("name") == "binary.bin" for item in listed.get("entries", [])), "ListDir omits binary file")
    moved = client.unary("filesystem.Filesystem", "Move", {"source": str(root / "nested" / "from-form.txt"), "destination": str(root / "moved.txt")}, user=user)
    require(moved.get("entry", {}).get("name") == "moved.txt", f"Move result mismatch: {moved}")

    tilde = client.unary("filesystem.Filesystem", "MakeDir", {"path": f"~/{prefix}-tilde"}, user=user)
    require(tilde.get("entry", {}).get("path") == f"/home/{user}/{prefix}-tilde", f"tilde path mismatch: {tilde}")
    unknown = client.request(
        "POST",
        "/filesystem.Filesystem/Stat",
        headers={"Content-Type": "application/json", "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION, "Authorization": basic_auth("not-a-real-user")},
        data=json.dumps({"path": "/tmp"}),
    )
    require(unknown.status_code == 401, f"unknown Basic user: {response_summary(unknown)}")

    link = root / "binary-link"
    link_frames = run_short_process(client, f"/bin/ln -s {root}/nested/binary.bin {link}", user=user)
    require(process_output(link_frames, "stderr") == b"", "symlink command failed")
    link_stat = client.unary("filesystem.Filesystem", "Stat", {"path": str(link)}, user=user)
    # Stat **跟随**符号链接，与参考实现一致：type 描述解析后的目标（utils.go 的
    # GetEntryInfo 用 os.Stat），permissions 描述链接自身（带 L 前缀），symlinkTarget
    # 给出解析后的绝对路径。实测 /usr/bin/envd-go 对指向文件的链接返回
    # FILE_TYPE_FILE、指向目录的返回 FILE_TYPE_DIRECTORY——断言 FILE_TYPE_SYMLINK
    # 会与基线冲突（本用例此前就是这么写的）。
    link_entry = link_stat.get("entry", {})
    require(
        link_entry.get("type") == "FILE_TYPE_FILE"
        and str(link_entry.get("permissions", "")).startswith("L")
        and link_entry.get("symlinkTarget") == f"{root}/nested/binary.bin",
        f"symlink Stat must follow the link like the baseline: {link_stat}",
    )

    watch_root = root / "watch"
    client.unary("filesystem.Filesystem", "MakeDir", {"path": str(watch_root / "child")}, user=user)
    watch_response, watch_reader = client.stream(
        "filesystem.Filesystem",
        "WatchDir",
        {"path": str(watch_root), "recursive": True, "includeEntry": True},
        user=user,
    )
    try:
        start = watch_reader.next()
        require(start.payload == {"start": {}}, f"WatchDir start mismatch: {start.payload}")
        write = client.request(
            "POST",
            "/files",
            headers={"Content-Type": "application/octet-stream"},
            params={"path": str(watch_root / "child" / "event.txt"), "username": user},
            data=b"watch event",
        )
        require(write.status_code == 200, response_summary(write))
        observed = False
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            frame = watch_reader.next()
            event = frame.payload.get("filesystem", {})
            if event.get("name", "").endswith("event.txt"):
                observed = True
                break
        require(observed, "WatchDir did not observe nested file write")
    finally:
        watch_response.close()

    client.unary("filesystem.Filesystem", "Remove", {"path": str(root)}, user=user)
    removed = client.request("GET", "/files", params={"path": str(root / "moved.txt"), "username": user})
    require(removed.status_code == 404, f"recursive Remove did not remove tree: {response_summary(removed)}")


def test_protocol_errors_and_logging(client: ProxyClient, user: str, prefix: str, expected_version: str) -> None:
    """验证媒体类型、未实现 RPC、压缩帧、资源边界、版本和日志脱敏。"""
    bad_media = client.request(
        "POST",
        "/filesystem.Filesystem/Stat",
        headers={"Content-Type": CONNECT_CONTENT_TYPE, "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION},
        data=b"{}",
    )
    require(bad_media.status_code == 415, f"unary content type mismatch: {response_summary(bad_media)}")
    unimplemented = client.request(
        "POST",
        "/filesystem.Filesystem/CreateWatcher",
        headers={"Content-Type": "application/json", "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION},
        data=json.dumps({"path": "/tmp"}),
    )
    require(unimplemented.status_code == 501 and unimplemented.json().get("code") == "unimplemented", response_summary(unimplemented))
    compressed = client.request(
        "POST",
        "/process.Process/Connect",
        headers={"Content-Type": CONNECT_CONTENT_TYPE, "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION, "Authorization": basic_auth(user)},
        data=encode_frame(0x01, b'{"process":{"pid":1}}'),
    )
    # 流式端点的**请求级**错误在流内报告（HTTP 200 + EndStream 错误帧），这是参考实现
    # connect-go 的行为：只有媒体类型不匹配才是 HTTP 415。此前这里断言 501 + JSON。
    require(compressed.status_code == 200, response_summary(compressed))
    require(
        compressed.headers.get("content-type", "").startswith("application/connect+"),
        response_summary(compressed),
    )
    compressed_frames = decode_frames(compressed.content)
    require(
        len(compressed_frames) == 1
        and compressed_frames[0][0] & END_STREAM_FLAG
        and json.loads(compressed_frames[0][1]).get("error", {}).get("code") == "unimplemented",
        f"compressed frame must fail in-band: {compressed_frames!r}",
    )

    oversized = client.request(
        "POST",
        "/process.Process/Connect",
        headers={"Content-Type": CONNECT_CONTENT_TYPE, "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION, "Authorization": basic_auth(user)},
        # 只发 5 字节帧头、声明一个超限长度：cube-envd 在分配载荷缓冲之前就按声明长度
        # 拒绝（这正是该上限的意义），因此不必真的推 64 MiB 过 CubeProxy——那样只会撞上
        # 客户端超时，测到的是网络而不是协议。
        data=bytes([0]) + struct.pack(">I", MAX_FRAME_BYTES + 1),
    )
    require(oversized.status_code == 200, response_summary(oversized))
    oversized_frames = decode_frames(oversized.content)
    require(
        len(oversized_frames) == 1
        and oversized_frames[0][0] & END_STREAM_FLAG
        and json.loads(oversized_frames[0][1]).get("error", {}).get("code") == "resource_exhausted",
        f"oversized frame must fail in-band: {oversized_frames!r}",
    )

    version_frames = run_short_process(client, "/usr/bin/envd -version", user=user)
    require(process_output(version_frames, "stdout").decode().strip() == expected_version, "envd version mismatch in sandbox")
    secret = f"{prefix}-secret-{uuid.uuid4().hex}"
    secret_frames = run_short_process(client, f"printf '%s' '{secret}'", user=user)
    require(process_output(secret_frames, "stdout").decode() == secret, "secret command did not execute")
    log = client.request("GET", "/files", params={"path": "/var/log/envd.log", "username": "root"})
    require(log.status_code == 200, response_summary(log))
    require(secret.encode() not in log.content, "envd log contains command output/content")


def parse_args() -> argparse.Namespace:
    """解析运行 harness 所需的远端控制面和镜像元数据参数。"""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-url", default="http://127.0.0.1:3000")
    parser.add_argument("--proxy-url", default="http://127.0.0.1")
    parser.add_argument("--domain", default="cube.app")
    parser.add_argument("--template-id", required=True)
    parser.add_argument("--envd-version", required=True)
    return parser.parse_args()


def main() -> None:
    """创建私有测试 sandbox，执行所有原始协议断言并始终清理。"""
    args = parse_args()
    prefix = f"cube-envd-e2e-{uuid.uuid4().hex[:10]}"
    sandbox_id, token = create_sandbox(args.api_url, args.template_id)
    client = ProxyClient(sandbox_id, token, args.proxy_url, args.domain)
    try:
        wait_for_health(client)
        test_proxy_access_control(client)
        test_init_and_process(client, "user", prefix)
        test_filesystem(client, "user", prefix)
        test_protocol_errors_and_logging(client, "user", prefix, args.envd_version)
    finally:
        delete_sandbox(args.api_url, sandbox_id)
    print("raw CubeProxy Connect-JSON E2E: PASS")


if __name__ == "__main__":
    main()
