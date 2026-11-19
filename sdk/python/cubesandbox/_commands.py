# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import base64
import json
import re
import struct
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    import httpx

    from .sandbox import Sandbox


ENVD_PORT = 49983
CONNECT_PROTOCOL_VERSION = "1"
CONNECT_CONTENT_TYPE = "application/connect+json"
CONNECT_END_STREAM_FLAG = 0x02
CONNECT_COMPRESSED_FLAG = 0x01
MAX_CONNECT_ENVELOPE_SIZE = 64 * 1024 * 1024
DEFAULT_ENVD_USER = "root"
_CONNECT_KNOWN_FLAGS = CONNECT_COMPRESSED_FLAG | CONNECT_END_STREAM_FLAG
# Hints shared with the Go SDK (sdk/go/connect.go). The routing hint is for
# HTML pages; a body that is not Connect-framed can also be a compressed
# response, so its hint names that cause too.
_ROUTING_HINT = (
    "check whether CUBE_PROXY_NODE_IP is configured or if the data-plane endpoint "
    "points to a web service"
)
_NOT_AN_ENVELOPE_HINT = (
    "check whether CUBE_PROXY_NODE_IP is configured, the data-plane endpoint "
    "points to a web service, or the response was compressed"
)
# Bounds for reading an HTTP error page on a Connect stream request.
_MAX_ROUTING_PAGE_PEEK_BYTES = 4096
_MAX_DIAGNOSTIC_LEN = 200


def _connect_header_error(header: bytes | bytearray) -> str | None:
    """Return a diagnostic when ``header`` cannot start a Connect envelope.

    A misrouted request can get an HTML page, JSON or a gzip body back with
    HTTP 200. Read as an envelope, ``<!DOCTYPE`` becomes flags ``0x3c`` and a
    length of 558124867, which was reported as an oversized message. Reject
    unknown flag bits before the length is interpreted, as the Go SDK does.
    """
    if header[0] & ~_CONNECT_KNOWN_FLAGS:
        return (
            f"response is not a Connect envelope (first 5 bytes: {bytes(header[:5]).hex()}); "
            f"{_NOT_AN_ENVELOPE_HINT}"
        )
    return None


def _media_type(resp: httpx.Response) -> str:
    return resp.headers.get("content-type", "").split(";", 1)[0].strip().lower()


def _html_response_error(resp: httpx.Response) -> str | None:
    """Return a diagnostic for an HTTP 200 ``text/html`` response.

    A Connect stream is never labelled ``text/html``; such a response means
    the request reached a Web UI or portal page instead of CubeProxy. The
    body is not read.
    """
    if resp.status_code != 200 or _media_type(resp) != "text/html":
        return None
    content_type = resp.headers.get("content-type", "")
    return (
        f'received HTML instead of a Connect response (content-type "{content_type}"); '
        f"{_ROUTING_HINT}"
    )


def _is_routing_page(media_type: str, body: bytes) -> bool:
    """Whether an HTTP error body looks like a Web UI or gateway page.

    A JSON error document keeps the server's message even when it is
    labelled ``text/html``.
    """
    trimmed = body.strip()
    if trimmed.startswith(b"{"):
        return False
    head = trimmed[: len(b"<!doctype")].lower()
    return media_type == "text/html" or head.startswith((b"<!doctype", b"<html"))


def _routing_page_hint(status_code: int) -> str:
    if status_code in (401, 403):
        return (
            "received an HTML/text page instead of a Connect stream; check credentials "
            "or whether a portal is intercepting the request"
        )
    if status_code >= 500:
        return (
            "received an HTML error page instead of a Connect stream; the request may "
            "have reached CubeProxy but the upstream envd or sandbox node is unreachable"
        )
    return f"response may be an HTML/text page instead of a Connect stream; {_ROUTING_HINT}"


def _truncate_diagnostic(message: str) -> str:
    if len(message) <= _MAX_DIAGNOSTIC_LEN:
        return message
    return message[:_MAX_DIAGNOSTIC_LEN] + "..."


