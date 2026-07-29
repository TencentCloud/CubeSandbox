# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""CubeSandbox-backed remote workspace for the OpenHands Agent SDK.

`CubeSandboxWorkspace` plays the same role as the SDK's own `DockerWorkspace`,
but instead of booting a fresh container per session it creates a CubeSandbox
MicroVM from a template whose boot snapshot already contains a *running*
OpenHands agent server (see `Dockerfile`). The result:

- hot start: the agent server is reachable moments after `Sandbox.create()`
  returns, because it was frozen live into the template snapshot;
- hardware isolation: every command, file edit, and script the agent runs
  happens inside a KVM MicroVM, not on the host;
- one flag to go private: ``private_traffic=True`` creates the sandbox with
  ``allow_public_traffic=False`` and sends the per-sandbox traffic token
  CubeProxy enforces with every workspace HTTP request (the Conversation
  WebSocket is exempt — see the ``private_traffic`` field docs);
- native lifecycle: `pause()` / `resume()` freeze and thaw the entire VM
  (agent server, shell sessions, and in-flight processes included), and a
  paused sandbox can be re-attached later — from a different process or
  days later — via ``CubeSandboxWorkspace(sandbox_id=...)``.

Usage:
    from cubesandbox_workspace import CubeSandboxWorkspace

    with CubeSandboxWorkspace(template="tpl-xxxxxxxx") as workspace:
        result = workspace.execute_command("echo hello from a MicroVM")
        print(result.stdout)

    # Keep a paused session and re-attach to it later:
    ws = CubeSandboxWorkspace(template="tpl-xxxxxxxx", kill_on_exit=False)
    ws.pause()
    sid = ws.sandbox_id
    ...  # process exit, hours pass ...
    ws2 = CubeSandboxWorkspace(sandbox_id=sid)  # auto-resumes

