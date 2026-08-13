"""Installer tests for the CubeSandbox Claude Code hook."""

from __future__ import annotations

import json
import os
import shlex
import shutil
import stat
import subprocess
import sys
from pathlib import Path

import pytest
from dotenv import dotenv_values


SOURCE_HOOKS = Path(__file__).resolve().parents[1] / "hooks"


@pytest.fixture
def installer_tree(tmp_path):
    integration = tmp_path / "source integration"
    hooks = integration / "hooks"
    shutil.copytree(SOURCE_HOOKS, hooks)
    return integration, hooks


def _run_installer(installer, claude_dir, home, *arguments, check=True):
    env = os.environ.copy()
    env["CLAUDE_DIR"] = str(claude_dir)
    env["HOME"] = str(home)
    env["PATH"] = str(Path(sys.executable).parent) + os.pathsep + env["PATH"]
    return subprocess.run(
        ["bash", str(installer), *arguments],
        check=check,
        capture_output=True,
        text=True,
        env=env,
    )


def test_install_idempotence_uninstall_and_config_whitelist(installer_tree, tmp_path):
    integration, source_hooks = installer_tree
    (integration / ".env").write_text(
        "CUBE_API_URL=http://cube.example\n"
        "CUBE_TEMPLATE_ID=tpl-safe\n"
        "CUBE_SANDBOX_USER=dev\n"
        "CUBE_SANDBOX_TIMEOUT=800\n"
        "ANTHROPIC_AUTH_TOKEN=llm-secret\n"
        "OPENAI_API_KEY=openai-secret\n"
        "UNRELATED=value\n",
        encoding="utf-8",
    )
    home = tmp_path / "home"
    claude_dir = home / "Claude Config"
    hooks_dir = claude_dir / "hooks"
    hooks_dir.mkdir(parents=True)
    unrelated_env = hooks_dir / ".env"
    unrelated_env.write_text("KEEP_ME=yes\n", encoding="utf-8")
    unrelated_hook = hooks_dir / "other_hook.py"
    unrelated_hook.write_text("# keep\n", encoding="utf-8")
    settings_path = claude_dir / "settings.json"
    original_settings = {
        "theme": "dark",
        "hooks": {
            "PreToolUse": [
                {
                    "matcher": "Bash",
                    "hooks": [{"type": "command", "command": "/existing/hook"}],
                },
                {
                    "matcher": "Read",
                    "hooks": [{"type": "command", "command": "/read/hook"}],
                },
            ],
            "PostToolUse": [{"matcher": "*", "hooks": []}],
        },
        "custom": {"preserved": True},
    }
    settings_path.write_text(json.dumps(original_settings), encoding="utf-8")
    settings_path.chmod(0o640)
    installer = source_hooks / "install.sh"

    _run_installer(installer, claude_dir, home)
    _run_installer(installer, claude_dir, home)

    installed = json.loads(settings_path.read_text(encoding="utf-8"))
    bash_hooks = installed["hooks"]["PreToolUse"][0]["hooks"]
    installed_rewrite = hooks_dir / "cubesandbox_rewrite.py"
    expected_command = f"{shlex.quote(str(installed_rewrite))} || exit 2"
    assert sum(item.get("command") == expected_command for item in bash_hooks) == 1
    assert installed["theme"] == "dark"
    assert installed["custom"] == {"preserved": True}
    assert (
        installed["hooks"]["PostToolUse"] == original_settings["hooks"]["PostToolUse"]
    )
    assert stat.S_IMODE(settings_path.stat().st_mode) == 0o640

    installed_exec = hooks_dir / "cubesandbox_exec.py"
    installed_config = hooks_dir / "cubesandbox.env"
    assert installed_rewrite.is_file()
    assert installed_exec.is_file()
    assert stat.S_IMODE(installed_rewrite.stat().st_mode) == 0o755
    assert stat.S_IMODE(installed_exec.stat().st_mode) == 0o755
    assert stat.S_IMODE(installed_config.stat().st_mode) == 0o600
    config = dotenv_values(installed_config)
    assert config == {
        "CUBE_API_URL": "http://cube.example",
        "CUBE_TEMPLATE_ID": "tpl-safe",
        "CUBE_SANDBOX_USER": "dev",
        "CUBE_SANDBOX_TIMEOUT": "800",
    }
    assert "ANTHROPIC_AUTH_TOKEN" not in installed_config.read_text(encoding="utf-8")
    assert "OPENAI_API_KEY" not in installed_config.read_text(encoding="utf-8")
    assert not (home / ".local" / "bin" / "cubesandbox-exec").exists()

    installed_rewrite.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
    installed_rewrite.chmod(0o755)
    failed_hook = subprocess.run(expected_command, shell=True, check=False)
    assert failed_hook.returncode == 2

    _run_installer(installer, claude_dir, home, "--uninstall")
    _run_installer(installer, claude_dir, home, "--uninstall")

    assert json.loads(settings_path.read_text(encoding="utf-8")) == original_settings
    assert stat.S_IMODE(settings_path.stat().st_mode) == 0o640
    assert not installed_rewrite.exists()
    assert not installed_exec.exists()
    assert not installed_config.exists()
    assert unrelated_env.read_text(encoding="utf-8") == "KEEP_ME=yes\n"
    assert unrelated_hook.is_file()


