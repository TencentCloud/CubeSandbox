"""Reference-only snapshot contracts combining plugin volumes and host mounts.

Rootfs and memory return to the snapshot point. Plugin volumes and raw host
mounts remain external references, so FromSnap, Rollback, and Clone observe
their latest data. Writable references are deliberately shared; read-only
attachments stay read-only after every restore path.
"""

from __future__ import annotations

import shlex
import sys
import time
import uuid
from dataclasses import dataclass, replace

import pytest

from adapters import create_adapter_with_capacity_retry
from framework.assertions import assert_command_ok
from framework.capabilities import HOST_MOUNT, ROLLBACK_CLONE, VOLUME_PLUGIN
from framework.cleanup import safe_kill
from framework.host_mount import (
    host_mount_metadata,
    mount_option,
    provision_host_dirs,
    skip_if_host_mount_unavailable,
    under_prefix,
)
from framework.volume import managed_volume

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.volume,
    pytest.mark.host_mount,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(VOLUME_PLUGIN),
    pytest.mark.requires_capability(HOST_MOUNT),
    pytest.mark.requires_capability(ROLLBACK_CLONE),
]

_PLUGIN_RW = "/mnt/sdk-plugin-rw"
_PLUGIN_RO = "/mnt/sdk-plugin-ro"
_HOST_RW = "/mnt/sdk-combined-host-rw"
_HOST_RO = "/mnt/sdk-combined-host-ro"
_HOST_RO_BACKING = "plugin-volume-snapshot-ro"
_provisioned_backends: set[str] = set()


@pytest.fixture(autouse=True)
def _require_and_provision_host_mount(sdk_backend, sdk_e2e_config):
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    if sdk_backend in _provisioned_backends:
        return
    provision_host_dirs(sdk_backend, sdk_e2e_config, [_HOST_RO_BACKING])
    _provisioned_backends.add(sdk_backend)


@dataclass(frozen=True)
class _Paths:
    token: str
    rootfs: str
    plugin_rw: str
    plugin_ro: str
    host_rw_dir: str
    host_rw: str
    host_ro_dir_via_rw: str
    host_ro_via_rw: str
    host_ro_dir: str
    host_ro: str


def _paths() -> _Paths:
    token = uuid.uuid4().hex
    host_rw_dir = f"{_HOST_RW}/sdk-e2e-snapshot/{token}"
    host_ro_dir_via_rw = (
        f"{_HOST_RW}/{_HOST_RO_BACKING}/sdk-e2e-snapshot/{token}"
    )
    host_ro_dir = f"{_HOST_RO}/sdk-e2e-snapshot/{token}"
    return _Paths(
        token=token,
        rootfs=f"/tmp/plugin-host-snapshot-{token}.txt",
        plugin_rw=f"{_PLUGIN_RW}/state-{token}.txt",
        plugin_ro=f"{_PLUGIN_RO}/state-{token}.txt",
        host_rw_dir=host_rw_dir,
        host_rw=f"{host_rw_dir}/state.txt",
        host_ro_dir_via_rw=host_ro_dir_via_rw,
        host_ro_via_rw=f"{host_ro_dir_via_rw}/state.txt",
        host_ro_dir=host_ro_dir,
        host_ro=f"{host_ro_dir}/state.txt",
    )


def _host_metadata() -> dict[str, str]:
    return host_mount_metadata(
        [
            mount_option(under_prefix(), _HOST_RW),
            mount_option(
                under_prefix(_HOST_RO_BACKING),
                _HOST_RO,
                read_only=True,
            ),
        ]
    )


def _create(
    sdk_backend,
    sdk_e2e_config,
    *,
    template: str | None = None,
    volume_mounts: dict | None = None,
    role: str,
):
    options: dict = {}
    if template is not None:
        options["template"] = template
    if volume_mounts is not None:
        options["volume_mounts"] = volume_mounts
    return create_adapter_with_capacity_retry(
        sdk_backend,
        sdk_e2e_config,
        metadata={
            "test_suite": "sdk_compat",
            "test_role": role,
            **(_host_metadata() if template is None else {}),
        },
        create_options=options,
    )


def _run_ok(sandbox, command: str, *, timeout: int) -> str:
    result = sandbox.run_command(command, timeout=timeout)
    assert_command_ok(result)
    return result.stdout.strip()