def _stream_http_error_detail(resp: httpx.Response) -> str:
    """Describe an HTTP error response to a Connect stream request.

    Like :func:`_http_error_detail`, but an HTML page (a misrouted request
    landing on a Web UI, gateway or portal) is reported from a bounded
    prefix of its body, followed by the same hint the Go SDK gives, instead
    of being returned in full.
    """
    chunks = resp.iter_bytes()
    peek = bytearray()
    for chunk in chunks:
        peek.extend(chunk)
        if len(peek) >= _MAX_ROUTING_PAGE_PEEK_BYTES:
            break
    if _is_routing_page(_media_type(resp), bytes(peek)):
        text = bytes(peek[:_MAX_ROUTING_PAGE_PEEK_BYTES]).decode("utf-8", "replace").strip()
        hint = _routing_page_hint(resp.status_code)
        return f"{_truncate_diagnostic(text)}; {hint}" if text else hint
    return _error_detail_from_body(bytes(peek) + b"".join(chunks))


@dataclass
class CommandResult:
    stdout: str
    stderr: str
    exit_code: int


class Commands:
    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox

    def run(
        self,
        cmd: str,
        *,
        timeout: float | None = None,
        cwd: str | None = None,
        envs: dict[str, str] | None = None,
        env: dict[str, str] | None = None,
        user: str | None = None,
        **kwargs,
    ) -> CommandResult:
        """Run a shell command inside the sandbox through envd's process API.

        The SDK always uses envd's Connect RPC endpoint directly so it does
        not rely on optional external protobuf/runtime packages.

        Args:
            env: Alias for envs, matching the E2B SDK command API.
            user: Sandbox user for envd process auth. Defaults to ``"root"``
                to keep old envd versions from rejecting requests with
                ``"no user specified"``.
        """
        process_envs = envs if envs is not None else (env or {})
        effective_user = user or DEFAULT_ENVD_USER
        return self._run_with_connect_api(
            cmd,
            timeout=timeout,
            cwd=cwd,
            envs=process_envs,
            user=effective_user,
        )

    def _run_with_connect_api(
        self,
        cmd: str,
        *,
        timeout: float | None,
        cwd: str | None,
        envs: dict[str, str],
        user: str | None,
    ) -> CommandResult:
        if self._sandbox._client is None:
            self._sandbox._client = self._sandbox._build_data_client()

        payload: dict = {
            "process": {
                "cmd": "/bin/bash",
                "args": ["-l", "-c", cmd],
                "envs": envs,
                "cwd": cwd or "",
            },
            "stdin": False,
        }

        headers = {
            "Content-Type": CONNECT_CONTENT_TYPE,
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
            "Connect-Content-Encoding": "identity",
        }
        # The timeout has two readers that spell "no deadline" differently, so
        # it is normalised once here.
        #
        # envd reads Connect-Timeout-Ms as a hard wall-clock deadline, so a zero
        # or negative one has already passed and the request never gets an
        # answer -- it hangs until the HTTP client gives up on headers, and
        # reports a transport error that says nothing about the cause. The
        # header is therefore omitted rather than sent as zero.
        #
        # httpx reads the same number as a client-side deadline, where only None
        # means "no deadline": 0 makes every socket operation time out at once,
        # and -1 is rejected outright. Forwarding the raw value would trade the
        # hang for an immediate failure, which is not what either value asks
        # for.
        #
        # Both non-positive values arrive in ordinary use: 0 is how the e2b SDK
        # spells "no deadline", and NEVER_TIMEOUT is how this one does. _pty.py
        # and the Go SDK already guard this way.
        effective_timeout = timeout if timeout is not None and timeout > 0 else None
        if effective_timeout is not None:
            headers["Connect-Timeout-Ms"] = str(int(effective_timeout * 1000))
        access_token = self._sandbox._data.get("envdAccessToken")
        if access_token:
            headers["X-Access-Token"] = access_token
        headers.update(_user_headers(user))

        url = f"http://{self._sandbox.get_host(ENVD_PORT)}/process.Process/Start"
        with self._sandbox._client.stream(
            "POST",
            url,
            content=_encode_connect_envelope(json.dumps(payload).encode("utf-8")),
            headers=headers,
            timeout=effective_timeout,
        ) as resp:
            if resp.status_code >= 400:
                detail = _stream_http_error_detail(resp)
                suffix = f": {detail}" if detail else ""
                raise RuntimeError(f"command failed: HTTP {resp.status_code}{suffix}")
            html_error = _html_response_error(resp)
            if html_error:
                raise RuntimeError(html_error)
            return _parse_process_start_stream(resp.iter_raw())


