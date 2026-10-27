# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Restart must not reuse pre-restart guest memory as a snapshot base.

A cold restart discards VM memory. If Cubelet later treats the pre-restart
template/snapshot as an incremental base, the captured memory image contains
stale pages that no live guest ever had, and restoring from it yields a broken
VM. These cases detect that by tracking a *memory-only* file (tmpfs): it is
guaranteed to be absent after a cold boot, so any pre-restart value observed
after a restore proves the image reused stale memory.

The suite deliberately checks the restored guest, not just success/failure of
Commit, because the historical defect was silent: snapshots were produced and
looked healthy.

A sandbox created straight from an image-backed template has *no* memory image
to inherit, so every capture there is a full dump and the defect cannot occur
(that is why such a case passes even on the broken build). The cases therefore
seed a **memory-bearing** template first: commit a sandbox, then create the
sandbox under test from that template so that it really restores memory. Only
then does a post-restart capture have a stale pre-restart image to fall back to.

Direction of the assertion matters. An incremental capture always rewrites the
pages the guest is dirtying, so a value written *after* the restart is present
even in a corrupt image. The damage is the mirror image: pages the guest never
touched keep the reflinked pre-restart bytes. Hence the probe writes a marker
before the restart, restarts (tmpfs is dropped: the guest's page is now fresh),
captures **without recreating the marker**, and requires the marker to be
*absent* from the restored guest. A build that reuses the stale image resurrects
the pre-restart bytes and fails this.
"""

from __future__ import annotations

import json
import os
import shlex
import subprocess
import time

import pytest
from adapters import create_adapter_with_capacity_retry
from adapters.api_adapter import ApiClient
from framework.assertions import assert_command_ok
from framework.capabilities import (
    PAUSE_RESUME,
    RESTART_POLICY,
    ROLLBACK_CLONE,
    RUN_CODE,
)
from framework.lifecycle import wait_until_data_plane_ready, wait_until_running
from framework.restart_policy import (
    crash_guest,
    restart_status,
    wait_for_restart_status,
    write_durable_marker,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.restart_policy,
    pytest.mark.p2,
    pytest.mark.slow,
    pytest.mark.requires_capability(RESTART_POLICY),
]

# /dev/shm is a tmpfs on the guest image. Keep the probing self-checking so a
# non-tmpfs environment fails loudly instead of silently weakening the test.
TMPFS_DIR = "/dev/shm/cube-e2e-memory-lineage"
PRE_RESTART_VALUE = "pre-restart-memory-only"
POST_RESTART_VALUE = "post-restart-memory-only"
STALE_ABSENT_MSG = (
    "the restored image still contains a memory-only file that only existed "
    "before the restart; the capture reused pre-restart guest memory as its "
    "incremental base"
)

_ONE_RESTART = {
    "initialIntervalSecond": 1,
    "multiplier": 1,
    "maxRestarts": 3,
}
_FAST_PROBE = {
    "httpGet": {"path": "/health", "port": 49983},
    "initialDelaySecond": 1,
    "periodSecond": 1,
    "failureThreshold": 1,
    "probeTimeoutSecond": 1,
}


def _run(adapter, command: str, *, timeout: int):
    return adapter.run_command(command, timeout=timeout)


def _assert_tmpfs(adapter, *, timeout: int) -> None:
    result = _run(adapter, f"stat -f -c %T {shlex.quote(TMPFS_DIR)}", timeout=timeout)
    assert_command_ok(result)
    assert result.stdout.strip() == "tmpfs", (
        f"{TMPFS_DIR} is {result.stdout.strip()!r}, not tmpfs; "
        "the probe would not prove anything about VM memory"
    )


def _prepare_tmpfs(adapter, *, timeout: int) -> None:
    result = _run(adapter, f"mkdir -p {shlex.quote(TMPFS_DIR)}", timeout=timeout)
    assert_command_ok(result)
    _assert_tmpfs(adapter, timeout=timeout)


def _write_tmpfs(adapter, value: str, *, timeout: int) -> None:
    # Recreate the directory first: a cold boot may drop the whole /dev/shm
    # subtree, and that is itself evidence the guest memory was discarded.
    path = f"{TMPFS_DIR}/marker"
    quoted = shlex.quote(value)
    result = _run(
        adapter,
        f"mkdir -p {shlex.quote(TMPFS_DIR)} && printf %s {quoted} > {path} && cat {path}",
        timeout=timeout,
    )
    assert_command_ok(result)
    assert result.stdout == value, f"memory-only marker readback = {result.stdout!r}"


def _read_tmpfs(adapter, *, timeout: int) -> str:
    path = f"{TMPFS_DIR}/marker"
    result = _run(adapter, f"cat {path} 2>/dev/null || true", timeout=timeout)
    return result.stdout


def _cold_restart(adapter, config, *, sandbox_id: str) -> None:
    """Crash the guest and wait until the same sandbox id is running again."""
    crash_guest(adapter, timeout=config.command_timeout)
    status = wait_for_restart_status(
        adapter,
        lambda item: bool(item)
        and item.get("restartState") == "Running"
        and int(item.get("restartCount") or 0) >= 1,
        timeout=config.restart_policy_wait,
    )
    assert status is not None
    assert adapter.sandbox_id == sandbox_id, "restart must reuse the sandbox id"
    wait_until_data_plane_ready(
        adapter,
        timeout=config.default_timeout,
        command_timeout=config.command_timeout,
    )


# CommitSandbox (POST /cube/sandbox/commit, served by CubeMaster on :8089) is
# the capture path that resolves incremental memory bases. The public SDK
# "create_snapshot" uses AppSnapshot instead, which always takes a full
# memory image and therefore cannot regress on stale-base reuse. These cases
# must drive the Commit path to cover the defect.
COMMIT_CLI = "cubemastercli"
COMMIT_ADDR = os.environ.get("CUBE_MASTER_ADDR", "127.0.0.1")
COMMIT_PORT = os.environ.get("CUBE_MASTER_PORT", "8089")


def _cli(*args: str, timeout: int = 900) -> subprocess.CompletedProcess:
    cmd = [COMMIT_CLI, "-a", COMMIT_ADDR, "-p", COMMIT_PORT, *args]
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)


def _commit_to_template(adapter, *, timeout: int = 900) -> str:
    """Commit the sandbox and wait until the template is actually usable.

    ``tpl commit --json`` prints the raw response and *skips* the build watch
    that the non-JSON path performs (``TemplateCommitCommand`` returns right
    after ``PrintAsJSON``). Waiting matters here for two reasons:

    * the commit job runs asynchronously against the live sandbox, so killing
      the source too early fails it with ``sandbox is not found``;
    * the template row only becomes restorable once the job is READY.
    """
    proc = _cli("tpl", "commit", "--sandbox-id", adapter.sandbox_id, "--json", timeout=timeout)
    output = (proc.stdout or "") + (proc.stderr or "")
    if proc.returncode != 0:
        raise AssertionError(f"commit failed rc={proc.returncode}: {output[:800]}")
    try:
        payload = json.loads(proc.stdout)
    except json.JSONDecodeError:
        raise AssertionError(f"commit returned non-JSON output: {output[:800]}")
    template_id = payload.get("template_id") or payload.get("templateID")
    if not template_id:
        raise AssertionError(f"commit response has no template_id: {payload!r}")
    build_id = payload.get("build_id") or payload.get("buildID")
    if build_id:
        watched = _cli("tpl", "build-watch", "--build-id", str(build_id), timeout=timeout)
        watch_output = (watched.stdout or "") + (watched.stderr or "")
        if watched.returncode != 0:
            raise AssertionError(
                f"commit build {build_id} did not complete: {watch_output[-800:]}"
            )
    return str(template_id)


def _delete_template(template_id: str | None, *, timeout: int = 300) -> None:
    """Delete a committed template to release its node memory/disk replicas.

    A small compute node cannot hold several committed memory images at once,
    and later commits in this suite need the room. Cleanup is best effort here:
    the case itself asserts on the snapshot contents, not on template GC.
    """
    if not template_id:
        return
    cmd = [
        COMMIT_CLI, "-a", COMMIT_ADDR, "-p", COMMIT_PORT,
        "tpl", "delete", template_id,
    ]
    try:
        subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, check=False)
    except subprocess.TimeoutExpired:
        pass


def _kill_retrying(adapter, config, *, cleanup_errors: list[str]) -> bool:
    """Release a sandbox and confirm the control plane dropped it.

    ``safe_kill`` is the harness helper, but its success only means the SDK call
    returned: after a Commit the backend keeps the row for a moment and a
    reconcile may re-add it. This case then immediately asks the scheduler for a
    second sandbox on the same single node, so a lingering row silently turns
    into ``130597 no more resource`` instead of a useful failure. Deleting over
    REST and polling until the sandbox is gone makes the release observable.
    """
    sandbox_id = adapter.sandbox_id
    api = ApiClient(config)
    deadline = time.monotonic() + 180
    last_error = ""
    try:
        while True:
            try:
                api.delete_sandbox(sandbox_id)
            except Exception as exc:  # noqa: BLE001 - retried below
                last_error = str(exc)
            if not api.get_sandbox(sandbox_id):
                return True
            if time.monotonic() >= deadline:
                cleanup_errors.append(
                    f"{sandbox_id} still present after delete: {last_error or 'no error reported'}"
                )
                return False
            time.sleep(3)
    finally:
        api.close()


def _restore_from(adapter, backend: str, config, snapshot_id: str, role: str):
    """Create a sandbox from a freshly committed template.

    A Commit is asynchronous from the caller's point of view: the template row
    becomes READY before the node/API view can restore from it, and the gap is
    observable as a transient not-found. Retry that specific error briefly;
    anything else must fail fast.

    Capacity is the scheduler's call, so this module never polls for it: the
    create below goes through ``create_adapter_with_capacity_retry``, which
    retries exactly the ``130597 no more resource`` window that follows a
    DELETE (``SDK_E2E_CREATE_CAPACITY_*``, 90s budget, scaled by the xdist
    worker count in ``conftest._scale_capacity_retries_for_xdist``). Do not
    reintroduce a CLI-derived capacity check here: ``cubemastercli li``
    truncates free CPU to whole cores and sums every node of a zone+cpu-type
    into one row, so it cannot answer "can this node host the create".
    """
    deadline = time.monotonic() + config.default_timeout
    last: Exception | None = None
    while True:
        try:
            return create_adapter_with_capacity_retry(
                backend,
                config,
                metadata={"test_suite": "sdk_compat", "test_role": role},
                create_options={"template": snapshot_id},
            )
        except Exception as exc:  # noqa: BLE001 - classify by message
            last = exc
            not_found = type(exc).__name__ == "TemplateNotFoundError" or "not found" in str(exc).lower()
            if not not_found or time.monotonic() >= deadline:
                raise
            time.sleep(1)


def _restart_and_confirm_memory_dropped(
    adapter, config, *, sandbox_id: str
) -> None:
    """Cold-restart `adapter` and prove the guest memory was really discarded.

    The tmpfs readback is the whole point: if the pre-restart marker were still
    readable, the guest never restarted and every later assertion would be
    vacuous. Deliberately nothing is written afterwards, so the only trace of
    that value is the stale pre-restart memory image.
    """
    _cold_restart(adapter, config, sandbox_id=sandbox_id)
    assert _read_tmpfs(adapter, timeout=config.command_timeout) == "", (
        "tmpfs survived the restart; this run did not exercise a cold boot"
    )


def _seed_memory_template(adapter, *, value: str) -> str:
    """Commit `adapter` so the resulting template carries a memory image.

    The returned template is what makes a later restart meaningful: restoring
    from it gives the guest real pre-restart memory, which the buggy build would
    then reuse as the incremental base after the restart.
    """
    _write_tmpfs(adapter, value, timeout=180)
    return _commit_to_template(adapter)


@pytest.mark.requires_capability(ROLLBACK_CLONE)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_ONE_RESTART,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_commit_after_restart_captures_post_restart_memory(
    sdk_sandbox, sdk_backend, sdk_e2e_config
):
    """Commit after a cold restart must capture post-restart memory only.

    Regression guard for the defect where the first capture after a restart
    reflinked the pre-restart memory image and asked for an incremental
    capture, leaving stale pages in the result. The pre-restart image here is a
    real one: the sandbox under test was created from a committed template, so
    it starts with ``PRE_RESTART_VALUE`` in tmpfs.
    """
    config = sdk_e2e_config
    seed_template = None
    snapshot_id = None
    target = None
    restored = None
    source_released = False
    target_released = False
    cleanup_errors: list[str] = []
    try:
        _prepare_tmpfs(sdk_sandbox, timeout=config.command_timeout)
        seed_template = _seed_memory_template(sdk_sandbox, value=PRE_RESTART_VALUE)
        source_released = _kill_retrying(sdk_sandbox, config, cleanup_errors=cleanup_errors)

        # This sandbox really restores memory from the seed template; if it did
        # not, the restart below would have no stale image to corrupt.
        target = _restore_from(
            sdk_sandbox, sdk_backend, config, seed_template,
            role="restart_then_commit_source",
        )
        wait_until_data_plane_ready(
            target, timeout=config.default_timeout, command_timeout=config.command_timeout
        )
        assert _read_tmpfs(target, timeout=config.command_timeout) == PRE_RESTART_VALUE, (
            "restored seed sandbox is missing the pre-restart memory value; "
            "the case would not exercise stale-base reuse"
        )

        _restart_and_confirm_memory_dropped(
            target, config, sandbox_id=target.sandbox_id
        )

        # The first capture after a restart is the vulnerable one. Commit is the
        # path that resolves an incremental memory base. Note nothing is written
        # to tmpfs here on purpose: the marker must stay absent in a correct
        # capture, and only a stale-base image can resurrect it.
        snapshot_id = _commit_to_template(target)
        target_released = _kill_retrying(target, config, cleanup_errors=cleanup_errors)
        restored = _restore_from(
            sdk_sandbox, sdk_backend, config, snapshot_id,
            role="restart_then_commit_restore",
        )
        wait_until_data_plane_ready(
            restored, timeout=config.default_timeout, command_timeout=config.command_timeout
        )

        stale = _read_tmpfs(restored, timeout=config.command_timeout)
        assert stale == "", STALE_ABSENT_MSG
        # A real, writable guest is still required: absence alone could also be
        # explained by a broken sandbox that cannot run commands.
        assert_command_ok(_run(restored, "true", timeout=config.command_timeout))
        _write_tmpfs(restored, POST_RESTART_VALUE, timeout=config.command_timeout)
    finally:
        import sys

        active_failure = sys.exc_info()[0] is not None
        if restored is not None:
            _kill_retrying(restored, config, cleanup_errors=cleanup_errors)
        if target is not None and not target_released:
            _kill_retrying(target, config, cleanup_errors=cleanup_errors)
        if not source_released:
            _kill_retrying(sdk_sandbox, config, cleanup_errors=cleanup_errors)
        if not active_failure:
            assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
        _delete_template(snapshot_id)
        _delete_template(seed_template)


@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_ONE_RESTART,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_pause_after_restart_captures_post_restart_memory(
    sdk_sandbox, sdk_backend, sdk_e2e_config
):
    """Pause after a cold restart must not inherit pre-restart memory either.

    Pause shares the incremental-base decision with Commit, so the same class
    of stale-page corruption applies to the pause package consumed by Resume.
    """
    config = sdk_e2e_config
    seed_template = None
    target = None
    resumed = None
    source_released = False
    cleanup_errors: list[str] = []
    try:
        _prepare_tmpfs(sdk_sandbox, timeout=config.command_timeout)
        seed_template = _seed_memory_template(sdk_sandbox, value=PRE_RESTART_VALUE)
        source_released = _kill_retrying(sdk_sandbox, config, cleanup_errors=cleanup_errors)

        target = _restore_from(
            sdk_sandbox, sdk_backend, config, seed_template,
            role="restart_then_pause_source",
        )
        wait_until_data_plane_ready(
            target, timeout=config.default_timeout, command_timeout=config.command_timeout
        )
        assert _read_tmpfs(target, timeout=config.command_timeout) == PRE_RESTART_VALUE, (
            "restored seed sandbox is missing the pre-restart memory value; "
            "the case would not exercise stale-base reuse"
        )

        _restart_and_confirm_memory_dropped(
            target, config, sandbox_id=target.sandbox_id
        )

        # Pause right after the restart, without touching the tmpfs marker, so a
        # correct pause package must not contain it either.
        target.pause(timeout=config.default_timeout)
        resumed = target.resume_or_connect(timeout=config.default_timeout)
        wait_until_running(resumed, timeout=config.default_timeout)
        wait_until_data_plane_ready(
            resumed, timeout=config.default_timeout, command_timeout=config.command_timeout
        )
        assert resumed.sandbox_id == target.sandbox_id
        stale = _read_tmpfs(resumed, timeout=config.command_timeout)
        assert stale == "", (
            "the resumed pause package still contains a memory-only file that "
            "only existed before the restart; the package reused pre-restart "
            "guest memory as its incremental base"
        )
        status = restart_status(resumed.info().raw)
        assert int((status or {}).get("restartCount") or 0) == 1, (
            f"resume must preserve the restart count, got {status!r}"
        )
    finally:
        import sys

        active_failure = sys.exc_info()[0] is not None
        # resumed/target are the same sandbox; kill exactly one handle.
        live = resumed if resumed is not None else (target or sdk_sandbox)
        _kill_retrying(live, config, cleanup_errors=cleanup_errors)
        if resumed is not None:
            try:
                resumed.close()
            except Exception:  # noqa: BLE001
                pass
        if not source_released and target is None:
            _kill_retrying(sdk_sandbox, config, cleanup_errors=cleanup_errors)
        if not active_failure:
            assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
        _delete_template(seed_template)


@pytest.mark.requires_capability(ROLLBACK_CLONE)
@pytest.mark.sandbox_create_options(
    restartPolicy="OnFailure",
    restartBackoff=_ONE_RESTART,
    livenessProbe=_FAST_PROBE,
    timeout=1800,
)
def test_second_commit_after_restart_returns_to_incremental_chain(
    sdk_sandbox, sdk_backend, sdk_e2e_config
):
    """Later commits after the forced full capture must stay correct.

    The first post-restart Commit is a full capture (there is no base). The
    second must be a correct follow-up capture. This case does not, and cannot
    from the public API, assert which snapshot type the backend chose.
    """
    config = sdk_e2e_config
    seed_template = None
    first_snapshot = None
    second_snapshot = None
    target = None
    restored = None
    source_released = False
    target_released = False
    cleanup_errors: list[str] = []
    try:
        _prepare_tmpfs(sdk_sandbox, timeout=config.command_timeout)
        seed_template = _seed_memory_template(sdk_sandbox, value=PRE_RESTART_VALUE)
        source_released = _kill_retrying(sdk_sandbox, config, cleanup_errors=cleanup_errors)

        target = _restore_from(
            sdk_sandbox, sdk_backend, config, seed_template,
            role="second_commit_source",
        )
        wait_until_data_plane_ready(
            target, timeout=config.default_timeout, command_timeout=config.command_timeout
        )
        assert _read_tmpfs(target, timeout=config.command_timeout) == PRE_RESTART_VALUE

        _restart_and_confirm_memory_dropped(
            target, config, sandbox_id=target.sandbox_id
        )

        first_snapshot = _commit_to_template(target)
        # Keep the first image: the second commit's memory image is incremental
        # and still references it as a base, so deleting it early would make the
        # second restore fail with "template not found" instead of testing the
        # lineage we care about.
        wait_until_running(target, timeout=config.default_timeout)
        wait_until_data_plane_ready(
            target, timeout=config.default_timeout, command_timeout=config.command_timeout
        )
        # Second capture runs on the re-established soft-dirty chain.
        _write_tmpfs(target, "second-commit-value", timeout=config.command_timeout)
        second_snapshot = _commit_to_template(target)
        target_released = _kill_retrying(target, config, cleanup_errors=cleanup_errors)

        restored = _restore_from(
            sdk_sandbox, sdk_backend, config, second_snapshot,
            role="second_commit_after_restart_restore",
        )
        wait_until_data_plane_ready(
            restored, timeout=config.default_timeout, command_timeout=config.command_timeout
        )
        # The second capture is incremental over the first one, so the value
        # written between them must be present, and the pre-restart marker must
        # still be absent (it was never recreated after the restart).
        assert _read_tmpfs(restored, timeout=config.command_timeout) == "second-commit-value", (
            "the second commit after a restart lost memory pages"
        )
        assert first_snapshot != second_snapshot
    finally:
        import sys

        active_failure = sys.exc_info()[0] is not None
        if restored is not None:
            _kill_retrying(restored, config, cleanup_errors=cleanup_errors)
        if target is not None and not target_released:
            _kill_retrying(target, config, cleanup_errors=cleanup_errors)
        if not source_released:
            _kill_retrying(sdk_sandbox, config, cleanup_errors=cleanup_errors)
        if not active_failure:
            assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
        _delete_template(second_snapshot)
        _delete_template(first_snapshot)
        _delete_template(seed_template)
