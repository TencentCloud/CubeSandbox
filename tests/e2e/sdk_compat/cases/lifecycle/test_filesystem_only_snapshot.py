# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Filesystem-only snapshots (issue #1907).

An fs-only snapshot captures the rootfs and metadata but no guest memory. A
sandbox started from it cold-boots: files are preserved, processes are fresh.
Everything after that start is ordinary lifecycle behaviour — pause/resume always
carries memory, and rollback only accepts full snapshots.

Prerequisites:

- a cluster running a release that supports fs-only snapshots. A control plane
  that does not advertise the mode in ``GET /cube/capabilities`` fails the cases
  instead of skipping them — there is no cluster switch to turn on;
- a READY template via ``CUBE_TEMPLATE_ID`` or ``--cube-template-id``.

``Sandbox.create_snapshot(mode=...)`` is part of the SDK, so the mode parameter
and the ``mode`` field of ``SnapshotInfo`` are covered through it
(``test_sdk_create_snapshot_sends_the_mode``). The remaining calls that need a
mode — fork, and the negative cases that must observe the raw HTTP status —
go through ``ApiClient``, which wraps the same REST endpoint the SDK uses.
Every other step uses the shared adapter.
"""

from __future__ import annotations

import pytest

from adapters import connect_adapter, create_adapter_with_capacity_retry
from adapters.api_adapter import ApiClient
from framework.assertions import assert_command_ok
from framework.capabilities import (
    FILESYSTEM_ONLY_SNAPSHOT,
    HOST_MOUNT,
    RESTART_POLICY,
    VOLUME_PLUGIN,
)
from framework.cleanup import delete_snapshot_after_runtime_release, safe_kill
from framework.lifecycle import (
    wait_until,
    wait_until_data_plane_ready,
    wait_until_paused,
)
from framework.restart_policy import (
    crash_guest,
    read_marker,
    wait_for_restart_status,
    write_durable_marker,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(FILESYSTEM_ONLY_SNAPSHOT),
]

FS_ONLY = "fs-only"
FULL = "full"
ROLLBACK_UNSUPPORTED = "snapshot_filesystem_only_rollback_unsupported"

# External mounts an fs-only package never carries. A cold boot re-mounts only
# what the caller declares on the create request, so a package that recorded
# them must not hand them to a child that stays silent about them.
_PLUGIN_RO = "/mnt/sdk-fs-only-plugin-ro"
_HOST_MOUNT = "/mnt/sdk-fs-only-host"
_HOST_BACKING = "sdk-fs-only-host"
_ROOTFS_STATE = "/tmp/sdk-fs-only-external.txt"

# Passed as $0 of a detached shell, so the process is identifiable without
# requiring pgrep/ps in the template image.
_MARKER = "sdk-compat-filesystem-only-marker"


def _guest_paths(source) -> tuple[str, str]:
    token = source.sandbox_id
    return (
        f"/tmp/sdk-compat-fs-only-{token}.txt",
        f"/tmp/sdk-compat-fs-only-{token}.pid",
    )


def _start_marker_process(sandbox, *, timeout: int, pid_path: str) -> None:
    """Start a detached guest process that only a memory capture can restore."""
    result = sandbox.run_command(
        # Record pid + kernel start time. The start time is what defeats both
        # cold boot (same pid file, different or absent process) and pid reuse
        # (a new process with the recycled pid reports a later start time).
        f"nohup sh -c 'while :; do sleep 1; done' {_MARKER} >/dev/null 2>&1 </dev/null & "
        f"pid=$!; read -r -a stat < /proc/$pid/stat || exit 1; "
        f"printf '%s %s\\n' \"$pid\" \"${{stat[21]}}\" >{pid_path}",
        timeout=timeout,
    )
    assert_command_ok(result)
    wait_until(
        lambda: _marker_alive(sandbox, timeout=timeout, pid_path=pid_path),
        timeout=timeout,
        description="detached marker process to appear in the guest",
    )


def _marker_alive(sandbox, *, timeout: int, pid_path: str) -> bool:
    result = sandbox.run_command(
        f'read -r pid start < {pid_path} || exit 1; '
        f'[ -n "$pid" ] && [ -n "$start" ] && kill -0 "$pid" 2>/dev/null || exit 1; '
        f'read -r -a stat < /proc/"$pid"/stat; '
        # Field 3 is the process state: a killed marker is reaped by the guest
        # agent, so "Z" only covers a small window and must not count as alive.
        f'[ "${{stat[2]:-}}" != "Z" ] && [ "${{stat[21]:-}}" = "$start" ]',
        timeout=timeout,
    )
    return result.exit_code == 0


def _stop_marker_process(sandbox, *, timeout: int, pid_path: str) -> None:
    """Kill the marker by pid; the file may carry more than that field."""
    assert_command_ok(
        sandbox.run_command(
            f'read -r pid _ < {pid_path} && kill "$pid"',
            timeout=timeout,
        )
    )


def _create_from_snapshot(
    sdk_backend,
    sdk_e2e_config,
    snapshot_id: str,
    *,
    role: str,
    create_options: dict | None = None,
):
    """Create a sandbox from a snapshot and wait until its guest answers.

    A cold boot is a full guest start, which takes longer than a memory
    restore, so the first data-plane call must not race envd/CubeProxy.
    ``create_options`` carries anything the case needs on top of the template
    (restart policy, TTL); the template itself always comes from the snapshot.
    """
    options = {"template": snapshot_id}
    options.update(create_options or {})
    sandbox = create_adapter_with_capacity_retry(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_suite": "sdk_compat", "test_role": role},
        create_options=options,
    )
    try:
        wait_until_data_plane_ready(
            sandbox,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
    except Exception:
        # The caller never receives the handle, so this is the only chance to
        # release the sandbox before the failure propagates.
        safe_kill(sandbox, sdk_e2e_config)
        raise
    return sandbox


def _create_snapshot(api: ApiClient, sandbox_id: str, *, mode: str | None = None) -> dict:
    """Create a snapshot and return the body.

    The response is validated for status and id only: id first so a caller can
    always clean the artifact up, even when the mode contract below fails.
    """
    status, body = api.create_snapshot(sandbox_id, mode=mode)
    label = mode or "default"
    assert status == 201, f"{label} snapshot rejected: HTTP {status} {body!r}"
    assert body.get("snapshotID"), f"create response missing snapshotID: {body!r}"
    return body


def _assert_snapshot_mode(snapshot: dict, expected: str) -> None:
    assert snapshot.get("mode") == expected, (
        f"snapshot {snapshot.get('snapshotID')} reports mode "
        f"{snapshot.get('mode')!r}, expected {expected!r}: {snapshot!r}"
    )


def _release(sdk_e2e_config, sandboxes, owner, snapshot_ids) -> list[str]:
    """Kill derived sandboxes, then delete the snapshots they booted from.

    The source sandbox is intentionally left to the fixture teardown; this
    helper only owns the resources a case creates itself.
    """
    errors: list[str] = []
    for sandbox in sandboxes:
        errors.extend(safe_kill(sandbox, sdk_e2e_config))
    for snapshot_id in snapshot_ids:
        try:
            delete_snapshot_after_runtime_release(
                owner,
                snapshot_id,
                timeout=sdk_e2e_config.default_timeout,
            )
        except Exception as exc:  # noqa: BLE001 - report, never mask the test failure
            errors.append(f"delete snapshot {snapshot_id} failed: {exc}")
    return errors


def test_fs_only_snapshot_reports_mode_and_keeps_source_running(
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path, pid_path = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "before-fs-only-snapshot")
    _start_marker_process(
        sdk_sandbox,
        timeout=sdk_e2e_config.command_timeout,
        pid_path=pid_path,
    )

    api = ApiClient(sdk_e2e_config)
    snapshot_id = None
    cleanup_errors: list[str] = []
    try:
        snapshot = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        snapshot_id = snapshot["snapshotID"]
        _assert_snapshot_mode(snapshot, FS_ONLY)

        # The capture leaves the source usable: the process is still there and
        # the filesystem is writable again when the call returns.
        assert _marker_alive(
            sdk_sandbox,
            timeout=sdk_e2e_config.command_timeout,
            pid_path=pid_path,
        ), "source sandbox lost its detached process across an fs-only snapshot"
        assert sdk_sandbox.info().state == "running", "source sandbox is not running"
        sdk_sandbox.write_file(state_path, "after-fs-only-snapshot")
        assert sdk_sandbox.read_file(state_path) == "after-fs-only-snapshot"
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [],
            sdk_sandbox,
            [snapshot_id] if snapshot_id else [],
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


def test_fs_only_cold_boot_restores_files_with_fresh_processes(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path, pid_path = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "before-fs-only-snapshot")
    _start_marker_process(
        sdk_sandbox,
        timeout=sdk_e2e_config.command_timeout,
        pid_path=pid_path,
    )

    api = ApiClient(sdk_e2e_config)
    snapshot_id = None
    children: list = []
    cleanup_errors: list[str] = []
    try:
        snapshot = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        snapshot_id = snapshot["snapshotID"]
        _assert_snapshot_mode(snapshot, FS_ONLY)

        # Appended one by one so a failure in the second create still cleans
        # up the first.
        for index in range(2):
            children.append(
                _create_from_snapshot(
                    sdk_backend,
                    sdk_e2e_config,
                    snapshot_id,
                    role=f"fs_only_cold_boot_{index}",
                )
            )

        for child in children:
            assert child.sandbox_id != sdk_sandbox.sandbox_id
            assert child.read_file(state_path) == "before-fs-only-snapshot", (
                f"sandbox {child.sandbox_id} did not inherit the source filesystem"
            )
            assert child.info().state == "running"
            assert_command_ok(
                child.run_command("true", timeout=sdk_e2e_config.command_timeout)
            )
            assert not _marker_alive(
                child,
                timeout=sdk_e2e_config.command_timeout,
                pid_path=pid_path,
            ), f"sandbox {child.sandbox_id} inherited a process; fs-only must cold-boot"

        children[0].write_file(state_path, "cold-boot-0")
        assert children[1].read_file(state_path) == "before-fs-only-snapshot", (
            "sibling sandboxes derived from one snapshot share writes"
        )
        assert sdk_sandbox.read_file(state_path) == "before-fs-only-snapshot", (
            "a sandbox created from a snapshot wrote through to its source"
        )
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            children,
            sdk_sandbox,
            [snapshot_id] if snapshot_id else [],
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


def test_fs_only_fork_derives_cold_boots_and_isolates_writes(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path, pid_path = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "before-fork")
    _start_marker_process(
        sdk_sandbox,
        timeout=sdk_e2e_config.command_timeout,
        pid_path=pid_path,
    )

    api = ApiClient(sdk_e2e_config)
    forks: list = []
    cleanup_errors: list[str] = []
    try:
        status, body = api.fork_sandbox(
            sdk_sandbox.sandbox_id,
            count=2,
            mode=FS_ONLY,
        )
        assert status == 201, f"fs-only fork rejected: HTTP {status} {body!r}"
        assert isinstance(body, list) and len(body) == 2, (
            f"fork returned an unexpected result: {body!r}"
        )

        # Connect every successful entry before failing so the cleanup covers
        # already-started siblings.
        failures = []
        for entry in body:
            if entry.get("error"):
                failures.append(entry["error"])
                continue
            fork_id = (entry.get("sandbox") or {}).get("sandboxID")
            assert fork_id, f"fork result missing sandboxID: {entry!r}"
            forks.append(connect_adapter(sdk_backend, fork_id, sdk_e2e_config))
        assert not failures, f"fork reported per-fork failures: {failures!r}"

        for fork in forks:
            wait_until_data_plane_ready(
                fork,
                timeout=sdk_e2e_config.default_timeout,
                command_timeout=sdk_e2e_config.command_timeout,
            )
            assert fork.read_file(state_path) == "before-fork", (
                f"fork {fork.sandbox_id} did not inherit the source filesystem"
            )
            assert not _marker_alive(
                fork,
                timeout=sdk_e2e_config.command_timeout,
                pid_path=pid_path,
            ), f"fork {fork.sandbox_id} inherited a process; fs-only must cold-boot"

        forks[0].write_file(state_path, "fork-zero")
        assert forks[1].read_file(state_path) == "before-fork", (
            "forks derived from one fs-only snapshot share writes"
        )
        assert sdk_sandbox.read_file(state_path) == "before-fork", (
            "a fork wrote through to its source sandbox"
        )
    finally:
        api.close()
        cleanup_errors = _release(sdk_e2e_config, forks, sdk_sandbox, [])
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


def test_rollback_to_fs_only_snapshot_is_rejected(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path, _ = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "before-fs-only-snapshot")

    api = ApiClient(sdk_e2e_config)
    snapshot_id = None
    child = None
    cleanup_errors: list[str] = []
    try:
        snapshot = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        snapshot_id = snapshot["snapshotID"]
        _assert_snapshot_mode(snapshot, FS_ONLY)
        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            snapshot_id,
            role="fs_only_rollback_rejected",
        )
        child.write_file(state_path, "after-cold-boot")

        status, body = api.rollback_sandbox(child.sandbox_id, snapshot_id)

        assert status == 400, f"rollback to an fs-only snapshot expected 400: {status} {body!r}"
        assert ROLLBACK_UNSUPPORTED in str(body.get("message", "")), (
            f"rollback rejection lacks the stable identifier: {body!r}"
        )
        # A rejected rollback is a no-op.
        assert child.info().state == "running", "rejected rollback left the sandbox unusable"
        assert child.read_file(state_path) == "after-cold-boot", (
            "rejected rollback still modified the sandbox filesystem"
        )
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [child] if child else [],
            sdk_sandbox,
            [snapshot_id] if snapshot_id else [],
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


def test_full_snapshot_from_fs_only_cold_boot_rolls_back(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path, pid_path = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "seed-for-fs-only-snapshot")

    api = ApiClient(sdk_e2e_config)
    fs_snapshot_id = None
    full_snapshot_ids: list[str] = []
    child = None
    cleanup_errors: list[str] = []
    try:
        seeding = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        fs_snapshot_id = seeding["snapshotID"]
        _assert_snapshot_mode(seeding, FS_ONLY)
        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            fs_snapshot_id,
            role="fs_only_cold_boot_full_snapshot",
        )
        child.write_file(state_path, "at-full-snapshot")
        _start_marker_process(
            child,
            timeout=sdk_e2e_config.command_timeout,
            pid_path=pid_path,
        )

        # No mode means full.
        full = _create_snapshot(api, child.sandbox_id)
        full_snapshot_ids.append(full["snapshotID"])
        full_snapshot_id = full["snapshotID"]
        _assert_snapshot_mode(full, FULL)

        child.write_file(state_path, "after-full-snapshot")
        _stop_marker_process(
            child,
            timeout=sdk_e2e_config.command_timeout,
            pid_path=pid_path,
        )
        wait_until(
            lambda: not _marker_alive(
                child,
                timeout=sdk_e2e_config.command_timeout,
                pid_path=pid_path,
            ),
            timeout=sdk_e2e_config.default_timeout,
            description="marker process to exit before rollback",
        )

        response = child.rollback(full_snapshot_id)

        assert response.get("sandboxID") == child.sandbox_id, (
            f"rollback moved the sandbox: {response!r}"
        )
        assert response.get("snapshotID") == full_snapshot_id, (
            f"rollback used an unexpected snapshot: {response!r}"
        )
        assert child.read_file(state_path) == "at-full-snapshot", (
            "rollback did not restore the filesystem captured in the full snapshot"
        )
        assert _marker_alive(
            child,
            timeout=sdk_e2e_config.command_timeout,
            pid_path=pid_path,
        ), "rollback to a full snapshot must restore guest memory"

        after_rollback = _create_snapshot(api, child.sandbox_id)
        full_snapshot_ids.append(after_rollback["snapshotID"])
        _assert_snapshot_mode(after_rollback, FULL)
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [child] if child else [],
            sdk_sandbox,
            ([fs_snapshot_id] if fs_snapshot_id else []) + full_snapshot_ids,
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


def test_fs_only_cold_boot_pause_resume_keeps_memory(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    state_path, pid_path = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "seed-for-fs-only-snapshot")

    api = ApiClient(sdk_e2e_config)
    snapshot_id = None
    child = None
    resumed = None
    cleanup_errors: list[str] = []
    try:
        snapshot = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        snapshot_id = snapshot["snapshotID"]
        _assert_snapshot_mode(snapshot, FS_ONLY)
        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            snapshot_id,
            role="fs_only_cold_boot_pause_resume",
        )
        child.write_file(state_path, "before-pause")
        _start_marker_process(
            child,
            timeout=sdk_e2e_config.command_timeout,
            pid_path=pid_path,
        )

        # pause/resume has no mode; the process survives the cycle.
        child.pause(timeout=sdk_e2e_config.default_timeout)
        wait_until_paused(child, timeout=sdk_e2e_config.default_timeout)
        resumed = child.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )

        assert resumed.sandbox_id == child.sandbox_id
        assert resumed.read_file(state_path) == "before-pause"
        assert _marker_alive(
            resumed,
            timeout=sdk_e2e_config.command_timeout,
            pid_path=pid_path,
        ), "resume did not restore guest memory captured by pause"
    finally:
        api.close()
        # `resumed` is a second handle to the same sandbox; destroy it once.
        owner = resumed if resumed is not None else child
        cleanup_errors = _release(
            sdk_e2e_config,
            [owner] if owner is not None else [],
            sdk_sandbox,
            [snapshot_id] if snapshot_id else [],
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


@pytest.mark.p1
def test_fs_only_snapshot_of_a_cold_booted_sandbox_keeps_files(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    """A cold-booted sandbox can be snapshotted again without losing its layer.

    The second package must name the writable layer the sandbox actually uses.
    Its own container id is not that name: the guest keeps the layer of the
    sandbox that created it. Deriving the name from the id would mount an empty
    directory on the next cold boot, and the files would be gone with no error.
    """
    state_path, _ = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "generation-0")

    api = ApiClient(sdk_e2e_config)
    snapshot_ids: list[str] = []
    child = None
    grandchild = None
    cleanup_errors: list[str] = []
    try:
        first = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        snapshot_ids.append(first["snapshotID"])
        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            first["snapshotID"],
            role="fs_only_second_generation_child",
        )
        assert child.read_file(state_path) == "generation-0", (
            "first-generation cold boot did not inherit the source filesystem"
        )
        child.write_file(state_path, "generation-1")

        second = _create_snapshot(api, child.sandbox_id, mode=FS_ONLY)
        snapshot_ids.append(second["snapshotID"])
        _assert_snapshot_mode(second, FS_ONLY)

        grandchild = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            second["snapshotID"],
            role="fs_only_second_generation_grandchild",
        )
        assert grandchild.read_file(state_path) == "generation-1", (
            "a snapshot of a cold-booted sandbox did not carry the writable layer "
            "it was written on; the second-generation cold boot sees an empty one"
        )
        # The sibling branch must stay isolated as well.
        assert sdk_sandbox.read_file(state_path) == "generation-0"
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [sb for sb in (grandchild, child) if sb is not None],
            sdk_sandbox,
            snapshot_ids,
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


@pytest.mark.p1
@pytest.mark.slow
@pytest.mark.requires_capability(RESTART_POLICY)
def test_fs_only_cold_boot_restart_keeps_files(
    sdk_backend,
    sdk_sandbox,
    sdk_e2e_config,
):
    """A cold boot from an fs-only snapshot restarts onto the same writable layer.

    This is the one place where "cold boot" and "restart" interact. A restart
    re-runs the container from the layer the sandbox was created with; if the
    harness rebuilt that path from the creation template instead, every file
    written after the cold boot would silently disappear on the first restart.
    """
    state_path, _ = _guest_paths(sdk_sandbox)
    sdk_sandbox.write_file(state_path, "before-fs-only-snapshot")

    api = ApiClient(sdk_e2e_config)
    snapshot_id = None
    child = None
    cleanup_errors: list[str] = []
    try:
        snapshot = _create_snapshot(api, sdk_sandbox.sandbox_id, mode=FS_ONLY)
        snapshot_id = snapshot["snapshotID"]
        _assert_snapshot_mode(snapshot, FS_ONLY)
        child = _create_from_snapshot(
            sdk_backend,
            sdk_e2e_config,
            snapshot_id,
            role="fs_only_cold_boot_restart",
            create_options={
                "restartPolicy": "OnFailure",
                "restartBackoff": {
                    "initialIntervalSecond": 1,
                    "multiplier": 1,
                    "maxRestarts": 2,
                },
                "timeout": 600,
            },
        )
        assert child.read_file(state_path) == "before-fs-only-snapshot", (
            "cold boot did not inherit the filesystem captured by the fs-only snapshot"
        )

        marker = "written-after-cold-boot"
        write_durable_marker(
            child,
            marker,
            timeout=sdk_e2e_config.command_timeout,
            path=state_path,
        )

        crash_guest(child, timeout=sdk_e2e_config.command_timeout)
        status = wait_for_restart_status(
            child,
            lambda item: bool(item)
            and item.get("restartState") == "Running"
            and item.get("restartCount", 0) >= 1,
            timeout=sdk_e2e_config.restart_policy_wait,
        )
        assert status is not None, "sandbox never came back after the guest crash"
        wait_until_data_plane_ready(
            child,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        assert (
            read_marker(child, timeout=sdk_e2e_config.command_timeout, path=state_path)
            == marker
        ), (
            "restart lost the files written after the cold boot; the writable "
            f"layer was not preserved across restart (status={status!r})"
        )
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [child] if child else [],
            sdk_sandbox,
            [snapshot_id] if snapshot_id else [],
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


@pytest.mark.p1
def test_sdk_create_snapshot_sends_the_mode(sdk_sandbox, sdk_e2e_config):
    """The SDK argument, not just the REST contract, selects fs-only.

    Without it the call is a full snapshot: the sandbox pauses and guest memory
    is stored, which is the opposite of what the caller asked for. Reading the
    recorded mode back is what proves the request carried it.
    """
    api = ApiClient(sdk_e2e_config)
    snapshot_id = None
    cleanup_errors: list[str] = []
    try:
        snapshot_id = sdk_sandbox.create_snapshot(mode=FS_ONLY)
        assert snapshot_id, "SDK created no snapshot"

        recorded = {
            item["snapshotID"]: item for item in api.list_snapshots(sdk_sandbox.sandbox_id)
        }
        assert snapshot_id in recorded, (
            f"snapshot {snapshot_id} is not listed for its sandbox: {recorded!r}"
        )
        assert recorded[snapshot_id].get("mode") == FS_ONLY, (
            "the SDK request did not carry mode=fs-only; the control plane recorded "
            f"{recorded[snapshot_id].get('mode')!r}"
        )
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [],
            sdk_sandbox,
            [snapshot_id] if snapshot_id else [],
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"


@pytest.mark.requires_capability(VOLUME_PLUGIN)
@pytest.mark.requires_capability(HOST_MOUNT)
def test_fs_only_snapshot_drops_external_mounts(sdk_backend, sdk_sandbox, sdk_e2e_config):
    """A package carries the rootfs only.

    Plugin volumes and raw host mounts recorded by the origin sandbox do not
    follow it into a cold boot, and the child is still an ordinary fs-only
    source afterwards.
    """
    from cubesandbox import VolumeMount

    from framework.host_mount import (
        host_mount_metadata,
        mount_option,
        provision_host_dirs,
        skip_if_host_mount_unavailable,
        under_prefix,
    )
    from framework.volume import managed_volume

    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)
    provision_host_dirs(sdk_backend, sdk_e2e_config, [_HOST_BACKING])

    api = ApiClient(sdk_e2e_config)
    snapshot_ids: list[str] = []
    child = None
    try:
        with managed_volume(sdk_e2e_config) as (volume_id, _volume_api):
            source = create_adapter_with_capacity_retry(
                sdk_backend,
                sdk_e2e_config,
                metadata={
                    "test_suite": "sdk_compat",
                    "test_role": "fs_only_external_source",
                    **host_mount_metadata(
                        [mount_option(under_prefix(_HOST_BACKING), _HOST_MOUNT)]
                    ),
                },
                create_options={
                    "volume_mounts": {_PLUGIN_RO: VolumeMount(volume_id, read_only=True)}
                },
            )
            try:
                assert_command_ok(
                    source.run_command(
                        f"printf rootfs-state > {_ROOTFS_STATE} && "
                        f"printf host-state > {_HOST_MOUNT}/state.txt",
                        timeout=sdk_e2e_config.command_timeout,
                    )
                )
                snapshot = _create_snapshot(api, source.sandbox_id, mode=FS_ONLY)
                snapshot_ids.append(snapshot["snapshotID"])
                _assert_snapshot_mode(snapshot, FS_ONLY)
            finally:
                safe_kill(source, sdk_e2e_config)

            child = _create_from_snapshot(
                sdk_backend,
                sdk_e2e_config,
                snapshot_ids[0],
                role="fs_only_external_child",
            )
            assert child.read_file(_ROOTFS_STATE) == "rootfs-state"
            # The plugin mount point survives in the captured rootfs as a plain
            # directory, so the mount table -- not a stat -- decides whether the
            # volume came along.
            mounted = child.run_command(
                f"grep -c ' {_PLUGIN_RO} ' /proc/mounts || true",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert mounted.stdout.strip() == "0", (
                f"{_PLUGIN_RO} is still mounted in a cold-booted guest: an "
                "fs-only package carries no external mount"
            )
            # The source wrote this file into the host directory, so it can only
            # be visible if the host mount followed the package.
            host_marker = child.run_command(
                f"test -e {_HOST_MOUNT}/state.txt",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert host_marker.exit_code != 0, (
                f"{_HOST_MOUNT}/state.txt is visible in a cold-booted guest: an "
                "fs-only package carries no external mount"
            )

            # Still a usable fs-only source: no stripped mount survives in its spec.
            second = _create_snapshot(api, child.sandbox_id, mode=FS_ONLY)
            snapshot_ids.append(second["snapshotID"])
    finally:
        api.close()
        cleanup_errors = _release(
            sdk_e2e_config,
            [child] if child is not None else [],
            sdk_sandbox,
            snapshot_ids,
        )
    assert not cleanup_errors, f"cleanup failed: {cleanup_errors!r}"
