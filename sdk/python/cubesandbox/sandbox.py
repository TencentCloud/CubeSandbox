# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import threading
from typing import Any, Callable, Dict

import httpx
import requests

from ._commands import CommandResult, Commands
from ._config import Config, _auth_headers
from ._exceptions import ApiError, AuthenticationError, CubeSandboxError, SandboxNotFoundError, TemplateNotFoundError
from ._filesystem import Filesystem
from ._models import Execution, ExecutionError, OutputMessage, Result, SandboxInfo, SnapshotInfo
from ._policy import (
    Rule,
    _build_network_payload,
    _build_network_update_body,
)
from ._pty import Pty
from ._stream import _parse_line
from ._transport import build_client
from ._volume import VolumeMountsArg, _serialize_volume_mounts

JUPYTER_PORT = 49999

#: Never-timeout sentinel. See docs/guide/lifecycle.md.
NEVER_TIMEOUT = -1

class _CloneCleanup:
    """Process-local ownership of one clone operation's temporary snapshot."""

    def __init__(self, snapshot_id: str, remaining: int, config: Config) -> None:
        self.snapshot_id = snapshot_id
        self.remaining = remaining
        self.config = config
        self._released: set[str] = set()
        self._cleanup_started = False
        self._lock = threading.Lock()

    def release(self, sandbox_id: str) -> None:
        with self._lock:
            if sandbox_id in self._released:
                return
            self._released.add(sandbox_id)
            self.remaining -= 1
            should_cleanup = self.remaining == 0
        if should_cleanup:
            self.cleanup()

    def cleanup(self) -> None:
        """Start best-effort deletion once, including failure backstops."""
        with self._lock:
            if self._cleanup_started:
                return
            self._cleanup_started = True
        try:
            Sandbox.delete_snapshot(self.snapshot_id, config=self.config)
        except Exception:  # noqa: BLE001 — best-effort snapshot cleanup
            return


def _check_response(resp: requests.Response) -> None:
    if resp.ok:
        return
    try:
        body = resp.json()
        msg = (
            (body.get("message") or body.get("detail") or resp.text)
            if isinstance(body, dict)
            else resp.text
        )
    except (ValueError, requests.JSONDecodeError):
        msg = resp.text or f"HTTP {resp.status_code}"
    code = resp.status_code
    if code in (401, 403):
        raise AuthenticationError(msg, code)
    if code == 404:
        raise (TemplateNotFoundError if "template" in msg.lower() else SandboxNotFoundError)(msg, code)
    raise ApiError(msg, code)


_VALID_ON_TIMEOUT = ("kill", "pause")


