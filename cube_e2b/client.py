"""
cube_e2b.client
~~~~~~~~~~~~~~~
Low-level HTTP client that talks to the CubeAPI (E2B-compatible REST API).
"""
from __future__ import annotations

import json
from typing import Any

import requests

from .config import SandboxConfig, get_default_config
from .exceptions import (
    ApiError,
    AuthenticationError,
    SandboxNotFoundError,
    TemplateNotFoundError,
)


def _raise_for_status(resp: requests.Response) -> None:
    """Translate HTTP error responses into SDK exceptions."""
    if resp.ok:
        return
    try:
        body = resp.json()
        message = body.get("message") or body.get("detail") or resp.text
    except Exception:
        message = resp.text or f"HTTP {resp.status_code}"

    code = resp.status_code
    if code == 401 or code == 403:
        raise AuthenticationError(message, status_code=code)
    if code == 404:
        if "template" in message.lower():
            raise TemplateNotFoundError(message, status_code=code)
        raise SandboxNotFoundError(message, status_code=code)
    raise ApiError(message, status_code=code)


class CubeApiClient:
    """Thin wrapper around CubeAPI REST endpoints."""

    def __init__(self, config: SandboxConfig | None = None) -> None:
        self._cfg = config or get_default_config()
        self._session = requests.Session()
        self._session.headers.update({
            "X-API-Key": self._cfg.api_key,
            "Content-Type": "application/json",
        })
        if self._cfg.ssl_cert_file:
            self._session.verify = self._cfg.ssl_cert_file

    # ------------------------------------------------------------------
    # Sandbox CRUD
    # ------------------------------------------------------------------

    def create_sandbox(
        self,
        template_id: str,
        timeout: int = 300,
        env_vars: dict[str, str] | None = None,
        metadata: dict[str, str] | None = None,
        **kwargs: Any,
    ) -> dict:
        """POST /sandboxes — create a new sandbox."""
        payload: dict[str, Any] = {"templateID": template_id, "timeout": timeout}
        if env_vars:
            payload["envVars"] = env_vars
        if metadata:
            payload["metadata"] = metadata
        payload.update(kwargs)

        resp = self._session.post(f"{self._cfg.api_url}/sandboxes", json=payload)
        _raise_for_status(resp)
        return resp.json()

    def get_sandbox(self, sandbox_id: str) -> dict:
        """GET /sandboxes/{sandboxID}"""
        resp = self._session.get(f"{self._cfg.api_url}/sandboxes/{sandbox_id}")
        _raise_for_status(resp)
        return resp.json()

    def list_sandboxes(self) -> list[dict]:
        """GET /sandboxes"""
        resp = self._session.get(f"{self._cfg.api_url}/sandboxes")
        _raise_for_status(resp)
        return resp.json()

    def delete_sandbox(self, sandbox_id: str) -> None:
        """DELETE /sandboxes/{sandboxID}"""
        resp = self._session.delete(f"{self._cfg.api_url}/sandboxes/{sandbox_id}")
        _raise_for_status(resp)

    def refresh_sandbox(self, sandbox_id: str, timeout: int = 300) -> None:
        """POST /sandboxes/{sandboxID}/timeout"""
        resp = self._session.post(
            f"{self._cfg.api_url}/sandboxes/{sandbox_id}/timeout",
            json={"timeout": timeout},
        )
        _raise_for_status(resp)

    def pause_sandbox(self, sandbox_id: str) -> None:
        """POST /sandboxes/{sandboxID}/pause"""
        resp = self._session.post(f"{self._cfg.api_url}/sandboxes/{sandbox_id}/pause")
        _raise_for_status(resp)

    def resume_sandbox(self, sandbox_id: str, timeout: int = 300) -> None:
        """POST /sandboxes/{sandboxID}/resume"""
        resp = self._session.post(
            f"{self._cfg.api_url}/sandboxes/{sandbox_id}/resume",
            json={"timeout": timeout},
        )
        _raise_for_status(resp)
