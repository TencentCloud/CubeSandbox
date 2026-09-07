# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Snapshot, rollback, and clone contracts for raw host mounts.

Raw host mounts are external references: VM memory and rootfs participate in a
snapshot, but files below the host mount do not. Rollback therefore restores
guest state while retaining the host directory's latest contents. Clones have
isolated guest state but deliberately share a writable raw host mount.

The allowed-prefix root is mounted read-write, and a provisioned child is
mounted read-only as a second virtiofs device. Each case operates in
UUID-named children and removes them before teardown, so concurrent runs do
not collide or leave payload files on the host.
"""

from __future__ import annotations

import shlex
import time
import uuid
from dataclasses import dataclass, replace

import pytest

from adapters import create_adapter_with_capacity_retry
from framework.assertions import assert_command_ok
from framework.capabilities import HOST_MOUNT, ROLLBACK_CLONE
from framework.cleanup import safe_kill
from framework.host_mount import (
    host_mount_metadata,
    mount_option,
    provision_host_dirs,
    skip_if_host_mount_unavailable,
    under_prefix,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.host_mount,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(HOST_MOUNT),
    pytest.mark.requires_capability(ROLLBACK_CLONE),
    pytest.mark.sandbox_create_options(
        metadata=host_mount_metadata(
            [
                mount_option(under_prefix(), "/mnt/sdk-host-rw", read_only=False),
                mount_option(
                    under_prefix("snapshot-ro"),
                    "/mnt/sdk-host-ro",
                    read_only=True,
                ),
            ]
        )
    ),
]

_RW_HOST_MOUNT = "/mnt/sdk-host-rw"
_RO_HOST_MOUNT = "/mnt/sdk-host-ro"
_provisioned_backends: set[str] = set()


@pytest.fixture(autouse=True)
def _provision_ro_host_dir(sdk_backend, sdk_e2e_config):
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    if sdk_backend in _provisioned_backends:
        return
    provision_host_dirs(sdk_backend, sdk_e2e_config, ["snapshot-ro"])
    _provisioned_backends.add(sdk_backend)


@dataclass(frozen=True)
class _TestPaths:
    rw_dir: str
    rw_file: str
    ro_dir_via_rw: str
    ro_dir: str
    ro_file_via_rw: str
    ro_file: str
    guest_file: str


def _run_ok(sandbox, command: str, *, timeout: int) -> str:
    result = sandbox.run_command(command, timeout=timeout)
    assert_command_ok(result)
    return result.stdout.strip()


def _paths() -> _TestPaths:
    token = uuid.uuid4().hex
    rw_dir = f"{_RW_HOST_MOUNT}/sdk-e2e-snapshot/{token}"
    ro_dir_via_rw = f"{_RW_HOST_MOUNT}/snapshot-ro/sdk-e2e-snapshot/{token}"
    ro_dir = f"{_RO_HOST_MOUNT}/sdk-e2e-snapshot/{token}"
    return _TestPaths(
        rw_dir=rw_dir,
        rw_file=f"{rw_dir}/external.txt",
        ro_dir_via_rw=ro_dir_via_rw,
        ro_dir=ro_dir,
        ro_file_via_rw=f"{ro_dir_via_rw}/external-ro.txt",
        ro_file=f"{ro_dir}/external-ro.txt",
        guest_file=f"/tmp/sdk-e2e-guest-{token}.txt",
    )


def _remove_host_dirs(sandbox, paths: _TestPaths, *, timeout: int) -> None:
    result = sandbox.run_command(
        f"rm -rf -- {shlex.quote(paths.rw_dir)} "
        f"{shlex.quote(paths.ro_dir_via_rw)}",
        timeout=timeout,
    )
    assert_command_ok(result)


def _assert_ro_mount_rejects_write(sandbox, path: str, *, timeout: int) -> None:
    result = sandbox.run_command(
        f"printf denied > {shlex.quote(path)}",
        timeout=timeout,
    )
    assert result.exit_code != 0, (
        f"read-only host mount unexpectedly accepted a write to {path!r}"
    )
    assert "read-only" in result.stderr.lower() or "read only" in result.stderr.lower(), (
        f"expected a read-only filesystem error, got stderr={result.stderr!r}"
    )


def _delete_snapshot_after_runtime_release(
    sandbox,
    snapshot_id: str,
    *,
    timeout: float,
) -> None:
    deadline = time.monotonic() + timeout
    while True:
        try:
            sandbox.delete_snapshot(snapshot_id)
            return
        except Exception as exc:  # noqa: BLE001 - normalize asynchronous ref release
            message = str(exc).lower()
            retryable = (
                "active runtime ref" in message
                or "attempt is already in progress" in message
            )
            if not retryable or time.monotonic() >= deadline:
                raise
            time.sleep(0.5)


def test_snapshot_restore_reverts_guest_and_keeps_external_mounts_current(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    """Creating from an exact snapshot restores guest state and remaps both mounts."""
    paths = _paths()
    snapshot_ids = []
    target_snapshot_id = None
    restored = None
    cleanup_errors = []
    try:
        _run_ok(
            sdk_sandbox,
            f"mkdir -p {shlex.quote(paths.rw_dir)} "
            f"{shlex.quote(paths.ro_dir_via_rw)} && "
            f"printf external-before > {shlex.quote(paths.rw_file)} && "
            f"printf external-ro-before > {shlex.quote(paths.ro_file_via_rw)} && "
            f"printf guest-before > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )

        target_snapshot_id = sdk_sandbox.create_snapshot()
        snapshot_ids.append(target_snapshot_id)
        assert target_snapshot_id.startswith("snap-"), (
            f"unexpected snapshot ID: {target_snapshot_id!r}"
        )
        _run_ok(
            sdk_sandbox,
            f"printf guest-decoy > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        snapshot_ids.append(sdk_sandbox.create_snapshot())
        _run_ok(
            sdk_sandbox,
            f"printf external-after > {shlex.quote(paths.rw_file)} && "
            f"printf external-ro-after > {shlex.quote(paths.ro_file_via_rw)} && "
            f"printf guest-after > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )

        restored = create_adapter_with_capacity_retry(
            sdk_backend,
            replace(sdk_e2e_config, cube_template_id=target_snapshot_id),
            metadata={
                "test_suite": "sdk_compat",
                "test_role": "host_mount_snapshot_restore",
            },
        )
        assert _run_ok(
            restored,
            f"cat {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "guest-before"
        assert _run_ok(
            restored,
            f"cat {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-after"
        assert _run_ok(
            restored,
            f"cat {shlex.quote(paths.ro_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-ro-after"
        _assert_ro_mount_rejects_write(
            restored,
            f"{paths.ro_dir}/denied.txt",
            timeout=sdk_e2e_config.command_timeout,
        )

        _run_ok(
            restored,
            f"printf restored-rw > {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert _run_ok(
            sdk_sandbox,
            f"cat {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "restored-rw"
    finally:
        try:
            _remove_host_dirs(
                sdk_sandbox,
                paths,
                timeout=sdk_e2e_config.command_timeout,
            )
        except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
            cleanup_errors.append(exc)
        if restored is not None:
            cleanup_errors.extend(safe_kill(restored, sdk_e2e_config))
        for snapshot_id in reversed(snapshot_ids):
            try:
                _delete_snapshot_after_runtime_release(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
                cleanup_errors.append(exc)
    assert not cleanup_errors, f"snapshot restore cleanup failed: {cleanup_errors!r}"


def test_snapshot_restore_chain_remaps_host_mounts_each_generation(
    sdk_sandbox,
    sdk_backend,
    sdk_e2e_config,
):
    """A snapshot made after restore must remap mounts on the next restore."""
    paths = _paths()
    snapshot_ids = []
    first_restore = None
    second_restore = None
    cleanup_errors = []
    try:
        _run_ok(
            sdk_sandbox,
            f"mkdir -p {shlex.quote(paths.rw_dir)} "
            f"{shlex.quote(paths.ro_dir_via_rw)} && "
            f"printf external-a > {shlex.quote(paths.rw_file)} && "
            f"printf external-ro-a > {shlex.quote(paths.ro_file_via_rw)} && "
            f"printf guest-a > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        snapshot_a = sdk_sandbox.create_snapshot()
        snapshot_ids.append(snapshot_a)

        first_restore = create_adapter_with_capacity_retry(
            sdk_backend,
            replace(sdk_e2e_config, cube_template_id=snapshot_a),
            metadata={
                "test_suite": "sdk_compat",
                "test_role": "host_mount_snapshot_chain_first_restore",
            },
        )
        assert _run_ok(
            first_restore,
            f"cat {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-a"
        assert _run_ok(
            first_restore,
            f"cat {shlex.quote(paths.ro_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-ro-a"
        _run_ok(
            first_restore,
            f"printf external-b > {shlex.quote(paths.rw_file)} && "
            f"printf guest-b > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        snapshot_b = first_restore.create_snapshot()
        snapshot_ids.append(snapshot_b)

        # Snapshot B contains filter state produced by a restored instance.
        # Destroy that instance so its per-sandbox bind path cannot mask a
        # failed remap when snapshot B is restored again.
        first_restore_cleanup = safe_kill(first_restore, sdk_e2e_config)
        first_restore = None
        assert not first_restore_cleanup, (
            f"first restored sandbox cleanup failed: {first_restore_cleanup!r}"
        )

        second_restore = create_adapter_with_capacity_retry(
            sdk_backend,
            replace(sdk_e2e_config, cube_template_id=snapshot_b),
            metadata={
                "test_suite": "sdk_compat",
                "test_role": "host_mount_snapshot_chain_second_restore",
            },
        )
        assert _run_ok(
            second_restore,
            f"cat {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "guest-b"
        assert _run_ok(
            second_restore,
            f"cat {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-b"
        assert _run_ok(
            second_restore,
            f"cat {shlex.quote(paths.ro_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-ro-a"
        _assert_ro_mount_rejects_write(
            second_restore,
            f"{paths.ro_dir}/denied-after-second-restore.txt",
            timeout=sdk_e2e_config.command_timeout,
        )
    finally:
        host_cleanup_error = None
        for cleanup_sandbox in (second_restore, first_restore, sdk_sandbox):
            if cleanup_sandbox is None:
                continue
            try:
                _remove_host_dirs(
                    cleanup_sandbox,
                    paths,
                    timeout=sdk_e2e_config.command_timeout,
                )
                host_cleanup_error = None
                break
            except Exception as exc:  # noqa: BLE001 - try another live mount
                host_cleanup_error = exc
        if host_cleanup_error is not None:
            cleanup_errors.append(host_cleanup_error)
        if second_restore is not None:
            cleanup_errors.extend(safe_kill(second_restore, sdk_e2e_config))
        if first_restore is not None:
            cleanup_errors.extend(safe_kill(first_restore, sdk_e2e_config))
        for snapshot_id in reversed(snapshot_ids):
            try:
                _delete_snapshot_after_runtime_release(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
                cleanup_errors.append(exc)
    assert not cleanup_errors, f"snapshot chain cleanup failed: {cleanup_errors!r}"


def test_rollback_restores_guest_state_but_not_host_mount_data(
    sdk_sandbox,
    sdk_e2e_config,
):
    """Rollback restores guest state while preserving external data and access."""
    paths = _paths()
    host_created_after = f"{paths.rw_dir}/created-after-snapshot.txt"
    host_deleted_after = f"{paths.rw_dir}/deleted-after-snapshot.txt"
    snapshot_ids = []
    target_snapshot_id = None
    cleanup_errors = []
    try:
        _run_ok(
            sdk_sandbox,
            f"mkdir -p {shlex.quote(paths.rw_dir)} "
            f"{shlex.quote(paths.ro_dir_via_rw)} && "
            f"printf external-before > {shlex.quote(paths.rw_file)} && "
            f"printf external-ro-before > {shlex.quote(paths.ro_file_via_rw)} && "
            f"printf delete-me > {shlex.quote(host_deleted_after)} && "
            f"printf guest-before > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        target_snapshot_id = sdk_sandbox.create_snapshot()
        snapshot_ids.append(target_snapshot_id)
        _run_ok(
            sdk_sandbox,
            f"printf guest-decoy > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        snapshot_ids.append(sdk_sandbox.create_snapshot())
        _run_ok(
            sdk_sandbox,
            f"printf external-after > {shlex.quote(paths.rw_file)} && "
            f"printf external-ro-after > {shlex.quote(paths.ro_file_via_rw)} && "
            f"printf created-after > {shlex.quote(host_created_after)} && "
            f"rm -- {shlex.quote(host_deleted_after)} && "
            f"printf guest-after > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )

        response = sdk_sandbox.rollback(target_snapshot_id)

        assert response.get("sandboxID") == sdk_sandbox.sandbox_id, response
        assert response.get("snapshotID") == target_snapshot_id, response
        assert _run_ok(
            sdk_sandbox,
            f"cat {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "guest-before"
        assert _run_ok(
            sdk_sandbox,
            f"cat {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-after"
        assert _run_ok(
            sdk_sandbox,
            f"cat {shlex.quote(paths.ro_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-ro-after"
        assert _run_ok(
            sdk_sandbox,
            f"cat {shlex.quote(host_created_after)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "created-after"
        _run_ok(
            sdk_sandbox,
            f"test ! -e {shlex.quote(host_deleted_after)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        _assert_ro_mount_rejects_write(
            sdk_sandbox,
            f"{paths.ro_dir}/denied-after-rollback.txt",
            timeout=sdk_e2e_config.command_timeout,
        )

        # Prove the restored virtiofs channel remains writable, not merely readable.
        assert _run_ok(
            sdk_sandbox,
            f"printf external-after-rollback > {shlex.quote(paths.rw_file)} && "
            f"cat {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        ) == "external-after-rollback"
    finally:
        try:
            _remove_host_dirs(
                sdk_sandbox,
                paths,
                timeout=sdk_e2e_config.command_timeout,
            )
        except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
            cleanup_errors.append(exc)
        cleanup_errors.extend(safe_kill(sdk_sandbox, sdk_e2e_config))
        for snapshot_id in reversed(snapshot_ids):
            try:
                _delete_snapshot_after_runtime_release(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
                cleanup_errors.append(exc)
    assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"


def test_clone_isolates_guest_state_and_shares_rw_host_mount(
    sdk_sandbox,
    sdk_e2e_config,
):
    """Fork-style clones isolate VM state and preserve both external mounts."""
    paths = _paths()
    clone_guest_file = f"{paths.guest_file}.clone-only"
    clones = []
    cleanup_errors = []
    try:
        _run_ok(
            sdk_sandbox,
            f"mkdir -p {shlex.quote(paths.rw_dir)} "
            f"{shlex.quote(paths.ro_dir_via_rw)} && "
            f"printf external-source > {shlex.quote(paths.rw_file)} && "
            f"printf external-ro-source > {shlex.quote(paths.ro_file_via_rw)} && "
            f"printf guest-source > {shlex.quote(paths.guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )

        clones = sdk_sandbox.clone(n=2, concurrency=2)
        assert len(clones) == 2, f"expected two clones, got {len(clones)}"
        left, right = clones

        for clone in clones:
            assert _run_ok(
                clone,
                f"cat {shlex.quote(paths.guest_file)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "guest-source"
            assert _run_ok(
                clone,
                f"cat {shlex.quote(paths.rw_file)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "external-source"
            assert _run_ok(
                clone,
                f"cat {shlex.quote(paths.ro_file)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "external-ro-source"
            _assert_ro_mount_rejects_write(
                clone,
                f"{paths.ro_dir}/denied-from-{clone.sandbox_id}.txt",
                timeout=sdk_e2e_config.command_timeout,
            )

        _run_ok(
            left,
            f"printf clone-overwrite > {shlex.quote(paths.guest_file)} && "
            f"printf clone-private > {shlex.quote(clone_guest_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        for sandbox in (right, sdk_sandbox):
            assert _run_ok(
                sandbox,
                f"cat {shlex.quote(paths.guest_file)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "guest-source"
            _run_ok(
                sandbox,
                f"test ! -e {shlex.quote(clone_guest_file)}",
                timeout=sdk_e2e_config.command_timeout,
            )

        _run_ok(
            left,
            f"printf external-from-left > {shlex.quote(paths.rw_file)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        for sandbox in (right, sdk_sandbox):
            assert _run_ok(
                sandbox,
                f"cat {shlex.quote(paths.rw_file)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "external-from-left"

        _run_ok(
            left,
            f"printf external-ro-from-left > {shlex.quote(paths.ro_file_via_rw)}",
            timeout=sdk_e2e_config.command_timeout,
        )
        for sandbox in (left, right, sdk_sandbox):
            assert _run_ok(
                sandbox,
                f"cat {shlex.quote(paths.ro_file)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "external-ro-from-left"

    finally:
        try:
            _remove_host_dirs(
                sdk_sandbox,
                paths,
                timeout=sdk_e2e_config.command_timeout,
            )
        except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
            cleanup_errors.append(exc)
        for clone in clones:
            cleanup_errors.extend(safe_kill(clone, sdk_e2e_config))
    assert not cleanup_errors, f"clone cleanup failed: {cleanup_errors!r}"
