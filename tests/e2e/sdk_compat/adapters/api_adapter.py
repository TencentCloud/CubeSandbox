# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import requests

from framework.config import SdkE2EConfig
from framework.volume_hints import volume_plugin_misconfigured_hint


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

    def list_sandboxes(self) -> list[dict]:
        resp = self._session.get(
            f"{self._config.cube_api_url}/sandboxes",
            timeout=self._config.api_timeout,
        )
        resp.raise_for_status()
        payload = resp.json()
        return payload if isinstance(payload, list) else []

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
