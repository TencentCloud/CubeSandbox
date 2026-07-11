# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Tests for cubesandbox_exec.py — session state management.

Tests the file-based session→sandbox_id persistence without needing
a live CubeSandbox deployment.
"""

from __future__ import annotations

import json
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

import importlib
import os


@pytest.fixture
def exec_module(tmp_state_dir):
    """Import/reload cubesandbox_exec with an isolated STATE_DIR and
    mock SDK symbols (so _bootstrap() is not needed)."""
    import cubesandbox_exec
    importlib.reload(cubesandbox_exec)
    # Populate the SDK symbols that _bootstrap() would normally set,
    # so state-management and get_sandbox tests work without a live SDK.
    cubesandbox_exec.Sandbox = MagicMock()
    cubesandbox_exec.Config = MagicMock()
    cubesandbox_exec.SandboxNotFoundError = type(
        "SandboxNotFoundError", (Exception,), {})
    cubesandbox_exec.CubeSandboxError = type(
        "CubeSandboxError", (Exception,), {})
    cubesandbox_exec.ApiError = type("ApiError", (Exception,), {})
    cubesandbox_exec.TEMPLATE_ID = "tpl-test"
    return cubesandbox_exec


# ── _state_path ───────────────────────────────────────────────────────


class TestStatePath:
    def test_alnum_session_unchanged(self, exec_module, tmp_state_dir):
        p = exec_module._state_path("abc123")
        assert p == tmp_state_dir / "abc123.json"

    def test_dashes_and_underscores_preserved(self, exec_module, tmp_state_dir):
        p = exec_module._state_path("session-id_42")
        assert p.name == "session-id_42.json"

    def test_special_chars_sanitized(self, exec_module, tmp_state_dir):
        p = exec_module._state_path("user/session@host")
        # / and @ → _
        assert p.name == "user_session_host.json"

    def test_empty_session_defaults(self, exec_module, tmp_state_dir):
        p = exec_module._state_path("")
        assert p.name == f"{exec_module.DEFAULT_SESSION}.json"

    def test_dot_dot_does_not_escape(self, exec_module, tmp_state_dir):
        """Path traversal must not escape STATE_DIR."""
        p = exec_module._state_path("../../../etc/passwd")
        assert p.parent == tmp_state_dir
        assert ".." not in p.parts[len(tmp_state_dir.parts):]


# ── _save_state / _load_state round-trip ─────────────────────────────


class TestStateRoundTrip:
    def test_save_then_load(self, exec_module, tmp_state_dir):
        state = {"sandbox_id": "sb-abc", "mount": "/data/project"}
        exec_module._save_state("test-session", state)
        loaded = exec_module._load_state("test-session")
        assert loaded == state

    def test_load_missing_returns_empty(self, exec_module, tmp_state_dir):
        assert exec_module._load_state("nonexistent") == {}

    def test_load_corrupted_json_returns_empty(self, exec_module, tmp_state_dir):
        # Write garbage to the state file
        path = exec_module._state_path("corrupted")
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("not valid json {{{")
        assert exec_module._load_state("corrupted") == {}

    def test_save_creates_state_dir(self, exec_module, tmp_state_dir):
        assert not tmp_state_dir.exists()
        exec_module._save_session_state = exec_module._save_state  # alias
        exec_module._save_state("s1", {"sandbox_id": "x"})
        assert tmp_state_dir.exists()

    def test_save_overwrites(self, exec_module, tmp_state_dir):
        exec_module._save_state("s1", {"sandbox_id": "old"})
        exec_module._save_state("s1", {"sandbox_id": "new"})
        assert exec_module._load_state("s1")["sandbox_id"] == "new"


# ── _clear_state ──────────────────────────────────────────────────────


class TestClearState:
    def test_clear_removes_file(self, exec_module, tmp_state_dir):
        exec_module._save_state("s1", {"sandbox_id": "x"})
        assert exec_module._state_path("s1").exists()
        exec_module._clear_state("s1")
        assert not exec_module._state_path("s1").exists()

    def test_clear_missing_is_noop(self, exec_module, tmp_state_dir):
        # Must not raise
        exec_module._clear_state("never-existed")

    def test_clear_then_load_returns_empty(self, exec_module, tmp_state_dir):
        exec_module._save_state("s1", {"sandbox_id": "x"})
        exec_module._clear_state("s1")
        assert exec_module._load_state("s1") == {}


# ── get_sandbox: state-driven reuse ──────────────────────────────────


class TestGetSandboxReuse:
    def test_reuses_cached_sandbox(self, exec_module, tmp_state_dir):
        """When a sandbox_id is cached, get_sandbox reconnects instead
        of creating a new one."""
        exec_module._save_state("sess1", {"sandbox_id": "sb-existing", "mount": None})

        mock_sb = MagicMock()
        exec_module.Sandbox.connect.return_value = mock_sb

        sb = exec_module.get_sandbox("sess1", mount=None)
        assert sb is mock_sb
        exec_module.Sandbox.connect.assert_called_once()
        exec_module.Sandbox.create.assert_not_called()

    def test_creates_new_when_cache_missing(self, exec_module, tmp_state_dir):
        mock_sb = MagicMock()
        mock_sb.sandbox_id = "sb-new"
        exec_module.Sandbox.connect.side_effect = exec_module.SandboxNotFoundError()
        exec_module.Sandbox.create.return_value = mock_sb

        sb = exec_module.get_sandbox("fresh-session", mount=None)
        assert sb is mock_sb
        exec_module.Sandbox.create.assert_called_once()
        saved = exec_module._load_state("fresh-session")
        assert saved["sandbox_id"] == "sb-new"
        assert saved["state_token"]

    def test_recreates_when_sandbox_expired(self, exec_module, tmp_state_dir):
        """If the cached sandbox is gone (SandboxNotFoundError), a new
        one is created and the state is updated."""
        exec_module._save_state("sess1", {"sandbox_id": "sb-dead", "mount": None})

        mock_sb = MagicMock()
        mock_sb.sandbox_id = "sb-reborn"
        exec_module.Sandbox.connect.side_effect = exec_module.SandboxNotFoundError()
        exec_module.Sandbox.create.return_value = mock_sb

        sb = exec_module.get_sandbox("sess1", mount=None)
        assert sb is mock_sb
        saved = exec_module._load_state("sess1")
        assert saved["sandbox_id"] == "sb-reborn"
        assert saved["state_token"]
