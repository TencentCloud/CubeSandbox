# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from typing import TYPE_CHECKING

from ._commands import DEFAULT_ENVD_USER, ENVD_PORT

if TYPE_CHECKING:
    from .sandbox import Sandbox


class Filesystem:
    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox

    def read(self, path: str, *, user: str | None = None) -> str:
        """Read a file through envd's HTTP file API."""
        if self._sandbox._client is None:
            self._sandbox._client = self._sandbox._build_data_client()
        effective_user = user or DEFAULT_ENVD_USER

        headers = {}
        access_token = self._sandbox._data.get("envdAccessToken")
        if access_token:
            headers["X-Access-Token"] = access_token

        resp = self._sandbox._client.get(
            f"http://{self._sandbox.get_host(ENVD_PORT)}/files",
            params={"path": path, "username": effective_user},
            headers=headers,
        )
        if resp.status_code != 200:
            message = resp.text or f"HTTP {resp.status_code}"
            try:
                body = resp.json()
                message = body.get("message") or body.get("detail") or message
            except Exception:  # noqa: BLE001 - best-effort error extraction
                pass
            raise IOError(f"Failed to read {path}: {message}")
        return resp.text

    def write(self, path: str, data: str | bytes, *, user: str | None = None) -> None:
        """Write a file through envd's HTTP file API."""
        if self._sandbox._client is None:
            self._sandbox._client = self._sandbox._build_data_client()
        effective_user = user or DEFAULT_ENVD_USER

        headers = {"Content-Type": "application/octet-stream"}
        access_token = self._sandbox._data.get("envdAccessToken")
        if access_token:
            headers["X-Access-Token"] = access_token

        body = data.encode("utf-8") if isinstance(data, str) else data
        params = {"path": path, "username": effective_user}
        resp = self._sandbox._client.post(
            f"http://{self._sandbox.get_host(ENVD_PORT)}/files",
            params=params,
            headers=headers,
            content=body,
        )
        if resp.status_code >= 400:
            multipart_headers = {}
            if access_token:
                multipart_headers["X-Access-Token"] = access_token
            resp = self._sandbox._client.post(
                f"http://{self._sandbox.get_host(ENVD_PORT)}/files",
                params=params,
                headers=multipart_headers,
                files={"file": (path, body)},
            )
        if resp.status_code >= 400:
            message = resp.text or f"HTTP {resp.status_code}"
            try:
                payload = resp.json()
                message = payload.get("message") or payload.get("detail") or message
            except Exception:  # noqa: BLE001 - best-effort error extraction
                pass
            raise IOError(f"Failed to write {path}: {message}")
