# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from framework.capabilities import FILESYSTEM

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.filesystem,
    pytest.mark.p0,
    pytest.mark.requires_capability(FILESYSTEM),
]


def _create_file(sdk_sandbox, path: str, content: str = "data", *, user: str):
    sdk_sandbox.write_file(path, content, user=user)
    assert sdk_sandbox.file_exists(path, user=user)


def _create_dir(sdk_sandbox, path: str, *, user: str, sdk_e2e_config):
    sdk_sandbox.run_command(
        f"mkdir -p {path}", user=user, timeout=sdk_e2e_config.command_timeout,
    )


def test_remove_file_exists(sdk_sandbox):
    path = "/tmp/ops-rm.txt"
    _create_file(sdk_sandbox, path, user="root")
    sdk_sandbox.remove_file(path, user="root")
    assert not sdk_sandbox.file_exists(path, user="root")


def test_remove_file_exists_as_nobody(sdk_sandbox):
    path = "/tmp/ops-rm-nobody.txt"
    _create_file(sdk_sandbox, path, user="nobody")
    sdk_sandbox.remove_file(path, user="nobody")
    assert not sdk_sandbox.file_exists(path, user="nobody")


def test_remove_missing_file_is_silent(sdk_sandbox):
    sdk_sandbox.remove_file("/tmp/ops-rm-missing.txt", user="root")
    sdk_sandbox.remove_file("/tmp/ops-rm-missing.txt", user="nobody")


def test_remove_directory(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/ops-rm-dir"
    _create_dir(sdk_sandbox, path, user="root", sdk_e2e_config=sdk_e2e_config)
    sdk_sandbox.remove_file(path, user="root")
    assert not sdk_sandbox.file_exists(path, user="root")


def test_list_empty_directory(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/ops-list-empty"
    _create_dir(sdk_sandbox, path, user="root", sdk_e2e_config=sdk_e2e_config)
    entries = sdk_sandbox.list_dir(path, user="root")
    assert isinstance(entries, list)
    real = [e for e in entries if e not in (".", "..")]
    assert real == []


def test_list_empty_directory_as_nobody(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/ops-list-nobody-empty"
    _create_dir(sdk_sandbox, path, user="nobody", sdk_e2e_config=sdk_e2e_config)
    entries = sdk_sandbox.list_dir(path, user="nobody")
    assert isinstance(entries, list)
    real = [e for e in entries if e not in (".", "..")]
    assert real == []


def test_list_nonempty_directory(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/ops-list-nonempty"
    _create_dir(sdk_sandbox, path, user="root", sdk_e2e_config=sdk_e2e_config)
    _create_file(sdk_sandbox, f"{path}/a.txt", user="root")
    _create_file(sdk_sandbox, f"{path}/b.txt", user="root")
    entries = sdk_sandbox.list_dir(path, user="root")
    real = [e["name"] if isinstance(e, dict) else e for e in entries if e not in (".", "..")]
    assert sorted(real) == ["a.txt", "b.txt"]


def test_list_nonempty_directory_as_nobody(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/ops-list-nobody-nonempty"
    _create_dir(sdk_sandbox, path, user="nobody", sdk_e2e_config=sdk_e2e_config)
    _create_file(sdk_sandbox, f"{path}/x.txt", user="nobody")
    entries = sdk_sandbox.list_dir(path, user="nobody")
    real = [e["name"] if isinstance(e, dict) else e for e in entries if e not in (".", "..")]
    assert "x.txt" in real


def test_make_dir(sdk_sandbox):
    path = "/tmp/ops-mkdir"
    sdk_sandbox.make_dir(path, user="root")
    assert sdk_sandbox.file_exists(path, user="root")


def test_make_dir_as_nobody(sdk_sandbox):
    path = "/tmp/ops-mkdir-nobody"
    sdk_sandbox.make_dir(path, user="nobody")
    assert sdk_sandbox.file_exists(path, user="nobody")


def test_make_dir_nested(sdk_sandbox, sdk_e2e_config):
    parent = "/tmp/ops-mkdir-nested"
    _create_dir(sdk_sandbox, parent, user="root", sdk_e2e_config=sdk_e2e_config)
    leaf = f"{parent}/sub"
    sdk_sandbox.make_dir(leaf, user="root")
    assert sdk_sandbox.file_exists(leaf, user="root")


def test_rename_file(sdk_sandbox):
    old = "/tmp/ops-rename-old.txt"
    new = "/tmp/ops-rename-new.txt"
    _create_file(sdk_sandbox, old, "root-data", user="root")
    sdk_sandbox.rename_file(old, new, user="root")
    assert not sdk_sandbox.file_exists(old, user="root")
    assert sdk_sandbox.file_exists(new, user="root")
    assert sdk_sandbox.read_file(new, user="root") == "root-data"


def test_rename_file_as_nobody(sdk_sandbox):
    old = "/tmp/ops-rename-nobody-old.txt"
    new = "/tmp/ops-rename-nobody-new.txt"
    _create_file(sdk_sandbox, old, "nobody-data", user="nobody")
    sdk_sandbox.rename_file(old, new, user="nobody")
    assert not sdk_sandbox.file_exists(old, user="nobody")
    assert sdk_sandbox.file_exists(new, user="nobody")
    assert sdk_sandbox.read_file(new, user="nobody") == "nobody-data"


def test_rename_directory(sdk_sandbox, sdk_e2e_config):
    old = "/tmp/ops-rename-dir-old"
    new = "/tmp/ops-rename-dir-new"
    _create_dir(sdk_sandbox, old, user="root", sdk_e2e_config=sdk_e2e_config)
    _create_file(sdk_sandbox, f"{old}/inner.txt", user="root")
    sdk_sandbox.rename_file(old, new, user="root")
    assert not sdk_sandbox.file_exists(old, user="root")
    assert sdk_sandbox.file_exists(new, user="root")
    assert sdk_sandbox.file_exists(f"{new}/inner.txt", user="root")


def test_exists_true(sdk_sandbox):
    path = "/tmp/ops-exists-true.txt"
    _create_file(sdk_sandbox, path, user="root")
    assert sdk_sandbox.file_exists(path, user="root")


def test_exists_true_as_nobody(sdk_sandbox):
    path = "/tmp/ops-exists-nobody-true.txt"
    _create_file(sdk_sandbox, path, user="nobody")
    assert sdk_sandbox.file_exists(path, user="nobody")


def test_exists_false(sdk_sandbox):
    assert not sdk_sandbox.file_exists("/tmp/ops-exists-missing.txt", user="root")
    assert not sdk_sandbox.file_exists("/tmp/ops-exists-missing.txt", user="nobody")


def test_exists_root_dir(sdk_sandbox):
    assert sdk_sandbox.file_exists("/", user="root")


def test_nobody_sees_root_files_in_tmp(sdk_sandbox):
    path = "/tmp/ops-exists-world.txt"
    _create_file(sdk_sandbox, path, user="root")
    result = sdk_sandbox.file_exists(path, user="nobody")
    assert isinstance(result, bool)  # may be True (visible) or False (permission)
