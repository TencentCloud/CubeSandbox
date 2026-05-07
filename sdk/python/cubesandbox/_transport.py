# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import ssl

import httpx

from ._config import Config


class IPOverrideTransport(httpx.HTTPTransport):
    """Routes all TCP connections to a fixed IP:port while preserving the Host header.

    Equivalent to ``curl --resolve host:port:ip``.
    Used when ``CUBE_PROXY_NODE_IP`` is set to bypass DNS resolution of ``*.cube.app``.
    """

    def __init__(self, ip: str, port: int, ssl_context: ssl.SSLContext | None = None, **kw):
        super().__init__(verify=ssl_context or True, **kw)
        self._ip = ip
        self._port = port

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        original_host = request.url.host
        url = request.url.copy_with(host=self._ip, port=self._port)
        proxied = httpx.Request(
            method=request.method,
            url=url,
            headers=[
                (k, original_host if k.lower() == "host" else v)
                for k, v in request.headers.raw
            ],
            content=request.content,
        )
        return super().handle_request(proxied)


def build_client(config: Config) -> httpx.Client:
    """Build an httpx client for sandbox stream requests.

    When ``config.proxy_node_ip`` is set, all connections are routed directly
    to that IP, bypassing DNS. The ``Host`` header retains the virtual hostname
    so CubeProxy can route to the correct sandbox.
    """
    if config.proxy_node_ip:
        transport = IPOverrideTransport(config.proxy_node_ip, config.proxy_port)
    else:
        transport = httpx.HTTPTransport()

    return httpx.Client(
        transport=transport,
        timeout=httpx.Timeout(connect=config.request_timeout, read=None, write=30, pool=30),
        follow_redirects=True,
    )
