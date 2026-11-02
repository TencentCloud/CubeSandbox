# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import io
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

import httpx
import pytest

from cubesandbox import (
    ApiError,
    Volume,
    VolumeContentUnavailableError,
    VolumeEntryStat,
    VolumeFileType,
    VolumePathNotFoundError,
)
from cubesandbox._config import Config
from cubesandbox._exceptions import AuthenticationError, ServiceBusyException
from cubesandbox._volume_content import VolumeContent


TOKEN = "volume-content-secret"


def entry(path="/data.txt", **values):
    return {
        "name": path.rsplit("/", 1)[-1], "type": "file", "path": path,
        "size": 3, "uid": 0, "gid": 0, "mode": 420,
        "atime": "2026-10-30T01:02:03Z", "mtime": "2026-10-30T01:02:03Z",
        "ctime": "2026-10-30T01:02:03Z", **values,
    }


@pytest.fixture
def gateway(monkeypatch):
    calls = []
    clients = []
    responder = {"handler": lambda req: httpx.Response(200, json=entry())}

    def handle(request):
        calls.append(request)
        assert request.headers["Authorization"] == f"Bearer {TOKEN}"
        assert "X-API-Key" not in request.headers
        assert "X-Access-Token" not in request.headers
        return responder["handler"](request)

    def client(self, request_timeout, stream=False):
        result = httpx.Client(
            transport=httpx.MockTransport(handle),
            headers={"Authorization": f"Bearer {self.token}"},
            follow_redirects=False,
        )
        clients.append(result)
        return result

    monkeypatch.setattr(VolumeContent, "_client", client)
    config = Config(api_url="https://management.test", api_key="management-secret",
                    volume_api_url="https://content.test/gateway")
    volume = Volume("vol-123", "test", TOKEN)
    volume._config = config
    return volume, calls, clients, responder


def test_class_management_and_instance_content_do_not_conflict(gateway):
    volume, calls, _, responder = gateway
    with patch("cubesandbox._volume._shared_session") as session:
        response = session.return_value.get.return_value
        response.status_code = 200
        response.json.return_value = [{"volumeID": "v1", "name": "one"}]
        assert Volume.list(config=volume._config)[0].volume_id == "v1"
        with pytest.raises(TypeError):
            volume.list()
        response.json.return_value = {"volumeID": "v1", "name": "one", "token": "management"}
        assert Volume.get_info("v1", config=volume._config).volume_id == "v1"
        with pytest.raises(ValueError):
            volume.get_info("v1")
        with pytest.raises(TypeError):
            volume.get_info(path="/data", volume_id="v1")
    responder["handler"] = lambda req: httpx.Response(200, json=[entry()])
    assert volume.list("/", depth=0)[0].type == VolumeFileType.FILE
    assert calls[-1].url.path == "/gateway/volumecontent/vol-123/dir"
    assert calls[-1].url.params["depth"] == "0"
    responder["handler"] = lambda req: httpx.Response(200, json=entry())
    assert isinstance(volume.get_info("/data.txt"), VolumeEntryStat)


@pytest.mark.parametrize("format,expected", [("text", "hello"), ("bytes", b"hello")])
def test_read_direct_without_sandbox(gateway, format, expected):
    volume, calls, clients, responder = gateway
    responder["handler"] = lambda req: httpx.Response(200, content=b"hello")
    with patch("cubesandbox.Sandbox.create", side_effect=AssertionError("must not create sandbox")):
        assert volume.read_file("/a space/#中文.txt", format) == expected
    assert calls[-1].method == "GET"
    assert calls[-1].url.params["path"] == "/a space/#中文.txt"
    assert calls[-1].url.path.endswith("/file")
    assert clients[-1].is_closed


@pytest.mark.parametrize("data,expected", [
    ("中文", "中文".encode()), (b"\x00\xff", b"\x00\xff"),
    (io.BytesIO(b"binary"), b"binary"), (io.StringIO("文字"), "文字".encode()),
    (b"", b""),
])
def test_write_raw_bytes_and_file_streams(gateway, data, expected):
    volume, calls, clients, _ = gateway
    assert volume.write_file("/data.txt", data, uid=0, gid=0, mode=0, force=False).path == "/data.txt"
    req = calls[-1]
    assert req.method == "PUT" and req.content == expected
    assert req.headers["Content-Type"] == "application/octet-stream"
    assert dict(req.url.params) == {"path": "/data.txt", "uid": "0", "gid": "0", "mode": "0", "force": "false"}
    assert clients[-1].is_closed


def test_directory_and_metadata_operations(gateway):
    volume, calls, _, _ = gateway
    volume.make_dir("/nested", mode=0o755, force=True)
    assert calls[-1].method == "POST" and calls[-1].url.path.endswith("/dir")
    volume.update_metadata("/data.txt", uid=0, mode=0)
    assert calls[-1].method == "PATCH"
    assert calls[-1].content == b'{"uid":0,"mode":0}'
    assert volume.exists("/data.txt")
    volume.remove("/data.txt")
    assert calls[-1].method == "DELETE"


