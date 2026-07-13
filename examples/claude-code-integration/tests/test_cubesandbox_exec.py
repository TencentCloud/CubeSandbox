"""Focused tests for the cached CubeSandbox hook executor."""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import importlib
import json
import os
import shutil
import stat
import subprocess
import sys
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from dotenv import dotenv_values


HOOKS_DIR = Path(__file__).resolve().parents[1] / "hooks"
sys.path.insert(0, str(HOOKS_DIR))


@pytest.fixture
def executor(monkeypatch, tmp_path):
    module = importlib.import_module("cubesandbox_exec")
    module = importlib.reload(module)
    module.STATE_DIR = tmp_path / "state"
    module.TEMPLATE_ID = "tpl-test"
    module.SANDBOX_TTL = 777
    module.SANDBOX_USER = "dev"
    module.ApiError = type("ApiError", (Exception,), {})
    module.CubeSandboxError = type("CubeSandboxError", (Exception,), {})
    module.SandboxNotFoundError = type(
        "SandboxNotFoundError", (module.CubeSandboxError,), {}
    )
    module.Config = MagicMock(name="Config")
    module.Sandbox = MagicMock(name="Sandbox")
    return module


def test_state_key_is_hashed_and_files_are_private(executor):
    session_id = "../../user/session"
    executor._save_state(session_id, {"sandbox_id": "sb-one"})
    state_path = executor._state_path(session_id)

    assert state_path.name == hashlib.sha256(session_id.encode()).hexdigest() + ".json"
    assert session_id not in state_path.name
    assert stat.S_IMODE(executor.STATE_DIR.stat().st_mode) == 0o700
    assert stat.S_IMODE(state_path.stat().st_mode) == 0o600
    assert executor._load_state(session_id) == {"sandbox_id": "sb-one"}


def test_state_write_is_atomic(executor, monkeypatch):
    replacements = []
    real_replace = os.replace

    def record_replace(source, destination):
        replacements.append((Path(source), Path(destination)))
        real_replace(source, destination)

    monkeypatch.setattr(executor.os, "replace", record_replace)
    executor._save_state("session", {"sandbox_id": "sb-atomic"})

    assert len(replacements) == 1
    assert replacements[0][1] == executor._state_path("session")
    assert list(executor.STATE_DIR.glob("*.tmp")) == []


@pytest.mark.skipif(not hasattr(os, "O_NOFOLLOW"), reason="O_NOFOLLOW unavailable")
def test_state_read_rejects_symlink(executor, tmp_path):
    executor._ensure_state_dir()
    target = tmp_path / "attacker-state.json"
    target.write_text('{"sandbox_id":"attacker"}', encoding="utf-8")
    executor._state_path("session").symlink_to(target)

    assert executor._load_state("session") == {}


def test_cached_sandbox_connect_refreshes_ttl(executor):
    executor._save_state(
        "session",
        {"sandbox_id": "sb-existing", "state_token": "token"},
    )
    connected = MagicMock()
    executor.Sandbox.connect.return_value = connected
    config = object()
    executor.Config.return_value = config

    assert executor.get_sandbox("session", None) is connected
    executor.Config.assert_called_once_with(timeout=777)
    executor.Sandbox.connect.assert_called_once_with("sb-existing", config=config)
    assert not hasattr(connected, "set_timeout") or not connected.set_timeout.called
    executor.Sandbox.create.assert_not_called()


def test_missing_cache_creates_sandbox_and_private_lock(executor):
    created = MagicMock(sandbox_id="sb-new")
    executor.Sandbox.create.return_value = created

    assert executor.get_sandbox("new-session", None) is created
    executor.Sandbox.create.assert_called_once_with("tpl-test", timeout=777)
    state = executor._load_state("new-session")
    assert state["sandbox_id"] == "sb-new"
    assert state["state_token"]
    assert stat.S_IMODE(executor._lock_path("new-session").stat().st_mode) == 0o600


def test_expired_cache_is_recreated(executor):
    executor._save_state("session", {"sandbox_id": "sb-expired"})
    executor.Sandbox.connect.side_effect = executor.SandboxNotFoundError("gone")
    created = MagicMock(sandbox_id="sb-fresh")
    executor.Sandbox.create.return_value = created

    assert executor.get_sandbox("session", None) is created
    assert executor._load_state("session")["sandbox_id"] == "sb-fresh"


