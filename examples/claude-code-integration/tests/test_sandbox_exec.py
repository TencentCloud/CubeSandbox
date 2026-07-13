"""Tests for sandbox_exec.py — session-based cross-process sandbox reuse."""

import os
from unittest.mock import Mock, patch

import pytest

import sandbox_exec


@pytest.fixture
def temp_session(monkeypatch, tmp_path):
    """Use a temporary session file for test isolation."""
    session_file = tmp_path / "session"
    monkeypatch.setattr(sandbox_exec, "SESSION_FILE", session_file)
    monkeypatch.setattr(sandbox_exec, "_sandbox", None)
    return session_file


# ── _get_sandbox ───────────────────────────────────────────────────────


class TestGetSandbox:
    """Tests for _get_sandbox() — cross-process session reuse via file."""

    def test_creates_new_sandbox_when_no_session(self, temp_session):
        mock_instance = Mock()
        mock_instance.sandbox_id = "sb-new"
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.create.return_value = mock_instance
            result = sandbox_exec._get_sandbox()
        assert result is mock_instance
        mock_cls.create.assert_called_once()
        assert temp_session.exists()
        assert temp_session.read_text() == "sb-new"

    def test_reconnects_from_session_file(self, temp_session):
        """Fix: Reuses sandbox from a previous --keep-alive run by reading
        the session file and calling Sandbox.connect() instead of create()."""
        temp_session.write_text("sb-existing")
        mock_instance = Mock()
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.connect.return_value = mock_instance
            result = sandbox_exec._get_sandbox(timeout=300)
        assert result is mock_instance
        mock_cls.connect.assert_called_once_with("sb-existing")
        mock_cls.create.assert_not_called()
        mock_instance.set_timeout.assert_called_once_with(300)

    def test_clears_stale_session_on_connect_failure(self, temp_session):
        """Fix: When reconnect fails (sandbox expired), the stale session
        file is cleaned up and a fresh sandbox is created."""
        temp_session.write_text("sb-stale")
        new_instance = Mock()
        new_instance.sandbox_id = "sb-fresh"
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.connect.side_effect = Exception("not found")
            mock_cls.create.return_value = new_instance
            result = sandbox_exec._get_sandbox()
        assert result is new_instance
        assert temp_session.read_text() == "sb-fresh"

    def test_reuses_in_process_cache(self, temp_session):
        mock_instance = Mock()
        sandbox_exec._sandbox = mock_instance
        result = sandbox_exec._get_sandbox()
        assert result is mock_instance

    def test_writes_session_file_on_create(self, temp_session):
        mock_instance = Mock()
        mock_instance.sandbox_id = "sb-123"
        with patch("sandbox_exec.Sandbox") as mock_cls:
            mock_cls.create.return_value = mock_instance
            sandbox_exec._get_sandbox()
        assert temp_session.read_text() == "sb-123"

    @pytest.mark.skipif(not hasattr(os, "O_NOFOLLOW"), reason="O_NOFOLLOW unavailable")
    def test_session_helpers_reject_symlink(self, temp_session, tmp_path):
        target = tmp_path / "attacker-session"
        target.write_text("sb-attacker")
        temp_session.symlink_to(target)

        assert sandbox_exec._read_session() is None
        with pytest.raises(OSError):
            sandbox_exec._write_session("sb-safe")
        assert target.read_text() == "sb-attacker"


# ── cleanup ────────────────────────────────────────────────────────────


class TestCleanup:
    """Tests for cleanup() — sandbox destruction and session clearing."""

    def test_kills_sandbox_and_clears_session(self, temp_session):
        mock_instance = Mock()
        sandbox_exec._sandbox = mock_instance
        temp_session.write_text("sb-1")
        sandbox_exec.cleanup()
        mock_instance.kill.assert_called_once()
        assert sandbox_exec._sandbox is None
        assert not temp_session.exists()

    def test_no_error_when_no_sandbox(self, temp_session):
        sandbox_exec._sandbox = None
        temp_session.write_text("sb-1")
        sandbox_exec.cleanup()
        assert not temp_session.exists()

    def test_no_error_when_kill_fails(self, temp_session):
        """Fix: cleanup catches kill exceptions so a dead sandbox doesn't
        mask the original error or leave the session file behind."""
        mock_instance = Mock()
        mock_instance.kill.side_effect = Exception("already dead")
        sandbox_exec._sandbox = mock_instance
        temp_session.write_text("sb-1")
        sandbox_exec.cleanup()
        assert sandbox_exec._sandbox is None
        assert not temp_session.exists()

    def test_clears_session_even_without_sandbox(self, temp_session):
        temp_session.write_text("sb-orphan")
        sandbox_exec._sandbox = None
        sandbox_exec.cleanup()
        assert not temp_session.exists()
