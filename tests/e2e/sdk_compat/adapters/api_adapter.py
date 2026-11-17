# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import requests

from framework.auth import auth_headers
from framework.config import SdkE2EConfig
from framework.volume_hints import volume_plugin_misconfigured_hint

MAX_V2_LIST_PAGES = 100

# Synchronous server-side work; these exceed the short metadata timeout
# used elsewhere in this client.
SNAPSHOT_REQUEST_TIMEOUT_S = 280.0
FORK_REQUEST_TIMEOUT_S = 1600.0


class ApiClient:
    """REST calls the SDK does not expose, plus cleanup and diagnostics."""

    def __init__(self, config: SdkE2EConfig) -> None:
        self._config = config
        self._session = requests.Session()
        self._session.headers.update(auth_headers())

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

    def list_sandboxes_v2_raw_query(self, raw_query: str) -> requests.Response:
        """One ``GET /v2/sandboxes`` page with a hand-built query string, bypassing ``params``."""
        return self._session.get(
            f"{self._config.cube_api_url}/v2/sandboxes?{raw_query}",
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

    def list_snapshots(self, sandbox_id: str | None = None) -> list[dict]:
        params = {"limit": 100}
        if sandbox_id:
            params["sandboxID"] = sandbox_id
        resp = self._session.get(
            f"{self._config.cube_api_url}/snapshots",
            params=params,
            timeout=self._config.api_timeout,
        )
        resp.raise_for_status()
        return resp.json()

    def create_snapshot(
        self,
        sandbox_id: str,
        *,
        mode: str | None = None,
        name: str | None = None,
    ) -> tuple[int, dict]:
        """POST /sandboxes/{id}/snapshots. Returns (status, body).

        The status is returned instead of raised so negative cases can assert
        the exact rejection code and the stable error identifier in the body.
        ``fork`` takes no ``mode`` argument in the SDK, and negative cases need
        the raw status, so snapshot-mode cases use this REST surface.
        """
        payload: dict = {}
        if mode is not None:
            payload["mode"] = mode
        if name is not None:
            payload["name"] = name
        resp = self._session.post(
            f"{self._config.cube_api_url}/sandboxes/{sandbox_id}/snapshots",
            json=payload,
            timeout=max(self._config.api_timeout, SNAPSHOT_REQUEST_TIMEOUT_S),
        )
        return resp.status_code, _json_body(resp)

    def fork_sandbox(
        self,
        sandbox_id: str,
        *,
        count: int = 1,
        mode: str | None = None,
        timeout: int | None = None,
    ) -> tuple[int, dict | list]:
        """POST /sandboxes/{id}/fork. Returns (status, body).

        A rejected fork answers with an ``ApiError`` object, so the body is
        returned unchanged: the caller needs the message to say why.
        """
        payload: dict = {"count": count}
        if mode is not None:
            payload["mode"] = mode
        if timeout is not None:
            payload["timeout"] = timeout
        resp = self._session.post(
            f"{self._config.cube_api_url}/sandboxes/{sandbox_id}/fork",
            json=payload,
            timeout=max(self._config.api_timeout, FORK_REQUEST_TIMEOUT_S),
        )
        return resp.status_code, _json_body(resp)

    def rollback_sandbox(
        self,
        sandbox_id: str,
        snapshot_id: str,
    ) -> tuple[int, dict]:
        """POST /sandboxes/{id}/rollback. Returns (status, body)."""
        resp = self._session.post(
            f"{self._config.cube_api_url}/sandboxes/{sandbox_id}/rollback",
            json={"snapshotID": snapshot_id},
            timeout=max(self._config.api_timeout, SNAPSHOT_REQUEST_TIMEOUT_S),
        )
        return resp.status_code, _json_body(resp)

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


def _json_body(resp: requests.Response) -> dict | list:
    try:
        return resp.json()
    except ValueError:
        return {"message": resp.text}
