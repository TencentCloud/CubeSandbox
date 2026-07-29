# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

EXAMPLE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE))

from _opencode_common import (
    SessionIdCapture,
    extract_session_id,
    render_jsonl_line,
    run_command,
    stream_writer,
)


class FakeCommands:
    def __init__(
        self,
        reject_envs: bool = False,
        failure: str | None = None,
        stdout_chunks: list[str] | None = None,
        result_stdout: str = "",
    ):
        self.reject_envs = reject_envs
        self.failure = failure
        self.stdout_chunks = stdout_chunks or []
        self.result_stdout = result_stdout
        self.kwargs = None

    def run(self, command, **kwargs):
        del command
        if self.failure:
            raise TypeError(self.failure)
        if self.reject_envs and "envs" in kwargs:
            raise TypeError("unexpected keyword argument 'envs'")
        self.kwargs = kwargs
        on_stdout = kwargs.get("on_stdout")
        if on_stdout is not None:
            for text in self.stdout_chunks:
                on_stdout(SimpleNamespace(line=text))
        return SimpleNamespace(
            stdout=self.result_stdout,
            stderr="",
            exit_code=0,
        )


class FakeSandbox:
    def __init__(
        self,
        reject_envs: bool = False,
        failure: str | None = None,
        stdout_chunks: list[str] | None = None,
        result_stdout: str = "",
    ):
        self.commands = FakeCommands(
            reject_envs,
            failure,
            stdout_chunks,
            result_stdout,
        )


class CommonHelperTests(unittest.TestCase):
    def test_extracts_one_session_id(self) -> None:
        output = "\n".join(
            [
                json.dumps({"type": "step_start", "sessionID": "ses_1"}),
                json.dumps({"type": "text", "sessionID": "ses_1"}),
            ]
        )
        self.assertEqual(extract_session_id(output), "ses_1")

    def test_rejects_ambiguous_session_ids(self) -> None:
        output = "\n".join(
            [
                json.dumps({"sessionID": "ses_1"}),
                json.dumps({"sessionID": "ses_2"}),
            ]
        )
        with self.assertRaises(SystemExit):
            extract_session_id(output)

    def test_older_sdk_env_fallback(self) -> None:
        sandbox = FakeSandbox(reject_envs=True)
        run_command(sandbox, "true", envs={"A": "B"})
        self.assertEqual(sandbox.commands.kwargs["env"], {"A": "B"})

    def test_does_not_mask_unrelated_type_error_with_envs(self) -> None:
        sandbox = FakeSandbox(failure="unrelated type failure")
        with self.assertRaisesRegex(TypeError, "unrelated"):
            run_command(sandbox, "true", envs={"A": "B"})

    def test_does_not_mask_semantic_error_that_mentions_envs(self) -> None:
        sandbox = FakeSandbox(failure="envs must contain only string values")
        with self.assertRaisesRegex(TypeError, "string values"):
            run_command(sandbox, "true", envs={"A": "B"})

    def test_does_not_fallback_when_envs_were_not_passed(self) -> None:
        sandbox = FakeSandbox(failure="unexpected keyword argument 'envs'")
        with self.assertRaisesRegex(TypeError, "envs"):
            run_command(sandbox, "true")

    def test_renderer_shows_text_and_tool_without_raw_json(self) -> None:
        stream = io.StringIO()
        with contextlib.redirect_stdout(stream):
            render_jsonl_line(
                json.dumps(
                    {
                        "type": "text",
                        "part": {"type": "text", "text": "finished"},
                    }
                )
            )
            render_jsonl_line(
                json.dumps(
                    {
                        "type": "tool_use",
                        "part": {
                            "tool": "bash",
                            "state": {
                                "status": "completed",
                                "input": {"command": "python3 -m unittest"},
                            },
                        },
                    }
                )
            )
        rendered = stream.getvalue()
        self.assertIn("finished", rendered)
        self.assertIn("python3 -m unittest", rendered)
        self.assertNotIn('"sessionID"', rendered)

    def test_renderer_supports_file_and_url_tool_event_keys(self) -> None:
        stream = io.StringIO()
        events = [
            {
                "type": "tool_use",
                "part": {
                    "tool": "read",
                    "state": {
                        "status": "completed",
                        "input": {"filePath": "/workspace/stats.py"},
                    },
                },
            },
            {
                "type": "tool_use",
                "part": {
                    "tool": "webfetch",
                    "state": {
                        "status": "completed",
                        "input": {"url": "https://example.invalid/docs"},
                    },
                },
            },
        ]
        with contextlib.redirect_stdout(stream):
            for event in events:
                render_jsonl_line(json.dumps(event))
        rendered = stream.getvalue()
        self.assertIn("/workspace/stats.py", rendered)
        self.assertIn("https://example.invalid/docs", rendered)

    def test_stream_writer_supports_text_streams(self) -> None:
        stream = io.StringIO()
        write = stream_writer(stream)
        write("first")
        write(" second")
        self.assertEqual(stream.getvalue(), "first second")

    def test_verbose_renderer_reports_unknown_event_type(self) -> None:
        stream = io.StringIO()
        with (
            mock.patch.dict(os.environ, {"OPENCODE_STREAM_VERBOSE": "1"}),
            contextlib.redirect_stderr(stream),
        ):
            render_jsonl_line(json.dumps({"type": "future_event"}))
        self.assertIn("[event:future_event] omitted", stream.getvalue())

    def test_streaming_capture_resolves_session_without_result_stdout(self) -> None:
        payload = (
            json.dumps(
                {
                    "type": "step_start",
                    "sessionID": "ses_stream",
                }
            )
            + "\n"
        )
        sandbox = FakeSandbox(
            stdout_chunks=[payload[:11], payload[11:29], payload[29:]]
        )
        capture = SessionIdCapture()
        with contextlib.redirect_stdout(io.StringIO()):
            result = run_command(
                sandbox,
                "opencode run",
                stream=True,
                json_event_handler=capture.observe,
            )
        self.assertEqual(result.stdout, "")
        self.assertEqual(capture.resolve(result.stdout), "ses_stream")

    def test_result_stdout_is_replayed_when_sdk_ignores_callback(self) -> None:
        payload = (
            json.dumps(
                {
                    "type": "step_start",
                    "sessionID": "ses_replayed",
                }
            )
            + "\n"
        )
        sandbox = FakeSandbox(result_stdout=payload)
        capture = SessionIdCapture()
        with contextlib.redirect_stdout(io.StringIO()):
            result = run_command(
                sandbox,
                "opencode run",
                stream=True,
                json_event_handler=capture.observe,
            )
        self.assertEqual(capture.resolve(result.stdout), "ses_replayed")

    def test_raw_streaming_still_captures_session_id(self) -> None:
        payload = (
            json.dumps(
                {
                    "type": "step_start",
                    "sessionID": "ses_raw",
                }
            )
            + "\n"
        )
        sandbox = FakeSandbox(stdout_chunks=[payload[:7], payload[7:]])
        capture = SessionIdCapture()
        raw_output = io.StringIO()
        with (
            mock.patch.dict(os.environ, {"OPENCODE_STREAM_RAW": "1"}),
            contextlib.redirect_stdout(raw_output),
        ):
            result = run_command(
                sandbox,
                "opencode run",
                stream=True,
                json_event_handler=capture.observe,
            )
        self.assertEqual(raw_output.getvalue(), payload)
        self.assertEqual(capture.resolve(result.stdout), "ses_raw")


if __name__ == "__main__":
    unittest.main()
