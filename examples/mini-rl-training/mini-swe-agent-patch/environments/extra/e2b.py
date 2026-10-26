"""E2B sandbox environment for mini-SWE-agent.

Uses the E2B SDK to create and manage sandboxes, executing commands
via envd's gRPC interface.

Performance optimization: a shared HTTP connection pool is used for all
API calls (create/delete sandbox), avoiding per-request TCP + TLS
handshake overhead that the default SDK incurs.

Required env vars:
    E2B_API_URL         - E2B API endpoint
    E2B_API_KEY         - API key
    CUBE_SSL_CERT_FILE  - (optional) CA cert for self-hosted setups;
                          applied as SSL_CERT_FILE right before SDK calls
                          to avoid breaking other HTTPS connections.

Config keys (passed via YAML):
    template_id    - E2B template ID (required)
    image          - ignored (for SWE-bench compat), template_id takes precedence
    cwd            - working directory (default: /testbed)
    timeout        - command timeout in seconds (default: 60)
    user           - sandbox user (default: root)
    sandbox_timeout - sandbox lifetime in seconds (default: 1800)
"""

import atexit
import logging
import os
import signal
import threading
import time
import weakref
from contextlib import contextmanager
from typing import Any

from pydantic import BaseModel

from minisweagent.exceptions import Submitted
from minisweagent.utils.serialize import recursive_merge

_active_sandboxes: list[weakref.ref] = []


def _cleanup_all_sandboxes(signum=None, frame=None):
    """Kill all active sandboxes on process exit or signal."""
    for ref in _active_sandboxes:
        env = ref()
        if env is not None:
            env.cleanup()
    _active_sandboxes.clear()
    if signum is not None:
        raise SystemExit(1)


atexit.register(_cleanup_all_sandboxes)

for _sig in (signal.SIGINT, signal.SIGTERM):
    prev = signal.getsignal(_sig)
    def _handler(signum, frame, _prev=prev):
        _cleanup_all_sandboxes(signum, frame)
        if callable(_prev) and _prev not in (signal.SIG_DFL, signal.SIG_IGN):
            _prev(signum, frame)
    signal.signal(_sig, _handler)


# ── Shared API client for connection reuse ───────────────────────────────

_api_client = None
_api_client_lock = threading.Lock()


def _get_shared_api_client():
    """
    Return a thread-safe shared API client with HTTP connection pooling.

    The default SDK creates a new ApiClient (new httpx.Client + TCP
    connection) per _create_sandbox / kill call.  Sharing one client
    lets the connection pool reuse keep-alive connections, saving TCP
    and TLS handshake overhead on every request.
    """
    global _api_client
    if _api_client is not None:
        return _api_client
    with _api_client_lock:
        if _api_client is not None:
            return _api_client
        from httpx import Limits
        from e2b.api import ApiClient
        from e2b.connection_config import ConnectionConfig

        config = ConnectionConfig()
        limits = Limits(
            max_connections=50,
            max_keepalive_connections=50,
            keepalive_expiry=300,
        )
        try:
            client = ApiClient(config, limits=limits)
        except TypeError:
            client = ApiClient(config)
        client.get_httpx_client()
        _api_client = client
        return _api_client


class SandboxInfo:
    """Pre-created sandbox connection details.

    Holds everything needed to connect to an already-running sandbox,
    skipping the API creation call.
    """
    __slots__ = ("sandbox_id", "sandbox_domain", "envd_version",
                 "envd_access_token", "api_call_ms")

    def __init__(self, sandbox_id: str, sandbox_domain: str | None,
                 envd_version: str, envd_access_token: str | None,
                 api_call_ms: float = 0.0):
        self.sandbox_id = sandbox_id
        self.sandbox_domain = sandbox_domain
        self.envd_version = envd_version
        self.envd_access_token = envd_access_token
        self.api_call_ms = api_call_ms

    def __getstate__(self):
        return {s: getattr(self, s) for s in self.__slots__}

    def __setstate__(self, state):
        for k, v in state.items():
            setattr(self, k, v)


