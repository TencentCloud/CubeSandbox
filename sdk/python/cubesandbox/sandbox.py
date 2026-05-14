# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import logging
import time
from typing import Any, Callable, Dict, Optional

import httpx

from ._commands import CommandResult, Commands
from ._config import Config
from ._exceptions import ApiError, AuthenticationError, CubeSandboxError, SandboxNotFoundError, TemplateNotFoundError
from ._filesystem import Filesystem
from ._models import Execution, ExecutionError, OutputMessage, Result
from ._stream import _parse_line
from ._transport import build_client

logger = logging.getLogger(__name__)

JUPYTER_PORT = 49999


def _check_response(resp: httpx.Response) -> None:
    if resp.is_success:
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

    def __init__(self, data: dict, config: Optional[Config] = None) -> None:
        self._data = data
        self._config = config or Config()
        self._session = self._build_session()
        self._client: httpx.Client | None = None
        self._commands = Commands(self)
        self._files = Filesystem(self)

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
        """Return the virtual hostname for a sandbox port.

        e.g. ``49999-<sandboxID>.cube.app``
        """
        return f"{port}-{self.sandbox_id}.{self.domain}"

    @property
    def commands(self) -> "Commands":
        return self._commands

    @property
    def files(self) -> "Filesystem":
        return self._files

    # ── factory methods ───────────────────────────────────────────────

    @classmethod
    def create(
        cls,
        template: str | None = None,
        *,
        timeout: int | None = None,
        env_vars: Dict[str, str] | None = None,
        metadata: Dict[str, str] | None = None,
        allow_internet_access: bool = True,
        network: Dict[str, Any] | None = None,
        config: Config | None = None,
        **kwargs: Any,
    ) -> "Sandbox":
        """POST /sandboxes - Create a new sandbox.

        Args:
            template: Template ID. Falls back to ``CUBE_TEMPLATE_ID`` env var.
            timeout: Sandbox TTL in seconds. Defaults to ``Config.timeout`` (300).
            env_vars: Environment variables injected into the sandbox.
            metadata: Arbitrary key-value metadata (e.g. network-policy, hostdir-mount).
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A running :class:`Sandbox` instance.

        Raises:
            ValueError: If no template ID is provided.
            ApiError: On unexpected backend error (HTTP 500).
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
        if not allow_internet_access:
            payload["allowInternetAccess"] = False
        if network:
            net: dict = {}
            if "allow_out" in network:
                net["allowOut"] = network["allow_out"]
            if "deny_out" in network:
                net["denyOut"] = network["deny_out"]
            if net:
                payload["network"] = net
        payload.update(kwargs)

        with httpx.Client(headers={"Content-Type": "application/json"}) as s:
            resp = s.post(f"{cfg.api_url}/sandboxes", json=payload)
        _check_response(resp)
        return cls(resp.json(), config=cfg)

    @classmethod
    def connect(cls, sandbox_id: str, *, config: Config | None = None) -> "Sandbox":
        """POST /sandboxes/:sandboxID/connect - Connect to an existing sandbox.

        Resumes the sandbox if it is currently paused.

        Args:
            sandbox_id: Sandbox identifier.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A :class:`Sandbox` instance connected to the existing sandbox.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: On unexpected backend error (HTTP 500).
        """
        cfg = config or Config()
        with httpx.Client(headers={"Content-Type": "application/json"}) as s:
            resp = s.post(
                f"{cfg.api_url}/sandboxes/{sandbox_id}/connect",
                json={"timeout": cfg.timeout},
            )
        _check_response(resp)
        return cls(resp.json(), config=cfg)

    # ── class-level API methods ───────────────────────────────────────

    @classmethod
    def list(cls, config: Config | None = None) -> list[dict]:
        """GET /sandboxes - List all running sandboxes (v1).

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A list of sandbox info dicts, each containing at least
            ``sandboxID``, ``templateID``, and ``state`` keys.
        """
        cfg = config or Config()
        with httpx.Client() as s:
            resp = s.get(f"{cfg.api_url}/sandboxes")
        _check_response(resp)
        return resp.json()

    @classmethod
    def list_v2(cls, config: Config | None = None) -> list[dict]:
        """GET /v2/sandboxes - List all running sandboxes (v2).

        Supports state / metadata filtering on the server side.

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A list of sandbox info dicts.
        """
        cfg = config or Config()
        with httpx.Client() as s:
            resp = s.get(f"{cfg.api_url}/v2/sandboxes")
        _check_response(resp)
        return resp.json()

    @classmethod
    def health(cls, config: Config | None = None) -> dict:
        """GET /health - Check the health of the CubeAPI service.

        Args:
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A dict with at least a ``status`` key, e.g.
            ``{"status": "ok", "sandboxes": 0}``.
        """
        cfg = config or Config()
        with httpx.Client() as s:
            resp = s.get(f"{cfg.api_url}/health")
        _check_response(resp)
        return resp.json()

    # ── code execution ────────────────────────────────────────────────

    def run_code(
        self,
        code: str,
        *,
        language: str | None = None,
        on_stdout: Callable[[OutputMessage], None] | None = None,
        on_stderr: Callable[[OutputMessage], None] | None = None,
        on_result: Callable[[Result], None] | None = None,
        on_error: Callable[[ExecutionError], None] | None = None,
        envs: Dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> Execution:
        """POST /execute - Execute code inside the sandbox.

        Streams the ndjson response from the sandbox's envd process via
        CubeProxy. When ``CUBE_PROXY_NODE_IP`` is set, connections bypass
        DNS resolution using :class:`IPOverrideTransport`.

        Args:
            code: Python code to execute.
            language: Kernel language override (default: ``"python"``).
                pass ``None`` (or omit) to use the sandbox's global namespace.
            on_stdout: Callback invoked for each stdout event.
            on_stderr: Callback invoked for each stderr event.
            on_result: Callback invoked for each result event.
            on_error: Callback invoked on execution error.
            envs: Per-execution environment variables.
            timeout: Read timeout in seconds (default: no timeout).

        Returns:
            :class:`Execution` with ``.text``, ``.logs``, and ``.error``.

        Raises:
            ApiError: If the execute endpoint returns HTTP 4xx/5xx.
        """
        if self._client is None:
            self._client = build_client(self._config)

        url = f"http://{self.get_host(JUPYTER_PORT)}/execute"
        payload = {
            "code": code,
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

    def pause(self, *, wait: bool = True, timeout: float = 30, interval: float = 1.0) -> None:
        """POST /sandboxes/:sandboxID/pause - Pause a sandbox.

        Preserves the sandbox memory snapshot. The sandbox can be resumed
        later via :meth:`connect`.

        Args:
            wait: If ``True`` (default), poll :meth:`get_info` until the sandbox
                state becomes ``"paused"`` before returning.
            timeout: Maximum seconds to wait when ``wait=True`` (default: 30).
            interval: Polling interval in seconds (default: 1.0).

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: If the sandbox cannot be paused (HTTP 409) or on
                unexpected backend error (HTTP 500).
            TimeoutError: If ``wait=True`` and sandbox does not reach
                ``"paused"`` state within ``timeout`` seconds.
        """
        resp = self._session.post(f"{self._config.api_url}/sandboxes/{self.sandbox_id}/pause")
        _check_response(resp)
        if wait:
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if self.get_info().get("state") == "paused":
                    return
                time.sleep(interval)
            raise TimeoutError(
                f"Sandbox {self.sandbox_id!r} did not reach 'paused' state within {timeout}s"
            )

    def resume(self, timeout: int = 300) -> None:
        """POST /sandboxes/:sandboxID/resume - Resume a paused sandbox.

        .. deprecated::
            Use :meth:`connect` instead, which auto-resumes paused sandboxes
            and returns a fresh :class:`Sandbox` instance.

        Args:
            timeout: Sandbox TTL in seconds after resume (default: 300).

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: If the sandbox is already running (HTTP 409) or on
                unexpected backend error (HTTP 500).
        """
        resp = self._session.post(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/resume",
            json={"timeout": timeout},
        )
        _check_response(resp)

    def kill(self) -> None:
        """DELETE /sandboxes/:sandboxID - Destroy a sandbox.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: On unexpected backend error (HTTP 500).
        """
        resp = self._session.delete(f"{self._config.api_url}/sandboxes/{self.sandbox_id}")
        _check_response(resp)

    def get_info(self) -> dict:
        """GET /sandboxes/:sandboxID - Get sandbox detail.

        Returns:
            A dict containing ``sandboxID``, ``state``, ``cpuCount``,
            ``memoryMB``, ``startedAt``, and other sandbox metadata.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: On unexpected backend error (HTTP 500).
        """
        resp = self._session.get(f"{self._config.api_url}/sandboxes/{self.sandbox_id}")
        _check_response(resp)
        return resp.json()

    def close(self) -> None:
        """Close the underlying httpx streaming client.

        Called automatically by :meth:`__exit__` and :meth:`__del__`.
        Safe to call multiple times.
        """
        if self._client is not None:
            self._client.close()
            self._client = None
        self._session.close()

    # ── context manager ───────────────────────────────────────────────

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *_: Any) -> None:
        try:
            self.kill()
        except CubeSandboxError:
            pass
        self.close()

    def __del__(self) -> None:
        self.close()

    def __repr__(self) -> str:
        return f"Sandbox(id={self.sandbox_id!r}, domain={self.domain!r})"

    # ── internal ──────────────────────────────────────────────────────

    def _build_session(self) -> httpx.Client:
        return httpx.Client(
            headers={"Content-Type": "application/json"},
            base_url=self._config.api_url,
        )
