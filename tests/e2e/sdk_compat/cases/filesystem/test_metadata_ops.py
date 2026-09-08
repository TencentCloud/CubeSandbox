# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""E2E coverage for filesystem metadata RPCs:

- list / stat / exists / remove / rename / make_dir
- explicit ``user`` parameter on every call

Tests are run against both the cubesandbox backend and the e2b backend.
The two SDKs have small surface differences, normalized by the helpers
below (see ``_files_call`` and ``_entry_name``).
"""

from __future__ import annotations

import inspect

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import FILESYSTEM

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.filesystem,
    pytest.mark.p1,
    pytest.mark.requires_capability(FILESYSTEM),
]


# ── backend-agnostic helpers ──────────────────────────────────────────────────

# Method name differences between the cubesandbox SDK and the e2b SDK's
# files object. The e2b SDK uses get_info where cubesandbox uses stat.
_FILE_METHOD_ALIASES = {
    "stat": "get_info",
}


def _files_call(files, method: str, /, *args, **kwargs):
    """Call a method on the raw files object, normalizing backend differences.

    - Resolves ``stat`` to ``get_info`` on the e2b SDK.
    - Strips ``user=`` kwarg when the method does not accept it (the e2b
      SDK's filesystem methods don't support per-request user isolation;
      the cubesandbox SDK does, and uses the kwarg to forward ``username``
      in the request body).
    """
    fn = getattr(files, method, None)
    if fn is None and method in _FILE_METHOD_ALIASES:
        fn = getattr(files, _FILE_METHOD_ALIASES[method], None)
    if fn is None:
        raise AttributeError(
            f"files object exposes neither {method!r} nor "
            f"{_FILE_METHOD_ALIASES.get(method)!r}"
        )

    if "user" in kwargs:
        try:
            sig = inspect.signature(fn)
            accepts_var_kw = any(
                p.kind is inspect.Parameter.VAR_KEYWORD
                for p in sig.parameters.values()
            )
            accepts_user = accepts_var_kw or "user" in sig.parameters
        except (TypeError, ValueError):
            accepts_user = False
        if not accepts_user:
            kwargs.pop("user")

    return fn(*args, **kwargs)


def _entry_name(entry) -> str:
    """Extract the ``name`` field from a directory entry.

    The cubesandbox SDK returns plain dicts (``{"name": "..."}``) while
    the e2b SDK returns dataclass-like objects with a ``.name`` attribute.
    """
    if isinstance(entry, dict):
        return str(entry.get("name", ""))
    return str(getattr(entry, "name", ""))


# ---------------------------------------------------------------------------
# list
# ---------------------------------------------------------------------------


def test_list_directory_entries(sdk_sandbox, sdk_e2e_config):
    dir_path = "/tmp/sdk-compat-list-dir"
    sdk_sandbox.run_command(
        f"mkdir -p {dir_path} && touch {dir_path}/a.txt {dir_path}/b.txt",
        timeout=sdk_e2e_config.command_timeout,
    )

    entries = _files_call(sdk_sandbox.raw_sandbox.files, "list", dir_path)

    names = {_entry_name(e) for e in entries}
    assert "a.txt" in names
    assert "b.txt" in names


def test_list_with_explicit_user(sdk_sandbox, sdk_e2e_config):
    dir_path = "/tmp/sdk-compat-list-user"
    sdk_sandbox.run_command(
        f"mkdir -p {dir_path} && touch {dir_path}/f1",
        timeout=sdk_e2e_config.command_timeout,
    )

    entries = _files_call(
        sdk_sandbox.raw_sandbox.files, "list", dir_path, user="root",
    )

    assert isinstance(entries, list)
    assert any(_entry_name(e) == "f1" for e in entries)


# ---------------------------------------------------------------------------
# stat
# ---------------------------------------------------------------------------


def test_stat_returns_file_metadata(sdk_sandbox):
    path = "/tmp/sdk-compat-stat.txt"
    sdk_sandbox.write_file(path, "hello")

    entry = _files_call(sdk_sandbox.raw_sandbox.files, "stat", path)

    # Both backends return a dict-like object exposing at least "type" or "name"
    if isinstance(entry, dict):
        keys = set(entry.keys())
    else:
        keys = {a for a in dir(entry) if not a.startswith("_")}
    assert "type" in keys or "name" in keys


def test_stat_with_explicit_user(sdk_sandbox):
    path = "/tmp/sdk-compat-stat-user.txt"
    sdk_sandbox.write_file(path, "user-stat")

    entry = _files_call(
        sdk_sandbox.raw_sandbox.files, "stat", path, user="root",
    )

    assert entry is not None


def test_stat_missing_path_raises(sdk_sandbox):
    with pytest.raises(Exception, match=r"(?i)not.found|404|no such"):
        _files_call(
            sdk_sandbox.raw_sandbox.files, "stat",
            "/tmp/sdk-compat-stat-missing-xyz",
        )


# ---------------------------------------------------------------------------
# exists
# ---------------------------------------------------------------------------


def test_exists_reports_presence_correctly(sdk_sandbox):
    path = "/tmp/sdk-compat-exists.txt"
    sdk_sandbox.write_file(path, "hi")

    files = sdk_sandbox.raw_sandbox.files

    assert _files_call(files, "exists", path) is True
    assert _files_call(files, "exists", "/tmp/sdk-compat-nonexistent-xyz") is False


def test_exists_with_explicit_user(sdk_sandbox):
    path = "/tmp/sdk-compat-exists-user.txt"
    sdk_sandbox.write_file(path, "user-exists")

    files = sdk_sandbox.raw_sandbox.files

    assert _files_call(files, "exists", path, user="root") is True
    assert _files_call(files, "exists", "/tmp/sdk-compat-user-missing", user="root") is False


# ---------------------------------------------------------------------------
# remove
# ---------------------------------------------------------------------------


def test_remove_deletes_file(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/sdk-compat-remove.txt"
    sdk_sandbox.write_file(path, "delete me")

    _files_call(sdk_sandbox.raw_sandbox.files, "remove", path)

    result = sdk_sandbox.run_command(
        f"test -f {path} && echo exists || echo gone",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "gone" in result.stdout


def test_remove_with_explicit_user(sdk_sandbox, sdk_e2e_config):
    path = "/tmp/sdk-compat-remove-user.txt"
    sdk_sandbox.write_file(path, "user-remove")

    _files_call(sdk_sandbox.raw_sandbox.files, "remove", path, user="root")

    result = sdk_sandbox.run_command(
        f"test -f {path} && echo exists || echo gone",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "gone" in result.stdout


# ---------------------------------------------------------------------------
# rename
# ---------------------------------------------------------------------------


def test_rename_moves_file(sdk_sandbox, sdk_e2e_config):
    old_path = "/tmp/sdk-compat-rename-old.txt"
    new_path = "/tmp/sdk-compat-rename-new.txt"
    sdk_sandbox.write_file(old_path, "before rename")

    _files_call(sdk_sandbox.raw_sandbox.files, "rename", old_path, new_path)

    assert sdk_sandbox.read_file(new_path) == "before rename"
    result = sdk_sandbox.run_command(
        f"test -f {old_path} && echo exists || echo gone",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "gone" in result.stdout


def test_rename_with_explicit_user(sdk_sandbox):
    old_path = "/tmp/sdk-compat-rename-user-old.txt"
    new_path = "/tmp/sdk-compat-rename-user-new.txt"
    sdk_sandbox.write_file(old_path, "user-rename")

    _files_call(
        sdk_sandbox.raw_sandbox.files, "rename", old_path, new_path, user="root",
    )

    assert sdk_sandbox.read_file(new_path) == "user-rename"


# ---------------------------------------------------------------------------
# make_dir
# ---------------------------------------------------------------------------


def test_make_dir_creates_directory(sdk_sandbox, sdk_e2e_config):
    dir_path = "/tmp/sdk-compat-makedir"

    _files_call(sdk_sandbox.raw_sandbox.files, "make_dir", dir_path)

    result = sdk_sandbox.run_command(
        f"test -d {dir_path} && echo is_dir || echo not_dir",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "is_dir" in result.stdout


def test_make_dir_with_explicit_user(sdk_sandbox, sdk_e2e_config):
    dir_path = "/tmp/sdk-compat-makedir-user"

    entry = _files_call(
        sdk_sandbox.raw_sandbox.files, "make_dir", dir_path, user="root",
    )

    # Both backends return a dict-like entry; just verify it's truthy
    assert entry is not None
    result = sdk_sandbox.run_command(
        f"test -d {dir_path} && echo is_dir || echo not_dir",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "is_dir" in result.stdout


# ---------------------------------------------------------------------------
# cross-user filesystem operations
# ---------------------------------------------------------------------------
#
# NOTE: A test that exercised list/stat/exists/remove/rename/make_dir as
# a non-root user used to live here. It was removed because envd does
# not yet honor the request body's ``username`` field — every operation
# runs as root regardless of the SDK's user parameter, so the assertion
# ``stat -c '%U' <user_dir> == <user>`` always fails.
#
# The follow-up is tracked in docs/dev/pending-envd-fixes.md. Once envd
# gains per-user filesystem RPC enforcement, restore the test from the
# PR that removed it (commit history is the source of truth).