def create_sandbox_info(
    template: str,
    timeout: int = 1800,
) -> SandboxInfo:
    """Create a sandbox via API and return its connection info.

    This is a standalone function suitable for use with ProcessPoolExecutor
    or ThreadPoolExecutor.  It uses the shared API client for connection
    reuse and is safe to call from any thread/process.
    """
    from e2b.api.client.types import Unset
    from e2b.api.client.models import NewSandbox, Error
    from e2b.api.client.api.sandboxes import post_sandboxes

    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE", "")
    old_ssl = os.environ.get("SSL_CERT_FILE")
    if cube_ssl:
        os.environ["SSL_CERT_FILE"] = cube_ssl

    try:
        api_client = _get_shared_api_client()
        t1 = time.monotonic()
        res = post_sandboxes.sync_detailed(
            body=NewSandbox(
                template_id=template,
                timeout=timeout,
                auto_pause=False,
                metadata={},
                env_vars={},
                secure=True,
                allow_internet_access=True,
            ),
            client=api_client,
        )
        api_ms = round((time.monotonic() - t1) * 1000)

        if res.status_code >= 300 or res.parsed is None:
            msg = getattr(res.parsed, "message", "") if res.parsed else ""
            raise Exception(f"Sandbox API error {res.status_code}: {msg or res.content}")
        if isinstance(res.parsed, Error):
            raise Exception(f"Sandbox API error: {res.parsed.message}")

        token = res.parsed.envd_access_token
        if isinstance(token, Unset):
            token = None
        domain = res.parsed.domain
        if isinstance(domain, Unset):
            domain = None

        return SandboxInfo(
            sandbox_id=res.parsed.sandbox_id,
            sandbox_domain=domain,
            envd_version=res.parsed.envd_version,
            envd_access_token=token,
            api_call_ms=api_ms,
        )
    finally:
        if old_ssl is None:
            os.environ.pop("SSL_CERT_FILE", None)
        elif old_ssl:
            os.environ["SSL_CERT_FILE"] = old_ssl


def batch_create_sandboxes(
    template: str,
    count: int,
    timeout: int = 1800,
    workers: int = 8,
    on_created: Any = None,
) -> list[SandboxInfo]:
    """Batch-create sandboxes using a process pool.

    Each worker process gets its own HTTP connection pool for true
    parallelism (no GIL).

    Args:
        template:    E2B template ID.
        count:       Number of sandboxes to create.
        timeout:     Sandbox lifetime in seconds.
        workers:     Concurrency level.
        on_created:  Optional callback(index, SandboxInfo) for progress.

    Returns:
        List of SandboxInfo in submission order.  Failed entries are None.
    """
    import concurrent.futures

    results: list[SandboxInfo | None] = [None] * count
    with concurrent.futures.ProcessPoolExecutor(max_workers=workers) as pool:
        futures = {
            pool.submit(create_sandbox_info, template, timeout): i
            for i in range(count)
        }
        for f in concurrent.futures.as_completed(futures):
            idx = futures[f]
            try:
                info = f.result()
                results[idx] = info
                if on_created:
                    on_created(idx, info)
            except Exception as e:
                logging.getLogger("minisweagent.e2b").error(
                    f"Failed to pre-create sandbox #{idx}: {e}"
                )
    return results


class E2BEnvironmentConfig(BaseModel):
    template_id: str = ""
    image: str = ""
    cwd: str = "/testbed"
    timeout: int = 60
    user: str = "root"
    sandbox_timeout: int = 1800
    env: dict[str, str] = {}
    interpreter: list[str] = ["bash", "-c"]


