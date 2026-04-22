from __future__ import annotations

import importlib
import json
import os
import sys
from pathlib import Path

import httpx
import pytest
from aiohttp import ClientSession, WSMsgType, web
from packaging.version import Version

from e2b import ConnectionConfig
from e2b.sandbox.main import SandboxBase
from e2b_code_interpreter import Sandbox as CodeInterpreterSandbox


EXAMPLE_DIR = Path(__file__).resolve().parent
if str(EXAMPLE_DIR) not in sys.path:
    sys.path.insert(0, str(EXAMPLE_DIR))


@pytest.fixture
def dev_sidecar():
    if "dev_sidecar" in sys.modules:
        module = importlib.reload(sys.modules["dev_sidecar"])
    else:
        module = importlib.import_module("dev_sidecar")
    return module


@pytest.fixture
def patched_sidecar(monkeypatch, dev_sidecar):
    monkeypatch.setenv("E2B_API_URL", "http://127.0.0.1:13000")
    monkeypatch.setenv("CUBE_REMOTE_PROXY_BASE", "https://127.0.0.1:11443")
    monkeypatch.setenv("CUBE_DEV_PROXY_URL", "http://127.0.0.1:12580")
    monkeypatch.delenv("E2B_DEBUG", raising=False)
    monkeypatch.delenv("E2B_DOMAIN", raising=False)
    dev_sidecar.setup_dev_sidecar()
    return dev_sidecar


def _make_sandbox() -> SandboxBase:
    class DummySandbox(SandboxBase):
        pass

    return DummySandbox(
        sandbox_id="sandbox-123",
        envd_version=Version("1.0.0"),
        envd_access_token=None,
        sandbox_domain="cube.app",
        connection_config=ConnectionConfig(api_url="http://127.0.0.1:13000"),
    )


async def _start_web_app(app: web.Application) -> tuple[web.AppRunner, str]:
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, host="127.0.0.1", port=0)
    await site.start()
    host, port = runner.addresses[0][:2]
    return runner, f"http://{host}:{port}"


@pytest.mark.asyncio
async def test_embedded_sidecar_binds_with_runner_addresses(monkeypatch, dev_sidecar):
    async def upstream_ok(_request: web.Request) -> web.Response:
        return web.Response(text="ok")

    upstream = web.Application()
    upstream.router.add_get("/", upstream_ok)
    upstream_runner, upstream_url = await _start_web_app(upstream)

    monkeypatch.setenv("CUBE_REMOTE_PROXY_BASE", upstream_url)
    monkeypatch.setenv("CUBE_DEV_PROXY_HOST", "127.0.0.1")
    monkeypatch.setenv("CUBE_DEV_PROXY_PORT", "12580")
    monkeypatch.delenv("CUBE_DEV_PROXY_URL", raising=False)

    try:
        dev_sidecar.setup_dev_sidecar()
        assert dev_sidecar._SIDECAR_URL.startswith("http://127.0.0.1:")
    finally:
        await upstream_runner.cleanup()


def test_setup_dev_sidecar_is_idempotent(monkeypatch, patched_sidecar):
    patched_sidecar.setup_dev_sidecar()

    assert patched_sidecar._PATCHED is True
    assert os.environ.get("CUBE_DEV_PROXY_URL") == "http://127.0.0.1:12580"
    assert os.environ.get("E2B_DEBUG") is None
    assert os.environ.get("E2B_DOMAIN") is None


def test_connection_config_and_sandbox_helpers_use_routed_urls(patched_sidecar):
    config = ConnectionConfig(api_url="http://127.0.0.1:13000")
    sandbox = _make_sandbox()

    assert (
        config.get_sandbox_url("sandbox-123", "cube.app")
        == "http://127.0.0.1:12580/sandboxes/router/sandbox-123/49983"
    )
    assert (
        sandbox.get_host(8080)
        == "127.0.0.1:12580/sandboxes/router/sandbox-123/8080"
    )
    assert (
        sandbox.download_url("/tmp/data.txt")
        == "http://127.0.0.1:12580/sandboxes/router/sandbox-123/49983/files?path=%2Ftmp%2Fdata.txt"
    )
    assert (
        sandbox.upload_url("/tmp/data.txt")
        == "http://127.0.0.1:12580/sandboxes/router/sandbox-123/49983/files?path=%2Ftmp%2Fdata.txt"
    )
    assert (
        sandbox.get_mcp_url()
        == "http://127.0.0.1:12580/sandboxes/router/sandbox-123/50005/mcp"
    )


