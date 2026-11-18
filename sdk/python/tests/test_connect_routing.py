# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Misrouted Connect streams (HTML or other non-envelope bodies with HTTP 200)."""

from __future__ import annotations

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


def test_header_error_rejects_unknown_flags_before_the_length():
    from cubesandbox._commands import _connect_header_error

    # "<!DOC" used to be read as flags 0x3c and a 558124867-byte length.
    message = _connect_header_error(HTML_PAGE[:5])
    assert message is not None
    assert "not a Connect envelope" in message
    assert HTML_PAGE[:5].hex() in message


@pytest.mark.parametrize("flags", [0x00, 0x01, 0x02, 0x03])
def test_header_error_accepts_connect_flags(flags):
    from cubesandbox._commands import _connect_header_error

    assert _connect_header_error(connect_envelope(flags, "{}")) is None


def test_commands_run_reports_html_content_type():
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html())):
        with pytest.raises(RuntimeError, match="received HTML instead of a Connect response") as exc:
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


def test_pty_create_reports_html_content_type():
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html())):
        with pytest.raises(CubeSandboxError, match="received HTML instead of a Connect response"):
            sb.pty.create(PtySize(rows=24, cols=80))


def test_pty_create_reports_non_envelope_body():
    sb = make_sandbox()
    with patch.object(sb, "_build_data_client", return_value=_client(_html(None))):
        with pytest.raises(CubeSandboxError, match="not a Connect envelope"):
            sb.pty.create(PtySize(rows=24, cols=80))
