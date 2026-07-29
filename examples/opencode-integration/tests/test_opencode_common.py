# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import contextlib
import io
import json
import sys
import unittest
from pathlib import Path

EXAMPLE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE))

from _opencode_common import (
    extract_session_id,
    render_jsonl_line,
    run_command,
)


class FakeCommands:
    def __init__(self, reject_envs: bool = False):
        self.reject_envs = reject_envs
        self.kwargs = None

    def run(self, command, **kwargs):
        del command
        if self.reject_envs and "envs" in kwargs:
            raise TypeError("unexpected keyword argument 'envs'")
        self.kwargs = kwargs
        return object()


class FakeSandbox:
    def __init__(self, reject_envs: bool = False):
        self.commands = FakeCommands(reject_envs)


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


if __name__ == "__main__":
    unittest.main()
