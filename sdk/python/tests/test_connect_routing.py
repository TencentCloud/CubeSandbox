# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Misrouted Connect streams (HTML or other non-envelope bodies with HTTP 200)."""

from __future__ import annotations

import base64
import gzip
import json
from unittest.mock import patch

import httpx
import pytest
from test_sandbox import connect_envelope, make_sandbox

from cubesandbox import PtySize
from cubesandbox._exceptions import CubeSandboxError

HTML_PAGE = b"<!DOCTYPE html><html><body>Cube Sandbox Web UI</body></html>"


def _client(response: httpx.Response) -> httpx.Client:
    return httpx.Client(transport=httpx.MockTransport(lambda request: response))


def _html(content_type: str | None = "text/html; charset=utf-8") -> httpx.Response:
    headers = {"content-type": content_type} if content_type else {}
    return httpx.Response(200, headers=headers, stream=httpx.ByteStream(HTML_PAGE))


class _ChunkedStream(httpx.SyncByteStream):
    """Deliver ``data`` in small pieces so envelope headers span chunks."""

    def __init__(self, data: bytes, size: int = 3) -> None:
        self._data = data
        self._size = size

    def __iter__(self):
        for i in range(0, len(self._data), self._size):
            yield self._data[i : i + self._size]


def _stream(*frames: bytes, chunked: bool = True) -> httpx.Response:
    body = b"".join(frames)
    stream = _ChunkedStream(body) if chunked else httpx.ByteStream(body)
    return httpx.Response(200, headers={"content-type": "application/connect+json"}, stream=stream)


def _data_event(**fields: bytes) -> bytes:
    payload = {k: base64.b64encode(v).decode() for k, v in fields.items()}
    return connect_envelope(0, json.dumps({"event": {"data": payload}}))


def _pty_open(sb, method: str):
    if method == "create":
        return sb.pty.create(PtySize(rows=24, cols=80))
    return sb.pty.connect(123)


# -- valid Connect streams --------------------------------------------------


@pytest.mark.parametrize("chunked", [False, True])
def test_commands_run_parses_a_valid_multi_frame_stream(chunked):
    sb = make_sandbox()
    response = _stream(
        connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
        _data_event(stdout=b"hello\n"),
        _data_event(stderr=b"warn\n"),
        _data_event(stdout=b"world\n"),
        connect_envelope(0, '{"event":{"end":{"exitCode":3,"exited":true}}}'),
        connect_envelope(0x02, "{}"),
        chunked=chunked,
    )
    with patch.object(sb, "_build_data_client", return_value=_client(response)):
        result = sb.commands.run("echo hello")
    assert result.stdout == "hello\nworld\n"
    assert result.stderr == "warn\n"
    assert result.exit_code == 3


@pytest.mark.parametrize("method", ["create", "connect"])
@pytest.mark.parametrize("chunked", [False, True])
def test_pty_parses_a_valid_stream(method, chunked):
    sb = make_sandbox()
    response = _stream(
        connect_envelope(0, '{"event":{"start":{"pid":123}}}'),
        _data_event(pty=b"$ "),
        _data_event(pty=b"ls\r\n"),
        connect_envelope(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
        connect_envelope(0x02, "{}"),
        chunked=chunked,
    )
    with patch.object(sb, "_build_data_client", return_value=_client(response)):
        handle = _pty_open(sb, method)
        output = b"".join(handle)
    assert handle.pid == 123
    assert output == b"$ ls\r\n"
    assert handle.exit_code == 0


def test_header_error_rejects_unknown_flags_before_the_length():
    from cubesandbox._commands import _connect_header_error

    # "<!DOC" used to be read as flags 0x3c and a 558124867-byte length.
    message = _connect_header_error(HTML_PAGE[:5])
    assert message is not None
    assert "not a Connect envelope" in message
    assert HTML_PAGE[:5].hex() in message


@pytest.mark.parametrize(
    "body",
    [
        HTML_PAGE,
        b"\xef\xbb\xbf" + HTML_PAGE,  # UTF-8 BOM
        b"\n" + HTML_PAGE,
        b'{"error":{"code":"unavailable","message":"no upstream"}}',
        gzip.compress(b"payload"),
    ],
    ids=["html", "bom", "leading-newline", "json", "gzip"],
)
def test_header_error_rejects_non_envelope_prefixes(body):
    from cubesandbox._commands import _connect_header_error

    message = _connect_header_error(body[:5])
    assert message is not None
    assert body[:5].hex() in message


@pytest.mark.parametrize("flags", [0x00, 0x01, 0x02, 0x03])
def test_header_error_accepts_connect_flags(flags):
    from cubesandbox._commands import _connect_header_error

    assert _connect_header_error(connect_envelope(flags, "{}")) is None


def test_commands_run_reports_html_content_type():
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html())):
        with pytest.raises(RuntimeError, match="^command failed: received HTML instead of") as exc:
            sb.commands.run("echo hi")
    assert "CUBE_PROXY_NODE_IP" in str(exc.value)