def _seed_rw_external_state(sandbox, paths: _Paths, *, timeout: int) -> None:
    _run_ok(
        sandbox,
        f"mkdir -p {shlex.quote(paths.host_rw_dir)} "
        f"{shlex.quote(paths.host_ro_dir_via_rw)} && "
        f"printf plugin-at-snapshot > {shlex.quote(paths.plugin_rw)} && "
        f"printf host-at-snapshot > {shlex.quote(paths.host_rw)} && "
        f"printf host-ro-at-snapshot > {shlex.quote(paths.host_ro_via_rw)} && "
        f"printf rootfs-at-snapshot > {shlex.quote(paths.rootfs)}",
        timeout=timeout,
    )


def _mutate_rw_external_state(sandbox, paths: _Paths, *, timeout: int) -> None:
    _run_ok(
        sandbox,
        f"printf plugin-after-snapshot > {shlex.quote(paths.plugin_rw)} && "
        f"printf host-after-snapshot > {shlex.quote(paths.host_rw)} && "
        f"printf host-ro-after-snapshot > {shlex.quote(paths.host_ro_via_rw)} && "
        f"printf rootfs-after-snapshot > {shlex.quote(paths.rootfs)}",
        timeout=timeout,
    )


def _assert_reference_state(sandbox, paths: _Paths, *, timeout: int) -> None:
    assert _run_ok(
        sandbox, f"cat {shlex.quote(paths.rootfs)}", timeout=timeout
    ) == "rootfs-at-snapshot"
    assert _run_ok(
        sandbox, f"cat {shlex.quote(paths.plugin_rw)}", timeout=timeout
    ) == "plugin-after-snapshot"
    assert _run_ok(
        sandbox, f"cat {shlex.quote(paths.host_rw)}", timeout=timeout
    ) == "host-after-snapshot"
    assert _run_ok(
        sandbox, f"cat {shlex.quote(paths.host_ro)}", timeout=timeout
    ) == "host-ro-after-snapshot"


def _assert_read_only(sandbox, path: str, *, timeout: int) -> None:
    result = sandbox.run_command(
        f"printf denied > {shlex.quote(path)}",
        timeout=timeout,
    )
    assert result.exit_code != 0, f"read-only mount accepted write to {path}"


def _remove_host_state(sandbox, paths: _Paths, *, timeout: int) -> None:
    _run_ok(
        sandbox,
        f"rm -rf -- {shlex.quote(paths.host_rw_dir)} "
        f"{shlex.quote(paths.host_ro_dir_via_rw)}",
        timeout=timeout,
    )


def _cleanup_host_state_after_failure(
    candidates,
    paths: _Paths,
    *,
    sdk_backend,
    sdk_e2e_config,
    cleanup_errors: list[object],
) -> None:
    for sandbox in candidates:
        if sandbox is None:
            continue
        try:
            _remove_host_state(
                sandbox, paths, timeout=sdk_e2e_config.command_timeout
            )
            return
        except Exception:  # noqa: BLE001 - try another live runtime
            pass

    cleanup = None
    try:
        cleanup = _create(
            sdk_backend,
            sdk_e2e_config,
            role="plugin_host_failed_test_cleanup",
        )
        _remove_host_state(
            cleanup, paths, timeout=sdk_e2e_config.command_timeout
        )
    except Exception as exc:  # noqa: BLE001 - preserve cleanup diagnostics
        cleanup_errors.append(exc)
    finally:
        if cleanup is not None:
            cleanup_errors.extend(safe_kill(cleanup, sdk_e2e_config))


def _delete_snapshot_after_runtime_release(
    owner,
    snapshot_id: str,
    *,
    timeout: float,
) -> None:
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


