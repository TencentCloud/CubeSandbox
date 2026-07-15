# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time
from collections.abc import Callable
from contextlib import contextmanager
from typing import Any

from adapters import connect_adapter, create_adapter, list_sandboxes
from adapters.base import SandboxAdapter
from framework.cleanup import safe_kill
from framework.config import SdkE2EConfig

PAUSED_STATES = frozenset({"paused"})
RUNNING_STATES = frozenset({"running"})
TERMINAL_STATES = frozenset({"terminated", "killed", "killing", "stopped"})
DEFAULT_IDLE_TIMEOUT = 30
IDLE_WAIT_MARGIN = 20
PLATFORM_LIFECYCLE_SKIP_REASON = (
    "platform lifecycle coordinator did not act within the configured window; "
    "ensure cube-lifecycle-manager is running, cube-proxy heartbeats are healthy, "
    "and CUBE_PROXY_NODE_IP can reach the proxy admin port "
    "(see docs/guide/lifecycle.md)"
)


from framework.models import state_from_raw


def wait_until(
    predicate: Callable[[], bool],
    *,
    timeout: float = 90,
    interval: float = 1,
    description: str = "condition",
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(interval)
    raise AssertionError(f"timed out waiting for {description} within {timeout}s")


def wait_until_state(
    adapter: SandboxAdapter,
    states: frozenset[str],
    *,
    timeout: float = 90,
    interval: float = 1,
) -> str:
    observed = "unknown"

    def _matches() -> bool:
        nonlocal observed
        observed = fetch_state(adapter)
        return observed in states

    wait_until(
        _matches,
        timeout=timeout,
        interval=interval,
        description=f"state in {sorted(states)} (last={observed!r})",
    )
    return observed


def wait_until_paused(adapter: SandboxAdapter, *, timeout: float = 90) -> str:
    return wait_until_state(adapter, PAUSED_STATES, timeout=timeout)


def wait_until_running(adapter: SandboxAdapter, *, timeout: float = 90) -> str:
    return wait_until_state(adapter, RUNNING_STATES, timeout=timeout)


def idle_past_timeout(idle_timeout: int, *, margin: int = IDLE_WAIT_MARGIN) -> None:
    time.sleep(idle_timeout + margin)


def wait_for_platform_pause(
    adapter: SandboxAdapter,
    config: SdkE2EConfig,
) -> bool:
    deadline = time.monotonic() + _platform_wait_timeout(config)
    interval = 1.0
    while time.monotonic() < deadline:
        if fetch_state(adapter) in PAUSED_STATES:
            return True
        time.sleep(interval)
        interval = min(interval * 2, 8.0)
    return False


def wait_for_platform_destroy(
    adapter: SandboxAdapter,
    sandbox_id: str,
    backend: str,
    config: SdkE2EConfig,
) -> tuple[bool, dict[str, Any]]:
    details: dict[str, Any] = {}
    deadline = time.monotonic() + _platform_wait_timeout(config)
    interval = 1.0
    while time.monotonic() < deadline:
        details["state"] = fetch_state(adapter)
        details["listed"] = sandbox_listed(sandbox_id, backend, config)
        if details["state"].startswith("unreachable") or details["state"] in TERMINAL_STATES:
            return True, details
        if details["listed"] is False:
            return True, details
        time.sleep(interval)
        interval = min(interval * 2, 8.0)
    return False, details


def sandbox_listed(sandbox_id: str, backend: str, config: SdkE2EConfig) -> bool | None:
    try:
        entries = list_sandboxes(backend, config)
    except Exception:
        return None
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        entry_id = entry.get("sandboxID")
        if entry_id is None:
            entry_id = entry.get("sandbox_id")
        if entry_id == sandbox_id:
            return True
    return False


def fetch_state(adapter: SandboxAdapter) -> str:
    try:
        info = adapter.info()
        state = state_from_raw(info.raw) or info.state
        return str(state or "unknown")
    except Exception as exc:
        return f"unreachable ({type(exc).__name__})"


def assert_connect_fails(
    sandbox_id: str,
    backend: str,
    config: SdkE2EConfig,
) -> str:
    try:
        connected = connect_adapter(backend, sandbox_id, config)
    except Exception as exc:
        return f"{type(exc).__name__}: {exc}"
    try:
        try:
            connected.run_command("true", timeout=config.command_timeout)
        except Exception as exc:
            return f"{type(exc).__name__}: {exc}"
        raise AssertionError("expected connect or command to fail for killed sandbox")
    finally:
        connected.close()


def is_terminal_failure(exc: Exception) -> bool:
    message = str(exc).lower()
    terminal_markers = (
        "404",
        "not found",
        "terminated",
        "terminal",
        "killed",
        "stopped",
        "does not exist",
        "already",
    )
    return any(marker in message for marker in terminal_markers)


def create_control_sandbox(
    backend: str,
    config: SdkE2EConfig,
    *,
    metadata: dict[str, str] | None = None,
) -> SandboxAdapter:
    return create_adapter(
        backend,
        config,
        metadata={
            "test_suite": "sdk_compat",
            "test_role": "lifecycle_control",
            **(metadata or {}),
        },
    )


@contextmanager
def managed_control_sandbox(
    backend: str,
    config: SdkE2EConfig,
    *,
    metadata: dict[str, str] | None = None,
):
    control = create_control_sandbox(backend, config, metadata=metadata)
    try:
        yield control
    finally:
        safe_kill(control, config)


def metadata_from_info(raw: dict[str, Any]) -> dict[str, Any]:
    metadata = raw.get("metadata")
    return metadata if isinstance(metadata, dict) else {}


def _platform_wait_timeout(config: SdkE2EConfig) -> int:
    return (
        config.platform_lifecycle_idle_timeout
        + config.platform_lifecycle_wait_margin
        + config.platform_lifecycle_poll_timeout
    )
