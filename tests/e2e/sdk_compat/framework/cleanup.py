# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time

from adapters.api_adapter import ApiClient
from adapters.base import SandboxAdapter
from framework.config import SdkE2EConfig


def safe_kill(adapter: SandboxAdapter, config: SdkE2EConfig) -> list[str]:
    """Best-effort sandbox cleanup.

    Returns diagnostic messages instead of raising, so teardown never hides the
    original test failure.
    """

    errors: list[str] = []
    kill_adapter = adapter
    kill_completed = False
    interrupt: BaseException | None = None
    try:
        try:
            state = adapter.info().state
        except Exception as exc:  # noqa: BLE001 - cleanup must continue
            state = None
            errors.append(f"{adapter.backend}.info failed for {adapter.sandbox_id}: {exc}")

        normalized_state = str(state).lower() if state is not None else None
        if normalized_state == "paused":
            # TODO: use direct deletion once the backend supports deleting
            # paused sandboxes without resuming them first.
            try:
                kill_adapter = adapter.resume_or_connect(timeout=config.default_timeout)
            except Exception as exc:  # noqa: BLE001 - fallback delete handles this
                errors.append(
                    f"{adapter.backend}.resume before kill failed for "
                    f"{adapter.sandbox_id}: {exc}"
                )

        kill_adapter.kill()
        kill_completed = True
        if normalized_state is None:
            _rest_delete(adapter, config, errors)
    except KeyboardInterrupt as exc:
        interrupt = exc
        errors.append(f"{kill_adapter.backend}.kill interrupted for {adapter.sandbox_id}: {exc}")
    except Exception as exc:  # noqa: BLE001 - teardown must be best-effort
        errors.append(f"{kill_adapter.backend}.kill failed for {adapter.sandbox_id}: {exc}")
        _rest_delete(adapter, config, errors)
    finally:
        if not kill_completed and interrupt is not None:
            _rest_delete(adapter, config, errors)
        try:
            if kill_adapter is not adapter:
                kill_adapter.close()
        except Exception as exc:  # noqa: BLE001
            errors.append(
                f"{kill_adapter.backend}.close failed for {adapter.sandbox_id}: {exc}"
            )
        try:
            adapter.close()
        except Exception as exc:  # noqa: BLE001
            errors.append(f"{adapter.backend}.close failed for {adapter.sandbox_id}: {exc}")
    if interrupt is not None:
        raise interrupt
    return errors


def delete_snapshot_after_runtime_release(
    adapter: SandboxAdapter,
    snapshot_id: str,
    *,
    timeout: float,
) -> None:
    """Delete a snapshot after the sandboxes that booted from it are gone.

    A snapshot that still has a live runtime reference is tombstoned right away
    by CubeMaster, so this is normally a single call. Two answers are still
    worth retrying: a delete job that is already running (a repeat DELETE, or
    cleanup racing the node releasing its reference) and, for older Master
    builds, a delete refused because a runtime still references the snapshot.
    Everything else is surfaced unchanged instead of being retried in a loop.
    """

    deadline = time.monotonic() + timeout
    while True:
        try:
            adapter.delete_snapshot(snapshot_id)
            return
        except Exception as exc:  # noqa: BLE001 - normalize backend conflict text
            message = str(exc).lower()
            retryable_conflict = (
                "attempt is already in progress" in message
                or "active runtime ref" in message
            )
            if not retryable_conflict or time.monotonic() >= deadline:
                raise
            time.sleep(0.5)


def _rest_delete(adapter: SandboxAdapter, config: SdkE2EConfig, errors: list[str]) -> None:
    api = None
    try:
        api = ApiClient(config)
        api.delete_sandbox(adapter.sandbox_id)
    except Exception as api_exc:  # noqa: BLE001
        errors.append(f"REST delete failed for {adapter.sandbox_id}: {api_exc}")
    finally:
        if api is not None:
            api.close()
