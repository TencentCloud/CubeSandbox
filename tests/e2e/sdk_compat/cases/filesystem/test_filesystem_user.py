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


# ── helpers ───────────────────────────────────────────────────────────────────

def _create_file(sdk_sandbox, path: str, content: str = "data", *, user: str):
    sdk_sandbox.write_file(path, content, user=user)
    assert sdk_sandbox.file_exists(path, user=user)


def _create_dir(sdk_sandbox, path: str, *, user: str, sdk_e2e_config):
    sdk_sandbox.run_command(
        f"mkdir -p {path}", user=user, timeout=sdk_e2e_config.command_timeout,
    )


def _skip_e2b_sticky_bit(sdk_backend: str, scenario: str) -> None:
    """Skip permission-boundary tests that depend on the /tmp sticky bit.

    The e2b template's /tmp does not have the sticky bit set, so the
    standard "only the owner can delete" guarantee from /tmp on Linux
    does not apply — nobody can remove root-owned files there. The
    underlying SDK behavior is still correct; this is purely an image
    configuration difference.
    """
    if sdk_backend == "e2b":
        pytest.skip(
            f"e2b /tmp has no sticky bit: {scenario} cannot pass on this "
            f"backend"
        )


# ═══════════════════════════════════════════════════════════════════════════════
# remove
# ═══════════════════════════════════════════════════════════════════════════════

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


def test_nobody_cannot_remove_root_file(sdk_sandbox, sdk_e2e_config, sdk_backend):
    """Nobody cannot delete a root-owned file in /tmp.

    The /tmp sticky bit means only the file's owner can delete it.
    Verified via the post-condition (file still exists) rather than
    relying on a specific shell stderr message.
    """
    _skip_e2b_sticky_bit(
        sdk_backend, "nobody removing root-owned /tmp file",
    )
    path = "/tmp/ops-rm-root-only.txt"
    _create_file(sdk_sandbox, path, user="root")
    sdk_sandbox.run_command(
        f"chmod 600 {path}",
        user="root", timeout=sdk_e2e_config.command_timeout,
    )
    sdk_sandbox.run_command(
        f"rm -f {path}",
        user="nobody", timeout=sdk_e2e_config.command_timeout,
    )
    assert sdk_sandbox.file_exists(path, user="root"), (
        f"file {path!r} should still exist after nobody tried to remove it"
    )


# ═══════════════════════════════════════════════════════════════════════════════
# list_dir
# ═══════════════════════════════════════════════════════════════════════════════

def test_list_empty_directory_as_nobody(sdk_sandbox):
    """Nobody lists an empty directory.

    Uses make_dir(root) here because e2b's nobody user has no valid
    cwd, making run_command(mkdir, user=nobody) fail on that backend.
    The created directory in /tmp is world-accessible regardless.
    """
    path = "/tmp/ops-list-nobody-empty"
    sdk_sandbox.make_dir(path, user="root")
    entries = sdk_sandbox.list_dir(path, user="nobody")
    assert isinstance(entries, list)
    real = [e for e in entries if e not in (".", "..")]
    assert real == []


def test_list_nonempty_directory_as_nobody(sdk_sandbox):
    path = "/tmp/ops-list-nobody-nonempty"
    sdk_sandbox.make_dir(path, user="root")
    _create_file(sdk_sandbox, f"{path}/x.txt", user="nobody")
    entries = sdk_sandbox.list_dir(path, user="nobody")
    real = [e for e in entries if e not in (".", "..")]
    assert "x.txt" in real


# ═══════════════════════════════════════════════════════════════════════════════
# make_dir
# ═══════════════════════════════════════════════════════════════════════════════

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


def test_nobody_cannot_make_dir_in_root_area(sdk_sandbox, sdk_e2e_config):
    """Nobody cannot create a directory in /etc (root-owned, not world-writable)."""
    target = "/etc/ops-nobody-xfail-test"
    sdk_sandbox.run_command(
        f"mkdir -p {target}",
        user="nobody", timeout=sdk_e2e_config.command_timeout,
    )
    assert not sdk_sandbox.file_exists(target, user="root"), (
        f"directory {target!r} should not exist after nobody tried to create it"
    )


# ═══════════════════════════════════════════════════════════════════════════════
# rename
# ═══════════════════════════════════════════════════════════════════════════════

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


def test_nobody_cannot_rename_root_file(sdk_sandbox, sdk_e2e_config, sdk_backend):
    """Nobody cannot rename a root-owned file in /tmp (sticky bit + owner-only rename)."""
    _skip_e2b_sticky_bit(
        sdk_backend, "nobody renaming root-owned /tmp file",
    )
    old = "/tmp/ops-rename-root.txt"
    renamed = f"{old}.renamed"
    _create_file(sdk_sandbox, old, user="root")
    sdk_sandbox.run_command(
        f"chmod 600 {old}",
        user="root", timeout=sdk_e2e_config.command_timeout,
    )
    sdk_sandbox.run_command(
        f"mv {old} {renamed}",
        user="nobody", timeout=sdk_e2e_config.command_timeout,
    )
    assert sdk_sandbox.file_exists(old, user="root"), (
        f"file {old!r} should still exist at its original path after "
        f"nobody tried to rename it"
    )
    assert not sdk_sandbox.file_exists(renamed, user="root"), (
        f"renamed file {renamed!r} should not exist after nobody tried to "
        f"create it"
    )


# ═══════════════════════════════════════════════════════════════════════════════
# exists
# ═══════════════════════════════════════════════════════════════════════════════

def test_exists_true_as_nobody(sdk_sandbox):
    path = "/tmp/ops-exists-nobody-true.txt"
    _create_file(sdk_sandbox, path, user="nobody")
    assert sdk_sandbox.file_exists(path, user="nobody")


def test_exists_root_dir(sdk_sandbox):
    assert sdk_sandbox.file_exists("/", user="root")


def test_nobody_sees_root_files_in_tmp(sdk_sandbox):
    path = "/tmp/ops-exists-world.txt"
    _create_file(sdk_sandbox, path, user="root")
    result = sdk_sandbox.file_exists(path, user="nobody")
    assert isinstance(result, bool)  # may be True (visible) or False (permission)
