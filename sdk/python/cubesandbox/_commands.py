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
    from .sandbox import Sandbox


ENVD_PORT = 49983
CONNECT_PROTOCOL_VERSION = "1"
CONNECT_CONTENT_TYPE = "application/connect+json"
CONNECT_END_STREAM_FLAG = 0x02
CONNECT_COMPRESSED_FLAG = 0x01
MAX_CONNECT_ENVELOPE_SIZE = 64 * 1024 * 1024
DEFAULT_ENVD_USER = "root"


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

        Uses the handwritten Connect RPC path — no dependency on the E2B
        protocol package.

        Args:
            env: Alias for envs, matching the E2B SDK command API.
            user: Sandbox user for envd process auth. Defaults to ``"root"``
                to keep old envd versions from rejecting requests with
                ``"no user specified"``.
        """
        process_envs = envs if envs is not None else (env or {})
        effective_user = user or DEFAULT_ENVD_USER
        return self._run(
            cmd,
            timeout=timeout,
            cwd=cwd,
            envs=process_envs,
            user=effective_user,
        )

    def _run(
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
            },
            "stdin": False,
        }
        # Default to / — the user's home directory may not exist (e.g.
        # nobody → /nonexistent on some images), which causes envd to
        # reject the request with "cwd does not exist".
        payload["process"]["cwd"] = cwd or "/"

        headers = {
            "Content-Type": CONNECT_CONTENT_TYPE,
            "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
            "Connect-Content-Encoding": "identity",
        }
        if timeout is not None:
            headers["Connect-Timeout-Ms"] = str(int(timeout * 1000))
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
            timeout=timeout,
        ) as resp:
            if resp.status_code >= 400:
                detail = _http_error_detail(resp)
                suffix = f": {detail}" if detail else ""
                raise RuntimeError(f"command failed: HTTP {resp.status_code}{suffix}")
            return _parse_process_start_stream(resp.iter_raw())


def _parse_process_start_stream(chunks) -> CommandResult:
    stdout: list[str] = []
    stderr: list[str] = []
    exit_code: int | None = None
    buffer = bytearray()

    for chunk in chunks:
        if not chunk:
            continue
        buffer.extend(chunk)
        while len(buffer) >= 5:
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
                stdout.append(_decode_process_bytes(data["stdout"]))
            if data.get("stderr"):
                stderr.append(_decode_process_bytes(data["stderr"]))
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

    return CommandResult(stdout="".join(stdout), stderr="".join(stderr), exit_code=exit_code)


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
    raw = resp.read()
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


def _decode_process_bytes(value: str) -> str:
    return base64.b64decode(value).decode("utf-8", "replace")


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
