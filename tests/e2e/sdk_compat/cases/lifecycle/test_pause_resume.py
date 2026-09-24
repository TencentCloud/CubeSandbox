# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from adapters import list_sandboxes
from framework.assertions import assert_code_ok, assert_command_ok
from framework.capabilities import PAUSE_RESUME, RUN_CODE
from framework.lifecycle import (
    metadata_from_info,
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.lifecycle,
    pytest.mark.p1,
    pytest.mark.requires_capability(PAUSE_RESUME),
]


def _pause_and_resume(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
    wait_until_data_plane_ready(
        resumed,
        timeout=sdk_e2e_config.default_timeout,
        command_timeout=sdk_e2e_config.command_timeout,
    )
    return resumed


def _listed_sandbox_id(entry: dict) -> str:
    for key in ("sandbox_id", "sandboxID", "id"):
        value = entry.get(key)
        if value:
            return str(value)
    return ""


@pytest.mark.sandbox_create_options(metadata={"sdk_compat_pause_meta": "keep-me"})
def test_pause_preserves_metadata(sdk_sandbox, sdk_backend, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)

    metadata = metadata_from_info(sdk_sandbox.info().raw)
    assert metadata.get("sdk_compat_pause_meta") == "keep-me"
    assert metadata.get("test_suite") == "sdk_compat"

    entries = list_sandboxes(sdk_backend, sdk_e2e_config)
    paused = [entry for entry in entries if _listed_sandbox_id(entry) == sdk_sandbox.sandbox_id]
    assert paused, "paused sandbox must stay visible in list"
    assert paused[0].get("metadata", {}).get("sdk_compat_pause_meta") == "keep-me"

    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        metadata = metadata_from_info(resumed.info().raw)
        assert metadata.get("sdk_compat_pause_meta") == "keep-me"
    finally:
        resumed.close()


def test_pause_sets_state_paused(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    state = wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    assert state == "paused"


def test_pause_and_connect_resume_preserves_files(sdk_sandbox, sdk_e2e_config):
    sdk_sandbox.write_file("/tmp/sdk-compat-pause.txt", "before-pause")

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        assert resumed.sandbox_id == sdk_sandbox.sandbox_id
        assert resumed.read_file("/tmp/sdk-compat-pause.txt") == "before-pause"
    finally:
        resumed.close()


def test_pause_and_connect_resume_allows_commands(sdk_sandbox, sdk_e2e_config):
    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        result = resumed.run_command(
            "printf resumed",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "resumed"
    finally:
        resumed.close()


@pytest.mark.sandbox_create_options(env_vars={"SDK_COMPAT_PAUSE_ENV": "pause-env"})
def test_pause_and_connect_resume_preserves_env_vars(sdk_sandbox, sdk_e2e_config):
    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        result = resumed.run_command(
            'printf "%s" "$SDK_COMPAT_PAUSE_ENV"',
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(result)
        assert result.stdout == "pause-env"
    finally:
        resumed.close()


@pytest.mark.requires_capability(RUN_CODE)
@pytest.mark.requires_code_interpreter
def test_pause_and_connect_resume_preserves_run_code_state(sdk_sandbox, sdk_e2e_config):
    first = sdk_sandbox.run_code(
        "sdk_compat_pause_value = 84",
        timeout=sdk_e2e_config.run_code_timeout,
    )
    assert_code_ok(first)

    resumed = _pause_and_resume(sdk_sandbox, sdk_e2e_config)
    try:
        second = resumed.run_code(
            "sdk_compat_pause_value + 1",
            timeout=sdk_e2e_config.run_code_timeout,
        )
        assert_code_ok(second)
        assert second.text == "85"
    finally:
        resumed.close()
