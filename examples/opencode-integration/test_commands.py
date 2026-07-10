# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import io
import shlex
import unittest
from contextlib import redirect_stderr
from types import SimpleNamespace

from _opencode_common import (
    CommandExecutionError,
    ensure_success,
    extract_session_id,
    opencode_command,
    run_command,
    safe_kill,
    sandbox_identifier,
)


class RecordingCommands:
    def __init__(self, result: object | None = None) -> None:
        self.result = result or SimpleNamespace(stdout="", stderr="", exit_code=0)
        self.calls: list[tuple[str, dict[str, object]]] = []

    def run(self, command: str, **kwargs: object) -> object:
        self.calls.append((command, kwargs))
        return self.result


class EnvAliasCommands(RecordingCommands):
    def run(self, command: str, **kwargs: object) -> object:
        self.calls.append((command, kwargs.copy()))
        if "envs" in kwargs:
            raise TypeError("unexpected keyword argument 'envs'")
        return self.result


class FakeSandbox:
    def __init__(self, commands: RecordingCommands | None = None) -> None:
        self.commands = commands or RecordingCommands()
        self.sandbox_id = "sb-test"
        self.kill_calls = 0

    def kill(self) -> None:
        self.kill_calls += 1


class FailingKillSandbox(FakeSandbox):
    def kill(self) -> None:
        raise RuntimeError("cleanup failed")


class CommandHelpersTest(unittest.TestCase):
    def test_opencode_command_quotes_every_dynamic_argument(self) -> None:
        command = opencode_command(
            "fix user's file; echo unsafe",
            model="anthropic/test-model",
            workspace="/work dir",
            title="quoted title",
            session_id="ses_123",
        )

        self.assertEqual(
            shlex.split(command),
            [
                "opencode",
                "run",
                "--model",
                "anthropic/test-model",
                "--dir",
                "/work dir",
                "--title",
                "quoted title",
                "--session",
                "ses_123",
                "--auto",
                "--format",
                "json",
                "fix user's file; echo unsafe",
            ],
        )

    def test_opencode_command_supports_continue_and_optional_flags(self) -> None:
        command = opencode_command(
            "continue",
            model="openai/model",
            continue_last=True,
            auto=False,
            json_format=False,
        )

        args = shlex.split(command)
        self.assertIn("--continue", args)
        self.assertNotIn("--auto", args)
        self.assertNotIn("--format", args)

    def test_opencode_command_rejects_invalid_combinations(self) -> None:
        with self.assertRaisesRegex(ValueError, "mutually exclusive"):
            opencode_command(
                "prompt",
                model="openai/model",
                session_id="ses_1",
                continue_last=True,
            )
        with self.assertRaisesRegex(ValueError, "prompt must not be empty"):
            opencode_command(" ", model="openai/model")
        with self.assertRaisesRegex(ValueError, "provider/model"):
            opencode_command("prompt", model="invalid")

    def test_run_command_forwards_sdk_arguments(self) -> None:
        commands = RecordingCommands()
        sandbox = FakeSandbox(commands)

        result = run_command(
            sandbox,
            "echo ok",
            cwd="/workspace",
            envs={"A": "B"},
            timeout=12.5,
        )

        self.assertIs(result, commands.result)
        self.assertEqual(
            commands.calls,
            [
                (
                    "echo ok",
                    {
                        "user": "root",
                        "cwd": "/workspace",
                        "timeout": 12.5,
                        "envs": {"A": "B"},
                    },
                )
            ],
        )

    def test_run_command_retries_only_the_env_alias(self) -> None:
        commands = EnvAliasCommands()

        run_command(FakeSandbox(commands), "echo ok", envs={"A": "B"})

        self.assertEqual(len(commands.calls), 2)
        self.assertIn("envs", commands.calls[0][1])
        self.assertEqual(commands.calls[1][1]["env"], {"A": "B"})

    def test_ensure_success_accepts_zero_and_redacts_failures(self) -> None:
        ensure_success(SimpleNamespace(stdout="ok", stderr="", exit_code=0), "run")

        result = SimpleNamespace(
            stdout="response secret-value",
            stderr="error secret-value",
            exit_code=7,
        )
        with self.assertRaises(CommandExecutionError) as raised:
            ensure_success(result, "run OpenCode", secrets=("secret-value",))

        self.assertNotIn("secret-value", str(raised.exception))
        self.assertIn("<redacted>", str(raised.exception))
        self.assertIn("exit 7", str(raised.exception))

    def test_extract_session_id_handles_events_lists_and_titles(self) -> None:
        jsonl = 'not-json\n{"type":"message","sessionID":"ses_event"}\n'
        listing = '[{"id":"ses_old","title":"old"},{"id":"ses_new","title":"wanted"}]'

        self.assertEqual(extract_session_id(jsonl), "ses_event")
        self.assertEqual(
            extract_session_id(listing, title="wanted"),
            "ses_new",
        )

    def test_extract_session_id_rejects_missing_or_wrong_title(self) -> None:
        with self.assertRaisesRegex(ValueError, "did not contain"):
            extract_session_id("no json here")
        with self.assertRaisesRegex(ValueError, "No OpenCode session"):
            extract_session_id('[{"id":"ses_1","title":"other"}]', title="wanted")

    def test_safe_kill_reports_cleanup_failure_without_raising(self) -> None:
        healthy = FakeSandbox()
        self.assertIsNone(safe_kill(healthy))
        self.assertEqual(healthy.kill_calls, 1)

        stderr = io.StringIO()
        with redirect_stderr(stderr):
            error = safe_kill(FailingKillSandbox(), label="source")

        self.assertIsInstance(error, RuntimeError)
        self.assertIn("cleanup failed", stderr.getvalue())
        self.assertIn("source sb-test", stderr.getvalue())

    def test_sandbox_identifier_supports_both_sdk_attribute_names(self) -> None:
        self.assertEqual(sandbox_identifier(SimpleNamespace(sandbox_id="native")), "native")
        self.assertEqual(sandbox_identifier(SimpleNamespace(id="e2b")), "e2b")


if __name__ == "__main__":
    unittest.main()
