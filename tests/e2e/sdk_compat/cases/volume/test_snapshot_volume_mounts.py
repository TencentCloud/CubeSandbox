# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Re-sending volumeMounts when creating a sandbox from a snapshot.

Official E2B requires clients to re-send volumeMounts on every create,
including creates from a snapshot. CubeSandbox snapshots already carry their
plugin volume mounts, so an identical re-send must be accepted, while any
change (path, read-only flag, volume set) must be rejected explicitly rather
than silently ignored. See issue #1887.

Prerequisites match ``test_read_only.py``: S3 Volume plugin + MinIO and a
READY template (``CUBE_TEMPLATE_ID``).
"""

from __future__ import annotations

import shlex
import sys
import time
import uuid

import pytest
from adapters import create_adapter, create_adapter_with_capacity_retry
from framework.assertions import assert_command_ok
from framework.capabilities import (
    ROLLBACK_CLONE,
    VOLUME_PLUGIN,
    capabilities_for_backend,
)
from framework.cleanup import safe_kill
from framework.volume import managed_volume

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
    pytest.mark.requires_capability(ROLLBACK_CLONE),
]

MOUNT_PATH = "/mnt/snapshot-resend"
OTHER_MOUNT_PATH = "/mnt/snapshot-resend-other"
EXTRA_MOUNT_PATH = "/mnt/snapshot-resend-extra"
MISMATCH_ERROR = "must match the snapshot's volume mounts"


@pytest.fixture(autouse=True)
def _require_snapshot_prerequisites(sdk_backend, sdk_e2e_config):
    if ROLLBACK_CLONE not in capabilities_for_backend(sdk_backend):
        pytest.skip(f"backend {sdk_backend!r} does not support capability {ROLLBACK_CLONE!r}")
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("CUBE_TEMPLATE_ID or --cube-template-id is required for volume bind")


def _create(sdk_backend, sdk_e2e_config, *, role: str, template=None, volume_mounts=None):
    options: dict = {}
    if template is not None:
        options["template"] = template
    if volume_mounts is not None:
        options["volume_mounts"] = volume_mounts
    return create_adapter_with_capacity_retry(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_suite": "sdk_compat", "test_role": role},
        create_options=options,
    )


def _run_ok(sandbox, command: str, *, timeout: int) -> str:
    result = sandbox.run_command(command, timeout=timeout)
    assert_command_ok(result)
    return result.stdout.strip()


def _delete_snapshot_after_runtime_release(owner, snapshot_id: str, *, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    while True:
        try:
            owner.delete_snapshot(snapshot_id)
            return
        except Exception as exc:  # noqa: BLE001
            retryable = (
                "active runtime ref" in str(exc).lower()
                or "attempt is already in progress" in str(exc).lower()
            )
            if not retryable or time.monotonic() >= deadline:
                raise
            time.sleep(0.5)


def _snapshot_source(sdk_backend, sdk_e2e_config, volume_mounts, *, role: str):
    """Create a source sandbox, snapshot it, kill it; return (owner, snapshot_id, cleanup_errors)."""
    source = _create(sdk_backend, sdk_e2e_config, volume_mounts=volume_mounts, role=role)
    try:
        snapshot_id = source.create_snapshot()
    except Exception:
        safe_kill(source, sdk_e2e_config)
        raise
    return source, snapshot_id, safe_kill(source, sdk_e2e_config)


def test_snapshot_restore_accepts_identical_volume_mounts(sdk_backend, sdk_e2e_config):
    token = uuid.uuid4().hex
    rootfs_path = f"/tmp/snapshot-resend-{token}.txt"
    volume_path = f"{MOUNT_PATH}/state-{token}.txt"
    timeout = sdk_e2e_config.command_timeout
    source = owner = restored = None
    snapshot_id = None
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        try:
            source = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={MOUNT_PATH: volume_id},
                role="snapshot_resend_source",
            )
            owner = source
            _run_ok(
                source,
                f"printf rootfs-at-snapshot > {shlex.quote(rootfs_path)} && "
                f"printf volume-at-snapshot > {shlex.quote(volume_path)}",
                timeout=timeout,
            )
            snapshot_id = source.create_snapshot()
            _run_ok(
                source,
                f"printf rootfs-after-snapshot > {shlex.quote(rootfs_path)} && "
                f"rm -f -- {shlex.quote(volume_path)} && "
                f"printf volume-after-snapshot > {shlex.quote(volume_path)}",
                timeout=timeout,
            )
            cleanup_errors.extend(safe_kill(source, sdk_e2e_config))
            source = None

            restored = _create(
                sdk_backend,
                sdk_e2e_config,
                template=snapshot_id,
                volume_mounts={MOUNT_PATH: volume_id},
                role="snapshot_resend_restore",
            )
            assert restored.read_file(rootfs_path) == "rootfs-at-snapshot"
            assert restored.read_file(volume_path) == "volume-after-snapshot"
            _run_ok(
                restored,
                f"rm -f -- {shlex.quote(volume_path)} && "
                f"printf volume-after-restore > {shlex.quote(volume_path)}",
                timeout=timeout,
            )
            assert restored.read_file(volume_path) == "volume-after-restore"

            cleanup_errors.extend(safe_kill(restored, sdk_e2e_config))
            restored = None
            _delete_snapshot_after_runtime_release(
                owner, snapshot_id, timeout=sdk_e2e_config.default_timeout
            )
            snapshot_id = None
        finally:
            active_failure = sys.exc_info()[0] is not None
            for sandbox in (restored, source):
                if sandbox is not None:
                    cleanup_errors.extend(safe_kill(sandbox, sdk_e2e_config))
            if snapshot_id is not None and owner is not None:
                try:
                    _delete_snapshot_after_runtime_release(
                        owner, snapshot_id, timeout=sdk_e2e_config.default_timeout
                    )
                except Exception as exc:  # noqa: BLE001
                    cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors


def test_snapshot_restore_accepts_identical_read_only_volume_mounts(sdk_backend, sdk_e2e_config):
    from cubesandbox import VolumeMount

    token = uuid.uuid4().hex
    seed_path = f"{MOUNT_PATH}/seed-{token}.txt"
    owner = restored = None
    snapshot_id = None
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        try:
            seed = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={MOUNT_PATH: volume_id},
                role="snapshot_resend_ro_seed",
            )
            try:
                seed.write_file(seed_path, "seeded")
            finally:
                cleanup_errors.extend(safe_kill(seed, sdk_e2e_config))

            read_only = {MOUNT_PATH: VolumeMount(volume_id, read_only=True)}
            owner, snapshot_id, errors = _snapshot_source(
                sdk_backend, sdk_e2e_config, read_only, role="snapshot_resend_ro_source"
            )
            cleanup_errors.extend(errors)

            restored = _create(
                sdk_backend,
                sdk_e2e_config,
                template=snapshot_id,
                volume_mounts={MOUNT_PATH: VolumeMount(volume_id, read_only=True)},
                role="snapshot_resend_ro_restore",
            )
            assert restored.read_file(seed_path) == "seeded"
            result = restored.run_command(
                f"printf denied > {shlex.quote(MOUNT_PATH)}/denied-{token}.txt",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert result.exit_code != 0, "read-only mount accepted a write after restore"

            cleanup_errors.extend(safe_kill(restored, sdk_e2e_config))
            restored = None
            _delete_snapshot_after_runtime_release(
                owner, snapshot_id, timeout=sdk_e2e_config.default_timeout
            )
            snapshot_id = None
        finally:
            active_failure = sys.exc_info()[0] is not None
            if restored is not None:
                cleanup_errors.extend(safe_kill(restored, sdk_e2e_config))
            if snapshot_id is not None and owner is not None:
                try:
                    _delete_snapshot_after_runtime_release(
                        owner, snapshot_id, timeout=sdk_e2e_config.default_timeout
                    )
                except Exception as exc:  # noqa: BLE001
                    cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors


def test_snapshot_restore_rejects_changed_volume_mounts(sdk_backend, sdk_e2e_config):
    from cubesandbox import VolumeMount

    owner = None
    snapshot_id = None
    cleanup_errors: list[object] = []
    with (
        managed_volume(sdk_e2e_config) as (volume_id, _api),
        managed_volume(sdk_e2e_config) as (extra_volume_id, _extra_api),
    ):
        try:
            owner, snapshot_id, errors = _snapshot_source(
                sdk_backend,
                sdk_e2e_config,
                {MOUNT_PATH: volume_id},
                role="snapshot_resend_mismatch_source",
            )
            cleanup_errors.extend(errors)

            variants = {
                "different path": {OTHER_MOUNT_PATH: volume_id},
                "read-only added": {MOUNT_PATH: VolumeMount(volume_id, read_only=True)},
                "extra volume": {MOUNT_PATH: volume_id, EXTRA_MOUNT_PATH: extra_volume_id},
            }
            unexpected: list[str] = []
            for label, mounts in variants.items():
                try:
                    accepted = create_adapter(
                        sdk_backend,
                        sdk_e2e_config,
                        metadata={
                            "test_suite": "sdk_compat",
                            "test_role": "snapshot_resend_mismatch_restore",
                        },
                        create_options={"template": snapshot_id, "volume_mounts": mounts},
                    )
                except Exception as exc:  # noqa: BLE001
                    assert MISMATCH_ERROR in str(exc), f"{label}: unexpected error {exc!r}"
                    continue
                unexpected.append(label)
                cleanup_errors.extend(safe_kill(accepted, sdk_e2e_config))
            assert not unexpected, f"changed volumeMounts were accepted: {unexpected}"

            _delete_snapshot_after_runtime_release(
                owner, snapshot_id, timeout=sdk_e2e_config.default_timeout
            )
            snapshot_id = None
        finally:
            active_failure = sys.exc_info()[0] is not None
            if snapshot_id is not None and owner is not None:
                try:
                    _delete_snapshot_after_runtime_release(
                        owner, snapshot_id, timeout=sdk_e2e_config.default_timeout
                    )
                except Exception as exc:  # noqa: BLE001
                    cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors
