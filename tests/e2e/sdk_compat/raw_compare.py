"""通过 CubeProxy 比较旧 E2B envd 和 Rust cube-envd 的临时运行器。"""

from __future__ import annotations

import argparse
import base64
import json
import struct
import time
import uuid
from typing import Any

import requests


END_STREAM_FLAG = 0x02


# 将一个 JSON 载荷封装为 Connect 协议帧。
def encode_frame(payload: dict[str, Any], *, flags: int = 0) -> bytes:
    encoded = json.dumps(payload, separators=(",", ":")).encode()
    return bytes([flags]) + struct.pack(">I", len(encoded)) + encoded


# 解析连续 Connect 帧，并将结束帧明确标记为 end。
def decode_frames(payload: bytes) -> list[dict[str, Any]]:
    frames: list[dict[str, Any]] = []
    offset = 0
    while offset < len(payload):
        if len(payload) - offset < 5:
            raise ValueError("truncated Connect frame header")
        flags = payload[offset]
        length = struct.unpack(">I", payload[offset + 1 : offset + 5])[0]
        offset += 5
        if len(payload) - offset < length:
            raise ValueError("truncated Connect frame payload")
        body = json.loads(payload[offset : offset + length])
        offset += length
        frames.append({"end": body} if flags == END_STREAM_FLAG else body)
    return frames


# 从长连接响应中读取指定长度的原始字节，处理底层分块读取。
def read_exact(raw: Any, size: int) -> bytes:
    chunks: list[bytes] = []
    remaining = size
    while remaining:
        chunk = raw.read(remaining)
        if not chunk:
            raise ValueError("unexpected EOF while reading Connect frame")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


# 从流式 HTTP 响应中读取并解码下一条 Connect 帧。
def read_stream_frame(response: requests.Response) -> dict[str, Any]:
    header = read_exact(response.raw, 5)
    flags = header[0]
    size = struct.unpack(">I", header[1:])[0]
    body = json.loads(read_exact(response.raw, size))
    return {"end": body} if flags == END_STREAM_FLAG else body


