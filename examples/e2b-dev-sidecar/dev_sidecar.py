from __future__ import annotations

import asyncio
import os
import threading
import warnings
from typing import Iterable
from urllib.parse import urlencode, urlsplit

from aiohttp import (
    ClientSession,
    ClientTimeout,
    TCPConnector,
    WSMsgType,
    hdrs,
    web,
)


HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}
WEBSOCKET_REQUEST_HEADERS = {
    hdrs.CONNECTION,
    hdrs.SEC_WEBSOCKET_ACCEPT,
    hdrs.SEC_WEBSOCKET_EXTENSIONS,
    hdrs.SEC_WEBSOCKET_KEY,
    hdrs.SEC_WEBSOCKET_PROTOCOL,
    hdrs.SEC_WEBSOCKET_VERSION,
    hdrs.UPGRADE,
}
_PATCHED = False
_ORIGINAL_CONNECTION_CONFIG_GET_SANDBOX_URL = None
_ORIGINAL_SANDBOX_BASE_GET_HOST = None
_ORIGINAL_SANDBOX_BASE_FILE_URL = None
_ORIGINAL_SANDBOX_BASE_GET_MCP_URL = None
_ORIGINAL_CODE_INTERPRETER_JUPYTER_URL = None
_ORIGINAL_ASYNC_CODE_INTERPRETER_JUPYTER_URL = None

_SIDECAR_LOCK = threading.Lock()
_SIDECAR_READY = threading.Event()
_SIDECAR_URL = ""
_SIDECAR_ERROR: BaseException | None = None
CONFIG_KEY = web.AppKey("config", dict)
SESSION_KEY = web.AppKey("session", ClientSession)


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _bool_env(name: str, default: bool) -> bool:
    value = _env(name)
    if not value:
        return default
    return value.lower() in {"1", "true", "yes", "on"}


def _strip_slash(value: str) -> str:
    return value.rstrip("/")


def _normalize_proxy_url(value: str) -> str:
    raw = value.strip().rstrip("/")
    if not raw:
        return ""
    return raw if "://" in raw else f"http://{raw}"


def _normalize_proxy_host(value: str) -> str:
    parsed = urlsplit(_normalize_proxy_url(value))
    host = parsed.netloc or parsed.path
    path = parsed.path if parsed.netloc else ""
    return f"{host}{path}".rstrip("/")


def _join_url(base: str, path: str, query: str = "") -> str:
    url = f"{_strip_slash(base)}{path}"
    if query:
        url = f"{url}?{query}"
    return url


def _build_router_path(sandbox_id: str, port: int, tail: str = "") -> str:
    normalized_tail = tail.lstrip("/")
    path = f"/sandboxes/router/{sandbox_id}/{port}"
    if normalized_tail:
        path = f"{path}/{normalized_tail}"
    return path


def _build_router_url(
    base_url: str,
    sandbox_id: str,
    port: int,
    tail: str = "",
    query: str = "",
) -> str:
    return _join_url(base_url, _build_router_path(sandbox_id, port, tail), query)


def _build_router_host(base_url: str, sandbox_id: str, port: int) -> str:
    return f"{_normalize_proxy_host(base_url)}{_build_router_path(sandbox_id, port)}"


def _build_upstream_ws_url(url: str) -> str:
    parsed = urlsplit(url)
    if parsed.scheme == "https":
        scheme = "wss"
    elif parsed.scheme == "http":
        scheme = "ws"
    else:
        scheme = parsed.scheme
    return parsed._replace(scheme=scheme).geturl()


def _copy_headers(
    headers: Iterable[tuple[str, str]],
    *,
    host: str | None,
    hop_by_hop: bool,
) -> dict[str, str]:
    copied: dict[str, str] = {}
    for key, value in headers:
        lowered = key.lower()
        if lowered == "host":
            continue
        if hop_by_hop and lowered in HOP_BY_HOP_HEADERS:
            continue
        copied[key] = value
    if host:
        copied["Host"] = host
    return copied


def _is_websocket_request(request: web.Request) -> bool:
    connection = request.headers.get(hdrs.CONNECTION, "")
    upgrade = request.headers.get(hdrs.UPGRADE, "")
    return "upgrade" in connection.lower() and upgrade.lower() == "websocket"


