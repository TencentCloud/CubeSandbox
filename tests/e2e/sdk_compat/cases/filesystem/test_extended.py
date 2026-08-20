# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import warnings

import pytest

from framework.capabilities import FILESYSTEM_EXTENDED

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.filesystem,
    pytest.mark.p1,
    pytest.mark.requires_capability(FILESYSTEM_EXTENDED),
]


def _entry_name(entry: dict) -> str:
    return str(entry.get("name") or entry.get("path") or "")


def _entry_type(entry: dict) -> str:
    return str(entry.get("type", "")).lower()


def test_list_stat_exists_remove_rename_and_mkdir(sdk_sandbox):
    root = f"/tmp/sdk-compat-fs-extended-{sdk_sandbox.sandbox_id}"
    nested = f"{root}/nested"
    source = f"{root}/source.txt"
    nested_file = f"{nested}/child.txt"
    renamed = f"{root}/renamed.txt"

    body_error: BaseException | None = None
    cleanup_errors: list[str] = []
    try:
        sdk_sandbox.make_dir(root)
        assert sdk_sandbox.file_exists(root)
        root_stat = sdk_sandbox.stat_file(root)
        assert _entry_name(root_stat).endswith(root.rsplit("/", 1)[-1]), root_stat
        assert "dir" in _entry_type(root_stat), root_stat

        sdk_sandbox.write_file(source, "extended-filesystem")
        assert sdk_sandbox.file_exists(source)

        stat = sdk_sandbox.stat_file(source)
        assert isinstance(stat, dict) and stat, f"empty stat for {source!r}"
        assert _entry_name(stat).endswith("source.txt"), stat
        assert "file" in _entry_type(stat), stat
        assert int(stat.get("size", -1)) == len("extended-filesystem"), stat
        assert stat.get("permissions"), stat

        sdk_sandbox.make_dir(nested)
        sdk_sandbox.write_file(nested_file, "nested")
        nested_entries = sdk_sandbox.list_files(nested)
        assert any(
            _entry_name(entry).endswith("child.txt") for entry in nested_entries
        ), nested_entries
        sdk_sandbox.remove_file(nested_file)
        assert sdk_sandbox.list_files(nested) == []
        sdk_sandbox.remove_file(nested)

        entries = sdk_sandbox.list_files(root)
        assert any(_entry_name(entry).endswith("source.txt") for entry in entries), (
            f"{source!r} missing from directory entries: {entries!r}"
        )

        moved = sdk_sandbox.rename_file(source, renamed)
        assert isinstance(moved, dict)
        assert not sdk_sandbox.file_exists(source)
        assert sdk_sandbox.file_exists(renamed)
        assert sdk_sandbox.read_file(renamed) == "extended-filesystem"

        sdk_sandbox.remove_file(renamed)
        assert not sdk_sandbox.file_exists(renamed)

        sdk_sandbox.remove_file(root)
        assert not sdk_sandbox.file_exists(root)
    except BaseException as exc:
        body_error = exc
        raise
    finally:
        # The fixture destroys the sandbox as a final safety net. Clean these
        # paths here as well so a failed assertion cannot poison a reused
        # sandbox during local debugging or a future wider-scoped fixture.
        for path in (nested_file, source, renamed, nested, root):
            try:
                if sdk_sandbox.file_exists(path):
                    sdk_sandbox.remove_file(path)
            except Exception as exc:  # noqa: BLE001 - collect every cleanup failure
                cleanup_errors.append(f"{path}: {exc}")
        if cleanup_errors:
            message = f"filesystem cleanup failed: {cleanup_errors!r}"
            if body_error is None:
                pytest.fail(message)
            elif hasattr(body_error, "add_note"):
                body_error.add_note(message)
            else:
                warnings.warn(message, RuntimeWarning, stacklevel=2)