def test_all_content_404_responses_become_path_not_found(gateway):
    volume, _, clients, responder = gateway
    responder["handler"] = lambda req: httpx.Response(404, json={"code": "path_not_found", "message": "missing"})
    assert not volume.exists("/missing")
    with pytest.raises(VolumePathNotFoundError):
        volume.read_file("/missing")
    responder["handler"] = lambda req: httpx.Response(404, text="404 page not found")
    assert not volume.exists("/missing")
    assert all(client.is_closed for client in clients)


@pytest.mark.parametrize("status,error", [
    (401, AuthenticationError), (403, ApiError),
    (409, ApiError), (500, ApiError), (405, ApiError),
    (501, ApiError), (503, ServiceBusyException),
])
def test_errors_do_not_retry_upload_or_leak_tokens(gateway, status, error):
    volume, calls, clients, responder = gateway
    responder["handler"] = lambda req: httpx.Response(status, json={"message": f"error {TOKEN}"})
    with pytest.raises(error) as exc:
        volume.write_file("/data.txt", b"payload")
    assert exc.value.status_code == status
    assert TOKEN not in str(exc.value)
    assert len(calls) == 1 and clients[-1].is_closed


def test_redirect_not_followed(gateway):
    volume, calls, _, responder = gateway
    responder["handler"] = lambda req: httpx.Response(307, headers={"Location": "https://untrusted.test"})
    with pytest.raises(ApiError):
        volume.read_file("/data.txt")
    assert len(calls) == 1


@pytest.mark.parametrize("path", ["relative", "../escape", "/a/../escape", "/a/./b", "/nul\x00", "/a\\b", None])
def test_invalid_path_never_sends_request(gateway, path):
    volume, calls, _, _ = gateway
    with pytest.raises(ValueError):
        volume.read_file(path)
    assert not calls


def test_invalid_options_and_root_remove_never_send(gateway):
    volume, calls, _, _ = gateway
    for action in (
        lambda: volume.read_file("/x", "unknown"),
        lambda: volume.write_file("/x", object()),
        lambda: volume.make_dir("/x", mode=-1),
        lambda: volume.make_dir("/x", force="true"),
        lambda: volume.list("/", depth=True),
        lambda: volume.remove("///"),
    ):
        with pytest.raises(ValueError):
            action()
    assert not calls


class TrackingStream(httpx.SyncByteStream):
    def __init__(self):
        self.closed = False
        self.parts_read = 0

    def __iter__(self):
        for _ in range(3):
            self.parts_read += 1
            yield b"x" * (64 * 1024)

    def close(self):
        self.closed = True


def test_stream_is_lazy_bounded_and_closeable(gateway):
    volume, calls, clients, responder = gateway
    body = TrackingStream()
    responder["handler"] = lambda req: httpx.Response(200, stream=body)
    chunks = volume.read_file("/big.bin", "stream")
    assert not calls
    assert len(next(chunks)) == 64 * 1024
    assert body.parts_read == 1
    chunks.close()
    assert body.closed and clients[-1].is_closed


def test_stream_failure_releases_resources(gateway):
    volume, _, clients, responder = gateway
    responder["handler"] = lambda req: httpx.Response(403, json={"message": "forbidden"})
    with pytest.raises(ApiError):
        list(volume.read_file("/file", "stream"))
    assert clients[-1].is_closed


def test_large_stream_error_is_bounded_and_closed(gateway):
    volume, _, clients, responder = gateway
    body = TrackingStream()
    responder["handler"] = lambda req: httpx.Response(500, stream=body)
    with pytest.raises(ApiError):
        list(volume.read_file("/file", "stream"))
    assert body.parts_read <= 2
    assert body.closed and clients[-1].is_closed


@pytest.mark.parametrize("token", ["secret ", " secret", "secret\x00", "secret\r\n", "文字", "secret:other", "secret\t", 123])
def test_invalid_bearer_is_rejected_without_disclosing_value(token):
    with pytest.raises(ValueError) as exc:
        VolumeContent("v", token, Config(volume_api_url="https://content.test"))
    assert "secret" not in str(exc.value)


@pytest.mark.parametrize("url", ["file:///tmp", "https://user:pass@host", "https://host?a=1", "https://host#f", "https://host?", "https://host#", "http://remote.test", "http://host:bad"])
def test_content_url_validation(url):
    with pytest.raises(ValueError):
        VolumeContent("v", TOKEN, Config(volume_api_url=url))