@pytest.mark.parametrize("content_type", [None, "application/connect+json"])
def test_commands_run_reports_non_envelope_body(content_type):
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html(content_type))):
        with pytest.raises(RuntimeError, match="not a Connect envelope") as exc:
            sb.commands.run("echo hi")
    assert "too large" not in str(exc.value)


def test_watch_dir_reports_html_content_type():
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html())):
        with pytest.raises(IOError, match="WatchDir failed: received HTML"):
            sb.files.watch_dir("/tmp")


def test_watch_dir_reports_non_envelope_body():
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html(None))):
        with sb.files.watch_dir("/tmp") as watcher:
            with pytest.raises(IOError, match="not a Connect envelope"):
                list(watcher)


@pytest.mark.parametrize("method", ["create", "connect"])
def test_pty_reports_html_content_type(method):
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html())):
        with pytest.raises(CubeSandboxError, match="received HTML instead of a Connect response"):
            _pty_open(sb, method)


@pytest.mark.parametrize("method", ["create", "connect"])
def test_pty_reports_non_envelope_body(method):
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html(None))):
        with pytest.raises(CubeSandboxError, match="not a Connect envelope"):
            _pty_open(sb, method)


# -- HTTP error pages ---------------------------------------------------------


def _error_page(status: int, body: bytes, content_type: str | None) -> httpx.Response:
    headers = {"content-type": content_type} if content_type else {}
    return httpx.Response(status, headers=headers, stream=httpx.ByteStream(body))


def _call(sb, target: str) -> str:
    """Run ``target`` against the mocked response and return its error message."""
    errors = (RuntimeError, IOError, CubeSandboxError)
    with pytest.raises(errors) as exc:
        if target == "run":
            sb.commands.run("echo hi")
        elif target == "watch":
            sb.files.watch_dir("/tmp")
        else:
            _pty_open(sb, target)
    return str(exc.value)


TARGETS = ["run", "watch", "create", "connect"]


@pytest.mark.parametrize("target", TARGETS)
@pytest.mark.parametrize(
    ("status", "hint"),
    [
        (404, "CUBE_PROXY_NODE_IP"),
        (502, "upstream envd or sandbox node is unreachable"),
        (401, "check credentials"),
    ],
)
def test_html_error_page_carries_routing_hint(target, status, hint):
    sb = make_sandbox()
    page = _error_page(status, b"<html><body>404 Not Found</body></html>", "text/html")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert f"HTTP {status}: <html><body>404 Not Found</body></html>; " in message
    assert hint in message


@pytest.mark.parametrize("target", TARGETS)
def test_html_error_page_without_content_type_carries_routing_hint(target):
    sb = make_sandbox()
    page = _error_page(404, b"\n<!DOCTYPE html><html></html>", None)
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert "CUBE_PROXY_NODE_IP" in message


@pytest.mark.parametrize("target", TARGETS)
def test_html_error_page_is_truncated(target):
    sb = make_sandbox()
    body = b"<!DOCTYPE html>" + b"x" * 100_000
    page = _error_page(502, body, "text/html")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert "x" * 300 not in message
    assert "...; received an HTML error page" in message


@pytest.mark.parametrize("target", TARGETS)
def test_json_error_keeps_server_message(target):
    sb = make_sandbox()
    body = b'{"code":"not_found","message":"process not found"}'
    page = _error_page(404, body, "text/html")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert message.endswith("HTTP 404: process not found")


@pytest.mark.parametrize("target", TARGETS)
def test_plain_text_error_has_no_routing_hint(target):
    sb = make_sandbox()
    page = _error_page(500, b"envd crashed", "text/plain")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert message.endswith("HTTP 500: envd crashed")


@pytest.mark.parametrize("target", TARGETS)
def test_404_text_page_carries_routing_hint(target):
    # Go's net/http.NotFound, a common shape for a misrouted request.
    sb = make_sandbox()
    page = _error_page(404, b"404 page not found\n", "text/plain; charset=utf-8")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert "HTTP 404: 404 page not found; response may be an HTML/text page" in message
    assert "CUBE_PROXY_NODE_IP" in message


@pytest.mark.parametrize(
    ("status", "body", "content_type", "routing"),
    [
        (404, b"<html><body>gateway</body></html>", None, True),
        (502, b'{"unexpected":1}', "text/html", True),
        (404, b'{"message": "process not fou', "text/html", False),
        (404, b'{"detail":"not found"}', "text/plain", False),
        (500, b"404 page not found", "text/plain", False),
        (404, b"   ", "text/plain", False),
        (404, b"not found", "application/octet-stream", False),
    ],
    ids=[
        "html-without-doctype",
        "html-labelled-non-error-json",
        "truncated-json",
        "json-error-text",
        "text-on-500",
        "blank-text-404",
        "binary-404",
    ],
)
def test_routing_page_classification_matches_go(status, body, content_type, routing):
    from cubesandbox._commands import _is_routing_page

    media_type = (content_type or "").split(";")[0]
    assert _is_routing_page(status, media_type, body) is routing


