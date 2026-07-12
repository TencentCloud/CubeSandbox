"""Focused tests for the CubeSandbox Claude Code rewrite hook."""

from __future__ import annotations

import importlib
import io
import json
import shlex
import subprocess
import sys
from pathlib import Path

import pytest


HOOKS_DIR = Path(__file__).resolve().parents[1] / "hooks"
sys.path.insert(0, str(HOOKS_DIR))
hook = importlib.import_module("cubesandbox_rewrite")


def _updated_input(payload):
    response = hook.rewrite_payload(payload)
    assert response is not None
    return response["hookSpecificOutput"]["updatedInput"]


@pytest.mark.parametrize(
    "command",
    [
        "printf ok; touch host-sentinel",
        "true && touch host-sentinel",
        "printf ok | tee host-sentinel",
        "printf 'line one\nline two'\nprintf done",
        "cd ~ && printf home",
        "python cubesandbox_exec.py --session other -- 'echo nested'",
    ],
)
def test_host_control_operators_remain_one_quoted_argument(
    command, monkeypatch, tmp_path
):
    capture = tmp_path / "capture arguments.py"
    capture.write_text(
        "import json, sys\nprint(json.dumps(sys.argv[1:]))\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(hook, "EXEC_SCRIPT", capture)

    payload = {
        "session_id": "session; touch host-sentinel",
        "cwd": "cwd && touch host-sentinel",
        "tool_name": "Bash",
        "tool_input": {"command": command, "timeout": 1500},
    }
    rewritten = _updated_input(payload)["command"]
    completed = subprocess.run(
        rewritten,
        cwd=tmp_path,
        shell=True,
        check=True,
        capture_output=True,
        text=True,
    )

    assert json.loads(completed.stdout) == [
        "--session=session; touch host-sentinel",
        "--mount=cwd && touch host-sentinel",
        "--timeout=1.500",
        "--",
        command,
    ]
    assert not (tmp_path / "host-sentinel").exists()


def test_rewrite_uses_current_python_and_sibling_executor():
    command = "echo 'quoted payload' && printf done"
    updated = _updated_input(
        {
            "session_id": "abc",
            "cwd": "/project with spaces",
            "tool_name": "Bash",
            "tool_input": {"command": command, "description": "keep me"},
        }
    )
    argv = shlex.split(updated["command"])

    assert argv == [
        sys.executable,
        str(hook.EXEC_SCRIPT),
        "--session=abc",
        "--mount=/project with spaces",
        "--",
        command,
    ]
    assert updated["description"] == "keep me"


def test_all_tool_input_fields_are_preserved():
    tool_input = {
        "command": "echo ok",
        "timeout": 2000,
        "description": "status",
        "run_in_background": True,
        "custom": {"nested": 1},
    }
    updated = _updated_input(
        {"session_id": "s", "tool_name": "Bash", "tool_input": tool_input}
    )

    assert updated.keys() == tool_input.keys()
    assert {key: updated[key] for key in tool_input if key != "command"} == {
        key: tool_input[key] for key in tool_input if key != "command"
    }


@pytest.mark.parametrize(
    "timeout", [0, -1, True, "1000", float("nan"), float("inf"), 10**1000]
)
def test_only_positive_finite_numeric_timeout_is_forwarded(timeout):
    argv = shlex.split(
        _updated_input(
            {
                "tool_name": "Bash",
                "tool_input": {"command": "echo ok", "timeout": timeout},
            }
        )["command"]
    )
    assert not any(argument.startswith("--timeout=") for argument in argv)


def test_empty_command_is_still_rewritten():
    argv = shlex.split(
        _updated_input({"tool_name": "Bash", "tool_input": {"command": ""}})["command"]
    )
    assert argv[-2:] == ["--", ""]


@pytest.mark.parametrize(
    "payload",
    [
        [],
        {},
        {"tool_name": "Bash", "tool_input": []},
        {"tool_name": "Bash", "tool_input": {"command": 7}},
    ],
)
def test_invalid_payload_is_rejected(payload):
    with pytest.raises(hook.HookInputError):
        hook.rewrite_payload(payload)


def test_proven_non_bash_payload_is_ignored():
    payload = {"tool_name": "Read", "tool_input": {"command": "cat file"}}

    assert hook.rewrite_payload(payload) is None


@pytest.mark.parametrize(
    "raw_payload",
    [
        "{not-json",
        "[]",
        json.dumps({"tool_name": "Bash", "tool_input": []}),
        json.dumps({"tool_name": "Bash", "tool_input": {"command": 7}}),
    ],
)
def test_main_fails_closed_for_malformed_or_invalid_bash_input(
    raw_payload, monkeypatch, capsys
):
    monkeypatch.setattr(hook.sys, "stdin", io.StringIO(raw_payload))

    assert hook.main() == 2
    captured = capsys.readouterr()
    assert captured.out == ""
    assert "[cubesandbox-rewrite] error:" in captured.err


def test_main_fails_closed_for_unexpected_errors(monkeypatch, capsys):
    monkeypatch.setattr(
        hook.sys,
        "stdin",
        io.StringIO(
            json.dumps({"tool_name": "Bash", "tool_input": {"command": "true"}})
        ),
    )

    def fail_rewrite(payload):
        raise RuntimeError("unexpected failure")

    monkeypatch.setattr(hook, "rewrite_payload", fail_rewrite)

    assert hook.main() == 2
    captured = capsys.readouterr()
    assert captured.out == ""
    assert "unexpected hook failure" in captured.err


def test_main_allows_only_proven_non_bash_events(monkeypatch, capsys):
    payload = {"tool_name": "Read", "tool_input": {"command": "cat file"}}
    monkeypatch.setattr(hook.sys, "stdin", io.StringIO(json.dumps(payload)))

    assert hook.main() == 0
    captured = capsys.readouterr()
    assert captured.out == ""
    assert captured.err == ""


def test_invalid_session_and_cwd_types_are_not_quoted():
    argv = shlex.split(
        _updated_input(
            {
                "session_id": {"bad": "type"},
                "cwd": ["bad"],
                "tool_name": "Bash",
                "tool_input": {"command": "echo ok"},
            }
        )["command"]
    )
    assert "--session=default" in argv
    assert not any(argument.startswith("--mount=") for argument in argv)