def test_empty_plugin_token_fails_before_default_endpoint_request():
    with pytest.raises(AuthenticationError):
        Volume("v", "v").read_file("/a")
    with pytest.raises(AuthenticationError):
        Volume("v", "v", config=Config(volume_api_url="https://content.test")).read_file("/a")


@pytest.mark.parametrize("timeout", [-1, float("inf"), float("nan"), True])
def test_timeout_validation(timeout):
    content = VolumeContent("v", TOKEN, Config(volume_api_url="https://content.test"))
    with pytest.raises(ValueError):
        content._client(timeout)


def test_e2b_zero_timeout_disables_transport_deadlines():
    content = VolumeContent("v", TOKEN, Config(volume_api_url="https://content.test"))
    with content._client(0) as client:
        assert client.timeout.connect is None
        assert client.timeout.read is None
        assert client.timeout.write is None
        assert client.timeout.pool is None


def test_production_client_sends_only_volume_bearer():
    content = VolumeContent("v", TOKEN, Config(api_key="other", volume_api_url="https://content.test"))
    with content._client(None) as client:
        assert client.headers["Authorization"] == f"Bearer {TOKEN}"
        assert "X-API-Key" not in client.headers
        assert not client.follow_redirects


def test_bad_response_is_not_silently_an_empty_directory(gateway):
    volume, _, _, responder = gateway
    responder["handler"] = lambda req: httpx.Response(200, json={"entries": []})
    with pytest.raises(ApiError):
        volume.list("/")
    responder["handler"] = lambda req: httpx.Response(200, json=entry(size="bad"))
    with pytest.raises(ApiError):
        volume.get_info("/data.txt")


@pytest.mark.parametrize("path", ["relative.txt", "/a/../b", "/a/./b", "/nul\x00", "/back\\slash"])
@pytest.mark.parametrize("operation", ["stat", "list"])
def test_response_paths_are_validated(gateway, path, operation):
    volume, _, clients, responder = gateway
    payload = entry(path=path)
    if operation == "list":
        payload = [payload]
    responder["handler"] = lambda req: httpx.Response(200, json=payload)
    with pytest.raises(ApiError):
        if operation == "list":
            volume.list("/")
        else:
            volume.get_info("/data.txt")
    assert clients[-1].is_closed


@pytest.mark.parametrize("volume_id", ["", "../other", "v/other", "vol\n", "vol%2fother", None])
def test_content_constructor_rejects_invalid_volume_id(volume_id):
    with pytest.raises(ValueError):
        VolumeContent(volume_id, TOKEN, Config(volume_api_url="https://content.test"))


def test_public_volume_validation_rejects_trailing_newline():
    from cubesandbox._volume import _validate_volume_id, _validate_name

    with pytest.raises(ValueError):
        _validate_volume_id("vol\n")
    with pytest.raises(ValueError):
        _validate_name("vol\n")


def test_content_config_from_environment(monkeypatch):
    monkeypatch.setenv("CUBE_VOLUME_API_URL", "https://content.test")
    assert Config().volume_api_url == "https://content.test"


def test_real_loopback_gateway_read_write_and_missing_path():
    files = {}
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            pass

        def respond(self, status, body, content_type="application/json"):
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def route(self):
            location = urlsplit(self.path)
            path = parse_qs(location.query)["path"][0]
            requests.append((self.command, location.path, path))
            assert self.headers["Authorization"] == f"Bearer {TOKEN}"
            assert self.headers.get("X-API-Key") is None
            assert self.headers.get("X-Access-Token") is None
            return location.path, path

        def do_PUT(self):
            _, path = self.route()
            assert self.headers["Content-Type"] == "application/octet-stream"
            files[path] = self.rfile.read(int(self.headers["Content-Length"]))
            self.respond(200, json.dumps(entry(path, size=len(files[path]))).encode())

        def do_GET(self):
            endpoint, path = self.route()
            if path not in files:
                self.respond(404, b'{"code":"path_not_found","message":"missing"}')
            elif endpoint.endswith("/file"):
                self.respond(200, files[path], "application/octet-stream")
            else:
                self.respond(200, json.dumps(entry(path, size=len(files[path]))).encode())

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        config = Config(volume_api_url=f"http://127.0.0.1:{server.server_port}")
        volume = Volume("v-test", "test", TOKEN, config=config)
        with patch("cubesandbox.Sandbox.create", side_effect=AssertionError("no helper sandbox")):
            assert volume.write_file("/a 空格.bin", b"\xff\x00hello").size == 7
            assert volume.read_file("/a 空格.bin", "bytes") == b"\xff\x00hello"
            assert b"".join(volume.read_file("/a 空格.bin", "stream")) == b"\xff\x00hello"
            assert volume.exists("/a 空格.bin")
            assert not volume.exists("/missing")
        assert all(endpoint.startswith("/volumecontent/v-test/") for _, endpoint, _ in requests)
        assert len(requests) == 5
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