def _serialize_lifecycle(lifecycle: Dict[str, Any]) -> Dict[str, Any]:
    """Translate a snake_case ``lifecycle`` dict to the camelCase wire shape
    used by CubeAPI (and by the e2b SDK).

    Validates ``on_timeout`` early so misspellings ("paused", "Pause") raise
    a clean ``ValueError`` at the call site instead of producing an opaque
    HTTP 4xx from the server.
    """
    out: Dict[str, Any] = {}
    on_timeout = lifecycle.get("on_timeout")
    if on_timeout is not None:
        if on_timeout not in _VALID_ON_TIMEOUT:
            raise ValueError(
                f"lifecycle.on_timeout must be one of {_VALID_ON_TIMEOUT!r}, "
                f"got {on_timeout!r}"
            )
        out["onTimeout"] = on_timeout
    if "auto_resume" in lifecycle:
        out["autoResume"] = bool(lifecycle["auto_resume"])
    return out


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
        self._config = config or Config()
        self._session = self._build_session()
        self._client: httpx.Client | None = None
        self._commands = Commands(self)
        self._files = Filesystem(self)
        self._pty = Pty(self)
        self._clone_cleanup: _CloneCleanup | None = None


    @property
    def sandbox_id(self) -> str:
        return self._data["sandboxID"]

    @property
    def template_id(self) -> str:
        return self._data["templateID"]

    @property
    def domain(self) -> str:
        return self._data.get("domain") or self._config.sandbox_domain

    @property
    def traffic_access_token(self) -> str | None:
        """Per-sandbox token returned when ``network.allow_public_traffic=False``.

        Send it as either the ``e2b-traffic-access-token`` (E2B-compatible)
        or ``cube-traffic-access-token`` header on every request to the
        sandbox's public URL — CubeProxy rejects unauthenticated traffic
        with HTTP 403 when the sandbox was created with public access
        restricted.

        ``None`` for sandboxes with the default (publicly reachable)
        configuration, including instances obtained via :meth:`connect` /
        :meth:`resume` (the token is only delivered on the original create
        response). Persist it client-side at create time if you need it
        later.
        """
        token = self._data.get("trafficAccessToken")
        return token or None

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

    @property
    def pty(self) -> "Pty":
        return self._pty


    @classmethod
    def create(
        cls,
        template: str | None = None,
        *,
        timeout: int | None = None,
        env_vars: Dict[str, str] | None = None,
        envs: Dict[str, str] | None = None,
        metadata: Dict[str, str] | None = None,
        distribution_scope: list[str] | None = None,
        allow_internet_access: bool = True,
        network: Dict[str, Any] | None = None,
        lifecycle: Dict[str, Any] | None = None,
        volume_mounts: VolumeMountsArg | None = None,
        config: Config | None = None,
        **kwargs: Any,
    ) -> "Sandbox":
        """POST /sandboxes - Create a new sandbox.

        Args:
            template: Template ID. Falls back to ``CUBE_TEMPLATE_ID`` env var.
            timeout: Sandbox idle timeout in seconds (``None`` omits the field).
                See ``docs/guide/lifecycle.md``.
            env_vars: Environment variables injected into the sandbox.
            envs: E2B-compatible alias for ``env_vars``. When both aliases are
                provided, they must contain the same values.
            metadata: Arbitrary key-value metadata (e.g. network-policy, host-mount).
            distribution_scope: Compute node IDs or host IPs eligible to run the
                sandbox. Pass a single entry to pin the sandbox to one node.
            allow_internet_access: When ``False``, the sandbox is blocked from
                making outbound traffic to the public internet.
            network: Egress network policy. Accepts keys:
                - ``allow_out`` / ``deny_out``: lists of CIDRs or hostnames (L3/L4).
                - ``mask_request_host``: Host authority forwarded to user services;
                  ``${PORT}`` expands to the requested sandbox port.
                - ``rules``: list of :class:`~cubesandbox.Rule` dataclasses (or
                  equivalent dicts with snake_case keys) for L7 host/path/SNI
                  matching, audit, and credential injection. For E2B parity,
                  a host-keyed mapping of per-host request transforms (e.g.
                  ``{"api.example.com": [{"transform": {"headers": {...}}}]}``)
                  is also accepted and converted into equivalent CubeEgress
                  inject rules.
            lifecycle: Optional dict mirroring the e2b SDK's ``lifecycle``
                object (https://e2b.dev/docs/sandbox/auto-resume). Accepts
                two keys:

                - ``on_timeout``: ``"kill"`` (default) or ``"pause"``. When
                  ``"pause"``, an idle sandbox is suspended instead of
                  deleted; its memory snapshot survives across the pause.
                - ``auto_resume``: ``bool``, default ``False``. Only
                  meaningful when ``on_timeout="pause"``. When ``True``, the
                  next request hitting the paused sandbox transparently
                  wakes it back up. When ``False`` you must call
                  :meth:`connect` to resume manually.

                Absent ``lifecycle`` keeps today's behaviour (idle sandboxes
                are killed).
            volume_mounts: Optional dict mapping mount paths to volumes
                (e2b-compatible). Key is the sandbox mount path, value is a
                :class:`~cubesandbox.Volume`,
                :class:`~cubesandbox.VolumeInfo`, or plain ``volumeID`` string.
                Wrap any of those in :class:`~cubesandbox.VolumeMount` to set
                Cube-specific attachment options such as ``read_only``::

                    Sandbox.create(volume_mounts={"/workspace": vol})
                    Sandbox.create(volume_mounts={"/workspace": "vol-123"})
                    Sandbox.create(
                        volume_mounts={"/dataset": VolumeMount(vol, read_only=True)}
                    )

                Each value must resolve to an existing ``volumeID`` created via
                :meth:`cubesandbox.Volume.create`. ``read_only`` applies to this
                sandbox attachment; it does not make the volume an immutable
                snapshot.
            config: SDK config. Uses default (env-based) config if omitted.

        Returns:
            A running :class:`Sandbox` instance.

        Raises:
            ValueError: If no template ID is provided, or if ``lifecycle``
                contains an unsupported ``on_timeout`` value.
            ApiError: On unexpected backend error (HTTP 500).
        """
        cfg = config or Config()
        tpl = template or cfg.template_id
        if not tpl:
            raise ValueError("template is required. Set CUBE_TEMPLATE_ID or pass template=")

        if env_vars is not None and envs is not None and env_vars != envs:
            raise ValueError("env_vars and envs must match when both are provided")
        sandbox_env_vars = env_vars if env_vars is not None else envs

        # Omitted when None; see docs/guide/lifecycle.md.
        payload: dict = {"templateID": tpl}
        if timeout is not None:
            payload["timeout"] = timeout
        if sandbox_env_vars:
            payload["envVars"] = sandbox_env_vars
        if metadata:
            payload["metadata"] = metadata
        if distribution_scope:
            payload["distributionScope"] = distribution_scope
        if not allow_internet_access:
            payload["allow_internet_access"] = False
        if network:
            net = _build_network_payload(network, allow_internet_access=allow_internet_access)
            if net:
                payload["network"] = net
        # Lifecycle: opt-in. Wire shape mirrors e2b
        # (https://e2b.dev/docs/sandbox/auto-resume) — a nested object with
        # camelCase keys. Absent => server-side default ("kill" on timeout).
        if lifecycle:
            payload["lifecycle"] = _serialize_lifecycle(lifecycle)
        if volume_mounts:
            payload["volumeMounts"] = _serialize_volume_mounts(volume_mounts)
        payload.update(kwargs)

        s = requests.Session()
        resp = s.post(f"{cfg.api_url}/sandboxes", json=payload,
                      headers={"Content-Type": "application/json", **_auth_headers(cfg)})
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
        s = requests.Session()
        # Connect omits timeout; see docs/guide/lifecycle.md.
        resp = s.post(f"{cfg.api_url}/sandboxes/{sandbox_id}/connect",
                      json={},
                      headers={"Content-Type": "application/json", **_auth_headers(cfg)})
        _check_response(resp)
        return cls(resp.json(), config=cfg)


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
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/sandboxes", headers=_auth_headers(cfg))
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
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/v2/sandboxes", headers=_auth_headers(cfg))
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
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/health", headers=_auth_headers(cfg))
        _check_response(resp)
        return resp.json()


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
            self._client = self._build_data_client()

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
        import time
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

    def resume(self, timeout: int | None = None) -> None:
        """POST /sandboxes/:sandboxID/resume - Resume a paused sandbox.

        .. deprecated::
            Use :meth:`connect` instead, which auto-resumes paused sandboxes
            and returns a fresh :class:`Sandbox` instance.

        Args:
            timeout: Sandbox TTL in seconds after resume (``None`` omits the field).
                See ``docs/guide/lifecycle.md``.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: If the sandbox is already running (HTTP 409) or on
                unexpected backend error (HTTP 500).
        """
        body: dict = {}
        if timeout is not None:
            body["timeout"] = timeout
        resp = self._session.post(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/resume",
            json=body,
        )
        _check_response(resp)

    def set_timeout(self, timeout: int) -> None:
        """POST /sandboxes/:sandboxID/timeout - Set sandbox idle timeout.

        Args:
            timeout: New idle timeout in seconds. ``0`` requests immediate
                timeout; positive values set a normal TTL; ``NEVER_TIMEOUT``
                (-1) disables idle timeout entirely.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: If the timeout is invalid or on unexpected backend error.
        """
        resp = self._session.post(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/timeout",
            json={"timeout": timeout},
        )
        _check_response(resp)

    def update_network(self, network: Dict[str, Any] | None = None) -> None:
        """PUT /sandboxes/:sandboxID/network - Replace the egress policy.

        Takes the whole policy as one object, including
        ``allow_internet_access``, matching E2B's ``SandboxNetworkUpdate``. The
        create path keeps ``allow_internet_access`` as a separate argument
        because E2B's create does too; the asymmetry is upstream's, and matching
        it is what lets code written against either SDK work unchanged.

        Takes effect on established connections too, not just new ones: a
        connection the new policy no longer allows is reset rather than left
        running until it closes.

        Args:
            network: The complete desired policy. A replacement, not a patch —
                an omitted key is cleared, and ``None`` clears everything.
                Accepts ``allow_out``, ``deny_out``, ``rules``,
                ``allow_internet_access``, plus the CubeSandbox extensions
                ``allow_public_traffic`` and ``mask_request_host``. ``rules``
                takes either CubeEgress's list of rules or E2B's
                ``{host: [{transform: ...}]}`` mapping.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: If the policy is invalid (HTTP 400), the sandbox is not
                running (HTTP 409), or on unexpected backend error.
        """
        resp = self._session.put(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/network",
            json=_build_network_update_body(network or {}),
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
        if self._clone_cleanup is not None:
            self._clone_cleanup.release(self.sandbox_id)

    def get_info(self) -> SandboxInfo:
        """GET /sandboxes/:sandboxID - Get sandbox detail.

        Returns:
            A :class:`SandboxInfo` exposing E2B-compatible ``snake_case``
            attributes (``sandbox_id``, ``template_id``, ``sandbox_domain``,
            ``started_at`` / ``end_at`` as ``datetime``, ``state`` as
            ``SandboxState | str | None``, ``cpu_count``, ``memory_mb``,
            ``envd_version``, ``metadata``, ``name`` ...), plus the
            CubeSandbox-specific ``disk_size_mb`` attribute. For backward
            compatibility the object is also a dict containing the raw CubeAPI
            JSON snapshot (excluding the sensitive ``envdAccessToken``), e.g.
            ``info["sandboxID"]`` or ``info.get("state")``.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: On unexpected backend error (HTTP 500).
        """
        resp = self._session.get(f"{self._config.api_url}/sandboxes/{self.sandbox_id}")
        _check_response(resp)
        return SandboxInfo.from_dict(resp.json())


    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *_: Any) -> None:
        try:
            self.kill()
        except CubeSandboxError:
            pass
        self.close()

    def close(self) -> None:
        """Close cached HTTP clients without destroying the sandbox."""
        self._reset_connections()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass

    def __repr__(self) -> str:
        return f"Sandbox(id={self.sandbox_id!r}, domain={self.domain!r})"


    def create_snapshot(self, name: str | None = None) -> SnapshotInfo:
        """POST /sandboxes/:sandboxID/snapshots — Create a snapshot (1.1).

        The sandbox is temporarily paused during snapshot creation.
        The snapshot persists independently of the sandbox lifecycle;
        it remains valid even after the sandbox is killed.

        Args:
            name: Optional template name for the snapshot.  When a template
                with this name already exists the new build is attached to it
                rather than creating a new template.

        Returns:
            :class:`SnapshotInfo` with ``snapshot_id`` and ``names``.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        payload: dict = {}
        if name is not None:
            payload["name"] = name
        resp = self._session.post(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/snapshots",
            json=payload,
        )
        _check_response(resp)
        return SnapshotInfo.from_dict(resp.json())

    @classmethod
    def list_snapshots(
        cls,
        *,
        sandbox_id: str | None = None,
        limit: int | None = None,
        next_token: str | None = None,
        config: Config | None = None,
    ) -> tuple[list[SnapshotInfo], str | None]:
        """GET /snapshots — List snapshots (1.2).

        Args:
            sandbox_id: Filter by source sandbox ID.
            limit: Page size (default: 100).
            next_token: Pagination cursor from a previous call.
            config: SDK config.  Uses default (env-based) config if omitted.

        Returns:
            A 2-tuple of ``(snapshots, next_token)`` where *next_token* is
            ``None`` when there are no more pages.
        """
        cfg = config or Config()
        params: dict = {}
        if sandbox_id is not None:
            params["sandboxID"] = sandbox_id
        if limit is not None:
            params["limit"] = limit
        if next_token is not None:
            params["nextToken"] = next_token
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/snapshots", params=params, headers=_auth_headers(cfg))
        _check_response(resp)
        items = [SnapshotInfo.from_dict(d) for d in (resp.json() or [])]
        nt = resp.headers.get("x-next-token") or None
        return items, nt

    @classmethod
    def delete_snapshot(cls, snapshot_id: str, *, config: Config | None = None) -> None:
        """DELETE /templates/:templateID — Delete a snapshot (1.3).

        Snapshots are stored as templates; this call removes the underlying
        template permanently.  Deleting the originating sandbox does **not**
        cascade-delete its snapshots.

        Args:
            snapshot_id: The ``snapshotID`` (= templateID) to delete.
            config: SDK config.  Uses default (env-based) config if omitted.

        Raises:
            TemplateNotFoundError: If the snapshot does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.delete(f"{cfg.api_url}/templates/{snapshot_id}", headers=_auth_headers(cfg))
        _check_response(resp)

    # 1.4 — create_from_snapshot is covered by Sandbox.create(template=snapshot_id).
    # See docstring of :meth:`create` for details.

    def rollback(self, snapshot_id: str) -> dict:
        """POST /sandboxes/:sandboxID/rollback — Roll back a sandbox to a snapshot (1.5).

        Reverts the sandbox's filesystem and memory to the state captured in
        *snapshot_id*.  The snapshot must belong to (or be compatible with)
        this sandbox.

        After a successful rollback the sandbox process is restarted from the
        snapshot image, which invalidates any TCP connections previously held
        open against it (jupyter-server inside the sandbox + the cube-api
        keep-alive pool). To prevent the next call (e.g. :meth:`run_code`)
        from racing against a half-closed socket, this method proactively
        closes the underlying HTTP clients so they are rebuilt lazily on
        demand. Callers don't need to do anything — ``sb.run_code(...)`` after
        ``sb.rollback(...)`` Just Works.

        Args:
            snapshot_id: Target snapshot ID.

        Returns:
            Response dict, e.g. ``{"sandboxID": ..., "snapshotID": ...,
            "status": "success"}``.

        Raises:
            SandboxNotFoundError: If the sandbox does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        resp = self._session.post(
            f"{self._config.api_url}/sandboxes/{self.sandbox_id}/rollback",
            json={"snapshotID": snapshot_id},
        )
        _check_response(resp)
        result = resp.json()
        # Rollback restarts the sandbox VM/process — old keep-alive sockets
        # are now pointing at a torn-down kernel. Drop both client pools so
        # the next request opens a fresh connection.
        self._reset_connections()
        return result

    def _reset_connections(self) -> None:
        """Close pooled HTTP connections so they are rebuilt on next use.

        Idempotent and best-effort. Used after operations that invalidate
        the sandbox's network identity (currently :meth:`rollback`; future
        callers may include ``resume``).
        """
        if self._client is not None:
            try:
                self._client.close()
            except Exception:  # noqa: BLE001 — best-effort
                pass
            self._client = None
        # ``_session`` is a requests.Session reused for the cube-api control
        # plane. rollback responses come from cube-api (not the sandbox VM)
        # so its sockets are usually fine, but resetting is cheap insurance
        # against any intermediate proxy that drops idle conns on rollback.
        try:
            self._session.close()
        except Exception:  # noqa: BLE001
            pass
        self._session = self._build_session()

    def clone(self, n: int = 1, *, concurrency: int = 1) -> list["Sandbox"]:
        """Clone this sandbox *n* times (1.6).

        Internally this executes three steps:

        1. :meth:`create_snapshot` — capture the current state.
        2. :func:`Sandbox.create` × n — spin up *n* sandboxes from the snapshot.
        3. Attach shared cleanup state to the clones. The temporary snapshot
           is deleted after the last clone is killed through this SDK process.

        If any sandbox creation fails, **all sibling sandboxes that did
        succeed are killed** before the exception propagates. This prevents
        leaked sandboxes when a partial failure happens halfway through a
        concurrent fan-out — the alternative (returning a partial list and
        raising) loses one or the other in any caller that doesn't carefully
        wrap the call in try/except. Snapshot cleanup is best-effort and is
        process-local; server-side timeout or deletion by another process is
        not observable by this SDK instance.

        Args:
            n: Number of clones to create (default: 1).
            concurrency: Maximum number of parallel ``Sandbox.create`` calls
                (default: 1, i.e. sequential). When > 1 the calls are dispatched
                via :class:`concurrent.futures.ThreadPoolExecutor`. The effective
                worker count is ``min(n, concurrency)`` so passing a value larger
                than ``n`` is harmless. Useful for fan-out scenarios where a
                single ``create`` round-trip dominates wall time. When set to 1
                no threads are spawned and behaviour is byte-identical to the
                sequential code path.

        Returns:
            List of *n* new :class:`Sandbox` instances. Order is unspecified
            when ``concurrency > 1`` (instances appear in the order their
            backend create call returned). When ``concurrency == 1`` the
            list preserves submission order.

        Raises:
            CubeSandboxError: If snapshot creation fails, or if any
                sandbox creation fails (the first error is re-raised after
                killing every sibling that did succeed).
            ApiError: On unexpected backend error.
        """
        snapshot = self.create_snapshot()
        snap_id = snapshot.snapshot_id
        cfg = self._config

        def _create_one() -> Sandbox:
            return Sandbox.create(template=snap_id, config=cfg)

        sandboxes: list[Sandbox] = []
        first_error: BaseException | None = None
        if concurrency <= 1 or n <= 1:
            # Sequential: short-circuit on first failure to preserve the
            # historical fail-fast behaviour.
            for _ in range(n):
                try:
                    sandboxes.append(_create_one())
                except BaseException as exc:  # noqa: BLE001
                    first_error = exc
                    break
        else:
            # Local import: keeps the default (sequential) path free of
            # threading machinery for callers that never opt-in.
            from concurrent.futures import ThreadPoolExecutor, as_completed

            workers = min(n, concurrency)
            with ThreadPoolExecutor(max_workers=workers) as pool:
                futures = [pool.submit(_create_one) for _ in range(n)]
                # Drain every future — never leak a Sandbox just because
                # an earlier sibling raised. We collect successes and the
                # first exception, then decide what to do once all futures
                # have settled.
                for fut in as_completed(futures):
                    try:
                        sandboxes.append(fut.result())
                    except BaseException as exc:  # noqa: BLE001
                        if first_error is None:
                            first_error = exc
                        # Keep draining: another in-flight create may
                        # still succeed and we must not drop its result.
        cleanup = _CloneCleanup(snap_id, len(sandboxes), cfg)
        for sb in sandboxes:
            sb._clone_cleanup = cleanup

        if first_error is not None:
            # We hit at least one failure. The caller asked for *n* clones
            # and got fewer — there is no clean way to return both partial
            # successes and an exception, so kill the orphans and propagate.
            # This is "all-or-nothing" semantics for the failure case.
            for sb in sandboxes:
                try:
                    sb.kill()
                except Exception:  # noqa: BLE001 — best-effort cleanup
                    pass
            # A failed kill cannot release its ownership. Force the idempotent
            # backstop after every surviving sibling has been attempted.
            cleanup.cleanup()
            raise first_error

        if not sandboxes:
            try:
                Sandbox.delete_snapshot(snap_id, config=cfg)
            except Exception:  # noqa: BLE001 — best-effort cleanup
                pass
        return sandboxes


    def _build_session(self) -> requests.Session:
        s = requests.Session()
        s.headers.update({"Content-Type": "application/json", **_auth_headers(self._config)})
        return s

    def _build_data_client(self) -> httpx.Client:
        """Build an HTTP client for CubeProxy-routed sandbox data-plane APIs."""
        return build_client(self._config, headers=self._traffic_token_headers())

    def _traffic_token_headers(self) -> dict[str, str]:
        """Headers that must accompany every CubeProxy data-plane request.

        When the sandbox was created with ``network.allow_public_traffic=False``,
        CubeProxy rejects unauthenticated traffic with 403. Attaching the token
        as a default header on the httpx client covers run_code, the Connect
        fallback path, and filesystem read/write in one place.

        Data-plane requests are also routed through CubeAPI's auth middleware,
        which requires ``X-API-Key`` (or ``Authorization: Bearer``) whenever the
        backend is started with an auth-callback URL. We therefore attach the
        API key here as well so run_code / commands / filesystem / pty all
        authenticate. When no key is configured ``_auth_headers`` returns ``{}``
        and behavior is unchanged.
        """
        headers = _auth_headers(self._config)
        token = self.traffic_access_token
        if token:
            headers["e2b-traffic-access-token"] = token
        return headers