class _CountingStream(httpx.SyncByteStream):
    """An endless body that records how much of it was read."""

    def __init__(self, chunk: bytes = b"x" * 1024) -> None:
        self.chunk = chunk
        self.read = 0

    def __iter__(self):
        while True:
            self.read += len(self.chunk)
            yield self.chunk


@pytest.mark.parametrize("target", TARGETS)
def test_error_body_read_is_bounded(target):
    sb = make_sandbox()
    stream = _CountingStream()
    page = httpx.Response(502, headers={"content-type": "text/plain"}, stream=stream)
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert stream.read <= 64 * 1024 + len(stream.chunk)
    assert message.endswith(" [truncated]")
    assert len(message) < 64 * 1024 + 100


class _FailingStream(httpx.SyncByteStream):
    def __iter__(self):
        yield b"partial"
        raise httpx.ReadError("connection reset")


@pytest.mark.parametrize("target", TARGETS)
def test_unreadable_error_body_keeps_the_status_error(target):
    sb = make_sandbox()
    page = httpx.Response(502, headers={"content-type": "text/plain"}, stream=_FailingStream())
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert message.endswith("HTTP 502")


def test_watch_dir_keeps_ioerror_when_error_body_fails():
    sb = make_sandbox()
    page = httpx.Response(502, headers={"content-type": "text/plain"}, stream=_FailingStream())
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        with pytest.raises(IOError, match="^WatchDir failed: HTTP 502$"):
            sb.files.watch_dir("/tmp")


@pytest.mark.parametrize("target", TARGETS)
def test_routing_page_is_cut_on_a_character_boundary(target):
    sb = make_sandbox()
    body = ("<html>" + "中" * 100).encode("utf-8")
    page = _error_page(502, body, "text/html")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    shown = message.split("HTTP 502: ", 1)[1].split("...; ", 1)[0]
    assert "\ufffd" not in shown
    assert len(shown.encode("utf-8")) <= 200


@pytest.mark.parametrize("target", TARGETS)
def test_control_characters_are_replaced(target):
    sb = make_sandbox()
    page = _error_page(500, b"\x1b[31mboom\x07", "text/plain")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert "\x1b" not in message and "\x07" not in message
    assert message.endswith("HTTP 500: \ufffd[31mboom\ufffd")


class _StallingStream(httpx.SyncByteStream):
    """Send ``first`` and then never send anything again."""

    def __init__(self, first: bytes) -> None:
        self.first = first
        self.waited = False

    def __iter__(self):
        yield self.first
        self.waited = True
        raise AssertionError("the error body was read past a recognised routing page")


@pytest.mark.parametrize("target", TARGETS)
@pytest.mark.parametrize(
    "status,first,content_type",
    [
        (502, b"<html><body>Bad gateway</body>", "text/html"),
        (502, b"<!DOCTYPE html><title>portal</title>", "text/plain"),
        (404, b"404 page not found\n", "text/plain; charset=utf-8"),
    ],
    ids=["html", "doctype-as-text", "404-text"],
)
def test_routing_page_is_reported_without_waiting_for_the_rest(target, status, first, content_type):
    sb = make_sandbox()
    stream = _StallingStream(first)
    page = httpx.Response(status, headers={"content-type": content_type}, stream=stream)
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert not stream.waited
    assert first.decode().strip()[:20] in message
    assert "Connect stream" in message


def test_routing_prefix_never_disagrees_with_the_full_body():
    from cubesandbox._commands import _is_routing_page, _is_routing_page_prefix

    bodies = [
        b"<html>x</html>",
        b"  <!doctype html>",
        b"404 page not found",
        b'{"message": "boom"}',
        b"{not json",
        b"plain text",
        b"",
    ]
    for status in (200, 404, 502):
        for media_type in ("text/html", "text/plain", "application/json", ""):
            for body in bodies:
                for n in range(1, len(body) + 1):
                    if _is_routing_page_prefix(status, media_type, body[:n]):
                        assert _is_routing_page(status, media_type, body), (status, media_type, body, n)


@pytest.mark.parametrize("target", TARGETS)
def test_crlf_is_kept_and_c1_controls_are_replaced(target):
    sb = make_sandbox()
    body = "line1\r\nline2\rline3 \u009b31m\u0085end".encode()
    page = _error_page(500, body, "text/plain")
    with patch.object(sb, "_build_data_client", return_value=_client(page)):
        message = _call(sb, target)
    assert message.endswith("HTTP 500: line1\nline2\nline3 �31m�end")


def test_html_content_type_is_sanitized():
    from cubesandbox._commands import _html_response_error

    resp = httpx.Response(200, headers={"content-type": "text/html; x=\x1b[31m"})
    message = _html_response_error(resp)
    assert message is not None and "\x1b" not in message
