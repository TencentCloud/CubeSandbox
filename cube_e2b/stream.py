"""
cube_e2b.stream
~~~~~~~~~~~~~~~
Data-stream helpers: HTTP and WebSocket access to sandbox services.

Key feature: when CUBE_PROXY_NODE_IP is set, DNS is bypassed entirely.
The SDK connects directly to the proxy IP and passes the virtual hostname
via the HTTP Host header (HTTP/WS) or TLS SNI (HTTPS/WSS).
"""
from __future__ import annotations

import socket
import ssl
from typing import Generator
from urllib.parse import urlparse

import requests
from requests.adapters import HTTPAdapter

from .config import SandboxConfig, get_default_config


# ---------------------------------------------------------------------------
# DNS-bypass HTTP adapter for requests
# ---------------------------------------------------------------------------

class _HostOverrideAdapter(HTTPAdapter):
    """HTTPAdapter that routes all connections to a fixed (ip, port) pair
    while preserving the original Host header for virtual-host routing."""

    def __init__(self, dest_ip: str, dest_port: int, **kwargs):
        self._dest_ip = dest_ip
        self._dest_port = dest_port
        super().__init__(**kwargs)

    def send(self, request, **kwargs):
        # Rewrite the URL so requests connects to the real IP/port,
        # but keep the Host header intact for CubeProxy routing.
        parsed = urlparse(request.url)
        original_host = parsed.hostname  # e.g. "49999-xxxx.cube.app"
        scheme = parsed.scheme           # "http" or "https"

        # Build new URL pointing at the proxy IP
        new_url = request.url.replace(
            f"{scheme}://{parsed.netloc}",
            f"{scheme}://{self._dest_ip}:{self._dest_port}",
        )
        request.url = new_url

        # Restore Host header so CubeProxy knows which sandbox to route to
        request.headers["Host"] = original_host
        return super().send(request, **kwargs)


# ---------------------------------------------------------------------------
# Public helpers
# ---------------------------------------------------------------------------

def build_stream_session(config: SandboxConfig | None = None) -> requests.Session:
    """Return a requests.Session pre-configured for sandbox stream access.

    If ``CUBE_PROXY_NODE_IP`` is set the session bypasses DNS and connects
    directly to the proxy IP.
    """
    cfg = config or get_default_config()
    session = requests.Session()
    if cfg.ssl_cert_file:
        session.verify = cfg.ssl_cert_file

    if cfg.proxy_node_ip:
        session.mount(
            "http://",
            _HostOverrideAdapter(cfg.proxy_node_ip, cfg.proxy_port_http),
        )
        session.mount(
            "https://",
            _HostOverrideAdapter(cfg.proxy_node_ip, cfg.proxy_port_https),
        )
    return session


def http_get(
    url: str,
    *,
    config: SandboxConfig | None = None,
    stream: bool = False,
    **kwargs,
) -> requests.Response:
    """GET *url* via a stream session (respects CUBE_PROXY_NODE_IP)."""
    session = build_stream_session(config)
    return session.get(url, stream=stream, **kwargs)


def http_post(
    url: str,
    *,
    config: SandboxConfig | None = None,
    **kwargs,
) -> requests.Response:
    """POST *url* via a stream session (respects CUBE_PROXY_NODE_IP)."""
    session = build_stream_session(config)
    return session.post(url, **kwargs)


def iter_sse(
    url: str,
    *,
    config: SandboxConfig | None = None,
    **kwargs,
) -> Generator[str, None, None]:
    """Yield Server-Sent Event *data* lines from *url*.

    Example::

        for line in iter_sse("http://49999-xxx.cube.app/events"):
            print(line)
    """
    resp = http_get(url, config=config, stream=True, **kwargs)
    resp.raise_for_status()
    for raw in resp.iter_lines(decode_unicode=True):
        if raw.startswith("data:"):
            yield raw[5:].strip()


def websocket_connect(
    host: str,
    path: str = "/",
    *,
    use_tls: bool = False,
    config: SandboxConfig | None = None,
):
    """Return a connected ``websockets`` client (sync wrapper via asyncio).

    Parameters
    ----------
    host:
        Virtual hostname, e.g. ``"49999-xxxx.cube.app"``.
    path:
        URL path, default ``"/"``.
    use_tls:
        Use ``wss://`` instead of ``ws://``.
    config:
        Optional :class:`~cube_e2b.config.SandboxConfig`.

    Returns a ``websockets.sync.client.ClientConnection`` (websockets ≥ 12).
    """
    try:
        from websockets.sync.client import connect as ws_connect  # websockets >= 12
    except ImportError:
        raise ImportError(
            "websockets >= 12 is required for WebSocket support. "
            "Install it with: pip install 'websockets>=12'"
        )

    cfg = config or get_default_config()
    scheme = "wss" if use_tls else "ws"
    uri = f"{scheme}://{host}{path}"

    extra_headers = {}
    ssl_context = None

    if cfg.proxy_node_ip:
        # Connect directly to the proxy IP; pass Host header for virtual routing
        port = cfg.proxy_port_https if use_tls else cfg.proxy_port_http
        sock = socket.create_connection((cfg.proxy_node_ip, port))

        if use_tls:
            ssl_context = ssl.create_default_context()
            if cfg.ssl_cert_file:
                ssl_context.load_verify_locations(cfg.ssl_cert_file)
            # SNI must match the virtual hostname
            sock = ssl_context.wrap_socket(sock, server_hostname=host)

        extra_headers["Host"] = host
        return ws_connect(
            uri,
            sock=sock,
            additional_headers=extra_headers,
            ssl=ssl_context if use_tls else None,
        )

    # Normal path — let the OS resolve DNS
    if use_tls and cfg.ssl_cert_file:
        ssl_context = ssl.create_default_context()
        ssl_context.load_verify_locations(cfg.ssl_cert_file)

    return ws_connect(uri, ssl=ssl_context if use_tls else None)
