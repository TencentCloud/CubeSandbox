from __future__ import annotations

import os
import sys
import time
import warnings
from pathlib import Path
from typing import Mapping, Optional

from dotenv import load_dotenv

_SIDECAR_READY = False

# Retry shape matches Cubelet (probe.go); per-attempt timeout is longer on the
# client because dev-machine → sidecar → CubeProxy → envd RTT exceeds in-cluster.
_ENVD_INIT_MAX_ATTEMPTS = 3
_ENVD_INIT_DEFAULT_ATTEMPT_TIMEOUT_S = 5.0
_ENVD_INIT_RETRY_DELAY_S = 0.025


def load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars.

    Does not start the dev sidecar. E2B data-plane scripts should call
    ``ensure_dev_sidecar()`` before ``Sandbox.create()`` when
    ``CUBE_REMOTE_PROXY_BASE`` is set.
    """
    candidate_paths = [
        Path(__file__).with_name(".env"),
        Path.cwd() / ".env",
    ]

    seen_paths = set()
    for path in candidate_paths:
        resolved_path = path.resolve()
        if resolved_path in seen_paths:
            continue
        seen_paths.add(resolved_path)

        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            break


def _e2b_sdk_sidecar_compatible() -> bool:
    try:
        from e2b import ConnectionConfig
        from e2b.sandbox.main import SandboxBase
    except ImportError:
        return False

    required_attrs = [
        (ConnectionConfig, "get_sandbox_url"),
        (SandboxBase, "get_host"),
        (SandboxBase, "_file_url"),
        (SandboxBase, "get_mcp_url"),
    ]
    return all(hasattr(owner, attr) for owner, attr in required_attrs)


def _code_interpreter_sidecar_patch_expected() -> bool:
    try:
        from e2b_code_interpreter.constants import JUPYTER_PORT
    except ImportError:
        return False
    return JUPYTER_PORT is not None


def ensure_dev_sidecar() -> bool:
    """Opt-in: route E2B SDK data-plane traffic via examples/e2b-dev-sidecar.

    Call before ``Sandbox.create()`` in scripts that hit the data plane
    (``commands.run``, ``run_code``, ``files.*``, etc.). Control-plane-only
    scripts can skip this. Best-effort: warns and returns False on failure.
    """
    global _SIDECAR_READY
    if _SIDECAR_READY:
        return True
    if not os.environ.get("CUBE_REMOTE_PROXY_BASE", "").strip():
        return True

    sidecar_dir = Path(__file__).resolve().parent.parent / "e2b-dev-sidecar"
    if not sidecar_dir.is_dir():
        warnings.warn(
            f"CUBE_REMOTE_PROXY_BASE is set but dev sidecar not found at {sidecar_dir}. "
            "Clone the full CubeSandbox repo (quickstart depends on examples/e2b-dev-sidecar/). "
            "Continuing without sidecar — control-plane scripts still work; data-plane needs *.cube.app DNS.",
            stacklevel=2,
        )
        return False

    if not _e2b_sdk_sidecar_compatible():
        warnings.warn(
            "installed E2B SDK is incompatible with examples/e2b-dev-sidecar; "
            "data-plane traffic will not be routed. "
            "Pin e2b-code-interpreter>=2.4,<3 or unset CUBE_REMOTE_PROXY_BASE.",
            stacklevel=2,
        )
        return False

    if str(sidecar_dir) not in sys.path:
        sys.path.insert(0, str(sidecar_dir))

    try:
        import dev_sidecar
        from dev_sidecar import setup_dev_sidecar

        setup_dev_sidecar()
        if not dev_sidecar._PATCHED:
            warnings.warn(
                "dev sidecar did not patch the E2B SDK; "
                "data-plane traffic will not be routed. "
                "See RuntimeWarning from dev_sidecar for details.",
                stacklevel=2,
            )
            return False
        if _code_interpreter_sidecar_patch_expected() and (
            dev_sidecar._ORIGINAL_CODE_INTERPRETER_JUPYTER_URL is None
        ):
            warnings.warn(
                "dev sidecar patched base E2B SDK paths but not e2b_code_interpreter; "
                "commands.run() / run_code() will not route through the sidecar.",
                stacklevel=2,
            )
            return False
    except Exception as exc:
        warnings.warn(
            f"dev sidecar setup failed ({exc!r}); continuing without it. "
            "Unset CUBE_REMOTE_PROXY_BASE or fix the sidecar config to route data-plane traffic.",
            stacklevel=2,
        )
        return False

    _SIDECAR_READY = True
    return True


def _envd_init_attempt_timeout_s() -> float:
    raw = os.environ.get("CUBE_ENVD_INIT_ATTEMPT_TIMEOUT_S", "").strip()
    if not raw:
        return _ENVD_INIT_DEFAULT_ATTEMPT_TIMEOUT_S
    try:
        return max(0.1, float(raw))
    except ValueError:
        warnings.warn(
            f"invalid CUBE_ENVD_INIT_ATTEMPT_TIMEOUT_S={raw!r}; "
            f"using {_ENVD_INIT_DEFAULT_ATTEMPT_TIMEOUT_S}s",
            stacklevel=2,
        )
        return _ENVD_INIT_DEFAULT_ATTEMPT_TIMEOUT_S


def _envd_init_port(sandbox) -> int:
    connection_config = getattr(sandbox, "connection_config", None)
    port = getattr(connection_config, "envd_port", None)
    return int(port) if port else 49983


def _is_retryable_envd_init_status_code(status_code: int) -> bool:
    # Matches Cubelet/services/cubebox/probe.go isRetryableEnvdInitStatusCode.
    return status_code in (502, 503, 504)


def _is_retryable_envd_init_transport_err(exc: BaseException) -> bool:
    # Matches Cubelet/services/cubebox/probe.go isRetryableEnvdInitTransportErr.
    import httpx

    if isinstance(exc, httpx.TimeoutException):
        return True
    if isinstance(exc, httpx.ConnectError):
        msg = str(exc).lower()
        return any(
            token in msg
            for token in ("connection refused", "connection reset", "broken pipe", "eof")
        )
    return False


def _traffic_access_headers(sandbox) -> dict[str, str]:
    token = getattr(sandbox, "traffic_access_token", None)
    if callable(token):
        token = token()
    if token:
        return {"e2b-traffic-access-token": str(token)}
    return {}


def apply_create_time_envs(sandbox, envs: Mapping[str, str]) -> bool:
    """Best-effort push of create-time env vars into envd via POST /init.

    Cubelet also calls /init during sandbox startup, but on some cubebox/VNC
    templates the vars do not survive entrypoint startup. Re-applying after
    ``Sandbox.create()`` helps those templates; on clusters where create-time
    envs already work, a failed re-apply is ignored.

    When the sandbox was created with ``allow_public_traffic=False``, pass
    ``sandbox.traffic_access_token`` via the ``e2b-traffic-access-token``
    header (done automatically when the attribute is present).

    Returns True when envd accepted the payload, False otherwise (warning only).
    """
    if not envs:
        return True

    ensure_dev_sidecar()

    import httpx

    port = _envd_init_port(sandbox)
    host = sandbox.get_host(port)
    base = host if "://" in host else f"http://{host}"
    url = f"{base}/init"
    payload = {"envVars": dict(envs)}
    attempt_timeout = _envd_init_attempt_timeout_s()
    headers = _traffic_access_headers(sandbox)

    last_error: Optional[str] = None
    for attempt in range(_ENVD_INIT_MAX_ATTEMPTS):
        try:
            response = httpx.post(
                url,
                json=payload,
                headers=headers,
                timeout=attempt_timeout,
            )
            if response.status_code < 300:
                return True
            last_error = f"HTTP {response.status_code}: {response.text[:200]}"
            if not _is_retryable_envd_init_status_code(response.status_code):
                break
        except httpx.HTTPError as exc:
            last_error = str(exc)
            if not _is_retryable_envd_init_transport_err(exc):
                break

        if attempt < _ENVD_INIT_MAX_ATTEMPTS - 1:
            time.sleep(_ENVD_INIT_RETRY_DELAY_S)

    warnings.warn(
        f"apply_create_time_envs failed after {_ENVD_INIT_MAX_ATTEMPTS} attempts "
        f"(envd port {port}, timeout {attempt_timeout}s each): {last_error}. "
        "Fallback: commands.run(..., envs={...}).",
        stacklevel=2,
    )
    return False