Environment: the E2B-compatible client reads `E2B_API_URL` and `E2B_API_KEY`,
the same convention as every other CubeSandbox example.
"""

from __future__ import annotations

import os
import time
from typing import TYPE_CHECKING, Any
from urllib.request import Request, urlopen

from e2b_code_interpreter import Sandbox
from openhands.sdk.logger import get_logger
from openhands.sdk.workspace import RemoteWorkspace
from pydantic import Field, PrivateAttr, SecretStr, model_validator

if TYPE_CHECKING:
    # Runtime never imports this (typing.Self is 3.11+); with postponed
    # annotations the `-> Self` hint below is never evaluated either, so the
    # module imports on any interpreter the OpenHands SDK itself supports.
    from typing import Self

logger = get_logger(__name__)

TRAFFIC_TOKEN_HEADER = "e2b-traffic-access-token"


class CubeSandboxWorkspace(RemoteWorkspace):
    """Remote workspace that runs the OpenHands agent server in a CubeSandbox
    MicroVM created from a pre-baked template."""

    # Overrides of parent fields with CubeSandbox-appropriate defaults.
    working_dir: str = Field(
        default="/workspace",
        description="Working directory inside the MicroVM.",
    )
    host: str = Field(
        default="",
        description="Agent server URL (set automatically after sandbox creation).",
    )

    # CubeSandbox-specific configuration.
    template: str | None = Field(
        default=None,
        description=(
            "CubeSandbox template id (tpl-...) built from this example's "
            "Dockerfile. Required unless sandbox_id is given. The template "
            "must autostart the agent server on agent_server_port."
        ),
    )
    sandbox_id: str | None = Field(
        default=None,
        description=(
            "Attach to an existing sandbox instead of creating one. A paused "
            "sandbox is resumed automatically. The attached sandbox is not "
            "killed on cleanup unless kill_on_exit is explicitly True."
        ),
    )
    agent_server_port: int = Field(
        default=8000,
        description="Port the agent server listens on inside the sandbox.",
    )
    proxy_http_port: int = Field(
        default_factory=lambda: int(os.getenv("CUBE_PROXY_HTTP_PORT", "80")),
        description=(
            "HTTP port of the deployment's public proxy (its "
            "CUBE_PROXY_HTTP_PORT). Appended to the public URL when not 80."
        ),
    )
    sandbox_timeout: int = Field(
        default=3600,
        description="Sandbox keepalive timeout in seconds passed to Sandbox.create().",
    )
    private_traffic: bool = Field(
        default=False,
        description=(
            "Create the sandbox with allow_public_traffic=False and send the "
            "per-sandbox traffic token with every workspace HTTP request, so "
            "only token holders can reach the agent server through the "
            "platform proxy. Off by default to match the platform default, "
            "like the other examples; strongly recommended for shared "
            "deployments. Limitation: OpenHands 1.38.0's RemoteConversation "
            "WebSocket does not attach custom headers, so full Conversation "
            "runs need public traffic (workspace API calls are unaffected)."
        ),
    )
    traffic_access_token: SecretStr | None = Field(
        default=None,
        exclude=True,
        repr=False,
        description=(
            "Traffic token for re-attaching to a private sandbox from a new "
            "process (persist workspace.traffic_token after creation). "
            "Captured automatically on the create path."
        ),
    )
    network: dict[str, Any] | None = Field(
        default=None,
        description=(
            "Extra network options forwarded to Sandbox.create(), e.g. egress "
            "allow_out/deny_out CIDR lists. Merged with private_traffic, "
            "which owns allow_public_traffic: passing private_traffic=True "
            "together with allow_public_traffic=True raises ValueError "
            "instead of silently going public."
        ),
    )
    kill_on_exit: bool | None = Field(
        default=None,
        description=(
            "Whether cleanup()/context exit kills the sandbox. Defaults to "
            "True for sandboxes this workspace created and False for "
            "sandboxes attached via sandbox_id."
        ),
    )
    health_check_timeout: float = Field(
        default=60.0,
        gt=0.0,
        description="Seconds to wait for the agent server /ready to pass.",
    )

    _sandbox: Sandbox | None = PrivateAttr(default=None)
    _owns_sandbox: bool = PrivateAttr(default=False)
    # The platform's connect/resume API deliberately does not re-surface the
    # traffic token (CubeProxy validates it from Redis), so the client must
    # preserve the original one across resume/re-attach.
    _saved_traffic_token: str | None = PrivateAttr(default=None)

    @model_validator(mode="before")
    @classmethod
    def _require_template_or_sandbox_id(cls, data: Any) -> Any:
        if isinstance(data, dict) and not (
            data.get("template") or data.get("sandbox_id")
        ):
            raise ValueError("either template or sandbox_id must be provided")
        return data

    def model_post_init(self, context: Any) -> None:
        """Create (or attach to) the sandbox and connect the RemoteWorkspace."""
        started = time.monotonic()
        if self.sandbox_id:
            sandbox = Sandbox.connect(self.sandbox_id)  # resumes if paused
            self._owns_sandbox = False
            if self.traffic_access_token is not None:
                self._saved_traffic_token = self.traffic_access_token.get_secret_value()
            logger.info(
                "Attached to CubeSandbox %s in %.0f ms",
                sandbox.sandbox_id,
                (time.monotonic() - started) * 1000,
            )
        else:
            network: dict[str, Any] = dict(self.network or {})
            if self.private_traffic:
                # private_traffic owns allow_public_traffic. Refuse rather
                # than silently resolve: a security flag that fails open on
                # contradictory input is worse than a loud error.
                if network.get("allow_public_traffic") is True:
                    raise ValueError(
                        "private_traffic=True conflicts with "
                        "network={'allow_public_traffic': True} — drop one"
                    )
                network["allow_public_traffic"] = False
            create_kwargs: dict[str, Any] = {
                "template": self.template,
                "timeout": self.sandbox_timeout,
            }
            if network:
                create_kwargs["network"] = network
            sandbox = Sandbox.create(**create_kwargs)
            self._owns_sandbox = True
            self._saved_traffic_token = getattr(sandbox, "traffic_access_token", None)
            logger.info(
                "Created CubeSandbox %s from template %s in %.0f ms",
                sandbox.sandbox_id,
                self.template,
                (time.monotonic() - started) * 1000,
            )
        self._sandbox = sandbox
        object.__setattr__(self, "sandbox_id", sandbox.sandbox_id)

        # From this point on the sandbox exists: any failure below (readiness
        # timeout, parent init error, or KeyboardInterrupt) must release it.
        # Sandboxes we created are killed; attached ones are left running.
        try:
            if not self.host:
                # Same assignment idiom the SDK's own DockerWorkspace uses in
                # its model_post_init.
                object.__setattr__(self, "host", self._public_url(sandbox))

            self._wait_for_ready(timeout=self.health_check_timeout)
            logger.info(
                "OpenHands agent server ready at %s (%.0f ms after create)",
                self.host,
                (time.monotonic() - started) * 1000,
            )

            # Initialize the parent RemoteWorkspace against the live server.
            super().model_post_init(context)
        except BaseException:  # incl. KeyboardInterrupt — must not leak the sandbox
            if self._owns_sandbox:
                self._kill_sandbox()
            else:
                self._sandbox = None
            raise

    def _public_url(self, sandbox: Sandbox) -> str:
        """The agent server's URL through the platform proxy.

        The proxy exposes in-sandbox ports as public hosts of the form
        "<port>-<sandbox-id>.<domain>" (see the quickstart mask-request-host
        example); the proxy's HTTP port is appended when it is not 80.
        """
        public_host = sandbox.get_host(self.agent_server_port)
        port_suffix = ""
        if self.proxy_http_port != 80 and ":" not in public_host:
            port_suffix = f":{self.proxy_http_port}"
        return f"http://{public_host}{port_suffix}"

    # ── Auth ──────────────────────────────────────────────────────────────

    @property
    def _traffic_token(self) -> str | None:
        return self._saved_traffic_token

    @property
    def traffic_token(self) -> str | None:
        """The per-sandbox traffic token (persist it to re-attach to a
        private sandbox from another process)."""
        return self._saved_traffic_token

    @property
    def _headers(self) -> dict[str, str]:
        """Parent headers (X-Session-API-Key) plus the CubeProxy per-sandbox
        traffic token when the sandbox runs with private traffic."""
        headers = dict(super()._headers)
        token = self._traffic_token
        if token:
            headers[TRAFFIC_TOKEN_HEADER] = token
        return headers

    # ── Accessors ─────────────────────────────────────────────────────────

    @property
    def sandbox(self) -> Sandbox:
        """The underlying E2B-compatible sandbox handle."""
        if self._sandbox is None:
            raise RuntimeError("Sandbox is not running (already cleaned up?)")
        return self._sandbox

    # ── Readiness ─────────────────────────────────────────────────────────

    def _wait_for_ready(self, *, timeout: float) -> None:
        """Poll the agent server /ready endpoint until it reports readiness.

        /ready (unlike the liveness /health) returns non-2xx until the server
        has fully finished initialization. Polls with urllib rather than the
        parent's HTTP client: the first call happens during model_post_init,
        before super().model_post_init() has set the client up. ``_headers``
        (session key + traffic token) is honored on both stacks.
        """
        ready_url = f"{self.host.rstrip('/')}/ready"
        deadline = time.monotonic() + timeout
        last_error: Exception | None = None
        while time.monotonic() < deadline:
            try:
                request = Request(ready_url, headers=self._headers)
                with urlopen(request, timeout=2.0) as resp:
                    if 200 <= getattr(resp, "status", 200) < 300:
                        return
            except Exception as exc:  # noqa: BLE001 - retried until deadline
                last_error = exc
            time.sleep(0.5)
        raise RuntimeError(
            f"Agent server at {ready_url} did not become ready within "
            f"{timeout:.0f}s (last error: {last_error!r}). Check that the "
            "template autostarts the agent server on port "
            f"{self.agent_server_port} and that the platform proxy is reachable."
        )

    @property
    def alive(self) -> bool:
        """Readiness through the authenticated client (the parent's bare
        /health probe cannot pass the private-traffic gate)."""
        try:
            response = self.client.get("/ready")
            return 200 <= response.status_code < 300
        except Exception:  # noqa: BLE001 - liveness probe returns bool, never raises
            return False

    # ── Lifecycle ─────────────────────────────────────────────────────────

    def pause(self) -> None:
        """Freeze the entire MicroVM — agent server, shells, and in-flight
        processes included. Memory and filesystem state are preserved.

        Combine with ``kill_on_exit=False`` to keep the paused sandbox for a
        later ``CubeSandboxWorkspace(sandbox_id=...)`` re-attach."""
        logger.info("Pausing sandbox %s", self.sandbox.sandbox_id)
        self.sandbox.pause()

    def resume(self) -> None:
        """Thaw a paused MicroVM and wait for the agent server to respond."""
        sandbox_id = self.sandbox.sandbox_id
        logger.info("Resuming sandbox %s", sandbox_id)
        sandbox = Sandbox.connect(sandbox_id)
        self._sandbox = sandbox
        # Re-derive the proxied URL like the create path does, and drop the
        # parent's cached HTTP client: its base_url is bound to the previous
        # host, so refreshing the field alone would keep requests pinned to a
        # stale address if the proxy scheme changed across the pause.
        object.__setattr__(self, "host", self._public_url(sandbox))
        self.reset_client()
        self._wait_for_ready(timeout=self.health_check_timeout)
        logger.info("Sandbox %s resumed", sandbox_id)

    def _kill_sandbox(self) -> None:
        if self._sandbox is not None:
            logger.info("Killing sandbox %s", self._sandbox.sandbox_id)
            try:
                self._sandbox.kill()
            finally:
                self._sandbox = None

    def cleanup(self) -> None:
        """Release the workspace.

        Kills the sandbox when ``kill_on_exit`` says so (default: only for
        sandboxes this workspace created); otherwise the sandbox keeps
        running/paused for a later re-attach. The HTTP client is closed
        either way."""
        self.reset_client()
        should_kill = (
            self._owns_sandbox if self.kill_on_exit is None else self.kill_on_exit
        )
        if should_kill:
            self._kill_sandbox()
        else:
            if self._sandbox is not None:
                logger.info(
                    "Leaving sandbox %s running (kill_on_exit=False); "
                    "re-attach later with CubeSandboxWorkspace(sandbox_id=...)",
                    self._sandbox.sandbox_id,
                )
            self._sandbox = None

    def __enter__(self) -> Self:
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> None:  # type: ignore[no-untyped-def]
        # RemoteWorkspace.__exit__ only sends the completion callback and
        # BaseWorkspace.__exit__ is a documented no-op, so cleanup must be
        # explicit — and in a finally, so a failed callback cannot leak the
        # sandbox.
        try:
            super().__exit__(exc_type, exc_val, exc_tb)
        finally:
            self.cleanup()