@pytest.mark.parametrize(
    ("settings", "message"),
    [
        ({"hooks": []}, "hooks must be a JSON object"),
        ({"hooks": {"PreToolUse": {}}}, "hooks.PreToolUse must be a JSON array"),
    ],
)
def test_invalid_settings_schema_fails_clearly(
    installer_tree, tmp_path, settings, message
):
    _, source_hooks = installer_tree
    home = tmp_path / "home"
    claude_dir = home / ".claude"
    claude_dir.mkdir(parents=True)
    (claude_dir / "settings.json").write_text(json.dumps(settings), encoding="utf-8")

    completed = _run_installer(
        source_hooks / "install.sh",
        claude_dir,
        home,
        check=False,
    )

    assert completed.returncode != 0
    assert message in completed.stderr


def test_malformed_settings_json_fails_clearly(installer_tree, tmp_path):
    _, source_hooks = installer_tree
    home = tmp_path / "home"
    claude_dir = home / ".claude"
    claude_dir.mkdir(parents=True)
    (claude_dir / "settings.json").write_text("{not-json", encoding="utf-8")

    completed = _run_installer(
        source_hooks / "install.sh", claude_dir, home, check=False
    )

    assert completed.returncode != 0
    assert "settings.json is not valid JSON" in completed.stderr
    assert "Traceback" not in completed.stderr


def test_reinstall_without_source_env_keeps_installed_config(installer_tree, tmp_path):
    integration, source_hooks = installer_tree
    (integration / ".env").write_text(
        "CUBE_API_URL=http://cube.example\nCUBE_TEMPLATE_ID=tpl-keep\n",
        encoding="utf-8",
    )
    home = tmp_path / "home"
    claude_dir = home / ".claude"
    installer = source_hooks / "install.sh"

    _run_installer(installer, claude_dir, home)
    installed_config = claude_dir / "hooks" / "cubesandbox.env"
    assert dotenv_values(installed_config)["CUBE_TEMPLATE_ID"] == "tpl-keep"

    (integration / ".env").unlink()
    completed = _run_installer(installer, claude_dir, home)

    assert "keeping existing" in completed.stderr
    assert dotenv_values(installed_config)["CUBE_TEMPLATE_ID"] == "tpl-keep"


def test_stale_hook_path_is_replaced_on_install(installer_tree, tmp_path):
    """A hook entry recorded from a previous checkout location must be
    updated in place instead of leaving a stale blocking entry behind."""
    _, source_hooks = installer_tree
    home = tmp_path / "home"
    claude_dir = home / ".claude"
    claude_dir.mkdir(parents=True)
    settings_path = claude_dir / "settings.json"
    stale_command = "/old/checkout/hooks/cubesandbox_rewrite.py || exit 2"
    settings_path.write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "matcher": "Bash",
                            "hooks": [{"type": "command", "command": stale_command}],
                        }
                    ]
                }
            }
        ),
        encoding="utf-8",
    )

    _run_installer(source_hooks / "install.sh", claude_dir, home)

    installed = json.loads(settings_path.read_text(encoding="utf-8"))
    commands = [
        item["command"] for item in installed["hooks"]["PreToolUse"][0]["hooks"]
    ]
    expected = (
        f"{shlex.quote(str(claude_dir / 'hooks' / 'cubesandbox_rewrite.py'))} || exit 2"
    )
    assert commands == [expected]


def test_symlinked_settings_is_written_through(installer_tree, tmp_path):
    """Dotfile-managed (symlinked) settings.json must stay a symlink; the
    target receives the update."""
    _, source_hooks = installer_tree
    home = tmp_path / "home"
    claude_dir = home / ".claude"
    claude_dir.mkdir(parents=True)
    target = tmp_path / "dotfiles" / "settings.json"
    target.parent.mkdir(parents=True)
    target.write_text(json.dumps({"theme": "dark"}), encoding="utf-8")
    link = claude_dir / "settings.json"
    link.symlink_to(target)

    _run_installer(source_hooks / "install.sh", claude_dir, home)

    assert link.is_symlink()
    installed = json.loads(target.read_text(encoding="utf-8"))
    assert installed["theme"] == "dark"
    commands = [
        item["command"]
        for group in installed["hooks"]["PreToolUse"]
        for item in group["hooks"]
    ]
    assert any("cubesandbox_rewrite.py" in command for command in commands)