async def _stream_http_proxy(
    request: web.Request,
    session: ClientSession,
    target_url: str,
    *,
    host: str | None,
) -> web.StreamResponse:
    body = await request.read()
    headers = _copy_headers(request.headers.items(), host=host, hop_by_hop=True)

    async with session.request(
        method=request.method,
        url=target_url,
        headers=headers,
        data=body if body else None,
        allow_redirects=False,
    ) as upstream:
        response = web.StreamResponse(status=upstream.status, reason=upstream.reason)
        for key, value in upstream.headers.items():
            lowered = key.lower()
            if lowered in HOP_BY_HOP_HEADERS or lowered == "content-length":
                continue
            response.headers[key] = value

        await response.prepare(request)
        async for chunk in upstream.content.iter_chunked(64 * 1024):
            await response.write(chunk)
        await response.write_eof()
        return response


async def _pump_websocket_to_upstream(
    downstream: web.WebSocketResponse,
    upstream,
) -> None:
    async for msg in downstream:
        if msg.type == WSMsgType.TEXT:
            await upstream.send_str(msg.data)
        elif msg.type == WSMsgType.BINARY:
            await upstream.send_bytes(msg.data)
        elif msg.type == WSMsgType.PING:
            await upstream.ping(msg.data)
        elif msg.type == WSMsgType.PONG:
            await upstream.pong(msg.data)
        elif msg.type == WSMsgType.CLOSE:
            await upstream.close(code=downstream.close_code or 1000)
            return


async def _pump_websocket_to_downstream(
    upstream,
    downstream: web.WebSocketResponse,
) -> None:
    async for msg in upstream:
        if msg.type == WSMsgType.TEXT:
            await downstream.send_str(msg.data)
        elif msg.type == WSMsgType.BINARY:
            await downstream.send_bytes(msg.data)
        elif msg.type == WSMsgType.PING:
            await downstream.ping(msg.data)
        elif msg.type == WSMsgType.PONG:
            await downstream.pong(msg.data)
        elif msg.type == WSMsgType.CLOSE:
            await downstream.close(code=upstream.close_code or 1000)
            return


async def _stream_websocket_proxy(
    request: web.Request,
    session: ClientSession,
    target_url: str,
    *,
    host: str | None,
) -> web.StreamResponse:
    request_headers = _copy_headers(
        request.headers.items(),
        host=host,
        hop_by_hop=False,
    )
    request_headers = {
        key: value
        for key, value in request_headers.items()
        if key in WEBSOCKET_REQUEST_HEADERS or key.lower() not in HOP_BY_HOP_HEADERS
    }

    async with session.ws_connect(
        _build_upstream_ws_url(target_url),
        headers=request_headers,
        protocols=request.headers.getall(hdrs.SEC_WEBSOCKET_PROTOCOL, []),
    ) as upstream:
        protocols = (upstream.protocol,) if upstream.protocol else ()
        downstream = web.WebSocketResponse(
            protocols=protocols,
            autoclose=True,
            autoping=True,
        )
        await downstream.prepare(request)

        forward_tasks = (
            asyncio.create_task(_pump_websocket_to_upstream(downstream, upstream)),
            asyncio.create_task(_pump_websocket_to_downstream(upstream, downstream)),
        )
        done, pending = await asyncio.wait(
            forward_tasks,
            return_when=asyncio.FIRST_COMPLETED,
        )
        for task in pending:
            task.cancel()
        for task in done:
            task.result()

        if not downstream.closed:
            await downstream.close(code=upstream.close_code or 1000)
        return downstream


async def proxy_sandbox(request: web.Request) -> web.StreamResponse:
    config = request.app[CONFIG_KEY]
    sandbox_id = request.match_info["sandbox_id"]
    port = int(request.match_info["port"])
    tail = request.match_info.get("tail", "")
    upstream_path = f"/{tail}" if tail else "/"
    url = _join_url(config["cube_proxy_base"], upstream_path, request.query_string)
    host = f"{port}-{sandbox_id}.{config['sandbox_domain']}"

    if _is_websocket_request(request):
        return await _stream_websocket_proxy(request, request.app[SESSION_KEY], url, host=host)
    return await _stream_http_proxy(request, request.app[SESSION_KEY], url, host=host)


async def health(_request: web.Request) -> web.Response:
    return web.json_response({"ok": True})


async def on_startup(app: web.Application) -> None:
    config = app[CONFIG_KEY]
    connector = TCPConnector(ssl=config["verify_ssl"])
    timeout = ClientTimeout(total=None, connect=30, sock_read=None)
    app[SESSION_KEY] = ClientSession(connector=connector, timeout=timeout)


async def on_cleanup(app: web.Application) -> None:
    await app[SESSION_KEY].close()


