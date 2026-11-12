# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""External mounts across a cold boot.

A sandbox created from a filesystem-only snapshot and a restarted sandbox both
rebuild their containers, so volumes must be re-declared and bound again. These
cases pin the user-visible result: the mount is there, data survives, read-only
stays read-only, and an unusable mount fails the create.
"""

from __future__ import annotations

import uuid

import pytest

from adapters import create_adapter, create_adapter_with_capacity_retry
from adapters.api_adapter import ApiClient
from framework.assertions import assert_command_ok
from framework.capabilities import (
    COMMANDS,
    FILESYSTEM_ONLY_SNAPSHOT,
    HOST_MOUNT,
    RESTART_POLICY,
    VOLUME_PLUGIN,
    require_capabilities,
)
from framework.cleanup import delete_snapshot_after_runtime_release, safe_kill
from framework.host_mount import (
    host_mount_metadata,
    is_missing_hostpath_source_error,
    mount_option,
    provision_host_dirs,
    skip_if_host_mount_unavailable,
    under_prefix,
    warn_pass_missing_hostpath_source,
)
from framework.lifecycle import wait_until_data_plane_ready
from framework.restart_policy import crash_guest, wait_for_restart_status
from framework.volume import managed_volume

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(COMMANDS),
]

FS_ONLY = "fs-only"
_PLUGIN_MOUNT = "/mnt/cold-start-plugin"
_HOST_MOUNT = "/mnt/cold-start-host"
_HOST_BACKING = "sdk-cold-start-host"
_FAST_BACKOFF = {"initialIntervalSecond": 1, "multiplier": 1, "maxRestarts": 5}
# A healthy guest keeps answering this probe; a crashed one fails it at once.
_HEALTHY_PROBE = {
    "httpGet": {"path": "/health", "port": 49983},
    "initialDelaySecond": 1,
    "periodSecond": 1,
    "failureThreshold": 1,
    "probeTimeoutSecond": 1,
}
_RESTART_OPTIONS = {
    "restartPolicy": "OnFailure",
    "restartBackoff": _FAST_BACKOFF,
    "livenessProbe": _HEALTHY_PROBE,
    "timeout": 1800,
}


def _count(status: dict | None) -> int:
    if not status:
        return 0
    return int(status.get("restartCount") or 0)


def _mount_entries(sandbox, config, path: str) -> list[str]:
    """Every guest mount table entry that lands on ``path``."""
    result = sandbox.run_command(
        f"cat /proc/self/mountinfo | grep -F ' {path} ' || true",
        timeout=config.command_timeout,
    )
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def _assert_mounted(
    sandbox, config, path: str, *, role: str, read_only: bool = False
) -> None:
    """Assert one virtiofs mount on ``path`` with the expected read-only mode."""
    entries = _mount_entries(sandbox, config, path)
    assert len(entries) == 1, (
        f"{role}: expected exactly one mount on {path} in sandbox "
        f"{sandbox.sandbox_id}, got {entries!r}"
    )
    before, _, after = entries[0].partition(" - ")
    assert after.startswith("virtiofs "), (
        f"{role}: {path} is not a virtiofs mount: {entries[0]!r}"
    )
    fields = before.split()
    assert len(fields) >= 6, f"{role}: unexpected mountinfo line: {entries[0]!r}"
    mount_options = set(fields[5].split(","))
    source = after.split()[1] if len(after.split()) > 1 else ""
    channel = "virtio_ro" if read_only else "virtio_rw"
    assert source == channel or source.startswith(f"{channel}/"), (
        f"{role}: {path} is served by {source!r}, want the {channel} channel"
    )
    assert ("ro" in mount_options) == read_only, (
        f"{role}: {path} mount options {sorted(mount_options)} disagree with "
        f"read_only={read_only}"
    )


def _cold_restart(sandbox, config) -> dict | None:
    """Crash the guest and wait for the same sandbox to come back running."""
    crash_guest(sandbox, timeout=config.command_timeout)
    status = wait_for_restart_status(
        sandbox,
        lambda item: bool(item)
        and item.get("restartState") == "Running"
        and _count(item) >= 1,
        timeout=config.restart_policy_wait,
    )
    assert status is not None, f"sandbox {sandbox.sandbox_id} did not restart"
    wait_until_data_plane_ready(
        sandbox,
        timeout=config.default_timeout,
        command_timeout=config.command_timeout,
    )
    return status


def _create_from_snapshot(
    sdk_backend, config, snapshot_id: str, *, role: str, metadata=None, options=None
):
    create_options = {"template": snapshot_id}
    create_options.update(options or {})
    sandbox = create_adapter_with_capacity_retry(
        sdk_backend,
        config,
        metadata={"test_suite": "sdk_compat", "test_role": role, **(metadata or {})},
        create_options=create_options,
    )
    try:
        wait_until_data_plane_ready(
            sandbox,
            timeout=config.default_timeout,
            command_timeout=config.command_timeout,
        )
    except Exception:
        safe_kill(sandbox, config)
        raise
    return sandbox


def _snapshot_api(config) -> ApiClient:
    return ApiClient(config)


@pytest.mark.requires_capability(RESTART_POLICY)
@pytest.mark.requires_capability(VOLUME_PLUGIN)
def test_restart_keeps_plugin_volume_mount(sdk_backend, sdk_e2e_config):
    """A cold restart re-mounts the plugin volume on its container path."""
    from cubesandbox import VolumeMount

    require_capabilities(sdk_backend, RESTART_POLICY, VOLUME_PLUGIN)

    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        sandbox = create_adapter(
            sdk_backend,
            sdk_e2e_config,
            metadata={"test_suite": "sdk_compat", "test_role": "restart_plugin_volume"},
            create_options={
                "volume_mounts": {_PLUGIN_MOUNT: VolumeMount(volume_id, read_only=False)},
                **_RESTART_OPTIONS,
            },
        )
        try:
            wait_until_data_plane_ready(
                sandbox,
                timeout=sdk_e2e_config.default_timeout,
                command_timeout=sdk_e2e_config.command_timeout,
            )
            _assert_mounted(sandbox, sdk_e2e_config, _PLUGIN_MOUNT, role="before restart")
            assert_command_ok(
                sandbox.run_command(
                    f"printf before-restart > {_PLUGIN_MOUNT}/marker",
                    timeout=sdk_e2e_config.command_timeout,
                )
            )

            _cold_restart(sandbox, sdk_e2e_config)

            _assert_mounted(sandbox, sdk_e2e_config, _PLUGIN_MOUNT, role="after restart")
            marker = sandbox.run_command(
                f"cat {_PLUGIN_MOUNT}/marker",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert_command_ok(marker)
            assert marker.stdout.strip() == "before-restart", (
                f"sandbox {sandbox.sandbox_id} lost the plugin volume data: {marker.stdout!r}"
            )
        finally:
            safe_kill(sandbox, sdk_e2e_config)


@pytest.mark.requires_capability(RESTART_POLICY)
@pytest.mark.requires_capability(HOST_MOUNT)
def test_restart_keeps_host_mount(sdk_backend, sdk_e2e_config):
    """A cold restart re-mounts a host directory on its container path."""
    require_capabilities(sdk_backend, RESTART_POLICY, HOST_MOUNT)
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    provision_host_dirs(sdk_backend, sdk_e2e_config, [_HOST_BACKING])

    sandbox = create_adapter(
        sdk_backend,
        sdk_e2e_config,
        metadata={
            "test_suite": "sdk_compat",
            "test_role": "restart_host_mount",
            **host_mount_metadata([mount_option(under_prefix(_HOST_BACKING), _HOST_MOUNT)]),
        },
        create_options=_RESTART_OPTIONS,
    )
    try:
        wait_until_data_plane_ready(
            sandbox,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        _assert_mounted(sandbox, sdk_e2e_config, _HOST_MOUNT, role="before restart")
        assert_command_ok(
            sandbox.run_command(
                f"printf before-restart > {_HOST_MOUNT}/marker",
                timeout=sdk_e2e_config.command_timeout,
            )
        )

        _cold_restart(sandbox, sdk_e2e_config)

        _assert_mounted(sandbox, sdk_e2e_config, _HOST_MOUNT, role="after restart")
        marker = sandbox.run_command(
            f"cat {_HOST_MOUNT}/marker", timeout=sdk_e2e_config.command_timeout
        )
        assert_command_ok(marker)
        assert marker.stdout.strip() == "before-restart", (
            f"sandbox {sandbox.sandbox_id} lost the host mount data: {marker.stdout!r}"
        )
    finally:
        safe_kill(sandbox, sdk_e2e_config)


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(VOLUME_PLUGIN)
def test_fs_only_create_mounts_plugin_volume(sdk_backend, sdk_sandbox, sdk_e2e_config):
    """A caller-declared plugin volume is mounted by the cold-booted guest."""
    from cubesandbox import VolumeMount

    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    try:
        status, snapshot = api.create_snapshot(sdk_sandbox.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]
        with managed_volume(sdk_e2e_config) as (volume_id, _volume_api):
            child = _create_from_snapshot(
                sdk_backend,
                sdk_e2e_config,
                snapshot_id,
                role="fs_only_mounts_plugin_volume",
                options={
                    "volume_mounts": {_PLUGIN_MOUNT: VolumeMount(volume_id, read_only=False)}
                },
            )
            _assert_mounted(child, sdk_e2e_config, _PLUGIN_MOUNT, role="fs-only child")
            written = child.run_command(
                f"printf child-data > {_PLUGIN_MOUNT}/marker && cat {_PLUGIN_MOUNT}/marker",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert_command_ok(written)
            assert written.stdout.strip() == "child-data", (
                f"sandbox {child.sandbox_id} cannot write its plugin volume: {written.stdout!r}"
            )
    finally:
        api.close()
        errors = safe_kill(child, sdk_e2e_config) if child is not None else []
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    sdk_sandbox, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001 - report, never mask the assertion
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(HOST_MOUNT)
def test_fs_only_create_mounts_host_directory(sdk_backend, sdk_sandbox, sdk_e2e_config):
    """A caller-declared host directory is visible to the cold-booted guest."""
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    provision_host_dirs(sdk_backend, sdk_e2e_config, [_HOST_BACKING])
    mount = [mount_option(under_prefix(_HOST_BACKING), _HOST_MOUNT)]

    # The child can only see this file if its own mount really landed.
    source = create_adapter_with_capacity_retry(
        sdk_backend,
        sdk_e2e_config,
        metadata={
            "test_suite": "sdk_compat",
            "test_role": "fs_only_host_mount_source",
            **host_mount_metadata(mount),
        },
    )
    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    try:
        wait_until_data_plane_ready(
            source,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(
            source.run_command(
                f"printf source-host > {_HOST_MOUNT}/from-source",
                timeout=sdk_e2e_config.command_timeout,
            )
        )
        status, snapshot = api.create_snapshot(source.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]

        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            snapshot_id,
            role="fs_only_mounts_host_directory",
            metadata=host_mount_metadata(mount),
        )
        _assert_mounted(child, sdk_e2e_config, _HOST_MOUNT, role="fs-only child")
        read_back = child.run_command(
            f"cat {_HOST_MOUNT}/from-source", timeout=sdk_e2e_config.command_timeout
        )
        assert_command_ok(read_back)
        assert read_back.stdout.strip() == "source-host", (
            f"sandbox {child.sandbox_id} cannot read the host directory: {read_back.stdout!r}"
        )
    finally:
        api.close()
        errors: list[str] = []
        for sandbox in (child, source):
            if sandbox is not None:
                errors.extend(safe_kill(sandbox, sdk_e2e_config))
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    source, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(RESTART_POLICY)
@pytest.mark.requires_capability(VOLUME_PLUGIN)
def test_fs_only_child_restart_keeps_plugin_volume(
    sdk_backend, sdk_sandbox, sdk_e2e_config
):
    """An fs-only child keeps its re-declared volume across its own restart."""
    from cubesandbox import VolumeMount

    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    try:
        status, snapshot = api.create_snapshot(sdk_sandbox.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]
        with managed_volume(sdk_e2e_config) as (volume_id, _volume_api):
            child = _create_from_snapshot(
                sdk_backend,
                sdk_e2e_config,
                snapshot_id,
                role="fs_only_child_restart",
                options={
                    "volume_mounts": {_PLUGIN_MOUNT: VolumeMount(volume_id, read_only=False)},
                    **_RESTART_OPTIONS,
                },
            )
            assert_command_ok(
                child.run_command(
                    f"printf child-before > {_PLUGIN_MOUNT}/marker",
                    timeout=sdk_e2e_config.command_timeout,
                )
            )

            _cold_restart(child, sdk_e2e_config)

            _assert_mounted(child, sdk_e2e_config, _PLUGIN_MOUNT, role="after restart")
            marker = child.run_command(
                f"cat {_PLUGIN_MOUNT}/marker", timeout=sdk_e2e_config.command_timeout
            )
            assert_command_ok(marker)
            assert marker.stdout.strip() == "child-before", (
                f"sandbox {child.sandbox_id} lost the volume across restart: {marker.stdout!r}"
            )
    finally:
        api.close()
        errors = safe_kill(child, sdk_e2e_config) if child is not None else []
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    sdk_sandbox, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(VOLUME_PLUGIN)
def test_fs_only_child_pause_resume_keeps_plugin_volume(
    sdk_backend, sdk_sandbox, sdk_e2e_config
):
    """pause/resume restores guest memory, so the volume stays where it was."""
    from cubesandbox import VolumeMount

    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    resumed = None
    try:
        status, snapshot = api.create_snapshot(sdk_sandbox.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]
        with managed_volume(sdk_e2e_config) as (volume_id, _volume_api):
            child = _create_from_snapshot(
                sdk_backend,
                sdk_e2e_config,
                snapshot_id,
                role="fs_only_child_pause_resume",
                options={
                    "volume_mounts": {_PLUGIN_MOUNT: VolumeMount(volume_id, read_only=False)}
                },
            )
            assert_command_ok(
                child.run_command(
                    f"printf paused > {_PLUGIN_MOUNT}/marker",
                    timeout=sdk_e2e_config.command_timeout,
                )
            )
            child.pause(timeout=sdk_e2e_config.default_timeout)
            resumed = child.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
            wait_until_data_plane_ready(
                resumed,
                timeout=sdk_e2e_config.default_timeout,
                command_timeout=sdk_e2e_config.command_timeout,
            )
            _assert_mounted(resumed, sdk_e2e_config, _PLUGIN_MOUNT, role="after resume")
            marker = resumed.run_command(
                f"cat {_PLUGIN_MOUNT}/marker", timeout=sdk_e2e_config.command_timeout
            )
            assert_command_ok(marker)
            assert marker.stdout.strip() == "paused", (
                f"sandbox {resumed.sandbox_id} lost the volume across resume: {marker.stdout!r}"
            )
    finally:
        api.close()
        owner = resumed if resumed is not None else child
        errors = safe_kill(owner, sdk_e2e_config) if owner is not None else []
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    sdk_sandbox, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(VOLUME_PLUGIN)
def test_fs_only_create_keeps_a_read_only_volume_read_only(
    sdk_backend, sdk_sandbox, sdk_e2e_config
):
    """A read-only volume declared on an fs-only child stays read-only."""
    from cubesandbox import VolumeMount

    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    try:
        status, snapshot = api.create_snapshot(sdk_sandbox.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]
        with managed_volume(sdk_e2e_config) as (volume_id, _volume_api):
            child = _create_from_snapshot(
                sdk_backend,
                sdk_e2e_config,
                snapshot_id,
                role="fs_only_read_only_volume",
                options={
                    "volume_mounts": {_PLUGIN_MOUNT: VolumeMount(volume_id, read_only=True)}
                },
            )
            _assert_mounted(
                child,
                sdk_e2e_config,
                _PLUGIN_MOUNT,
                role="fs-only read-only child",
                read_only=True,
            )
            denied = child.run_command(
                f"printf denied > {_PLUGIN_MOUNT}/denied-marker",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert denied.exit_code != 0, (
                f"sandbox {child.sandbox_id} wrote through a read-only volume"
            )
    finally:
        api.close()
        errors = safe_kill(child, sdk_e2e_config) if child is not None else []
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    sdk_sandbox, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001 - report, never mask the assertion
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(RESTART_POLICY)
@pytest.mark.requires_capability(HOST_MOUNT)
def test_fs_only_child_restart_keeps_host_mount(sdk_backend, sdk_sandbox, sdk_e2e_config):
    """An fs-only child keeps its re-declared host directory across restart."""
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    provision_host_dirs(sdk_backend, sdk_e2e_config, [_HOST_BACKING])
    mount = [mount_option(under_prefix(_HOST_BACKING), _HOST_MOUNT)]

    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    try:
        status, snapshot = api.create_snapshot(sdk_sandbox.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]
        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            snapshot_id,
            role="fs_only_child_restart_host_mount",
            metadata=host_mount_metadata(mount),
            options=_RESTART_OPTIONS,
        )
        _assert_mounted(child, sdk_e2e_config, _HOST_MOUNT, role="before restart")
        assert_command_ok(
            child.run_command(
                f"printf child-before > {_HOST_MOUNT}/marker",
                timeout=sdk_e2e_config.command_timeout,
            )
        )

        _cold_restart(child, sdk_e2e_config)

        _assert_mounted(child, sdk_e2e_config, _HOST_MOUNT, role="after restart")
        marker = child.run_command(
            f"cat {_HOST_MOUNT}/marker", timeout=sdk_e2e_config.command_timeout
        )
        assert_command_ok(marker)
        assert marker.stdout.strip() == "child-before", (
            f"sandbox {child.sandbox_id} lost the host mount across restart: "
            f"{marker.stdout!r}"
        )
    finally:
        api.close()
        errors = safe_kill(child, sdk_e2e_config) if child is not None else []
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    sdk_sandbox, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors


@pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT)
@pytest.mark.requires_capability(HOST_MOUNT)
def test_fs_only_child_rejects_a_missing_host_path(
    sdk_backend, sdk_sandbox, sdk_e2e_config
):
    """A host path missing on the target node fails the fs-only create."""
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    missing = under_prefix("sdk-cold-start-missing", uuid.uuid4().hex)

    api = _snapshot_api(sdk_e2e_config)
    snapshot_id = None
    child = None
    try:
        status, snapshot = api.create_snapshot(sdk_sandbox.sandbox_id, mode=FS_ONLY)
        assert status in (200, 201), (status, snapshot)
        snapshot_id = snapshot["snapshotID"]
        try:
            child = create_adapter(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_role": "fs_only_missing_host_path",
                    **host_mount_metadata([mount_option(missing, _HOST_MOUNT)]),
                },
                create_options={"template": snapshot_id},
            )
        except Exception as exc:  # noqa: BLE001 - the rejection is the assertion
            if warn_pass_missing_hostpath_source(
                exc, host_paths=[missing], role="fs_only_missing_host_path"
            ):
                return
            if is_missing_hostpath_source_error(exc):
                return
            raise
        pytest.fail(
            f"fs-only create with a missing hostPath ({missing}) succeeded as "
            f"{child.sandbox_id}: the guest would see an empty directory"
        )
    finally:
        api.close()
        errors = safe_kill(child, sdk_e2e_config) if child is not None else []
        if snapshot_id:
            try:
                delete_snapshot_after_runtime_release(
                    sdk_sandbox, snapshot_id, timeout=sdk_e2e_config.default_timeout
                )
            except Exception as exc:  # noqa: BLE001
                errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
        assert not errors, errors
