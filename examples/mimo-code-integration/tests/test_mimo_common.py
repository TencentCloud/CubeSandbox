# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import contextlib
import io
import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

EXAMPLE_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE_DIR))

from _mimo_common import (  # noqa: E402
    JsonlCollector,
    ensure_success,
    events_contain_text,
    is_unexpected_keyword_error,
    kill_sandbox,
    parse_jsonl,
    run_command,
    run_mimo_command,
    session_id_from_events,
    session_list_contains,
)


def event(event_type: str, session_id: str = "ses_123", **extra) -> str:
    return json.dumps(
        {"type": event_type, "timestamp": 1, "sessionID": session_id, **extra}
    )


class MimoCommonTests(unittest.TestCase):
    def test_unexpected_keyword_error_matches_only_named_argument(self) -> None:
        self.assertTrue(
            is_unexpected_keyword_error(
                TypeError("got an unexpected keyword argument 'envs'"),
                "envs",
            )
        )
        self.assertFalse(
            is_unexpected_keyword_error(
                TypeError("an envs value caused an internal error"),
                "envs",
            )
        )

    def test_parse_jsonl_ignores_non_json_lines(self) -> None:
        text = f"warning\n{event('step_start')}\n{event('text')}\n"
        events = parse_jsonl(text)
        self.assertEqual([item["type"] for item in events], ["step_start", "text"])

    def test_parse_jsonl_respects_max_events(self) -> None:
        text = "\n".join(event("text", index=i) for i in range(10)) + "\n"
        events = parse_jsonl(text, max_events=3)
        self.assertEqual(len(events), 3)

    def test_collector_reassembles_arbitrary_chunks(self) -> None:
        collector = JsonlCollector()
        first = event("step_start")
        second = event("text", part={"type": "text", "text": "done"})
        payload = f"{first}\n{second}\n"

        collector(payload[:17])
        collector(SimpleNamespace(line=payload[17:43]))
        collector(payload[43:])
        collector.flush()

        self.assertEqual(len(collector.events), 2)
        self.assertEqual(collector.events[1]["part"]["text"], "done")

    def test_collector_flushes_final_non_newline_event(self) -> None:
        collector = JsonlCollector()
        collector(event("step_finish"))
        self.assertEqual(collector.events, [])
        collector.flush()
        self.assertEqual(collector.events[0]["type"], "step_finish")

    def test_collector_stops_after_max_events(self) -> None:
        collector = JsonlCollector(max_events=2)
        for index in range(5):
            collector(event("text", index=index) + "\n")
        collector.flush()
        self.assertEqual(len(collector.events), 2)
        self.assertTrue(collector.truncated)

    def test_collector_stops_after_max_bytes(self) -> None:
        collector = JsonlCollector(max_bytes=40)
        collector(event("text", part={"type": "text", "text": "a" * 200}) + "\n")
        collector(event("text", part={"type": "text", "text": "late"}) + "\n")
        collector.flush()
        self.assertTrue(collector.truncated)
        self.assertLessEqual(collector._bytes, 40)

    def test_session_id_requires_one_consistent_id(self) -> None:
        events = [{"sessionID": "ses_123"}, {"sessionID": "ses_123"}]
        self.assertEqual(session_id_from_events(events), "ses_123")

        with self.assertRaisesRegex(SystemExit, "multiple session IDs"):
            session_id_from_events(
                [{"sessionID": "ses_one"}, {"sessionID": "ses_two"}]
            )

    def test_session_id_rejects_missing_id(self) -> None:
        with self.assertRaisesRegex(SystemExit, "no sessionID"):
            session_id_from_events([{"type": "error"}])

    def test_events_contain_text_searches_nested_strings(self) -> None:
        events = [
            {
                "type": "text",
                "part": {"type": "text", "text": "CONTINUITY=token-abc"},
            }
        ]
        self.assertTrue(events_contain_text(events, "CONTINUITY=token-abc"))
        self.assertFalse(events_contain_text(events, "missing"))

    def test_session_list_supports_array_and_envelope(self) -> None:
        array = json.dumps([{"id": "ses_123"}])
        envelope = json.dumps({"sessions": [{"id": "ses_123"}]})
        self.assertTrue(session_list_contains(array, "ses_123"))
        self.assertTrue(session_list_contains(envelope, "ses_123"))
        self.assertFalse(session_list_contains(array, "ses_missing"))

    def test_ensure_success_reports_nonzero_exit(self) -> None:
        result = SimpleNamespace(exit_code=7, stdout="out", stderr="err")
        with self.assertRaisesRegex(
            SystemExit, "(?s)run test \\(exit 7\\).*STDOUT:\\nout\\nSTDERR:\\nerr"
        ):
            ensure_success(result, "run test")

    def test_run_command_retries_only_envs_signature_mismatch(self) -> None:
        class Commands:
            def __init__(self) -> None:
                self.calls = []

            def run(self, command, **kwargs):
                self.calls.append((command, kwargs))
                if "envs" in kwargs:
                    raise TypeError("unexpected keyword argument 'envs'")
                return SimpleNamespace(exit_code=0)

        commands = Commands()
        sandbox = SimpleNamespace(commands=commands)
        result = run_command(sandbox, "true", envs={"KEY": "value"})

        self.assertEqual(result.exit_code, 0)
        self.assertIn("envs", commands.calls[0][1])
        self.assertEqual(commands.calls[1][1]["env"], {"KEY": "value"})

    def test_run_command_does_not_mask_unrelated_type_error(self) -> None:
        class Commands:
            @staticmethod
            def run(command, **kwargs):
                raise TypeError("timeout must be numeric")

        sandbox = SimpleNamespace(commands=Commands())
        with self.assertRaisesRegex(TypeError, "timeout must be numeric"):
            run_command(sandbox, "true", envs={"KEY": "value"})

    def test_run_mimo_reads_bounded_event_file(self) -> None:
        payload = event("text", part={"type": "text", "text": "done"}) + "\n"
        commands: list[str] = []

        class Commands:
            @staticmethod
            def run(command, **kwargs):
                commands.append(command)
                if "head -c" in command:
                    return SimpleNamespace(exit_code=0, stdout=payload, stderr="")
                return SimpleNamespace(exit_code=0, stdout="", stderr="")

        result, events = run_mimo_command(
            SimpleNamespace(commands=Commands()),
            "mimo run",
            cwd="/workspace",
            envs={"MIMOCODE_HOME": "/root/.mimocode"},
            timeout=60,
            max_event_bytes=128,
            max_events=10,
        )

        self.assertEqual(result.exit_code, 0)
        self.assertEqual(events[0]["sessionID"], "ses_123")
        self.assertIn("mimo run", commands[0])
        self.assertIn(">", commands[0])
        self.assertIn("head -c 128", commands[1])

    def test_run_mimo_does_not_pass_streaming_callbacks(self) -> None:
        seen: list[dict] = []

        class Commands:
            @staticmethod
            def run(command, **kwargs):
                seen.append(kwargs)
                if "head -c" in command:
                    return SimpleNamespace(
                        exit_code=0,
                        stdout=event("step_finish") + "\n",
                        stderr="",
                    )
                return SimpleNamespace(exit_code=0, stdout="", stderr="")

        run_mimo_command(
            SimpleNamespace(commands=Commands()),
            "mimo run",
            cwd="/workspace",
            envs={"MIMOCODE_HOME": "/root/.mimocode"},
            timeout=60,
        )
        self.assertEqual(len(seen), 2)
        for kwargs in seen:
            self.assertNotIn("on_stdout", kwargs)
            self.assertNotIn("on_stderr", kwargs)

    def test_cleanup_failure_is_fatal_without_primary_error(self) -> None:
        class Sandbox:
            @staticmethod
            def kill():
                raise RuntimeError("unreachable")

        with self.assertRaisesRegex(SystemExit, "Clean it up manually"):
            kill_sandbox(Sandbox(), "sbx_123", run_failed=False)

    def test_cleanup_failure_does_not_mask_primary_error(self) -> None:
        class Sandbox:
            @staticmethod
            def kill():
                raise RuntimeError("unreachable")

        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            kill_sandbox(Sandbox(), "sbx_123", run_failed=True)
        self.assertIn("Clean it up manually", stderr.getvalue())


