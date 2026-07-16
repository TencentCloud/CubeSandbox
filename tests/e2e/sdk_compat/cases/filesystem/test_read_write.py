# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import FILESYSTEM

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.filesystem,
    pytest.mark.p0,
    pytest.mark.requires_capability(FILESYSTEM),
]


def test_file_write_read_roundtrip(sdk_sandbox):
    path = "/tmp/sdk-compat-file.txt"

    sdk_sandbox.write_file(path, "hello file")

    assert sdk_sandbox.read_file(path) == "hello file"


def test_written_file_is_visible_to_commands(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/sdk-compat-command-visible.txt"

    sdk_sandbox.write_file(path, "from-files-api")
    result = sdk_sandbox.run_command(
        f"cat {path}",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert result.stdout == "from-files-api"


def test_file_overwrite_replaces_previous_content(sdk_sandbox):
    path = "/tmp/sdk-compat-overwrite.txt"

    sdk_sandbox.write_file(path, "old")
    sdk_sandbox.write_file(path, "new")

    assert sdk_sandbox.read_file(path) == "new"


def test_multiline_file_roundtrip(sdk_sandbox):
    path = "/tmp/sdk-compat-multiline.txt"
    content = "alpha\nbeta\ngamma\n"

    sdk_sandbox.write_file(path, content)

    assert sdk_sandbox.read_file(path) == content


def test_deep_path_file_roundtrip(sdk_sandbox, sdk_e2e_config):
    directory = "/tmp/sdk-compat/deep/path"
    path = f"{directory}/file.txt"
    result = sdk_sandbox.run_command(
        f"mkdir -p {directory}",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)

    sdk_sandbox.write_file(path, "deep-content")

    assert sdk_sandbox.read_file(path) == "deep-content"


def test_large_text_file_roundtrip(sdk_sandbox):
    path = "/tmp/sdk-compat-large.txt"
    content = "sdk-compat-large-line\n" * 4096

    sdk_sandbox.write_file(path, content)

    assert sdk_sandbox.read_file(path) == content


def test_command_created_file_is_visible_to_files_api(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/sdk-compat-command-created.txt"
    result = sdk_sandbox.run_command(
        f"printf command-created > {path}",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert sdk_sandbox.read_file(path) == "command-created"


def test_reading_missing_file_raises(sdk_sandbox):
    with pytest.raises(
        Exception,
        match=r"(?i)not found|does not exist|no such file|404",
    ):  # noqa: B017 - SDKs expose backend-specific file errors
        sdk_sandbox.read_file("/tmp/sdk-compat-does-not-exist.txt")