class E2BEnvironment:
    def __init__(self, *, logger: logging.Logger | None = None,
                 sandbox_info: SandboxInfo | None = None, **kwargs):
        self.logger = logger or logging.getLogger("minisweagent.e2b")
        self.config = E2BEnvironmentConfig(**kwargs)
        self._killed = False
        self._cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE", "")

        try:
            from e2b_code_interpreter import Sandbox
            from e2b.connection_config import ConnectionConfig
        except ImportError:
            raise ImportError("e2b-code-interpreter is required: pip install e2b-code-interpreter")

        if sandbox_info is not None:
            self._connect_existing(sandbox_info, Sandbox, ConnectionConfig)
        else:
            self._create_new(Sandbox, ConnectionConfig)

        _active_sandboxes.append(weakref.ref(self))

    def _connect_existing(self, info: SandboxInfo, Sandbox, ConnectionConfig):
        """Connect to a pre-created sandbox (no API call needed)."""
        self.api_call_ms = info.api_call_ms

        t2 = time.monotonic()
        extra_headers = {}
        if info.envd_access_token:
            extra_headers["X-Access-Token"] = info.envd_access_token
        conn = ConnectionConfig(extra_sandbox_headers=extra_headers)
        with self._ssl_context():
            self.sandbox = Sandbox(
                sandbox_id=info.sandbox_id,
                sandbox_domain=info.sandbox_domain,
                envd_version=info.envd_version,
                envd_access_token=info.envd_access_token,
                connection_config=conn,
            )
        self.sdk_init_ms = round((time.monotonic() - t2) * 1000)
        self.setup_time_ms = self.api_call_ms + self.sdk_init_ms

        self.logger.info(
            f"Connected to sandbox: {self.sandbox.sandbox_id} "
            f"(pre-created api={self.api_call_ms}ms, sdk_init={self.sdk_init_ms}ms)"
        )

    def _create_new(self, Sandbox, ConnectionConfig):
        """Create a new sandbox via API (original behavior)."""
        from e2b.api.client.types import Unset
        from e2b.api.client.models import NewSandbox, Error
        from e2b.api.client.api.sandboxes import post_sandboxes

        template = self.config.template_id or os.environ.get("CUBE_TEMPLATE_ID", "")
        if not template:
            raise ValueError(
                "E2B template_id is required. Set it in config YAML or via CUBE_TEMPLATE_ID env var."
            )

        self.logger.info(f"Creating E2B sandbox from template {template}...")
        with self._ssl_context():
            api_client = _get_shared_api_client()

            t1 = time.monotonic()
            res = post_sandboxes.sync_detailed(
                body=NewSandbox(
                    template_id=template,
                    timeout=self.config.sandbox_timeout,
                    auto_pause=False,
                    metadata={},
                    env_vars={},
                    secure=True,
                    allow_internet_access=True,
                ),
                client=api_client,
            )
            self.api_call_ms = round((time.monotonic() - t1) * 1000)

            if res.status_code >= 300 or res.parsed is None:
                msg = getattr(res.parsed, "message", "") if res.parsed else ""
                raise Exception(f"Sandbox API error {res.status_code}: {msg or res.content}")
            if isinstance(res.parsed, Error):
                raise Exception(f"Sandbox API error: {res.parsed.message}")

            token = res.parsed.envd_access_token
            if isinstance(token, Unset):
                token = None

            t2 = time.monotonic()
            extra_headers = {}
            if token:
                extra_headers["X-Access-Token"] = token
            conn = ConnectionConfig(extra_sandbox_headers=extra_headers)
            domain = res.parsed.domain
            if isinstance(domain, Unset):
                domain = None
            self.sandbox = Sandbox(
                sandbox_id=res.parsed.sandbox_id,
                sandbox_domain=domain,
                envd_version=res.parsed.envd_version,
                envd_access_token=token,
                connection_config=conn,
            )
            self.sdk_init_ms = round((time.monotonic() - t2) * 1000)

        self.setup_time_ms = self.api_call_ms + self.sdk_init_ms
        self.logger.info(
            f"Sandbox created: {self.sandbox.sandbox_id} "
            f"(api={self.api_call_ms}ms, sdk_init={self.sdk_init_ms}ms, total={self.setup_time_ms}ms)"
        )

    @contextmanager
    def _ssl_context(self):
        """Temporarily set SSL_CERT_FILE for E2B SDK calls, then restore."""
        old = os.environ.get("SSL_CERT_FILE")
        if self._cube_ssl:
            os.environ["SSL_CERT_FILE"] = self._cube_ssl
        try:
            yield
        finally:
            if old is None:
                os.environ.pop("SSL_CERT_FILE", None)
            else:
                os.environ["SSL_CERT_FILE"] = old

    def execute(self, action: dict, cwd: str = "", *, timeout: int | None = None) -> dict[str, Any]:
        command = action.get("command", "")
        cwd = cwd or self.config.cwd
        effective_timeout = timeout or self.config.timeout

        full_cmd = f"cd {cwd} && {command}"

        try:
            with self._ssl_context():
                result = self.sandbox.commands.run(
                    full_cmd,
                    user=self.config.user,
                    timeout=effective_timeout,
                )
            combined = result.stdout
            if result.stderr:
                combined = combined + result.stderr if combined else result.stderr
            output = {
                "output": combined or "",
                "returncode": result.exit_code,
                "exception_info": "",
            }
        except TimeoutError as e:
            self.logger.warning(f"Command timed out after {effective_timeout}s, cleaning up sandbox")
            self.cleanup()
            raise
        except Exception as e:
            output = {
                "output": str(e) if str(e) else "",
                "returncode": -1,
                "exception_info": f"An error occurred while executing the command: {e}",
                "extra": {"exception_type": type(e).__name__, "exception": str(e)},
            }

        self._check_finished(output)
        return output

    def _check_finished(self, output: dict):
        lines = output.get("output", "").lstrip().splitlines(keepends=True)
        if lines and lines[0].strip() == "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT" and output["returncode"] == 0:
            submission = "".join(lines[1:])
            raise Submitted(
                {
                    "role": "exit",
                    "content": submission,
                    "extra": {"exit_status": "Submitted", "submission": submission},
                }
            )

    def get_template_vars(self, **kwargs) -> dict[str, Any]:
        return recursive_merge(self.config.model_dump(), kwargs)

    def serialize(self) -> dict:
        return {
            "info": {
                "config": {
                    "environment": self.config.model_dump(mode="json"),
                    "environment_type": f"{self.__class__.__module__}.{self.__class__.__name__}",
                }
            }
        }

    def cleanup(self):
        if self._killed:
            return
        if hasattr(self, "sandbox") and self.sandbox is not None:
            try:
                with self._ssl_context():
                    from e2b.api.client.api.sandboxes import delete_sandboxes_sandbox_id
                    api_client = _get_shared_api_client()
                    delete_sandboxes_sandbox_id.sync_detailed(
                        self.sandbox.sandbox_id, client=api_client,
                    )
                self._killed = True
                self.logger.info(f"E2B sandbox {self.sandbox.sandbox_id} killed.")
            except Exception as e:
                self.logger.warning(f"Failed to kill sandbox: {e}")

    def __del__(self):
        self.cleanup()
