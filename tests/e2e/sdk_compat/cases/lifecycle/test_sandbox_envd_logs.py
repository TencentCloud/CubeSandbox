# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Single-node cubecli checks for envd init logs.

Tests GET envd ``/health`` so the access log writes to init stdout;
``cubecli logs`` must show that after create, resume, snapshot, and
rollback. Template-build logs stay under ``/data/log/template`` and are
read with ``cubecli logs --tpl``. Multi-node clusters skip: cubecli can
only see the local Cubelet's files.
"""

from __future__ import annotations

import pytest

from framework.capabilities import COMMANDS, PAUSE_RESUME, ROLLBACK_CLONE
from framework.cleanup import safe_kill
from framework.lifecycle import (
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)
from framework.sandbox_logs import (
    HOST_LOG_ROOT,
    TEMPLATE_ENVD_MARKER,
    assert_host_logs_present,
    contains_envd_access_log,
    host_log_dir,
    read_cubecli_logs,
    skip_unless_single_node_cubecli,
    template_log_dir,
    trigger_envd,
    wait_for_envd_rpc,
    wait_for_host_log_contains,
    wait_host_logs_absent,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
]


def _require_cubesandbox(sdk_backend: str) -> None:
    if sdk_backend != "cubesandbox":
        pytest.skip("cubecli envd-log checks only apply to the cubesandbox backend")


@pytest.fixture
def cubecli_logs_target(sdk_backend: str) -> tuple[str, str]:
    _require_cubesandbox(sdk_backend)
    return skip_unless_single_node_cubecli()


def _trigger_and_wait(
    adapter,
    cubecli: str,
    address: str,
    *,
    command_timeout: int,
    wait_timeout: float,
    min_count: int,
) -> str:
    marker = trigger_envd(adapter, timeout=command_timeout)
    return wait_for_envd_rpc(
        cubecli,
        adapter.sandbox_id,
        address=address,
        timeout=wait_timeout,
        min_count=min_count,
        needles=(marker,),
    )


@pytest.mark.requires_capability(COMMANDS)
def test_create_envd_rpc_visible_in_cubecli_logs(
    sdk_sandbox,
    sdk_e2e_config,
    cubecli_logs_target,
):
    cubecli, address = cubecli_logs_target
    assert_host_logs_present(sdk_sandbox.sandbox_id)
    logs = _trigger_and_wait(
        sdk_sandbox,
        cubecli,
        address,
        command_timeout=sdk_e2e_config.command_timeout,
        wait_timeout=sdk_e2e_config.command_timeout,
        min_count=1,
    )
    assert contains_envd_access_log(logs)
    sandbox_id = sdk_sandbox.sandbox_id
    cleanup_errors = safe_kill(sdk_sandbox, sdk_e2e_config)
    assert not cleanup_errors, f"sandbox cleanup failed: {cleanup_errors!r}"
    wait_host_logs_absent(sandbox_id, timeout=sdk_e2e_config.default_timeout)


@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.requires_capability(PAUSE_RESUME)
def test_resume_appends_envd_rpc_to_cubecli_logs(
    sdk_sandbox,
    sdk_e2e_config,
    cubecli_logs_target,
):
    cubecli, address = cubecli_logs_target
    _trigger_and_wait(
        sdk_sandbox,
        cubecli,
        address,
        command_timeout=sdk_e2e_config.command_timeout,
        wait_timeout=sdk_e2e_config.command_timeout,
        min_count=1,
    )
    assert_host_logs_present(sdk_sandbox.sandbox_id)

    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        assert resumed.sandbox_id == sdk_sandbox.sandbox_id
        assert_host_logs_present(resumed.sandbox_id)
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        marker = trigger_envd(resumed, timeout=sdk_e2e_config.command_timeout)
        wait_for_host_log_contains(
            resumed.sandbox_id,
            marker,
            timeout=sdk_e2e_config.command_timeout,
        )
        logs = wait_for_envd_rpc(
            cubecli,
            resumed.sandbox_id,
            address=address,
            timeout=sdk_e2e_config.command_timeout,
            min_count=1,
            needles=(marker,),
        )
        assert contains_envd_access_log(logs)
    finally:
        resumed.close()


@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.requires_capability(ROLLBACK_CLONE)
def test_snapshot_rollback_keeps_envd_cubecli_logs(
    sdk_sandbox,
    sdk_e2e_config,
    cubecli_logs_target,
):
    cubecli, address = cubecli_logs_target
    _trigger_and_wait(
        sdk_sandbox,
        cubecli,
        address,
        command_timeout=sdk_e2e_config.command_timeout,
        wait_timeout=sdk_e2e_config.command_timeout,
        min_count=1,
    )
    assert_host_logs_present(sdk_sandbox.sandbox_id)

    snapshot_id = None
    cleanup_errors = []
    cleanup_error = None
    try:
        snapshot_id = sdk_sandbox.create_snapshot()
        _trigger_and_wait(
            sdk_sandbox,
            cubecli,
            address,
            command_timeout=sdk_e2e_config.command_timeout,
            wait_timeout=sdk_e2e_config.command_timeout,
            min_count=1,
        )
        assert_host_logs_present(sdk_sandbox.sandbox_id)

        sdk_sandbox.rollback(snapshot_id)
        assert_host_logs_present(sdk_sandbox.sandbox_id)
        wait_until_data_plane_ready(
            sdk_sandbox,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        marker = trigger_envd(sdk_sandbox, timeout=sdk_e2e_config.command_timeout)
        wait_for_host_log_contains(
            sdk_sandbox.sandbox_id,
            marker,
            timeout=sdk_e2e_config.command_timeout,
        )
        logs = wait_for_envd_rpc(
            cubecli,
            sdk_sandbox.sandbox_id,
            address=address,
            timeout=sdk_e2e_config.command_timeout,
            min_count=1,
            needles=(marker,),
        )
        assert contains_envd_access_log(logs)
    finally:
        cleanup_errors.extend(safe_kill(sdk_sandbox, sdk_e2e_config))
        if snapshot_id:
            try:
                from cases.lifecycle.test_rollback_clone import (
                    _delete_snapshot_after_runtime_release,
                )

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


def test_template_envd_output_in_cubecli_tpl_logs(
    sdk_backend,
    sdk_e2e_config,
    cubecli_logs_target,
):
    _require_cubesandbox(sdk_backend)
    if not sdk_e2e_config.cube_template_id:
        pytest.skip("set CUBE_TEMPLATE_ID for template envd log checks")
    cubecli, _address = cubecli_logs_target
    logs = read_cubecli_logs(
        cubecli,
        sdk_e2e_config.cube_template_id,
        template=True,
    )
    if TEMPLATE_ENVD_MARKER not in logs:
        logs = logs + "\n" + read_cubecli_logs(
            cubecli,
            sdk_e2e_config.cube_template_id,
            template=True,
            stream="stderr",
        )
    assert TEMPLATE_ENVD_MARKER in logs, (
        f"expected {TEMPLATE_ENVD_MARKER!r} in cubecli logs --tpl, got {logs!r}"
    )


@pytest.mark.requires_capability(COMMANDS)
@pytest.mark.requires_capability(ROLLBACK_CLONE)
def test_snapshot_start_envd_rpc_visible_in_cubecli_logs(
    sdk_sandbox,
    sdk_e2e_config,
    cubecli_logs_target,
):
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    from cases.lifecycle.test_rollback_clone import (
        _delete_snapshot_after_runtime_release,
    )

    cubecli, address = cubecli_logs_target
    _trigger_and_wait(
        sdk_sandbox,
        cubecli,
        address,
        command_timeout=sdk_e2e_config.command_timeout,
        wait_timeout=sdk_e2e_config.command_timeout,
        min_count=1,
    )
    snapshot_id = None
    child = None
    cleanup_error = None
    try:
        snapshot_id = sdk_sandbox.create_snapshot()
        child = CubeSandboxAdapter.create(
            sdk_e2e_config,
            create_options={"template": snapshot_id},
        )
        wait_until_running(child, timeout=sdk_e2e_config.default_timeout)
        wait_until_data_plane_ready(
            child,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        assert_host_logs_present(child.sandbox_id)
        logs = _trigger_and_wait(
            child,
            cubecli,
            address,
            command_timeout=sdk_e2e_config.command_timeout,
            wait_timeout=sdk_e2e_config.command_timeout,
            min_count=1,
        )
        assert contains_envd_access_log(logs)
        child_id = child.sandbox_id
        child_errors = safe_kill(child, sdk_e2e_config)
        child = None
        assert not child_errors, f"snapshot-start sandbox cleanup failed: {child_errors!r}"
        wait_host_logs_absent(child_id, timeout=sdk_e2e_config.default_timeout)
    finally:
        if child is not None:
            safe_kill(child, sdk_e2e_config)
        if snapshot_id:
            try:
                _delete_snapshot_after_runtime_release(
                    sdk_sandbox,
                    snapshot_id,
                    timeout=sdk_e2e_config.default_timeout,
                )
            except Exception as exc:  # noqa: BLE001 - do not mask the test failure
                cleanup_error = exc
    assert cleanup_error is None, (
        f"snapshot cleanup failed for {snapshot_id}: {cleanup_error}"
    )


def test_template_build_writes_cubecli_tpl_logs(
    sdk_backend,
    sdk_e2e_config,
    cubecli_logs_target,
):
    _require_cubesandbox(sdk_backend)
    from cubesandbox import Config, Template
    from cubesandbox._exceptions import ApiError, TemplateNotFoundError
    from cases.templates.test_alias import (
        DEFAULT_IMAGE,
        DEFAULT_WRITABLE_LAYER_SIZE,
        _delete_with_retry,
        _wait_for_ready,
    )
    from framework.build_throttle import template_build_slot

    cubecli, _address = cubecli_logs_target
    cfg = Config(api_url=sdk_e2e_config.cube_api_url)
    created_id = None
    try:
        with template_build_slot(label="envd_logs_template_build"):
            job = Template.build(
                image=DEFAULT_IMAGE,
                writable_layer_size=DEFAULT_WRITABLE_LAYER_SIZE,
                config=cfg,
            )
            created_id = job.template_id
            _wait_for_ready(created_id, cfg, timeout=600)
        logs = read_cubecli_logs(cubecli, created_id, template=True)
        if TEMPLATE_ENVD_MARKER not in logs:
            logs = logs + "\n" + read_cubecli_logs(
                cubecli,
                created_id,
                template=True,
                stream="stderr",
            )
        assert TEMPLATE_ENVD_MARKER in logs, (
            f"expected {TEMPLATE_ENVD_MARKER!r} in cubecli logs --tpl "
            f"after template build, got {logs!r}"
        )
        assert template_log_dir(created_id).is_dir(), (
            f"missing template log dir {template_log_dir(created_id)}"
        )
    finally:
        if created_id:
            try:
                _delete_with_retry(created_id, cfg)
            except (ApiError, TemplateNotFoundError):
                pass


def test_cubecli_reads_host_log_without_mntns(cubecli_logs_target):
    import shutil

    cubecli, address = cubecli_logs_target
    sandbox_id = "aabbccddeeff00112233445566778899"
    directory = host_log_dir(sandbox_id)
    try:
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "stdout").write_text("host-path-compat-marker\n")
        (directory / "stderr").write_text("")
        logs = read_cubecli_logs(cubecli, sandbox_id, address=address)
        assert "host-path-compat-marker" in logs
    finally:
        shutil.rmtree(directory, ignore_errors=True)


def test_cubecli_missing_log_mentions_host_and_bundle(cubecli_logs_target):
    cubecli, address = cubecli_logs_target
    sandbox_id = "ffffffffffffffffffffffffffffffff"
    with pytest.raises(AssertionError) as exc:
        read_cubecli_logs(cubecli, sandbox_id, address=address)
    message = str(exc.value)
    assert "log file not found" in message
    assert HOST_LOG_ROOT in message
    assert sandbox_id in message