# 将 base64 编码的进程输出转换成便于旧新比较的 UTF-8 文本。
def decode_output(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    try:
        return base64.b64decode(value).decode("utf-8", errors="replace")
    except ValueError:
        return value


# 从进程 Connect 帧中抽取 stdout、stderr、PTY 输出和结束事件。
def summarize_process_frames(frames: list[dict[str, Any]]) -> dict[str, Any]:
    summary: dict[str, Any] = {
        "stdout": "",
        "stderr": "",
        "pty": "",
        "start_pid": None,
        "end": None,
        "end_stream": None,
    }
    for frame in frames:
        if "end" in frame:
            summary["end_stream"] = frame["end"]
            continue
        event = frame.get("event", {})
        start = event.get("start", {})
        if isinstance(start, dict) and isinstance(start.get("pid"), int):
            summary["start_pid"] = start["pid"]
        data = event.get("data", {})
        if isinstance(data, dict):
            summary["stdout"] += decode_output(data.get("stdout"))
            summary["stderr"] += decode_output(data.get("stderr"))
            summary["pty"] += decode_output(data.get("pty"))
        if isinstance(event.get("end"), dict):
            summary["end"] = event["end"]
    return summary


# 安全地读取 JSON 响应，保留非 JSON 错误体的前缀。
def response_body(response: requests.Response) -> Any:
    if not response.content:
        return {}
    try:
        return response.json()
    except ValueError:
        return response.text[:512]


# 封装一个通过 CubeProxy 发送的 HTTP 请求，并固定虚拟 Host 与 token。
class ProxyClient:
    """访问单个私有 sandbox 的原始 HTTP/Connect 客户端。"""

    # 初始化 CubeProxy 地址、sandbox 身份和两种兼容 token header。
    def __init__(self, proxy_url: str, sandbox_id: str, domain: str, token: str) -> None:
        self._proxy_url = proxy_url.rstrip("/")
        self._host = f"49983-{sandbox_id}.{domain}"
        self._token = token
        self._session = requests.Session()

    # 返回请求所需的虚拟 Host、可选 Basic 用户和可选 traffic token。
    def headers(
        self,
        *,
        content_type: str | None = None,
        token_header: str | None = "cube-traffic-access-token",
        basic: bool = False,
        extra: dict[str, str] | None = None,
    ) -> dict[str, str]:
        headers = {"Host": self._host}
        if content_type:
            headers["Content-Type"] = content_type
        if token_header:
            headers[token_header] = self._token if token_header != "invalid" else "invalid-token"
        if basic:
            headers["Authorization"] = "Basic cm9vdDo="
        if extra:
            headers.update(extra)
        return headers

    # 发送普通 HTTP 请求并返回完整响应。
    def request(
        self,
        method: str,
        path: str,
        *,
        headers: dict[str, str],
        timeout: tuple[float, float] = (15, 30),
        **kwargs: Any,
    ) -> requests.Response:
        return self._session.request(
            method,
            f"{self._proxy_url}{path}",
            headers=headers,
            timeout=timeout,
            **kwargs,
        )

    # 发送一元 JSON Connect RPC，并返回状态与 JSON 或文本错误体。
    def unary(
        self,
        service: str,
        method: str,
        payload: dict[str, Any],
    ) -> tuple[int, Any]:
        response = self.request(
            "POST",
            f"/{service}/{method}",
            headers=self.headers(
                content_type="application/json",
                basic=True,
                extra={"Connect-Protocol-Version": "1"},
            ),
            json=payload,
        )
        return response.status_code, response_body(response)

    # 打开一个服务端流式 Connect RPC，调用方负责读取并关闭响应。
    def open_stream(
        self,
        service: str,
        method: str,
        payload: dict[str, Any],
        *,
        extra_headers: dict[str, str] | None = None,
    ) -> requests.Response:
        headers = self.headers(
            content_type="application/connect+json",
            basic=True,
            extra={"Connect-Protocol-Version": "1", **(extra_headers or {})},
        )
        return self.request(
            "POST",
            f"/{service}/{method}",
            headers=headers,
            data=encode_frame(payload),
            stream=True,
            timeout=(15, 45),
        )

    # 启动一个有限时进程并读取所有 Connect 帧。
    def run_process(
        self,
        command: str,
        args: list[str],
        *,
        tag: str | None = None,
        extra_headers: dict[str, str] | None = None,
        pty: bool = False,
    ) -> tuple[int, list[dict[str, Any]]]:
        payload: dict[str, Any] = {
            "process": {"cmd": command, "args": args, "envs": {}},
            "stdin": False,
        }
        if tag:
            payload["tag"] = tag
        if pty:
            payload["pty"] = {"size": {"cols": 80, "rows": 24}}
        response = self.open_stream(
            "process.Process",
            "Start",
            payload,
            extra_headers=extra_headers,
        )
        try:
            return response.status_code, decode_frames(response.content)
        finally:
            response.close()


# 通过 CubeAPI 创建私有 sandbox，并返回创建响应中的身份和 traffic token。
def create_private_sandbox(api_url: str, template_id: str) -> dict[str, Any]:
    payload = {
        "templateID": template_id,
        "timeout": 180,
        "metadata": {"test_suite": "envd_raw_compare", "run_id": uuid.uuid4().hex},
        "network": {"allowPublicTraffic": False},
    }
    response = requests.post(
        f"{api_url.rstrip('/')}/sandboxes",
        json=payload,
        headers={"Content-Type": "application/json"},
        timeout=(15, 120),
    )
    response.raise_for_status()
    body = response.json()
    sandbox_id = body.get("sandboxID")
    token = body.get("trafficAccessToken")
    if not isinstance(sandbox_id, str) or not isinstance(token, str):
        raise RuntimeError(f"private sandbox create response lacks id/token: {body!r}")
    return body


# 等待通过 CubeProxy 的带 token 健康检查成功，确保 guest envd 已可接收请求。
def wait_for_envd(client: ProxyClient) -> int:
    last_status = 0
    for _ in range(60):
        response = client.request("GET", "/health", headers=client.headers())
        last_status = response.status_code
        response.close()
        if 200 <= last_status < 300:
            return last_status
        time.sleep(1)
    raise RuntimeError(f"envd did not become healthy, last status {last_status}")


# 删除测试创建的 sandbox，删除失败仅记录在结果中而不覆盖原始异常。
def delete_sandbox(api_url: str, sandbox_id: str) -> str | None:
    try:
        response = requests.delete(f"{api_url.rstrip('/')}/sandboxes/{sandbox_id}", timeout=(15, 60))
        if response.status_code not in (200, 202, 204, 404):
            return f"HTTP {response.status_code}: {response.text[:256]}"
    except requests.RequestException as exc:
        return str(exc)
    return None
