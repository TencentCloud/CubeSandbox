# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import hashlib
import json
import os
import posixpath
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import pytest


@pytest.mark.parametrize("write", [False, True])
def test_direct_e2e_against_loopback_gateway(write):
    token = "loopback-volume-token"
    initial = {"/data.bin": b"\x00\xffhello", "/text.txt": "hello 中文\n".encode()}
    files = dict(initial)
    directories = {"/"}
    modes = {}
    calls = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def body(self):
            if self.headers.get("Transfer-Encoding") == "chunked":
                body = bytearray()
                while True:
                    size = int(self.rfile.readline().split(b";", 1)[0], 16)
                    if size == 0:
                        while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                            pass
                        return bytes(body)
                    body.extend(self.rfile.read(size))
                    assert self.rfile.read(2) == b"\r\n"
            return self.rfile.read(int(self.headers.get("Content-Length", "0")))

        def respond(self, status, body=None, binary=False):
            data = body if binary else json.dumps(body).encode() if body is not None else b""
            self.send_response(status)
            self.send_header("Content-Type", "application/octet-stream" if binary else "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            try:
                self.wfile.write(data)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def entry(self, path):
            return {
                "name": posixpath.basename(path) or "/", "path": path,
                "type": "directory" if path in directories else "file",
                "size": len(files.get(path, b"")), "mode": modes.get(path, 0o755 if path in directories else 0o644),
                "uid": 0, "gid": 0, "atime": "2026-11-02T00:00:00Z",
                "mtime": "2026-11-02T00:00:00Z", "ctime": "2026-11-02T00:00:00Z",
            }

        def route(self):
            location = urlsplit(self.path)
            calls.append((self.command, location.path))
            body = self.body()
            if location.path in ("/proxy/volumes", "/proxy/volumes/v-test"):
                if self.headers.get("X-API-Key") != "loopback-management-key":
                    return self.respond(401, {"message": "unauthorized"})
                assert self.command == "GET"
                if location.path.endswith("/v-test"):
                    return self.respond(200, {"volumeID": "v-test", "name": "test", "token": token})
                return self.respond(200, [{"volumeID": "v-test", "name": "test"}])
            assert location.path.startswith("/proxy/volumecontent/v-test/")
            assert self.headers.get("X-API-Key") is None
            assert self.headers.get("X-Access-Token") is None
            if self.headers.get("Authorization") != "Bearer " + token:
                return self.respond(401, {"message": "unauthorized"})
            path = parse_qs(location.query)["path"][0]
            params = parse_qs(location.query)
            endpoint = location.path.rsplit("/", 1)[-1]
            if self.command == "GET":
                if path not in files and path not in directories:
                    return self.respond(404)
                if endpoint == "file":
                    return self.respond(200, files[path], binary=True)
                if endpoint == "dir":
                    entries = [self.entry(item) for item in sorted(set(files) | directories)
                               if item != path and posixpath.dirname(item) == path]
                    return self.respond(200, entries)
                return self.respond(200, self.entry(path))
            if self.command == "POST":
                assert endpoint == "dir" and path not in directories and path not in files
                assert posixpath.dirname(path) in directories
                directories.add(path)
                return self.respond(201, self.entry(path))
            if self.command == "PUT":
                assert endpoint == "file" and self.headers["Content-Type"] == "application/octet-stream"
                assert posixpath.dirname(path) in directories
                if path in files and params.get("force") == ["false"]:
                    return self.respond(409, {"message": "exists"})
                files[path] = body
                if "mode" in params:
                    modes[path] = int(params["mode"][0])
                return self.respond(201, self.entry(path))
            if self.command == "PATCH":
                assert endpoint == "path" and path in files
                assert self.headers["Content-Type"] == "application/json"
                modes[path] = json.loads(body)["mode"]
                return self.respond(200, self.entry(path))
            assert self.command == "DELETE" and endpoint == "path"
            if path in files:
                del files[path]
            elif path in directories and path != "/":
                assert not any(posixpath.dirname(item) == path for item in set(files) | directories)
                directories.remove(path)
            else:
                return self.respond(404)
            modes.pop(path, None)
            return self.respond(204)

        do_GET = do_PUT = do_POST = do_PATCH = do_DELETE = route

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{server.server_port}/proxy"
        env = {key: value for key, value in os.environ.items() if not key.startswith(("CUBE_", "E2B_"))
               and key.lower() not in ("http_proxy", "https_proxy", "all_proxy", "no_proxy", "pytest_addopts")}
        env.update({
            "CUBE_API_URL": url, "CUBE_API_KEY": "loopback-management-key",
            "CUBE_VOLUME_API_URL": url, "CUBE_VOLUME_ID": "v-test", "CUBE_VOLUME_CONTENT_TOKEN": token,
            "CUBE_VOLUME_TEST_PATH": "/data.bin", "CUBE_VOLUME_TEXT_PATH": "/text.txt",
            "CUBE_VOLUME_TEST_SHA256": hashlib.sha256(initial["/data.bin"]).hexdigest(),
            "CUBE_VOLUME_TEST_MANAGEMENT": "1", "CUBE_VOLUME_E2E_WRITE": "1" if write else "0",
            "NO_PROXY": "127.0.0.1", "E2B_HTTP_VERSION": "1.1",
        })
        result = subprocess.run(
            [sys.executable, "-m", "pytest", "-q", "tests/test_volume_direct_e2e.py", "--run-e2e"],
            cwd=Path(__file__).resolve().parents[1], env=env, capture_output=True, text=True, timeout=60,
        )
        assert result.returncode == 0, result.stdout + result.stderr
        assert ("16 passed" if write else "12 passed, 4 skipped") in result.stdout
        assert files == initial and directories == {"/"}
        if not write:
            assert all(method == "GET" for method, _ in calls)
        else:
            assert {method for method, _ in calls} == {"GET", "POST", "PUT", "PATCH", "DELETE"}
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