def test_external_sidecar_scheme_is_preserved(monkeypatch, dev_sidecar):
    monkeypatch.setenv("E2B_API_URL", "http://127.0.0.1:13000")
    monkeypatch.setenv("CUBE_REMOTE_PROXY_BASE", "https://127.0.0.1:11443")
    monkeypatch.setenv("CUBE_DEV_PROXY_URL", "https://proxy.example:8443/base")

    dev_sidecar.setup_dev_sidecar()
    sandbox = _make_sandbox()

    assert (
        sandbox.download_url("/tmp/data.txt")
        == "https://proxy.example:8443/base/sandboxes/router/sandbox-123/49983/files?path=%2Ftmp%2Fdata.txt"
    )
    assert (
        sandbox.get_mcp_url()
        == "https://proxy.example:8443/base/sandboxes/router/sandbox-123/50005/mcp"
    )


def test_code_interpreter_uses_routed_http_jupyter_url(patched_sidecar):
    sandbox = CodeInterpreterSandbox(
        sandbox_id="sandbox-123",
        envd_version=Version("1.0.0"),
        envd_access_token=None,
        sandbox_domain="cube.app",
        connection_config=ConnectionConfig(api_url="http://127.0.0.1:13000"),
        traffic_access_token=None,
    )

    assert (
        sandbox._jupyter_url
        == "http://127.0.0.1:12580/sandboxes/router/sandbox-123/49999"
    )


@pytest.mark.asyncio
async def test_http_proxy_forwards_path_query_body_and_host(monkeypatch, dev_sidecar):
    async def upstream_handler(request: web.Request) -> web.Response:
        payload = {
            "host": request.headers["Host"],
            "path_qs": request.path_qs,
            "body": (await request.read()).decode(),
        }
        return web.json_response(payload)

    upstream = web.Application()
    upstream.router.add_route("*", "/{tail:.*}", upstream_handler)
    upstream_runner, upstream_url = await _start_web_app(upstream)

    monkeypatch.setenv("CUBE_REMOTE_PROXY_BASE", upstream_url)
    monkeypatch.setenv("CUBE_REMOTE_SANDBOX_DOMAIN", "cube.app")
    sidecar_runner, sidecar_url = await _start_web_app(dev_sidecar.build_app())

    try:
        async with httpx.AsyncClient() as client:
            response = await client.post(
                f"{sidecar_url}/sandboxes/router/sbx-1/8080/api/v1/run?hello=1",
                content=b"payload",
            )

        assert response.status_code == 200
        assert response.json() == {
            "host": "8080-sbx-1.cube.app",
            "path_qs": "/api/v1/run?hello=1",
            "body": "payload",
        }
    finally:
        await sidecar_runner.cleanup()
        await upstream_runner.cleanup()


@pytest.mark.asyncio
async def test_websocket_proxy_forwards_frames_and_host(monkeypatch, dev_sidecar):
    async def upstream_ws(request: web.Request) -> web.StreamResponse:
        ws = web.WebSocketResponse()
        await ws.prepare(request)

        async for message in ws:
            if message.type == WSMsgType.TEXT:
                await ws.send_str(
                    json.dumps(
                        {
                            "host": request.headers["Host"],
                            "path_qs": request.path_qs,
                            "body": message.data,
                        }
                    )
                )
                await ws.close()
        return ws

    upstream = web.Application()
    upstream.router.add_get("/{tail:.*}", upstream_ws)
    upstream_runner, upstream_url = await _start_web_app(upstream)

    monkeypatch.setenv("CUBE_REMOTE_PROXY_BASE", upstream_url)
    monkeypatch.setenv("CUBE_REMOTE_SANDBOX_DOMAIN", "cube.app")
    sidecar_runner, sidecar_url = await _start_web_app(dev_sidecar.build_app())

    try:
        async with ClientSession() as session:
            async with session.ws_connect(
                f"{sidecar_url}/sandboxes/router/sbx-1/9000/ws?room=red"
            ) as ws:
                await ws.send_str("hello")
                message = await ws.receive()

        assert json.loads(message.data) == {
            "host": "9000-sbx-1.cube.app",
            "path_qs": "/ws?room=red",
            "body": "hello",
        }
    finally:
        await sidecar_runner.cleanup()
        await upstream_runner.cleanup()