def build_app() -> web.Application:
    cube_proxy_base = _strip_slash(_env("CUBE_REMOTE_PROXY_BASE", "https://127.0.0.1:11443"))
    sandbox_domain = _env("CUBE_REMOTE_SANDBOX_DOMAIN", "cube.app")
    verify_ssl = _bool_env("CUBE_REMOTE_PROXY_VERIFY_SSL", False)

    app = web.Application(client_max_size=64 * 1024 * 1024)
    app[CONFIG_KEY] = {
        "cube_proxy_base": cube_proxy_base,
        "sandbox_domain": sandbox_domain,
        "verify_ssl": verify_ssl,
    }
    app.on_startup.append(on_startup)
    app.on_cleanup.append(on_cleanup)

    app.router.add_get("/healthz", health)
    app.router.add_route("*", "/sandboxes/router/{sandbox_id}/{port}", proxy_sandbox)
    app.router.add_route(
        "*",
        "/sandboxes/router/{sandbox_id}/{port}/{tail:.*}",
        proxy_sandbox,
    )
    return app


async def _start_embedded_sidecar(host: str, preferred_port: int) -> int:
    app = build_app()
    runner = web.AppRunner(app)
    await runner.setup()

    ports_to_try = list(range(preferred_port, preferred_port + 32)) + [0]
    last_error: OSError | None = None

    for port in ports_to_try:
        site = web.TCPSite(runner, host=host, port=port)
        try:
            await site.start()
        except OSError as exc:
            last_error = exc
            continue

        for address in runner.addresses:
            if isinstance(address, tuple) and len(address) >= 2:
                return int(address[1])
        raise RuntimeError("Embedded dev sidecar started without a bound address")

    raise RuntimeError("Failed to bind embedded dev sidecar") from last_error


def _run_embedded_sidecar(host: str, preferred_port: int) -> None:
    global _SIDECAR_ERROR, _SIDECAR_URL

    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    async def _bootstrap() -> None:
        global _SIDECAR_URL
        port = await _start_embedded_sidecar(host, preferred_port)
        _SIDECAR_URL = f"http://{host}:{port}"
        _SIDECAR_READY.set()

    try:
        loop.run_until_complete(_bootstrap())
        loop.run_forever()
    except Exception as exc:  # pragma: no cover
        _SIDECAR_ERROR = exc
        _SIDECAR_READY.set()
        raise
    finally:  # pragma: no cover
        loop.stop()
        loop.close()


def _ensure_embedded_sidecar() -> str:
    global _SIDECAR_ERROR, _SIDECAR_URL

    explicit_url = _normalize_proxy_url(_env("CUBE_DEV_PROXY_URL", ""))
    if explicit_url:
        _SIDECAR_URL = explicit_url
        return explicit_url

    if _SIDECAR_URL:
        return _SIDECAR_URL

    with _SIDECAR_LOCK:
        if _SIDECAR_URL:
            return _SIDECAR_URL

        _SIDECAR_READY.clear()
        _SIDECAR_ERROR = None

        host = _env("CUBE_DEV_PROXY_HOST", "127.0.0.1")
        preferred_port = int(_env("CUBE_DEV_PROXY_PORT", "12580"))

        thread = threading.Thread(
            target=_run_embedded_sidecar,
            args=(host, preferred_port),
            name="cube-dev-sidecar",
            daemon=True,
        )
        thread.start()

    _SIDECAR_READY.wait(timeout=10)
    if _SIDECAR_ERROR is not None:
        raise RuntimeError("Embedded dev sidecar failed to start") from _SIDECAR_ERROR
    if not _SIDECAR_URL:
        raise RuntimeError("Embedded dev sidecar did not become ready in time")
    return _SIDECAR_URL


def _current_sidecar_url() -> str:
    return _ensure_embedded_sidecar().rstrip("/")


def _build_sandbox_file_url(
    sandbox_base,
    path: str,
    user: str | None = None,
    signature: str | None = None,
    signature_expiration: int | None = None,
) -> str:
    query = {"path": path} if path else {}
    if user:
        query["username"] = user
    if signature:
        query["signature"] = signature
    if signature_expiration:
        if signature is None:
            raise ValueError("signature_expiration requires signature to be set")
        query["signature_expiration"] = str(signature_expiration)

    return _build_router_url(
        _current_sidecar_url(),
        sandbox_base.sandbox_id,
        sandbox_base.connection_config.envd_port,
        "files",
        urlencode(query),
    )


def _build_code_interpreter_url(sandbox_base, port: int, tail: str = "") -> str:
    return _build_router_url(
        _current_sidecar_url(),
        sandbox_base.sandbox_id,
        port,
        tail,
    )


