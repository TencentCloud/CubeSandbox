# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Minimal streamable-HTTP MCP client for gateway E2E cases.

Speaks just enough JSON-RPC (initialize, tools/list, tools/call) to verify the
in-sandbox mcp-gateway without adding an MCP SDK dependency to the suite.
"""

from __future__ import annotations

import itertools
import json
from typing import Any
from urllib.parse import urlsplit

import httpx

from framework.config import SdkE2EConfig

PROTOCOL_VERSION = "2025-06-18"


class McpError(RuntimeError):
    pass


def route(url: str, config: SdkE2EConfig) -> tuple[str, dict[str, str]]:
    """Return the URL to dial and extra headers for a public sandbox URL.

    With ``CUBE_PROXY_NODE_IP`` set, dial CubeProxy directly and keep the
    virtual sandbox hostname in ``Host`` (same as the SDK transports).
    """
    if not config.cube_proxy_node_ip:
        return url, {}
    parts = urlsplit(url)
    target = f"http://{config.cube_proxy_node_ip}:{config.cube_proxy_port_http}{parts.path or '/'}"
    return target, {"Host": parts.netloc}


class McpHttpClient:
    def __init__(self, url: str, token: str | None, config: SdkE2EConfig, *, timeout: float = 60) -> None:
        self._url, headers = route(url, config)
        headers["Accept"] = "application/json, text/event-stream"
        if token is not None:
            headers["Authorization"] = f"Bearer {token}"
        self._client = httpx.Client(headers=headers, timeout=timeout)
        self._ids = itertools.count(1)
        self._session_id: str | None = None

    def __enter__(self) -> "McpHttpClient":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()

    def close(self) -> None:
        self._client.close()

    def post_raw(self, payload: dict[str, Any]) -> httpx.Response:
        headers = {"Content-Type": "application/json"}
        if self._session_id:
            headers["Mcp-Session-Id"] = self._session_id
            headers["Mcp-Protocol-Version"] = PROTOCOL_VERSION
        return self._client.post(self._url, json=payload, headers=headers)

    def request(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        request_id = next(self._ids)
        response = self.post_raw({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params or {}})
        if response.status_code != 200:
            raise McpError(f"{method}: HTTP {response.status_code} {response.text[:300]!r}")
        self._session_id = response.headers.get("Mcp-Session-Id", self._session_id)
        message = _find_response(response, request_id)
        if "error" in message:
            raise McpError(f"{method}: {message['error']}")
        return message["result"]

    def initialize(self) -> dict[str, Any]:
        result = self.request(
            "initialize",
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "cube-sdk-e2e", "version": "1"},
            },
        )
        response = self.post_raw({"jsonrpc": "2.0", "method": "notifications/initialized"})
        if response.status_code not in (200, 202):
            raise McpError(f"notifications/initialized: HTTP {response.status_code}")
        return result

    def list_tools(self) -> list[dict[str, Any]]:
        tools: list[dict[str, Any]] = []
        cursor = None
        while True:
            result = self.request("tools/list", {"cursor": cursor} if cursor else {})
            tools.extend(result.get("tools", []))
            cursor = result.get("nextCursor")
            if not cursor:
                return tools

    def call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        return self.request("tools/call", {"name": name, "arguments": arguments})


def tool_text(result: dict[str, Any]) -> str:
    return "".join(item.get("text", "") for item in result.get("content", []) if item.get("type") == "text")


def _find_response(response: httpx.Response, request_id: int) -> dict[str, Any]:
    content_type = response.headers.get("content-type", "")
    if content_type.startswith("application/json"):
        messages = [response.json()]
    else:
        messages = [
            json.loads(line[len("data:"):].strip())
            for line in response.text.splitlines()
            if line.startswith("data:") and line[len("data:"):].strip()
        ]
    for message in messages:
        if isinstance(message, dict) and message.get("id") == request_id:
            return message
    raise McpError(f"no JSON-RPC response for id {request_id} in {response.text[:300]!r}")
