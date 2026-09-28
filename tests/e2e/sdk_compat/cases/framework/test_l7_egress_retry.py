# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Hermetic coverage for L7 HTTP command retry behavior."""

from __future__ import annotations

import socket
import subprocess
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from framework.l7_egress import (
    L7_ATTEMPTS,
    http_json_command,
    l7_command_timeout,
)

pytestmark = pytest.mark.framework


@contextmanager
def _scripted_server(
    responses: list[tuple[int | None, str]],
) -> Iterator[tuple[str, list[int]]]:
    request_count = [0]

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            index = min(request_count[0], len(responses) - 1)
            status, body = responses[index]
            request_count[0] += 1
            if status is None:
                self.close_connection = True
                self.connection.shutdown(socket.SHUT_RDWR)
                self.connection.close()
                return
            payload = body.encode()
            self.send_response(status)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *_args: object) -> None:
            pass

    class Server(ThreadingHTTPServer):
        def handle_error(self, *_args: object) -> None:
            pass

    server = Server(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_address[1]}/headers", request_count
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def _run_http_command(
    url: str, *, attempts: int = L7_ATTEMPTS
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        http_json_command(url, timeout=2, attempts=attempts, backoff_enabled=False),
        shell=True,
        check=False,
        capture_output=True,
        text=True,
        timeout=10,
    )


@pytest.mark.parametrize("gateway_status", [502, 504])
def test_http_command_retries_gateway_failure(gateway_status: int) -> None:
    with _scripted_server([(gateway_status, "temporary"), (200, "recovered")]) as (
        url,
        request_count,
    ):
        result = _run_http_command(url)

    assert result.returncode == 0
    assert result.stdout == "STATUS:200\nrecovered\n"
    assert request_count == [2]


def test_http_command_retries_transport_failure() -> None:
    with _scripted_server([(None, ""), (200, "recovered")]) as (url, request_count):
        result = _run_http_command(url)

    assert result.returncode == 0
    assert result.stdout == "STATUS:200\nrecovered\n"
    assert request_count == [2]


def test_http_command_returns_last_gateway_failure_after_attempt_limit() -> None:
    with _scripted_server([(504, "first"), (502, "second"), (504, "last")]) as (
        url,
        request_count,
    ):
        result = _run_http_command(url)

    assert result.returncode == 0
    assert result.stdout == "STATUS:504\nlast\n"
    assert request_count == [3]


def test_http_command_does_not_retry_policy_denial() -> None:
    with _scripted_server([(403, "denied"), (200, "should not run")]) as (
        url,
        request_count,
    ):
        result = _run_http_command(url)

    assert result.returncode == 0
    assert result.stdout == "STATUS:403\ndenied\n"
    assert request_count == [1]


def test_l7_command_timeout_uses_requested_retry_budget() -> None:
    assert l7_command_timeout(10, request_timeout=2, attempts=4) == 44
    assert l7_command_timeout(10, request_timeout=10, attempts=2) == 91
    assert l7_command_timeout(100, request_timeout=10, attempts=2) == 100


def test_http_command_does_not_retry_successful_response() -> None:
    with _scripted_server([(200, "missing header"), (200, "should not run")]) as (
        url,
        request_count,
    ):
        result = _run_http_command(url)

    assert result.returncode == 0
    assert result.stdout == "STATUS:200\nmissing header\n"
    assert request_count == [1]
