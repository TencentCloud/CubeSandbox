# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""External lifecycle contract for deleting a referenced snapshot."""

from __future__ import annotations

import sys
import time

import pytest

from adapters import create_adapter, create_adapter_with_capacity_retry
from framework.assertions import assert_command_ok
from framework.capabilities import ROLLBACK_CLONE
from framework.cleanup import safe_kill
from framework.lifecycle import wait_until

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(ROLLBACK_CLONE),
]

_SNAPSHOT_CLEANUP_MIN_TIMEOUT = 300.0


def _is_snapshot_not_found(exc: Exception, snapshot_id: str) -> bool:
    message = str(exc).lower()
    resource = snapshot_id.lower()
    status_code = getattr(exc, "status_code", None)
    if status_code is not None:
        return status_code == 404
    if type(exc).__name__ == "TemplateNotFoundError":
        return resource in message
    return resource in message and "not found" in message


def _is_delete_retryable(exc: Exception) -> bool:
    status_code = getattr(exc, "status_code", None)
    if status_code is not None:
        return status_code in (409, 500, 502, 503, 504)
    message = str(exc).lower()
    return "in progress" in message or "already deleting" in message


def _wait_for_physical_cleanup(owner, snapshot_id: str, *, timeout: float) -> None:
    """Observe eventual cleanup through the strongest public SDK signal.

    DELETE is idempotently successful while the tombstone row remains. Once the
    last runtime reference is released and cleanup removes that row, the same
    public SDK call returns not-found. The SDK exposes no tombstone/physical
    status endpoint, so a successful retry may also help start cleanup after a
    lost asynchronous finalizer; either path must converge to not-found.
    """
    started = time.monotonic()
    timeout = max(timeout, _SNAPSHOT_CLEANUP_MIN_TIMEOUT)
    deadline = started + timeout
    last_observation = "DELETE still succeeded for the tombstone"
    while time.monotonic() < deadline:
        try:
            owner.delete_snapshot(snapshot_id)
        except Exception as exc:  # noqa: BLE001 - normalize SDK/API versions
            if _is_snapshot_not_found(exc, snapshot_id):
                return
            if not _is_delete_retryable(exc):
                raise
            last_observation = f"delete remains retryable: {exc}"
        else:
            last_observation = "DELETE still succeeded for the tombstone"
        time.sleep(0.5)
    elapsed = time.monotonic() - started
    raise AssertionError(
        f"snapshot {snapshot_id} was not physically removed within {timeout}s "
        f"after its runtime reference was released (elapsed={elapsed:.1f}s; "
        f"{last_observation})"
    )


def test_delete_referenced_snapshot_retires_new_use_but_keeps_runtime_alive(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    state_path = f"/tmp/sdk-compat-snapshot-delete-{sdk_sandbox.sandbox_id}.txt"
    restored = None
    unexpected_restore = None
    snapshot_id = None
    cleanup_errors: list[object] = []

    try:
        sdk_sandbox.write_file(state_path, "captured")
        snapshot_id = sdk_sandbox.create_snapshot()
        assert snapshot_id in sdk_sandbox.list_snapshot_ids()

        restored = create_adapter_with_capacity_retry(
            sdk_backend,
            sdk_e2e_config,
            metadata={
                "test_suite": "sdk_compat",
                "test_role": "referenced_snapshot_delete_restore",
            },
            create_options={"template": snapshot_id},
        )
        assert restored.read_file(state_path) == "captured"

        # The public delete call must logically retire (tombstone) the snapshot
        # even though this running sandbox still holds its runtime reference.
        sdk_sandbox.delete_snapshot(snapshot_id)
        wait_until(
            lambda: snapshot_id not in sdk_sandbox.list_snapshot_ids(),
            timeout=sdk_e2e_config.default_timeout,
            interval=0.5,
            description=f"snapshot {snapshot_id} to disappear from SDK listing",
        )

        # A repeat DELETE stays idempotently successful while the runtime
        # reference keeps the tombstone and physical artifacts alive.
        sdk_sandbox.delete_snapshot(snapshot_id)

        # Logical retirement rejects new consumers but must not invalidate the
        # already-running VM that still depends on the snapshot artifacts.
        assert restored.read_file(state_path) == "captured"
        restored.write_file(state_path, "still-running-after-delete")
        assert restored.read_file(state_path) == "still-running-after-delete"
        command = restored.run_command("true", timeout=sdk_e2e_config.command_timeout)
        assert_command_ok(command)

        try:
            unexpected_restore = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_role": "deleted_snapshot_restore_rejected",
                },
                create_options={"template": snapshot_id},
            )
        except Exception as exc:  # noqa: BLE001 - assert SDK-version-neutral error
            assert _is_snapshot_not_found(exc, snapshot_id), (
                "a tombstoned snapshot should reject new restores as not-found, "
                f"got {type(exc).__name__}: {exc}"
            )
        else:
            pytest.fail(
                "a tombstoned snapshot unexpectedly accepted a new restore "
                f"({unexpected_restore.sandbox_id})"
            )

        release_errors = safe_kill(restored, sdk_e2e_config)
        restored = None
        assert not release_errors, f"restored sandbox cleanup failed: {release_errors!r}"

        # Listing proves immediate visibility semantics; a later 404 from the
        # idempotent DELETE proves the hidden tombstone itself was reclaimed.
        _wait_for_physical_cleanup(
            sdk_sandbox,
            snapshot_id,
            timeout=sdk_e2e_config.default_timeout,
        )
        snapshot_id = None
    finally:
        active_failure = sys.exc_info()[0] is not None
        for sandbox in (unexpected_restore, restored):
            if sandbox is not None:
                cleanup_errors.extend(safe_kill(sandbox, sdk_e2e_config))
        if snapshot_id is not None:
            try:
                _wait_for_physical_cleanup(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - preserve test diagnostics
                cleanup_errors.append(exc)
        if not active_failure:
            assert not cleanup_errors, cleanup_errors