def test_snapshot_restores_rootfs_and_keeps_both_external_sources_current(
    sdk_backend,
    sdk_e2e_config,
):
    paths = _paths()
    source = restored = owner = None
    snapshot_id = None
    host_state_created = False
    host_state_removed = False
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, api):
        try:
            source = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RW: volume_id},
                role="plugin_host_snapshot_source",
            )
            _seed_rw_external_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_created = True
            snapshot_id = source.create_snapshot()
            owner = source
            _mutate_rw_external_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )

            cleanup_errors.extend(safe_kill(source, sdk_e2e_config))
            source = None
            assert api.delete_volume(volume_id) == 409

            restored = _create(
                sdk_backend,
                sdk_e2e_config,
                template=snapshot_id,
                role="plugin_host_snapshot_restore",
            )
            _assert_reference_state(
                restored, paths, timeout=sdk_e2e_config.command_timeout
            )
            _assert_read_only(
                restored,
                f"{paths.host_ro_dir}/denied.txt",
                timeout=sdk_e2e_config.command_timeout,
            )
            _remove_host_state(
                restored, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_removed = True

            cleanup_errors.extend(safe_kill(restored, sdk_e2e_config))
            restored = None
            _delete_snapshot_after_runtime_release(
                owner,
                snapshot_id,
                timeout=sdk_e2e_config.default_timeout,
            )
            snapshot_id = None
            assert api.delete_volume(volume_id) == 204
        finally:
            active_failure = sys.exc_info()[0] is not None
            if host_state_created and not host_state_removed:
                _cleanup_host_state_after_failure(
                    (restored, source),
                    paths,
                    sdk_backend=sdk_backend,
                    sdk_e2e_config=sdk_e2e_config,
                    cleanup_errors=cleanup_errors,
                )
            for sandbox in (restored, source):
                if sandbox is not None:
                    cleanup_errors.extend(safe_kill(sandbox, sdk_e2e_config))
            if snapshot_id is not None and owner is not None:
                try:
                    _delete_snapshot_after_runtime_release(
                        owner,
                        snapshot_id,
                        timeout=sdk_e2e_config.default_timeout,
                    )
                except Exception as exc:  # noqa: BLE001
                    cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors


def test_rollback_reverts_rootfs_but_keeps_plugin_and_host_data_current(
    sdk_backend,
    sdk_e2e_config,
):
    paths = _paths()
    source = None
    snapshot_id = None
    host_state_created = False
    host_state_removed = False
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        try:
            source = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RW: volume_id},
                role="plugin_host_rollback_source",
            )
            _seed_rw_external_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_created = True
            snapshot_id = source.create_snapshot()
            _mutate_rw_external_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )

            response = source.rollback(snapshot_id)
            assert response.get("snapshotID") == snapshot_id, response
            _assert_reference_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            _run_ok(
                source,
                f"printf plugin-after-rollback > {shlex.quote(paths.plugin_rw)} && "
                f"printf host-after-rollback > {shlex.quote(paths.host_rw)}",
                timeout=sdk_e2e_config.command_timeout,
            )
            _remove_host_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_removed = True
        finally:
            active_failure = sys.exc_info()[0] is not None
            if host_state_created and not host_state_removed:
                _cleanup_host_state_after_failure(
                    (source,),
                    paths,
                    sdk_backend=sdk_backend,
                    sdk_e2e_config=sdk_e2e_config,
                    cleanup_errors=cleanup_errors,
                )
            if source is not None:
                cleanup_errors.extend(safe_kill(source, sdk_e2e_config))
            if snapshot_id is not None and source is not None:
                try:
                    _delete_snapshot_after_runtime_release(
                        source,
                        snapshot_id,
                        timeout=sdk_e2e_config.default_timeout,
                    )
                except Exception as exc:  # noqa: BLE001
                    cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors


