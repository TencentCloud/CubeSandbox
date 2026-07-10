# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from framework.assertions import assert_command_ok
from framework.capabilities import FILESYSTEM

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
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


def test_command_created_file_is_visible_to_files_api(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/sdk-compat-command-created.txt"
    result = sdk_sandbox.run_command(
        f"printf command-created > {path}",
        timeout=sdk_e2e_config.command_timeout,
    )

    assert_command_ok(result)
    assert sdk_sandbox.read_file(path) == "command-created"


def test_exists_for_present_file(sdk_sandbox):
    path = "/tmp/sdk-compat-exists.txt"

    sdk_sandbox.write_file(path, "hi")

    assert sdk_sandbox.exists(path) is True


def test_exists_for_missing_file(sdk_sandbox):
    assert sdk_sandbox.exists("/tmp/sdk-compat-nonexistent-xxx") is False


@pytest.mark.p1
def test_make_dir_and_list(sdk_sandbox):
    dir_path = "/tmp/sdk-compat-dir"

    sdk_sandbox.make_dir(dir_path)
    sdk_sandbox.write_file(f"{dir_path}/a.txt", "a")
    sdk_sandbox.write_file(f"{dir_path}/b.txt", "b")

    entries = sdk_sandbox.list_dir(dir_path)

    assert len(entries) == 2
    names = {entry.name for entry in entries}
    assert "a.txt" in names
    assert "b.txt" in names


@pytest.mark.p1
def test_remove_file(sdk_sandbox):
    path = "/tmp/sdk-compat-rm.txt"

    sdk_sandbox.write_file(path, "x")
    assert sdk_sandbox.exists(path) is True

    sdk_sandbox.remove_file(path)

    assert sdk_sandbox.exists(path) is False


@pytest.mark.p2
def test_make_dir_creates_nested(sdk_sandbox):
    sdk_sandbox.make_dir("/tmp/sdk-compat-nested/sub")

    assert sdk_sandbox.exists("/tmp/sdk-compat-nested/sub") is True
