# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Pseudo-terminal (PTY) interface for CubeSandbox.

Mirrors the E2B SDK's ``sandbox.pty`` API but speaks envd's Connect-JSON RPC
directly over httpx — no dependency on the ``e2b`` / ``e2b_connect`` Python
packages. The on-the-wire encoding is the same one used by
:func:`cubesandbox._commands._run_with_connect_fallback`: each Connect frame
is a 5-byte envelope (1 flags byte + big-endian uint32 length) followed by a
JSON body, and protobuf ``bytes`` fields are base64 strings on the JSON side.
"""

from __future__ import annotations

import base64
import json
import struct
from dataclasses import dataclass
from typing import TYPE_CHECKING, Callable, Generator, Iterable, Optional, Union

from ._commands import (
    CONNECT_COMPRESSED_FLAG,
    CONNECT_CONTENT_TYPE,
    CONNECT_END_STREAM_FLAG,
    CONNECT_PROTOCOL_VERSION,
    DEFAULT_ENVD_USER,
    ENVD_PORT,
    MAX_CONNECT_ENVELOPE_SIZE,
    _encode_connect_envelope,
    _exit_code_from_status,
    _http_error_detail,
    _raise_connect_end_stream,
    _user_headers,
)
from ._exceptions import CubeSandboxError

if TYPE_CHECKING:
    from .sandbox import Sandbox


PtyOutput = bytes
"""Raw bytes streamed from the PTY master side."""


@dataclass
class PtySize:
    """Pseudo-terminal size."""

    rows: int
    """Number of rows."""
    cols: int
    """Number of columns."""


# Connect-JSON Signal enum values. The wire format uses string names rather
# than the integer protobuf numbers.
_SIGNAL_SIGKILL = "SIGNAL_SIGKILL"


class PtyHandle:
    """Handle to a running PTY.

    Iterating over the handle yields :data:`PtyOutput` chunks until the PTY
    process exits or the caller calls :meth:`disconnect`. Use :meth:`wait` to
    drain the stream with optional callbacks, or read chunks directly via the
    iterator protocol when integrating with a UI / event loop.
    """

    def __init__(
        self,
        pid: int,
        events: Generator[dict, None, None],
        handle_kill: Callable[[], bool],
        handle_send_stdin: Callable[[bytes, Optional[float]], None],
        handle_resize: Callable[["PtySize", Optional[float]], None],
    ) -> None:
        self._pid = pid
        self._events = events
        self._handle_kill = handle_kill
        self._handle_send_stdin = handle_send_stdin
        self._handle_resize = handle_resize
        self._exit_code: Optional[int] = None
        self._error: Optional[str] = None
        self._exited: bool = False

    @property
    def pid(self) -> int:
        """PTY process ID."""
        return self._pid

    @property
    def exit_code(self) -> Optional[int]:
        """Exit code once the PTY process has finished, otherwise ``None``."""
        return self._exit_code

    @property
    def error(self) -> Optional[str]:
        """Error message reported by envd for the PTY, if any."""
        return self._error

    def __iter__(self) -> Generator[PtyOutput, None, None]:
        return self._handle_events()

    def _handle_events(self) -> Generator[PtyOutput, None, None]:
        try:
            for event in self._events:
                # ``event`` is the parsed ``event`` field of a ProcessEvent JSON
                # message; data/end live one level down.
                data = event.get("data") or {}
                if data.get("pty"):
                    yield base64.b64decode(data["pty"])
                end = event.get("end")
                if end is not None:
                    self._exit_code = _extract_exit_code(end)
                    self._error = end.get("error") or None
                    self._exited = True
        finally:
            try:
                self._events.close()
            except Exception:
                pass

    def wait(
        self,
        on_data: Optional[Callable[[PtyOutput], None]] = None,
    ) -> int:
        """Block until the PTY exits and return its exit code.

        :param on_data: Callback invoked with each PTY output chunk.

        :raises CubeSandboxError: If the PTY stream ends without an end event,
            or if envd reports an error.
        :return: Exit code of the PTY process.
        """
        for chunk in self:
            if on_data is not None:
                on_data(chunk)

        if not self._exited:
            raise CubeSandboxError("PTY stream ended without an end event")
        if self._error:
            raise CubeSandboxError(f"PTY exited with error: {self._error}")
        return int(self._exit_code or 0)

    def disconnect(self) -> None:
        """Stop receiving events from the PTY without killing it.

        The PTY keeps running inside the sandbox; reconnect later via
        :meth:`Pty.connect`.
        """
        try:
            self._events.close()
        except Exception:
            pass

    def kill(self) -> bool:
        """Send ``SIGKILL`` to the PTY process.

        :return: ``True`` if the PTY was killed, ``False`` if it could not be
            found (e.g. already exited).
        """
        return self._handle_kill()

    def send_stdin(
        self,
        data: Union[str, bytes],
        request_timeout: Optional[float] = None,
    ) -> None:
        """Send input to the PTY master side.

        :param data: Bytes (or UTF-8 string) to write to the PTY.
        :param request_timeout: Per-request timeout in seconds.
        """
        if isinstance(data, str):
            data = data.encode("utf-8")
        self._handle_send_stdin(data, request_timeout)

    def resize(
        self,
        size: PtySize,
        request_timeout: Optional[float] = None,
    ) -> None:
        """Resize the PTY window."""
        self._handle_resize(size, request_timeout)


class Pty:
    """Module for interacting with PTYs (pseudo-terminals) in the sandbox.

    Mirrors the E2B SDK's ``sandbox.pty`` namespace: ``create`` to start a new
    interactive shell, ``connect`` to reattach to an existing one, plus
    ``kill``/``send_stdin``/``resize`` for ad-hoc control without holding a
    :class:`PtyHandle`.
    """

    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox

    # --- HTTP plumbing ------------------------------------------------

    def _client(self):
        if self._sandbox._client is None:
            self._sandbox._client = self._sandbox._build_data_client()
        return self._sandbox._client

    def _build_headers(
        self,
        *,
        streaming: bool,
        user: Optional[str] = None,
        timeout: Optional[float] = None,
    ) -> dict:
        # The Connect protocol uses two different wire formats. Unary calls
        # are plain JSON over HTTP (Content-Type: application/json, no
        # 5-byte envelope), and streaming calls use the framed
        # application/connect+json format. envd enforces this strictly: it
        # answers a unary endpoint with HTTP 415 if you send the streaming
        # content type, and the Accept-Post header on that 415 response
        # explicitly lists application/json as the unary option.
        if streaming:
            headers = {
                "Content-Type": CONNECT_CONTENT_TYPE,
                "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
                "Connect-Content-Encoding": "identity",
            }
        else:
            headers = {
                "Content-Type": "application/json",
                "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
            }
        if timeout is not None and timeout > 0:
            headers["Connect-Timeout-Ms"] = str(int(timeout * 1000))
        access_token = self._sandbox._data.get("envdAccessToken")
        if access_token:
            headers["X-Access-Token"] = access_token
        if user:
            headers.update(_user_headers(user))
        return headers

    def _url(self, method: str) -> str:
        return f"http://{self._sandbox.get_host(ENVD_PORT)}/process.Process/{method}"

    def _unary(
        self,
        method: str,
        payload: dict,
        *,
        user: Optional[str] = None,
        request_timeout: Optional[float] = None,
        allow_not_found: bool = False,
    ) -> Optional[dict]:
        """Send a unary Connect-JSON request and return the parsed response.

        Connect's unary wire format is just ``application/json`` over POST
        — no 5-byte envelope, no end-stream frame, response is the bare
        JSON message body.

        :param allow_not_found: If True, return ``None`` on HTTP 404 / Connect
            ``not_found`` instead of raising. Used by :meth:`kill` so callers
            can distinguish "killed" from "didn't exist".
        """
        client = self._client()
        headers = self._build_headers(
            streaming=False, user=user, timeout=request_timeout
        )

        resp = client.post(
            self._url(method),
            content=json.dumps(payload).encode("utf-8"),
            headers=headers,
            timeout=request_timeout,
        )
        if resp.status_code >= 400:
            if allow_not_found and _is_not_found(resp):
                return None
            detail = _http_error_detail(resp)
            suffix = f": {detail}" if detail else ""
            raise CubeSandboxError(
                f"{method} failed: HTTP {resp.status_code}{suffix}",
                resp.status_code,
            )

        raw = resp.content
        if not raw:
            return {}
        try:
            return json.loads(raw.decode("utf-8"))
        except json.JSONDecodeError as exc:
            raise CubeSandboxError(f"{method}: invalid JSON response: {exc}")

    # --- Selector RPCs ------------------------------------------------

    def kill(
        self,
        pid: int,
        request_timeout: Optional[float] = None,
    ) -> bool:
        """Send ``SIGKILL`` to a PTY process.

        :return: ``True`` if the PTY was killed, ``False`` if not found.
        """
        result = self._unary(
            "SendSignal",
            {
                "process": {"pid": pid},
                "signal": _SIGNAL_SIGKILL,
            },
            request_timeout=request_timeout,
            allow_not_found=True,
        )
        return result is not None

    def send_stdin(
        self,
        pid: int,
        data: Union[str, bytes],
        request_timeout: Optional[float] = None,
    ) -> None:
        """Send input to a PTY identified by *pid*."""
        if isinstance(data, str):
            data = data.encode("utf-8")
        self._unary(
            "SendInput",
            {
                "process": {"pid": pid},
                "input": {"pty": base64.b64encode(data).decode("ascii")},
            },
            request_timeout=request_timeout,
        )

    def resize(
        self,
        pid: int,
        size: PtySize,
        request_timeout: Optional[float] = None,
    ) -> None:
        """Resize a running PTY."""
        self._unary(
            "Update",
            {
                "process": {"pid": pid},
                "pty": {"size": {"rows": size.rows, "cols": size.cols}},
            },
            request_timeout=request_timeout,
        )

    # --- Streaming RPCs -----------------------------------------------

    def create(
        self,
        size: PtySize,
        *,
        user: Optional[str] = None,
        cwd: Optional[str] = None,
        envs: Optional[dict] = None,
        timeout: Optional[float] = 60,
        request_timeout: Optional[float] = None,
    ) -> PtyHandle:
        """Start a new PTY running an interactive login bash shell."""
        envs = dict(envs) if envs else {}
        envs.setdefault("TERM", "xterm-256color")
        envs.setdefault("LANG", "C.UTF-8")
        envs.setdefault("LC_ALL", "C.UTF-8")
        effective_user = user or DEFAULT_ENVD_USER

        payload = {
            "process": {
                "cmd": "/bin/bash",
                "args": ["-i", "-l"],
                "envs": envs,
            },
            "pty": {"size": {"rows": size.rows, "cols": size.cols}},
        }
        if cwd:
            payload["process"]["cwd"] = cwd

        return self._open_stream(
            "Start",
            payload,
            user=effective_user,
            timeout=timeout,
            request_timeout=request_timeout,
        )

    def connect(
        self,
        pid: int,
        *,
        timeout: Optional[float] = 60,
        request_timeout: Optional[float] = None,
    ) -> PtyHandle:
        """Reattach to an already-running PTY."""
        return self._open_stream(
            "Connect",
            {"process": {"pid": pid}},
            timeout=timeout,
            request_timeout=request_timeout,
        )

    def _open_stream(
        self,
        method: str,
        payload: dict,
        *,
        user: Optional[str] = None,
        timeout: Optional[float] = None,
        request_timeout: Optional[float] = None,
    ) -> PtyHandle:
        client = self._client()
        headers = self._build_headers(streaming=True, user=user, timeout=timeout)
        body = _encode_connect_envelope(json.dumps(payload).encode("utf-8"))

        # Open a streaming POST; httpx returns a context-managed response. We
        # cannot use ``with`` here because the stream's lifetime is the PTY's
        # lifetime — close happens in PtyHandle.disconnect / wait.
        resp_ctx = client.stream(
            "POST",
            self._url(method),
            content=body,
            headers=headers,
            timeout=timeout,
        )
        resp = resp_ctx.__enter__()
        try:
            if resp.status_code >= 400:
                detail = _http_error_detail(resp)
                suffix = f": {detail}" if detail else ""
                raise CubeSandboxError(
                    f"{method} failed: HTTP {resp.status_code}{suffix}",
                    resp.status_code,
                )

            event_iter = _iter_connect_events(resp.iter_raw(), resp_ctx)
            try:
                start_event = next(event_iter)
            except StopIteration:
                raise CubeSandboxError(f"{method}: stream closed before start event")

            start = (start_event or {}).get("start")
            if not start or "pid" not in start:
                raise CubeSandboxError(
                    f"{method}: expected start event, got {start_event!r}"
                )
            pid = int(start["pid"])
        except BaseException:
            try:
                resp_ctx.__exit__(None, None, None)
            except Exception:
                pass
            raise

        return PtyHandle(
            pid=pid,
            events=event_iter,
            handle_kill=lambda: self.kill(pid),
            handle_send_stdin=lambda data, rt: self.send_stdin(pid, data, rt),
            handle_resize=lambda size, rt: self.resize(pid, size, rt),
        )


# --- Connect-JSON stream parsing ---------------------------------------

def _iter_connect_events(
    chunks: Iterable[bytes],
    resp_ctx,
) -> Generator[dict, None, None]:
    """Yield the ``event`` field of each ProcessEvent JSON message.

    Closes ``resp_ctx`` (the httpx streaming context) when the generator is
    closed or exhausted, mirroring how ``_parse_process_start_stream`` is
    used in :mod:`cubesandbox._commands` but adapted for streaming consumers.
    """
    buffer = bytearray()
    try:
        for chunk in chunks:
            if not chunk:
                continue
            buffer.extend(chunk)
            while len(buffer) >= 5:
                flags = buffer[0]
                size = struct.unpack(">I", buffer[1:5])[0]
                if size > MAX_CONNECT_ENVELOPE_SIZE:
                    raise CubeSandboxError(
                        f"Connect stream message too large: {size} bytes"
                    )
                if len(buffer) < 5 + size:
                    break

                raw = bytes(buffer[5 : 5 + size])
                del buffer[: 5 + size]

                if flags & CONNECT_COMPRESSED_FLAG:
                    raise CubeSandboxError(
                        "unsupported compressed Connect stream message"
                    )
                if flags & CONNECT_END_STREAM_FLAG:
                    # Raises if the trailer carries an error; otherwise the
                    # stream is just done.
                    _raise_connect_end_stream(raw)
                    return

                message = json.loads(raw.decode("utf-8"))
                event = message.get("event")
                if event is not None:
                    yield event

        if buffer:
            raise CubeSandboxError("Connect stream ended with a partial message")
    finally:
        try:
            resp_ctx.__exit__(None, None, None)
        except Exception:
            pass


def _extract_exit_code(end: dict) -> Optional[int]:
    """Best-effort exit-code extraction from an end event.

    envd has historically used both ``exitCode`` and ``exit_code`` in
    Connect-JSON responses, plus a free-form ``status`` string for older
    builds. Try them in priority order so we stay compatible with whatever
    the sandbox happens to emit.
    """
    if "exitCode" in end:
        return int(end["exitCode"])
    if "exit_code" in end:
        return int(end["exit_code"])
    parsed = _exit_code_from_status(end.get("status"))
    if parsed is not None:
        return parsed
    if end.get("exited"):
        return 0
    return None


def _is_not_found(resp) -> bool:
    """Detect Connect's ``not_found`` error code on a unary response.

    Matches both the HTTP-level 404 and the Connect-JSON error body envd
    sends when a PID has already exited (``{"code":"not_found", ...}``).
    """
    if resp.status_code == 404:
        return True
    try:
        body = resp.json()
    except Exception:
        return False
    if isinstance(body, dict):
        code = body.get("code")
        if isinstance(code, str) and code.lower() == "not_found":
            return True
    return False
