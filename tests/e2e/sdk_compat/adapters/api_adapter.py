# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import requests

from framework.config import SdkE2EConfig


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

    def close(self) -> None:
        self._session.close()