class RunMimoCodeBoundaryTests(unittest.TestCase):
    def test_smoke_entry_uses_placeholder_not_raw_secret(self) -> None:
        import run_mimo_code

        with patch.object(run_mimo_code, "load_local_dotenv"), patch.object(
            run_mimo_code,
            "parse_args",
            return_value=SimpleNamespace(
                template="tpl",
                workspace="/workspace",
                prompt="prompt",
                agent="build",
                sandbox_timeout=60,
                exec_timeout=30,
                no_seed=True,
                skip_result_check=True,
                raw=False,
            ),
        ), patch.dict(
            "os.environ",
            {
                "CUBE_TEMPLATE_ID": "tpl",
                "E2B_API_URL": "http://cube.example",
                "E2B_API_KEY": "cube-key",
                "MIMO_API_KEY": "real-secret-key",
            },
            clear=False,
        ), patch.object(run_mimo_code, "create_sandbox") as create, patch.object(
            run_mimo_code, "verify_ca_bundle"
        ), patch.object(run_mimo_code, "show_secret_boundary"), patch.object(
            run_mimo_code,
            "run_command",
            return_value=SimpleNamespace(exit_code=0, stdout="1.0.0\n", stderr=""),
        ), patch.object(
            run_mimo_code,
            "run_mimo_command",
            return_value=(
                SimpleNamespace(exit_code=0, stdout="", stderr=""),
                [{"sessionID": "ses_abc"}],
            ),
        ) as run_mimo, patch.object(run_mimo_code, "kill_sandbox"):
            create.return_value = SimpleNamespace(sandbox_id="sbx_1")
            self.assertEqual(run_mimo_code.main(), 0)

        self.assertEqual(create.call_args.args[1], "real-secret-key")
        envs = run_mimo.call_args.kwargs["envs"]
        self.assertEqual(envs["MIMO_API_KEY"], "cube-egress-managed-placeholder")
        self.assertNotEqual(envs["MIMO_API_KEY"], "real-secret-key")


if __name__ == "__main__":
    unittest.main()