def setup_dev_sidecar() -> None:
    from e2b import ConnectionConfig
    from e2b.sandbox.main import SandboxBase

    global _PATCHED
    global _ORIGINAL_CONNECTION_CONFIG_GET_SANDBOX_URL
    global _ORIGINAL_SANDBOX_BASE_GET_HOST
    global _ORIGINAL_SANDBOX_BASE_FILE_URL
    global _ORIGINAL_SANDBOX_BASE_GET_MCP_URL
    global _ORIGINAL_CODE_INTERPRETER_JUPYTER_URL
    global _ORIGINAL_ASYNC_CODE_INTERPRETER_JUPYTER_URL

    base_url = _current_sidecar_url()

    if not _PATCHED:
        required_attrs = [
            (ConnectionConfig, "get_sandbox_url"),
            (SandboxBase, "get_host"),
            (SandboxBase, "_file_url"),
            (SandboxBase, "get_mcp_url"),
        ]
        for owner, attr in required_attrs:
            if not hasattr(owner, attr):
                warnings.warn(
                    f"{owner.__name__}.{attr} not found; the installed E2B SDK is incompatible "
                    "with this dev sidecar example.",
                    RuntimeWarning,
                    stacklevel=2,
                )
                return

        _ORIGINAL_CONNECTION_CONFIG_GET_SANDBOX_URL = ConnectionConfig.get_sandbox_url
        _ORIGINAL_SANDBOX_BASE_GET_HOST = SandboxBase.get_host
        _ORIGINAL_SANDBOX_BASE_FILE_URL = SandboxBase._file_url
        _ORIGINAL_SANDBOX_BASE_GET_MCP_URL = SandboxBase.get_mcp_url
        try:
            from e2b_code_interpreter.code_interpreter_sync import Sandbox as CodeInterpreterSandbox
            from e2b_code_interpreter.constants import JUPYTER_PORT
        except ImportError:
            CodeInterpreterSandbox = None
            JUPYTER_PORT = None
        try:
            from e2b_code_interpreter.code_interpreter_async import AsyncSandbox as AsyncCodeInterpreterSandbox
        except ImportError:
            AsyncCodeInterpreterSandbox = None

        def __connection_config_get_sandbox_url(self, sandbox_id: str, sandbox_domain: str) -> str:
            if self._sandbox_url:
                return self._sandbox_url
            return _build_router_url(_current_sidecar_url(), sandbox_id, self.envd_port)

        def __sandbox_base_get_host(self, port: int) -> str:
            return _build_router_host(_current_sidecar_url(), self.sandbox_id, port)

        def __sandbox_base_file_url(
            self,
            path: str,
            user: str | None = None,
            signature: str | None = None,
            signature_expiration: int | None = None,
        ) -> str:
            return _build_sandbox_file_url(
                self,
                path,
                user,
                signature,
                signature_expiration,
            )

        def __sandbox_base_get_mcp_url(self) -> str:
            return _build_router_url(
                _current_sidecar_url(),
                self.sandbox_id,
                self.mcp_port,
                "mcp",
            )

        def __code_interpreter_jupyter_url(self) -> str:
            return _build_code_interpreter_url(self, JUPYTER_PORT)

        ConnectionConfig.get_sandbox_url = __connection_config_get_sandbox_url
        SandboxBase.get_host = __sandbox_base_get_host
        SandboxBase._file_url = __sandbox_base_file_url
        SandboxBase.get_mcp_url = __sandbox_base_get_mcp_url
        if CodeInterpreterSandbox is not None and JUPYTER_PORT is not None:
            _ORIGINAL_CODE_INTERPRETER_JUPYTER_URL = CodeInterpreterSandbox._jupyter_url
            CodeInterpreterSandbox._jupyter_url = property(__code_interpreter_jupyter_url)
        if AsyncCodeInterpreterSandbox is not None and JUPYTER_PORT is not None:
            _ORIGINAL_ASYNC_CODE_INTERPRETER_JUPYTER_URL = AsyncCodeInterpreterSandbox._jupyter_url
            AsyncCodeInterpreterSandbox._jupyter_url = property(__code_interpreter_jupyter_url)
        _PATCHED = True

    os.environ["CUBE_DEV_PROXY_URL"] = base_url


def main() -> None:
    host = _env("CUBE_DEV_PROXY_HOST", "127.0.0.1")
    port = int(_env("CUBE_DEV_PROXY_PORT", "12580"))
    app = build_app()
    web.run_app(app, host=host, port=port)


if __name__ == "__main__":
    main()