def _parse_process_start_stream(chunks) -> CommandResult:
    stdout: bytearray = bytearray()
    stderr: bytearray = bytearray()
    exit_code: int | None = None
    buffer = bytearray()

    for chunk in chunks:
        if not chunk:
            continue
        buffer.extend(chunk)
        while len(buffer) >= 5:
            header_error = _connect_header_error(buffer)
            if header_error:
                raise RuntimeError(header_error)
            flags = buffer[0]
            size = struct.unpack(">I", buffer[1:5])[0]
            if size > MAX_CONNECT_ENVELOPE_SIZE:
                raise RuntimeError(f"Connect stream message too large: {size} bytes")
            if len(buffer) < 5 + size:
                break

            raw = bytes(buffer[5 : 5 + size])
            del buffer[: 5 + size]

            if flags & CONNECT_COMPRESSED_FLAG:
                raise RuntimeError("unsupported compressed Connect stream message")
            if flags & CONNECT_END_STREAM_FLAG:
                _raise_connect_end_stream(raw)
                continue

            event = json.loads(raw.decode("utf-8")).get("event") or {}
            data = event.get("data") or {}
            if data.get("stdout"):
                stdout.extend(base64.b64decode(data["stdout"]))
            if data.get("stderr"):
                stderr.extend(base64.b64decode(data["stderr"]))
            end = event.get("end")
            if end is not None:
                if "exitCode" in end:
                    exit_code = int(end["exitCode"])
                elif "exit_code" in end:
                    exit_code = int(end["exit_code"])
                elif _exit_code_from_status(end.get("status")) is not None:
                    exit_code = _exit_code_from_status(end.get("status"))
                elif end.get("error"):
                    raise RuntimeError(f"process failed: {end['error']}")
                else:
                    raise RuntimeError("process EndEvent missing exit code")

    if buffer:
        raise RuntimeError("Connect stream ended with a partial message")
    if exit_code is None:
        raise RuntimeError("process stream ended without EndEvent")

    return CommandResult(
        stdout=stdout.decode("utf-8", "replace"),
        stderr=stderr.decode("utf-8", "replace"),
        exit_code=exit_code,
    )


def _encode_connect_envelope(data: bytes, flags: int = 0) -> bytes:
    return bytes([flags]) + struct.pack(">I", len(data)) + data


def _raise_connect_end_stream(raw: bytes) -> None:
    if not raw:
        return
    payload = json.loads(raw.decode("utf-8"))
    error = payload.get("error")
    if not error:
        return
    message = (error.get("message") or "Connect stream error").strip()
    code = error.get("code")
    if code:
        raise RuntimeError(f"{code}: {message}")
    raise RuntimeError(message)


def _http_error_detail(resp) -> str:
    # Only called on HTTP error responses. Reading the body here is safe because
    # successful command streams are parsed incrementally by iter_raw().
    return _error_detail_from_body(resp.read())


def _error_detail_from_body(raw: bytes) -> str:
    if not raw:
        return ""
    text = raw.decode("utf-8", "replace").strip()
    try:
        payload = json.loads(text)
    except Exception:
        return text
    if isinstance(payload, dict):
        message = payload.get("message")
        if isinstance(message, str) and message.strip():
            return message.strip()
        error = payload.get("error")
        if isinstance(error, dict):
            message = error.get("message")
            if isinstance(message, str) and message.strip():
                return message.strip()
    return text


def _exit_code_from_status(status: object) -> int | None:
    if not isinstance(status, str):
        return None
    match = re.search(r"(?:exit status|exited with code)\s+(-?\d+)", status)
    if match:
        return int(match.group(1))
    signal_match = re.search(r"(?:signal|terminated by signal)\s+(\d+)", status)
    if signal_match:
        return 128 + int(signal_match.group(1))
    if status == "exited":
        return 0
    return None


def _basic_auth_user(user: str) -> str:
    token = base64.b64encode(f"{user}:".encode("utf-8")).decode("utf-8")
    return f"Basic {token}"


def _user_headers(user: str | None) -> dict[str, str]:
    return {"Authorization": _basic_auth_user(user)} if user else {}
