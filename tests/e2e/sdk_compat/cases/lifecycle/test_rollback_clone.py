# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time

import pytest

from framework.assertions import assert_code_ok
from framework.capabilities import ROLLBACK_CLONE, RUN_CODE
from framework.cleanup import safe_kill

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(ROLLBACK_CLONE),
]

def _state_path(sdk_sandbox) -> str:
    return f"/tmp/sdk-compat-rollback-clone-{sdk_sandbox.sandbox_id}.txt"


def _delete_snapshot_after_runtime_release(
    sdk_sandbox,
    snapshot_id: str,
    *,
    timeout: float,
) -> None:
    deadline = time.monotonic() + timeout
    while True:
        try:
            sdk_sandbox.delete_snapshot(snapshot_id)
            return
        except Exception as exc:  # noqa: BLE001 - normalize backend conflict text
            message = str(exc).lower()
            runtime_ref_conflict = (
                "active runtime ref" in message
                or "attempt is already in progress" in message
            )
            if not runtime_ref_conflict or time.monotonic() >= deadline:
                raise
            time.sleep(0.5)


def test_rollback_restores_snapshot_filesystem_state(
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path = _state_path(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "before-snapshot")
    snapshot_id = None
    cleanup_errors = []
    cleanup_error = None
    try:
        snapshot_id = sdk_sandbox.create_snapshot()
        sdk_sandbox.write_file(state_path, "after-snapshot")

        response = sdk_sandbox.rollback(snapshot_id)

        assert response.get("sandboxID") == sdk_sandbox.sandbox_id, (
            f"rollback response has unexpected sandbox: {response!r}"
        )
        assert response.get("snapshotID") == snapshot_id, (
            f"rollback response has unexpected snapshot: {response!r}"
        )
        assert sdk_sandbox.read_file(state_path) == "before-snapshot"
    finally:
        # Rollback makes this snapshot the sandbox's active runtime base.
        # CubeMaster correctly rejects deleting it until the runtime reference
        # is released by destroying the sandbox.
        cleanup_errors.extend(safe_kill(sdk_sandbox, sdk_e2e_config))
        if snapshot_id:
            try:
                _delete_snapshot_after_runtime_release(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - do not mask the test failure
                cleanup_error = exc
    assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
    assert cleanup_error is None, (
        f"snapshot cleanup failed for {snapshot_id}: {cleanup_error}"
    )


def test_failed_rollback_keeps_sandbox_usable(sdk_sandbox):
    state_path = _state_path(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "still-usable")
    with pytest.raises(Exception, match="(?i)(not found|does not exist)"):
        sdk_sandbox.rollback("snap-sdk-compat-missing")
    assert sdk_sandbox.read_file(state_path) == "still-usable"


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_rollback_restores_kernel_state(sdk_sandbox, sdk_e2e_config):
    seeded = sdk_sandbox.run_code(
        "sdk_rollback_value = 41",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(seeded)
    snapshot_id = None
    cleanup_errors = []
    cleanup_error = None
    try:
        snapshot_id = sdk_sandbox.create_snapshot()
        changed = sdk_sandbox.run_code(
            "sdk_rollback_value = 99",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(changed)
        sdk_sandbox.rollback(snapshot_id)
        restored = sdk_sandbox.run_code(
            "sdk_rollback_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(restored)
        assert restored.text == "42"
    finally:
        cleanup_errors.extend(safe_kill(sdk_sandbox, sdk_e2e_config))
        if snapshot_id:
            try:
                _delete_snapshot_after_runtime_release(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - do not mask the test failure
                cleanup_error = exc
    assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
    assert cleanup_error is None, (
        f"snapshot cleanup failed for {snapshot_id}: {cleanup_error}"
    )


def test_clone_preserves_snapshot_state_and_isolates_clones(
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path = _state_path(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "source-state")
    clones = []
    try:
        clones = sdk_sandbox.clone(n=2, concurrency=2)
        clone_ids = [clone.sandbox_id for clone in clones]

        assert len(clones) == 2
        assert len(set(clone_ids)) == 2, f"clone IDs are not unique: {clone_ids!r}"
        assert sdk_sandbox.sandbox_id not in clone_ids
        for clone in clones:
            assert clone.read_file(state_path) == "source-state", (
                f"clone {clone.sandbox_id} did not preserve source state"
            )

        clones[0].write_file(state_path, "clone-zero")
        assert clones[1].read_file(state_path) == "source-state"
        assert sdk_sandbox.read_file(state_path) == "source-state"
    finally:
        cleanup_errors = []
        for clone in clones:
            cleanup_errors.extend(safe_kill(clone, sdk_e2e_config))
    assert not cleanup_errors, f"clone cleanup failed: {cleanup_errors!r}"


def test_clone_default_returns_one_and_cleans_temporary_snapshot(
    sdk_sandbox,
    sdk_e2e_config,
):
    before = sdk_sandbox.list_snapshot_ids()
    state_path = _state_path(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "default-clone")
    clones = sdk_sandbox.clone()
    assert len(clones) == 1
    try:
        assert clones[0].read_file(state_path) == "default-clone"
    finally:
        cleanup_errors = safe_kill(clones[0], sdk_e2e_config)
    assert not cleanup_errors, f"clone cleanup failed: {cleanup_errors!r}"

    deadline = time.monotonic() + sdk_e2e_config.default_timeout
    while time.monotonic() < deadline:
        if sdk_sandbox.list_snapshot_ids() == before:
            break
        time.sleep(0.5)
    assert sdk_sandbox.list_snapshot_ids() == before, (
        "clone temporary snapshot was not deleted after the last clone was killed"
    )


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_clone_preserves_kernel_state(sdk_sandbox, sdk_e2e_config):
    seeded = sdk_sandbox.run_code(
        "sdk_clone_value = 84",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(seeded)
    clones = sdk_sandbox.clone()
    assert len(clones) == 1
    try:
        inherited = clones[0].run_code(
            "sdk_clone_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(inherited)
        assert inherited.text == "85"
    finally:
        cleanup_errors = safe_kill(clones[0], sdk_e2e_config)
    assert not cleanup_errors, f"clone cleanup failed: {cleanup_errors!r}"