def test_snapshot_restore_chain_remaps_plugin_and_host_mounts_each_generation(
    sdk_backend,
    sdk_e2e_config,
):
    paths = _paths()
    source = first = second = owner = None
    snapshot_ids: list[str] = []
    host_state_created = False
    host_state_removed = False
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, api):
        try:
            source = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RW: volume_id},
                role="plugin_host_chain_source",
            )
            _seed_rw_external_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_created = True
            snapshot_a = source.create_snapshot()
            snapshot_ids.append(snapshot_a)
            owner = source
            cleanup_errors.extend(safe_kill(source, sdk_e2e_config))
            source = None

            first = _create(
                sdk_backend,
                sdk_e2e_config,
                template=snapshot_a,
                role="plugin_host_chain_first",
            )
            _run_ok(
                first,
                f"printf rootfs-generation-b > {shlex.quote(paths.rootfs)} && "
                f"printf plugin-generation-b > {shlex.quote(paths.plugin_rw)} && "
                f"printf host-generation-b > {shlex.quote(paths.host_rw)}",
                timeout=sdk_e2e_config.command_timeout,
            )
            snapshot_b = first.create_snapshot()
            snapshot_ids.append(snapshot_b)
            _run_ok(
                first,
                f"printf rootfs-after-generation-b > {shlex.quote(paths.rootfs)} && "
                f"printf plugin-after-generation-b > "
                f"{shlex.quote(paths.plugin_rw)} && "
                f"printf host-after-generation-b > {shlex.quote(paths.host_rw)}",
                timeout=sdk_e2e_config.command_timeout,
            )
            cleanup_errors.extend(safe_kill(first, sdk_e2e_config))
            first = None

            second = _create(
                sdk_backend,
                sdk_e2e_config,
                template=snapshot_b,
                role="plugin_host_chain_second",
            )
            assert _run_ok(
                second,
                f"cat {shlex.quote(paths.rootfs)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "rootfs-generation-b"
            assert _run_ok(
                second,
                f"cat {shlex.quote(paths.plugin_rw)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "plugin-after-generation-b"
            assert _run_ok(
                second,
                f"cat {shlex.quote(paths.host_rw)}",
                timeout=sdk_e2e_config.command_timeout,
            ) == "host-after-generation-b"
            _remove_host_state(
                second, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_removed = True

            cleanup_errors.extend(safe_kill(second, sdk_e2e_config))
            second = None
            _delete_snapshot_after_runtime_release(
                owner,
                snapshot_ids.pop(),
                timeout=sdk_e2e_config.default_timeout,
            )
            assert api.delete_volume(volume_id) == 409
            _delete_snapshot_after_runtime_release(
                owner,
                snapshot_ids.pop(),
                timeout=sdk_e2e_config.default_timeout,
            )
            assert api.delete_volume(volume_id) == 204
        finally:
            active_failure = sys.exc_info()[0] is not None
            if host_state_created and not host_state_removed:
                _cleanup_host_state_after_failure(
                    (second, first, source),
                    paths,
                    sdk_backend=sdk_backend,
                    sdk_e2e_config=sdk_e2e_config,
                    cleanup_errors=cleanup_errors,
                )
            for sandbox in (second, first, source):
                if sandbox is not None:
                    cleanup_errors.extend(safe_kill(sandbox, sdk_e2e_config))
            if owner is not None:
                for snapshot_id in reversed(snapshot_ids):
                    try:
                        _delete_snapshot_after_runtime_release(
                            owner,
                            snapshot_id,
                            timeout=sdk_e2e_config.default_timeout,
                        )
                    except Exception as exc:  # noqa: BLE001
                        cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors


def test_clone_isolates_rootfs_and_shares_writable_plugin_and_host_mounts(
    sdk_backend,
    sdk_e2e_config,
):
    paths = _paths()
    source = None
    clones = []
    host_state_created = False
    host_state_removed = False
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        try:
            source = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RW: volume_id},
                role="plugin_host_clone_source",
            )
            _seed_rw_external_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_created = True
            clones = source.clone(n=2, concurrency=2)
            assert len(clones) == 2
            left, right = clones

            _run_ok(
                left,
                f"printf left-rootfs > {shlex.quote(paths.rootfs)} && "
                f"rm -f -- {shlex.quote(paths.plugin_rw)} && "
                f"printf left-plugin > {shlex.quote(paths.plugin_rw)} && "
                f"printf left-host > {shlex.quote(paths.host_rw)}",
                timeout=sdk_e2e_config.command_timeout,
            )
            for sandbox in (right, source):
                assert _run_ok(
                    sandbox,
                    f"cat {shlex.quote(paths.rootfs)}",
                    timeout=sdk_e2e_config.command_timeout,
                ) == "rootfs-at-snapshot"
                assert _run_ok(
                    sandbox,
                    f"cat {shlex.quote(paths.plugin_rw)}",
                    timeout=sdk_e2e_config.command_timeout,
                ) == "left-plugin"
                assert _run_ok(
                    sandbox,
                    f"cat {shlex.quote(paths.host_rw)}",
                    timeout=sdk_e2e_config.command_timeout,
                ) == "left-host"
            _remove_host_state(
                source, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_removed = True
        finally:
            active_failure = sys.exc_info()[0] is not None
            if host_state_created and not host_state_removed:
                _cleanup_host_state_after_failure(
                    (*clones, source),
                    paths,
                    sdk_backend=sdk_backend,
                    sdk_e2e_config=sdk_e2e_config,
                    cleanup_errors=cleanup_errors,
                )
            for clone in clones:
                cleanup_errors.extend(safe_kill(clone, sdk_e2e_config))
            if source is not None:
                cleanup_errors.extend(safe_kill(source, sdk_e2e_config))
            if not active_failure:
                assert not cleanup_errors, cleanup_errors


def test_read_only_plugin_and_host_mounts_remain_read_only_after_restore(
    sdk_backend,
    sdk_e2e_config,
):
    from cubesandbox import VolumeMount

    paths = _paths()
    seed = source = writer = restored = owner = None
    clones = []
    snapshot_id = None
    host_state_created = False
    host_state_removed = False
    cleanup_errors: list[object] = []
    with managed_volume(sdk_e2e_config) as (volume_id, _api):
        try:
            seed = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RW: volume_id},
                role="plugin_ro_seed",
            )
            seed.write_file(paths.plugin_rw, "plugin-ro-at-snapshot")
            cleanup_errors.extend(safe_kill(seed, sdk_e2e_config))
            seed = None

            source = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RO: VolumeMount(volume_id, read_only=True)},
                role="plugin_host_ro_source",
            )
            _run_ok(
                source,
                f"mkdir -p {shlex.quote(paths.host_rw_dir)} "
                f"{shlex.quote(paths.host_ro_dir_via_rw)} && "
                f"printf host-ro-at-snapshot > {shlex.quote(paths.host_ro_via_rw)} && "
                f"printf rootfs-at-snapshot > {shlex.quote(paths.rootfs)}",
                timeout=sdk_e2e_config.command_timeout,
            )
            host_state_created = True
            assert source.read_file(paths.plugin_ro) == "plugin-ro-at-snapshot"
            snapshot_id = source.create_snapshot()
            owner = source

            writer = _create(
                sdk_backend,
                sdk_e2e_config,
                volume_mounts={_PLUGIN_RW: volume_id},
                role="plugin_ro_external_writer",
            )
            writer.write_file(paths.plugin_rw, "plugin-ro-after-snapshot")
            cleanup_errors.extend(safe_kill(writer, sdk_e2e_config))
            writer = None
            source.write_file(paths.host_ro_via_rw, "host-ro-after-snapshot")
            source.write_file(paths.rootfs, "rootfs-after-snapshot")

            response = source.rollback(snapshot_id)
            assert response.get("snapshotID") == snapshot_id, response
            assert source.read_file(paths.rootfs) == "rootfs-at-snapshot"
            assert source.read_file(paths.plugin_ro) == "plugin-ro-after-snapshot"
            assert source.read_file(paths.host_ro) == "host-ro-after-snapshot"
            for path in (
                f"{_PLUGIN_RO}/denied-after-rollback-{paths.token}.txt",
                f"{paths.host_ro_dir}/denied-after-rollback.txt",
            ):
                _assert_read_only(
                    source,
                    path,
                    timeout=sdk_e2e_config.command_timeout,
                )

            clones = source.clone(n=1)
            assert len(clones) == 1
            clone = clones[0]
            assert clone.read_file(paths.plugin_ro) == "plugin-ro-after-snapshot"
            assert clone.read_file(paths.host_ro) == "host-ro-after-snapshot"
            for path in (
                f"{_PLUGIN_RO}/denied-from-clone-{paths.token}.txt",
                f"{paths.host_ro_dir}/denied-from-clone.txt",
            ):
                _assert_read_only(
                    clone,
                    path,
                    timeout=sdk_e2e_config.command_timeout,
                )

            cleanup_errors.extend(safe_kill(source, sdk_e2e_config))
            source = None

            restored = _create(
                sdk_backend,
                sdk_e2e_config,
                template=snapshot_id,
                role="plugin_host_ro_restore",
            )
            assert restored.read_file(paths.rootfs) == "rootfs-at-snapshot"
            assert restored.read_file(paths.plugin_ro) == "plugin-ro-after-snapshot"
            assert restored.read_file(paths.host_ro) == "host-ro-after-snapshot"
            _assert_read_only(
                restored,
                f"{_PLUGIN_RO}/denied-{paths.token}.txt",
                timeout=sdk_e2e_config.command_timeout,
            )
            _assert_read_only(
                restored,
                f"{paths.host_ro_dir}/denied.txt",
                timeout=sdk_e2e_config.command_timeout,
            )
            _remove_host_state(
                restored, paths, timeout=sdk_e2e_config.command_timeout
            )
            host_state_removed = True
        finally:
            active_failure = sys.exc_info()[0] is not None
            if host_state_created and not host_state_removed:
                _cleanup_host_state_after_failure(
                    (*clones, restored, writer, source, seed),
                    paths,
                    sdk_backend=sdk_backend,
                    sdk_e2e_config=sdk_e2e_config,
                    cleanup_errors=cleanup_errors,
                )
            for clone in clones:
                cleanup_errors.extend(safe_kill(clone, sdk_e2e_config))
            for sandbox in (restored, writer, source, seed):
                if sandbox is not None:
                    cleanup_errors.extend(safe_kill(sandbox, sdk_e2e_config))
            if snapshot_id is not None and owner is not None:
                try:
                    _delete_snapshot_after_runtime_release(
                        owner,
                        snapshot_id,
                        timeout=sdk_e2e_config.default_timeout,
                    )
                except Exception as exc:  # noqa: BLE001
                    cleanup_errors.append(exc)
            if not active_failure:
                assert not cleanup_errors, cleanup_errors
