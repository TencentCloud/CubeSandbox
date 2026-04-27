"""
cube_e2b.sandbox
~~~~~~~~~~~~~~~~
Main Sandbox class — create, manage, and access CubeSandbox instances.
"""
from __future__ import annotations

from typing import Any

import requests

from .client import CubeApiClient
from .config import SandboxConfig, get_default_config
from .stream import build_stream_session, iter_sse, websocket_connect


class Sandbox:
    """Represents a running CubeSandbox instance.

    Typical usage::

        with Sandbox.create(template="tpl-xxxx") as sb:
            resp = sb.http_get(49999, "/health")
            print(resp.text)

    Or manually::

        sb = Sandbox.create(template="tpl-xxxx", timeout=600)
        try:
            host = sb.get_host(49999)
            print(f"Sandbox available at http://{host}")
        finally:
            sb.kill()
    """

    def __init__(self, data: dict, config: SandboxConfig | None = None) -> None:
        self._data = data
        self._cfg = config or get_default_config()
        self._client = CubeApiClient(self._cfg)

    # ------------------------------------------------------------------
    # Factory
    # ------------------------------------------------------------------

    @classmethod
    def create(
        cls,
        template: str | None = None,
        *,
        timeout: int | None = None,
        env_vars: dict[str, str] | None = None,
        metadata: dict[str, str] | None = None,
        config: SandboxConfig | None = None,
        **kwargs: Any,
    ) -> "Sandbox":
        """Create a new sandbox and return a :class:`Sandbox` instance.

        Parameters
        ----------
        template:
            Template ID (e.g. ``"tpl-6265796cee124256b4dcd6a1"``).
            Falls back to ``CUBE_TEMPLATE_ID`` env var if not given.
        timeout:
            Sandbox TTL in seconds (default: ``config.default_timeout``).
        env_vars:
            Environment variables to inject into the sandbox.
        metadata:
            Arbitrary string key/value metadata.
        config:
            Optional :class:`~cube_e2b.config.SandboxConfig`.
        """
        cfg = config or get_default_config()
        tpl = template or cfg.default_template_id
        if not tpl:
            raise ValueError(
                "template must be provided or CUBE_TEMPLATE_ID must be set."
            )
        ttl = timeout if timeout is not None else cfg.default_timeout
        client = CubeApiClient(cfg)
        data = client.create_sandbox(tpl, timeout=ttl, env_vars=env_vars, metadata=metadata, **kwargs)
        return cls(data, config=cfg)

    @classmethod
    def connect(cls, sandbox_id: str, *, config: SandboxConfig | None = None) -> "Sandbox":
        """Connect to an already-running sandbox by ID."""
        cfg = config or get_default_config()
        client = CubeApiClient(cfg)
        data = client.get_sandbox(sandbox_id)
        return cls(data, config=cfg)

    # ------------------------------------------------------------------
    # Properties
    # ------------------------------------------------------------------

    @property
    def sandbox_id(self) -> str:
        return self._data["sandboxID"]

    @property
    def template_id(self) -> str:
        return self._data["templateID"]

    @property
    def domain(self) -> str:
        """Base domain, e.g. ``"cube.app"``."""
        return self._data.get("domain") or "cube.app"

    @property
    def raw(self) -> dict:
        """Raw API response dict."""
        return self._data

    # ------------------------------------------------------------------
    # URL / Host helpers
    # ------------------------------------------------------------------

    def get_host(self, port: int) -> str:
        """Return the virtual hostname for *port*.

        Format: ``"<port>-<sandboxID>.<domain>"``
        e.g. ``"49999-5405bd0b3b584ac6bafb7656ebe19f8c.cube.app"``
        """
        return f"{port}-{self.sandbox_id}.{self.domain}"

    def get_url(self, port: int, protocol: str = "http") -> str:
        """Return a full URL for *port*.

        When ``CUBE_PROXY_NODE_IP`` is set the URL will point directly to the
        proxy IP (avoids DNS), but the Host header is still set correctly by
        :func:`~cube_e2b.stream.build_stream_session`.

        Parameters
        ----------
        port:
            The sandbox service port (e.g. ``49999``).
        protocol:
            ``"http"``, ``"https"``, ``"ws"``, or ``"wss"``.
        """
        host = self.get_host(port)
        if self._cfg.proxy_node_ip:
            # Return a URL with the virtual hostname — the stream session
            # adapter will rewrite the actual TCP target to proxy_node_ip.
            ip_port = (
                self._cfg.proxy_port_https
                if protocol in ("https", "wss")
                else self._cfg.proxy_port_http
            )
            # Keep using virtual hostname so Host header injection works
            return f"{protocol}://{host}"
        return f"{protocol}://{host}"

    # ------------------------------------------------------------------
    # HTTP convenience
    # ------------------------------------------------------------------

    def _stream_session(self) -> requests.Session:
        return build_stream_session(self._cfg)

    def http_get(
        self,
        port: int,
        path: str = "/",
        *,
        use_tls: bool = False,
        **kwargs,
    ) -> requests.Response:
        """HTTP GET to the sandbox service at *port*.

        Respects ``CUBE_PROXY_NODE_IP`` for DNS bypass.
        """
        protocol = "https" if use_tls else "http"
        url = f"{protocol}://{self.get_host(port)}{path}"
        session = self._stream_session()
        return session.get(url, **kwargs)

    def http_post(
        self,
        port: int,
        path: str = "/",
        *,
        use_tls: bool = False,
        **kwargs,
    ) -> requests.Response:
        """HTTP POST to the sandbox service at *port*."""
        protocol = "https" if use_tls else "http"
        url = f"{protocol}://{self.get_host(port)}{path}"
        session = self._stream_session()
        return session.post(url, **kwargs)

    def iter_sse(self, port: int, path: str = "/", *, use_tls: bool = False):
        """Yield SSE data lines from the sandbox service at *port*."""
        protocol = "https" if use_tls else "http"
        url = f"{protocol}://{self.get_host(port)}{path}"
        yield from iter_sse(url, config=self._cfg)

    # ------------------------------------------------------------------
    # WebSocket
    # ------------------------------------------------------------------

    def connect_ws(self, port: int, path: str = "/", *, use_tls: bool = False):
        """Open a WebSocket connection to the sandbox service at *port*.

        Returns a ``websockets.sync.client.ClientConnection``.
        Requires ``websockets >= 12``.

        Example::

            with sb.connect_ws(49999, "/ws") as ws:
                ws.send("hello")
                print(ws.recv())
        """
        host = self.get_host(port)
        return websocket_connect(host, path, use_tls=use_tls, config=self._cfg)

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    def kill(self) -> None:
        """Terminate the sandbox immediately."""
        self._client.delete_sandbox(self.sandbox_id)

    def pause(self) -> None:
        """Pause (snapshot) the sandbox to free compute resources."""
        self._client.pause_sandbox(self.sandbox_id)

    def resume(self, timeout: int = 300) -> None:
        """Resume a paused sandbox."""
        self._client.resume_sandbox(self.sandbox_id, timeout=timeout)

    def refresh(self, timeout: int = 300) -> None:
        """Extend the sandbox TTL by *timeout* seconds from now."""
        self._client.refresh_sandbox(self.sandbox_id, timeout=timeout)

    def info(self) -> dict:
        """Return current sandbox detail from the API."""
        return self._client.get_sandbox(self.sandbox_id)

    # ------------------------------------------------------------------
    # Context manager
    # ------------------------------------------------------------------

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *args) -> None:
        try:
            self.kill()
        except Exception:
            pass

    def __repr__(self) -> str:
        return (
            f"Sandbox(id={self.sandbox_id!r}, "
            f"template={self.template_id!r}, "
            f"domain={self.domain!r})"
        )
