# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
from __future__ import annotations

from typing import Any, Callable, Dict, Optional

import httpx
import requests

from ._config import Config
from ._exceptions import ApiError, AuthenticationError, CubeSandboxError, SandboxNotFoundError, TemplateNotFoundError
from ._models import Context, Execution, ExecutionError, OutputMessage, Result
from ._stream import _parse_line
from ._transport import build_client

JUPYTER_PORT = 49999


def _check_response(resp: requests.Response) -> None:
    if resp.ok:
        return
    try:
        msg = resp.json().get("message") or resp.json().get("detail") or resp.text
    except Exception:
        msg = resp.text or f"HTTP {resp.status_code}"
    code = resp.status_code
    if code in (401, 403):
        raise AuthenticationError(msg, code)
    if code == 404:
        raise (TemplateNotFoundError if "template" in msg.lower() else SandboxNotFoundError)(msg, code)
    raise ApiError(msg, code)


class Sandbox:
    """A CubeSandbox code execution environment.

    Example::

        with Sandbox.create() as sb:
            sb.run_code("x = 1")
            result = sb.run_code("x + 1")
            print(result.text)   # "2"
    """

    def __init__(self, data: dict, config: Config) -> None:
        self._data = data
        self._config = config
        self._session = self._build_session()
        self._client: httpx.Client | None = None

    # ── properties ───────────────────────────────────────────────────

    @property
    def sandbox_id(self) -> str:
        return self._data["sandboxID"]

    @property
    def template_id(self) -> str:
        return self._data["templateID"]

    @property
    def domain(self) -> str:
        return self._data.get("domain") or self._config.sandbox_domain

    def get_host(self, port: int) -> str:
        """Return the virtual hostname for a sandbox port, e.g. ``49999-<id>.cube.app``."""
        return f"{port}-{self.sandbox_id}.{self.domain}"

    # ── factory methods ───────────────────────────────────────────────

    @classmethod
    def create(
        cls,
        template: str | None = None,
        *,
        timeout: int | None = None,
        env_vars: Dict[str, str] | None = None,
        metadata: Dict[str, str] | None = None,
        config: Config | None = None,
        **kwargs: Any,
    ) -> "Sandbox":
        """Create a new sandbox.

        Args:
            template: Template ID. Falls back to ``CUBE_TEMPLATE_ID`` env var.
            timeout: Sandbox TTL in seconds. Defaults to ``Config.timeout`` (300).
            env_vars: Environment variables injected into the sandbox.
            metadata: Arbitrary key-value metadata.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A running :class:`Sandbox` instance.
        """
        cfg = config or Config()
        tpl = template or cfg.template_id
        if not tpl:
            raise ValueError("template is required. Set CUBE_TEMPLATE_ID or pass template=")

        payload: dict = {"templateID": tpl, "timeout": timeout or cfg.timeout}
        if env_vars:
            payload["envVars"] = env_vars
        if metadata:
            payload["metadata"] = metadata
        payload.update(kwargs)

        s = requests.Session()
        resp = s.post(f"{cfg.api_url}/sandboxes", json=payload,
                      headers={"Content-Type": "application/json"})
        _check_response(resp)
        return cls(resp.json(), config=cfg)

    @classmethod
    def connect(cls, sandbox_id: str, *, config: Config | None = None) -> "Sandbox":
        """Connect to an existing sandbox, resuming it if paused."""
        cfg = config or Config()
        s = requests.Session()
        resp = s.post(f"{cfg.api_url}/sandboxes/{sandbox_id}/connect",
                      json={"timeout": cfg.timeout},
                      headers={"Content-Type": "application/json"})
        _check_response(resp)
        return cls(resp.json(), config=cfg)

    # ── class-level API methods ───────────────────────────────────────

    @classmethod
    def list(cls, config: Config | None = None) -> list[dict]:
        """Return all running sandboxes from the v1 API.

        Calls ``GET /sandboxes`` and returns the JSON response as a list of
        sandbox dicts (each dict contains at least ``sandboxID`` and
        ``templateID`` keys).

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A list of sandbox info dicts.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/sandboxes")
        _check_response(resp)
        return resp.json()

    @classmethod
    def list_v2(cls, config: Config | None = None) -> list[dict]:
        """Return all running sandboxes from the v2 API.

        Calls ``GET /v2/sandboxes`` and returns the JSON response as a list of
        sandbox dicts.

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A list of sandbox info dicts.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/v2/sandboxes")
        _check_response(resp)
        return resp.json()

    @classmethod
    def health(cls, config: Config | None = None) -> dict:
        """Check the health of the sandbox API.

        Calls ``GET /health`` and returns the response dict, e.g.
        ``{"status": "ok", "sandboxes": 0}``.

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A dict with at least a ``status`` key.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/health")
        _check_response(resp)
        return resp.json()

    # ── code execution ────────────────────────────────────────────────

    # ── context management ──────────────────────────────────────────

    def create_context(
        self,
        *,
        language: str = "python",
        cwd: str = "/home/user",
    ) -> "Context":
        """Create a new kernel context on the sandbox's envd.

        The context keeps variable state alive between :meth:`run_code` calls.
        envd generates the ``id``; use the returned :class:`Context` object
        with subsequent ``run_code`` calls.

        Args:
            language: Kernel language (default ``"python"``)
            cwd: Working directory for the context.

        Returns:
            A :class:`Context` with the server-assigned ``id``.
        """
        if self._client is None:
            self._client = build_client(self._config)
        url = f"http://{self.get_host(JUPYTER_PORT)}/contexts"
        resp = self._client.post(
            url,
            json={"language": language, "cwd": cwd},
            headers={"Content-Type": "application/json"},
        )
        if resp.status_code >= 400:
            raise ApiError(f"create_context failed: HTTP {resp.status_code}", resp.status_code)
        data = resp.json()
        return Context(id=data["id"], language=data["language"], cwd=data["cwd"])

    def delete_context(self, context: "Context") -> None:
        """Delete a kernel context from the sandbox's envd."""
        if self._client is None:
            self._client = build_client(self._config)
        url = f"http://{self.get_host(JUPYTER_PORT)}/contexts/{context.id}"
        resp = self._client.delete(url)
        if resp.status_code >= 400:
            raise ApiError(f"delete_context failed: HTTP {resp.status_code}", resp.status_code)

    def run_code(
        self,
        code: str,
        *,
        language: str | None = None,
        context: Context | None = None,
        on_stdout: Callable[[OutputMessage], None] | None = None,
        on_stderr: Callable[[OutputMessage], None] | None = None,
        on_result: Callable[[Result], None] | None = None,
        on_error: Callable[[ExecutionError], None] | None = None,
        envs: Dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> Execution:
        """Execute code in the sandbox and return the result.

        Streams the ndjson response from the sandbox's envd process via CubeProxy.
        When ``CUBE_PROXY_NODE_IP`` is set, the connection bypasses DNS resolution.

        Args:
            code: Python code to execute.
            context: Kernel context for sharing state across calls.
            on_stdout: Callback for stdout events.
            on_stderr: Callback for stderr events.
            on_result: Callback for result events.
            on_error: Callback for error events.
            envs: Per-execution environment variables.
            timeout: Read timeout in seconds (default: no timeout).

        Returns:
            :class:`Execution` with ``.text``, ``.logs``, and ``.error``.
        """
        if self._client is None:
            self._client = build_client(self._config)

        url = f"http://{self.get_host(JUPYTER_PORT)}/execute"
        payload = {
            "code": code,
            "context_id": context.id if context else None,
            "language": language,
            "env_vars": envs,
        }
        execution = Execution()

        with self._client.stream(
            "POST", url,
            json=payload,
            headers={"Content-Type": "application/json"},
            timeout=httpx.Timeout(
                connect=self._config.request_timeout,
                read=timeout,
                write=30,
                pool=30,
            ),
        ) as resp:
            if resp.status_code >= 400:
                raise ApiError(f"execute failed: HTTP {resp.status_code}", resp.status_code)
            for line in resp.iter_lines():
                _parse_line(execution, line,
                            on_stdout=on_stdout, on_stderr=on_stderr,
                            on_result=on_result, on_error=on_error)

        return execution

    # ── lifecycle ─────────────────────────────────────────────────────

    def pause(self) -> None:
        """Pause the sandbox (preserves memory snapshot)."""
        resp = self._session.post(f"{self._config.api_url}/sandboxes/{self.sandbox_id}/pause")
        _check_response(resp)

    def resume(self, timeout: int = 300) -> None:
        """Resume a paused sandbox. Deprecated — use :meth:`connect` instead."""
        resp = self._session.post(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/resume",
            json={"timeout": timeout},
        )
        _check_response(resp)

    def kill(self) -> None:
        """Destroy the sandbox."""
        resp = self._session.delete(f"{self._config.api_url}/sandboxes/{self.sandbox_id}")
        _check_response(resp)

    def get_info(self) -> dict:
        """Return sandbox details from the API."""
        resp = self._session.get(f"{self._config.api_url}/sandboxes/{self.sandbox_id}")
        _check_response(resp)
        return resp.json()

    # ── context manager ───────────────────────────────────────────────

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *_: Any) -> None:
        try:
            self.kill()
        except CubeSandboxError:
            pass
        if self._client:
            self._client.close()

    def __repr__(self) -> str:
        return f"Sandbox(id={self.sandbox_id!r}, domain={self.domain!r})"

    # ── internal ──────────────────────────────────────────────────────

    def _build_session(self) -> requests.Session:
        s = requests.Session()
        s.headers.update({"Content-Type": "application/json"})
        return s
