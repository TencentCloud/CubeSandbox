# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import requests

from framework.config import SdkE2EConfig
from framework.volume_hints import volume_plugin_misconfigured_hint

MAX_V2_LIST_PAGES = 100


class ApiClient:
    """Small REST client used for cleanup and diagnostics, not as a primary user path."""

    def __init__(self, config: SdkE2EConfig) -> None:
        self._config = config
        self._session = requests.Session()
        api_key = os.environ.get("CUBE_API_KEY")
        if api_key:
            self._session.headers.update({"Authorization": f"Bearer {api_key}"})

    def health(self) -> dict:
        resp = self._session.get(
            f"{self._config.cube_api_url}/health",
            timeout=self._config.api_timeout,
        )
        resp.raise_for_status()
        return resp.json()

    def get_template(self, template_id: str) -> dict:
        resp = self._session.get(
            f"{self._config.cube_api_url}/templates/{template_id}",
            timeout=self._config.api_timeout,
        )
        if resp.status_code == 404:
            return {}
        resp.raise_for_status()
        return resp.json()

    def delete_sandbox(self, sandbox_id: str) -> None:
        resp = self._session.delete(
            f"{self._config.cube_api_url}/sandboxes/{sandbox_id}",
            timeout=self._config.api_timeout,
        )
        if resp.status_code not in (200, 202, 204, 404):
            raise RuntimeError(f"failed to delete sandbox {sandbox_id}: HTTP {resp.status_code} {resp.text}")

    def get_sandbox(self, sandbox_id: str) -> dict:
        resp = self._session.get(
            f"{self._config.cube_api_url}/sandboxes/{sandbox_id}",
            timeout=self._config.api_timeout,
        )
        if resp.status_code == 404:
            return {}
        resp.raise_for_status()
        return resp.json()

    def list_sandboxes(self, metadata: str | None = None) -> list[dict]:
        resp = self._session.get(
            f"{self._config.cube_api_url}/sandboxes",
            params={"metadata": metadata} if metadata is not None else None,
            timeout=self._config.api_timeout,
        )
        resp.raise_for_status()
        payload = resp.json()
        return payload if isinstance(payload, list) else []

    def list_sandboxes_raw(self, metadata: str) -> requests.Response:
        """One ``GET /sandboxes`` response, unparsed, so tests can assert on 4xx."""
        return self._session.get(
            f"{self._config.cube_api_url}/sandboxes",
            params={"metadata": metadata},
            timeout=self._config.api_timeout,
        )

    def list_sandboxes_v2_raw(self, params: dict[str, str] | None = None) -> requests.Response:
        """One ``GET /v2/sandboxes`` page, unparsed, so tests can assert on 4xx."""
        return self._session.get(
            f"{self._config.cube_api_url}/v2/sandboxes",
            params=params,
            timeout=self._config.api_timeout,
        )

    def list_sandboxes_v2(self, params: dict[str, str] | None = None) -> list[dict]:
        """Every ``GET /v2/sandboxes`` page, following ``x-next-token``."""
        query = {"limit": "1000", **(params or {})}
        entries: list[dict] = []
        for _ in range(MAX_V2_LIST_PAGES):
            resp = self.list_sandboxes_v2_raw(query)
            resp.raise_for_status()
            payload = resp.json()
            entries.extend(payload if isinstance(payload, list) else [])
            token = resp.headers.get("x-next-token")
            if not token:
                return entries
            query["nextToken"] = token
        raise RuntimeError(f"GET /v2/sandboxes exceeded {MAX_V2_LIST_PAGES} pages")

    def create_volume(self, name: str = "", *, driver: str | None = None) -> dict:
        payload: dict = {"name": name}
        if driver:
            payload["driver"] = driver
        resp = self._session.post(
            f"{self._config.cube_api_url}/volumes",
            json=payload,
            timeout=self._config.api_timeout,
        )
        if resp.status_code != 201:
            raise RuntimeError(
                f"failed to create volume name={name!r}: "
                f"HTTP {resp.status_code} {resp.text}. "
                f"{volume_plugin_misconfigured_hint()}"
            )
        body = resp.json()
        if not isinstance(body, dict):
            raise RuntimeError(f"create volume returned non-object body: {body!r}")
        return body

    def list_volumes(self) -> list[dict]:
        resp = self._session.get(
            f"{self._config.cube_api_url}/volumes",
            timeout=self._config.api_timeout,
        )
        resp.raise_for_status()
        payload = resp.json()
        return payload if isinstance(payload, list) else []

    def get_volume(self, volume_id: str) -> tuple[int, dict]:
        resp = self._session.get(
            f"{self._config.cube_api_url}/volumes/{volume_id}",
            timeout=self._config.api_timeout,
        )
        if resp.status_code == 404:
            return 404, {}
        if resp.status_code == 200:
            body = resp.json()
            return 200, body if isinstance(body, dict) else {}
        raise RuntimeError(
            f"failed to get volume {volume_id}: "
            f"HTTP {resp.status_code} {resp.text}. "
            f"{volume_plugin_misconfigured_hint()}"
        )

    def delete_volume(self, volume_id: str) -> int:
        """DELETE /volumes/{id}. Returns HTTP status (204 / 409 / 404 / …)."""
        resp = self._session.delete(
            f"{self._config.cube_api_url}/volumes/{volume_id}",
            timeout=self._config.api_timeout,
        )
        return resp.status_code

    def close(self) -> None:
        self._session.close()
