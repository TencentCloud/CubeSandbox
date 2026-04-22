from __future__ import annotations

import asyncio
import os
import threading
from typing import Iterable
from urllib.parse import urlsplit

from aiohttp import ClientSession, ClientTimeout, TCPConnector, web


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

_PATCHED = False
_ORIGINAL_GET_HOST = None
_SIDECAR_LOCK = threading.Lock()
_SIDECAR_READY = threading.Event()
_SIDECAR_URL = ""
_SIDECAR_ERROR: BaseException | None = None


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _bool_env(name: str, default: bool) -> bool:
    value = _env(name)
    if not value:
        return default
    return value.lower() in {"1", "true", "yes", "on"}


def _strip_slash(value: str) -> str:
    return value.rstrip("/")


def _normalize_proxy_host(value: str) -> str:
    raw = value.strip().rstrip("/")
    if not raw:
        return ""

    parsed = urlsplit(raw if "://" in raw else f"http://{raw}")
    host = parsed.netloc or parsed.path
    path = parsed.path if parsed.netloc else ""
    return f"{host}{path}".rstrip("/")


def _join_url(base: str, path: str, query: str) -> str:
    url = f"{_strip_slash(base)}{path}"
    if query:
        url = f"{url}?{query}"
    return url


def _copy_headers(headers: Iterable[tuple[str, str]], *, host: str | None) -> dict[str, str]:
    copied: dict[str, str] = {}
    for key, value in headers:
        if key.lower() in HOP_BY_HOP_HEADERS:
            continue
        if key.lower() == "host":
            continue
        copied[key] = value
    if host:
        copied["Host"] = host
    return copied


async def _stream_proxy(
    request: web.Request,
    session: ClientSession,
    target_url: str,
    *,
    host: str | None,
) -> web.StreamResponse:
    body = await request.read()
    headers = _copy_headers(request.headers.items(), host=host)

    async with session.request(
        method=request.method,
        url=target_url,
        headers=headers,
        data=body if body else None,
        allow_redirects=False,
    ) as upstream:
        response = web.StreamResponse(status=upstream.status, reason=upstream.reason)
        for key, value in upstream.headers.items():
            if key.lower() in HOP_BY_HOP_HEADERS:
                continue
            if key.lower() == "content-length":
                continue
            response.headers[key] = value

        await response.prepare(request)
        async for chunk in upstream.content.iter_chunked(64 * 1024):
            await response.write(chunk)
        await response.write_eof()
        return response


async def proxy_sandbox(request: web.Request) -> web.StreamResponse:
    config = request.app["config"]
    sandbox_id = request.match_info["sandbox_id"]
    port = request.match_info["port"]
    tail = request.match_info.get("tail", "")
    upstream_path = f"/{tail}" if tail else "/"
    url = _join_url(config["cube_proxy_base"], upstream_path, request.query_string)
    host = f"{port}-{sandbox_id}.{config['sandbox_domain']}"
    return await _stream_proxy(request, request.app["session"], url, host=host)


async def health(_request: web.Request) -> web.Response:
    return web.json_response({"ok": True})


async def on_startup(app: web.Application) -> None:
    config = app["config"]
    connector = TCPConnector(ssl=config["verify_ssl"])
    timeout = ClientTimeout(total=None, connect=30, sock_read=None)
    app["session"] = ClientSession(connector=connector, timeout=timeout)


async def on_cleanup(app: web.Application) -> None:
    await app["session"].close()


def build_app() -> web.Application:
    cube_proxy_base = _strip_slash(_env("CUBE_REMOTE_PROXY_BASE", "https://127.0.0.1:11443"))
    sandbox_domain = _env("CUBE_REMOTE_SANDBOX_DOMAIN", "cube.app")
    verify_ssl = _bool_env("CUBE_REMOTE_PROXY_VERIFY_SSL", False)

    app = web.Application(client_max_size=64 * 1024 * 1024)
    app["config"] = {
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

        sockets = getattr(site._server, "sockets", None)
        if not sockets:
            raise RuntimeError("Embedded dev sidecar started without a bound socket")
        return int(sockets[0].getsockname()[1])

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
    except BaseException as exc:  # pragma: no cover
        _SIDECAR_ERROR = exc
        _SIDECAR_READY.set()
        raise
    finally:  # pragma: no cover
        loop.stop()
        loop.close()


def _ensure_embedded_sidecar() -> str:
    global _SIDECAR_ERROR

    explicit_url = _env("CUBE_DEV_PROXY_URL", "").rstrip("/")
    if explicit_url:
        return explicit_url if "://" in explicit_url else f"http://{explicit_url}"

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


def setup_dev_sidecar() -> None:
    from e2b import ConnectionConfig

    global _PATCHED, _ORIGINAL_GET_HOST

    base_url = _ensure_embedded_sidecar().rstrip("/")
    normalized_host = _normalize_proxy_host(base_url)

    if not _PATCHED:
        _ORIGINAL_GET_HOST = ConnectionConfig.get_host

        def __connection_config_get_host(
            self,
            sandbox_id: str,
            sandbox_domain: str,
            port: int,
        ) -> str:
            current_proxy = _normalize_proxy_host(_ensure_embedded_sidecar())
            if not current_proxy:
                return _ORIGINAL_GET_HOST(self, sandbox_id, sandbox_domain, port)
            return f"{current_proxy}/sandboxes/router/{sandbox_id}/{port}"

        ConnectionConfig.get_host = __connection_config_get_host
        _PATCHED = True

    os.environ["CUBE_DEV_PROXY_URL"] = base_url
    os.environ["E2B_DEBUG"] = "true"
    os.environ["E2B_DOMAIN"] = normalized_host


def main() -> None:
    host = _env("CUBE_DEV_PROXY_HOST", "127.0.0.1")
    port = int(_env("CUBE_DEV_PROXY_PORT", "12580"))
    app = build_app()
    web.run_app(app, host=host, port=port)


if __name__ == "__main__":
    main()