def test_unexpected_reconnect_error_falls_back(executor, capsys):
    executor._save_state("session", {"sandbox_id": "sb-cached"})
    executor.Sandbox.connect.side_effect = ConnectionError("network down")

    assert executor._try_cached_sandbox("session") is None
    assert "creating a new sandbox" in capsys.readouterr().err


def test_mount_is_read_only_and_api_rejection_falls_back(executor, capsys):
    created = MagicMock(sandbox_id="sb-fallback")
    executor.Sandbox.create.side_effect = [executor.ApiError("denied"), created]
    mount = "/project with 'quotes'"

    assert executor._create_sandbox(mount) is created
    first_call = executor.Sandbox.create.call_args_list[0]
    metadata = json.loads(first_call.kwargs["metadata"]["host-mount"])
    assert metadata == [{"hostPath": mount, "mountPath": mount, "readOnly": True}]
    assert executor.Sandbox.create.call_args_list[1].args == ("tpl-test",)
    assert executor.Sandbox.create.call_args_list[1].kwargs == {"timeout": 777}
    assert "without the mount" in capsys.readouterr().err


def test_run_propagates_output_exit_and_timeout(executor, monkeypatch, capsys):
    executor._save_state(
        "session",
        {"sandbox_id": "sb", "state_token": "safe_token-123"},
    )
    sandbox = MagicMock()
    sandbox.commands.run.return_value = SimpleNamespace(
        stdout="standard output\n",
        stderr="standard error\n",
        exit_code=23,
    )
    monkeypatch.setattr(
        executor, "_get_sandbox_locked", MagicMock(return_value=sandbox)
    )

    assert executor.run("printf ok", "session", 3.5, "/project") == 23
    captured = capsys.readouterr()
    assert captured.out == "standard output\n"
    assert captured.err == "standard error\n"
    call = sandbox.commands.run.call_args
    assert call.kwargs == {"timeout": 3.5, "user": "dev"}
    assert "eval -- 'printf ok'" in call.args[0]


def test_parallel_runs_for_one_session_are_serialized(executor, monkeypatch):
    executor._save_state(
        "session",
        {"sandbox_id": "sb", "state_token": "safe-token"},
    )
    sandbox = MagicMock()
    monkeypatch.setattr(
        executor, "_get_sandbox_locked", MagicMock(return_value=sandbox)
    )

    session_mutex = threading.Lock()
    attempt_mutex = threading.Lock()
    active_mutex = threading.Lock()
    lock_state = threading.local()
    second_lock_attempted = threading.Event()
    first_command_started = threading.Event()
    release_first_command = threading.Event()
    attempts = 0
    active_commands = 0
    max_active_commands = 0

    @contextlib.contextmanager
    def observed_session_lock(session_id):
        nonlocal attempts
        assert session_id == "session"
        with attempt_mutex:
            attempts += 1
            if attempts == 2:
                second_lock_attempted.set()
        with session_mutex:
            lock_state.held = True
            try:
                yield
            finally:
                lock_state.held = False

    def command_run(*args, **kwargs):
        nonlocal active_commands, max_active_commands
        assert lock_state.held
        with active_mutex:
            active_commands += 1
            max_active_commands = max(max_active_commands, active_commands)
            call_number = sandbox.commands.run.call_count
        try:
            if call_number == 1:
                first_command_started.set()
                assert release_first_command.wait(timeout=5)
            return SimpleNamespace(stdout="", stderr="", exit_code=0)
        finally:
            with active_mutex:
                active_commands -= 1

    monkeypatch.setattr(executor, "_session_lock", observed_session_lock)
    sandbox.commands.run.side_effect = command_run

    with ThreadPoolExecutor(max_workers=2) as pool:
        first = pool.submit(executor.run, "first", "session", 3.5, None)
        second = pool.submit(executor.run, "second", "session", 3.5, None)
        try:
            assert first_command_started.wait(timeout=5)
            assert second_lock_attempted.wait(timeout=5)
            assert sandbox.commands.run.call_count == 1
        finally:
            release_first_command.set()
        assert first.result(timeout=5) == 0
        assert second.result(timeout=5) == 0

    assert sandbox.commands.run.call_count == 2
    assert max_active_commands == 1


