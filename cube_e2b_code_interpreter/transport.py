"""
cube_e2b_code_interpreter.transport
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
HTTP transport with optional DNS bypass (CUBE_PROXY_NODE_IP).

DNS 对比：
    e2b cloud：
        get_host(49999) → "49999-<id>.e2b.app"
        httpx 直接解析 DNS，e2b cloud 负责 *.e2b.app 泛解析

    CubeSandbox 本机：
        get_host(49999) → "49999-<id>.cube.app"
        CoreDNS 监听 127.0.0.54:53，本机自动解析 *.cube.app → 127.0.0.1

    CubeSandbox 远程客户端（CUBE_PROXY_NODE_IP=9.135.79.34）：
        TCP 直连 9.135.79.34:80
        HTTP Host 头: 49999-<id>.cube.app
        CubeProxy 按 Host 路由 → 不需要 DNS
"""
from __future__ import annotations

import socket
import ssl
from typing import Optional

import httpx
from .config import SandboxConfig


class IPOverrideTransport(httpx.HTTPTransport):
    """
    httpx Transport：把所有连接重定向到固定 (ip, port)，
    同时保留 Host 头供 CubeProxy 虚拟主机路由。

    等同于 curl --resolve 或 requests 的自定义 HTTPAdapter。
    """

    def __init__(self, dest_ip: str, dest_port: int, ssl_context=None, **kwargs):
        verify = ssl_context if ssl_context is not None else True
        super().__init__(verify=verify, **kwargs)
        self._dest_ip = dest_ip
        self._dest_port = dest_port

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        # 强制替换 URL 中的 host:port 为代理 IP:port
        # Host 头已由 httpx 从原始 URL 设置好，保留不变让 CubeProxy 路由
        original_host = request.url.host
        url = request.url.copy_with(host=self._dest_ip, port=self._dest_port)
        new_request = httpx.Request(
            method=request.method,
            url=url,
            headers=[(k, original_host if k.lower() == "host" else v)
                     for k, v in request.headers.raw],
            content=request.content,
        )
        return super().handle_request(new_request)


def build_httpx_client(config: SandboxConfig) -> httpx.Client:
    """
    构建用于访问 sandbox 服务（通过 CubeProxy）的 httpx.Client。

    当 CUBE_PROXY_NODE_IP 未设置时：
        行为与原版 e2b_code_interpreter 完全相同——由 OS DNS 解析域名。

    当 CUBE_PROXY_NODE_IP 已设置时：
        使用 IPOverrideTransport，所有请求直连代理 IP，
        Host 头自动保留虚拟主机名（CubeProxy 用来路由 sandbox）。
    """
    ssl_context = None
    if config.ssl_cert_file:
        ssl_context = ssl.create_default_context()
        ssl_context.load_verify_locations(config.ssl_cert_file)

    if config.proxy_node_ip:
        transport = IPOverrideTransport(
            dest_ip=config.proxy_node_ip,
            dest_port=config.proxy_port_http,
            ssl_context=ssl_context,
        )

    else:
        transport = httpx.HTTPTransport(verify=ssl_context if ssl_context else True)

    return httpx.Client(
        transport=transport,
        timeout=httpx.Timeout(
            connect=config.request_timeout,
            read=None,   # 流式读取不设超时
            write=config.request_timeout,
            pool=config.request_timeout,
        ),
        follow_redirects=True,
    )