def test_state_shell_persists_cwd_and_exports_and_quotes_mount(executor, tmp_path):
    mount = tmp_path / "mount'; touch host-sentinel; echo '"
    mount.mkdir()
    state_token = "local-state-token"
    state_dir = Path("/tmp") / (".cubesandbox-state-" + state_token)
    shutil.rmtree(state_dir, ignore_errors=True)
    try:
        first = executor._state_shell(
            "mkdir child; cd child; export HOOK_MARKER='value with spaces'",
            state_token,
            str(mount),
        )
        subprocess.run(["bash", "-c", first], cwd=tmp_path, check=True)

        second = executor._state_shell(
            'printf \'%s|%s\' "$PWD" "$HOOK_MARKER"',
            state_token,
            str(mount),
        )
        completed = subprocess.run(
            ["bash", "-c", second],
            cwd=tmp_path,
            check=True,
            capture_output=True,
            text=True,
        )
        assert completed.stdout == str(mount / "child") + "|value with spaces"
        assert not (tmp_path / "host-sentinel").exists()
    finally:
        shutil.rmtree(state_dir, ignore_errors=True)


def test_reset_kills_cached_sandbox_and_clears_state(executor, monkeypatch):
    executor._save_state("session", {"sandbox_id": "sb-reset"})
    sandbox = MagicMock()
    executor.Sandbox.connect.return_value = sandbox
    lock_held = False

    @contextlib.contextmanager
    def observed_session_lock(session_id):
        nonlocal lock_held
        assert session_id == "session"
        lock_held = True
        try:
            yield
        finally:
            lock_held = False

    def assert_locked_clear(session_id):
        assert lock_held
        executor._state_path(session_id).unlink()

    def assert_locked_kill():
        assert lock_held

    sandbox.kill.side_effect = assert_locked_kill
    monkeypatch.setattr(executor, "_session_lock", observed_session_lock)
    monkeypatch.setattr(executor, "_clear_state", assert_locked_clear)

    executor.reset("session")

    sandbox.kill.assert_called_once_with()
    assert executor._load_state("session") == {}


def test_config_loader_whitelists_and_preserves_exported_values(
    executor, monkeypatch, tmp_path
):
    integration = tmp_path / "claude-code-integration"
    hooks = integration / "hooks"
    hooks.mkdir(parents=True)
    (hooks / "cubesandbox.env").write_text(
        "CUBE_API_URL=http://owned\n"
        "CUBE_TEMPLATE_ID=tpl-owned\n"
        "ANTHROPIC_AUTH_TOKEN=owned-secret\n",
        encoding="utf-8",
    )
    (integration / ".env").write_text(
        "CUBE_TEMPLATE_ID=tpl-parent\n"
        "CUBE_SANDBOX_TIMEOUT=900\n"
        "OPENAI_API_KEY=parent-secret\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(executor, "SCRIPT_DIR", hooks)
    for key in executor.CONFIG_KEYS:
        monkeypatch.delenv(key, raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    monkeypatch.setenv("CUBE_API_URL", "http://exported")

    executor._load_config_values(dotenv_values)

    assert os.environ["CUBE_API_URL"] == "http://exported"
    assert os.environ["CUBE_TEMPLATE_ID"] == "tpl-owned"
    assert os.environ["CUBE_SANDBOX_TIMEOUT"] == "900"
    assert "ANTHROPIC_AUTH_TOKEN" not in os.environ
    assert "OPENAI_API_KEY" not in os.environ


@pytest.mark.parametrize("value", ["0", "-1", "nan", "inf", "not-a-number"])
def test_executor_timeout_must_be_finite_and_positive(executor, value):
    with pytest.raises(argparse.ArgumentTypeError):
        executor._positive_float(value)


def test_invalid_default_timeout_is_reported_as_bootstrap_error(executor, monkeypatch):
    monkeypatch.setenv("CUBE_EXEC_TIMEOUT", "nan")

    with pytest.raises(executor.BootstrapError, match="CUBE_EXEC_TIMEOUT"):
        executor._default_exec_timeout()
