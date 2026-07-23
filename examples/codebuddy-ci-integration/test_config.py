# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import unittest
from unittest.mock import patch

from config import codebuddy_command, codebuddy_env, positive_int, workspace


class CodeBuddyConfigTests(unittest.TestCase):
    def test_command_is_headless_json_and_has_session(self):
        command = codebuddy_command("inspect README", session_id="ci-1", max_turns=2)
        self.assertIn("codebuddy --print --output-format json", command)
        self.assertIn("--session-id ci-1", command)
        self.assertIn("--max-turns 2", command)
        self.assertIn("'inspect README'", command)

    def test_command_can_resume(self):
        command = codebuddy_command("continue", session_id="ci-1", max_turns=1, resume=True)
        self.assertIn("--resume ci-1", command)

    def test_command_includes_optional_model(self):
        with patch.dict(os.environ, {"CODEBUDDY_MODEL": "gpt-5.5"}, clear=True):
            command = codebuddy_command("task", session_id="ci", max_turns=1)
        self.assertIn("--model gpt-5.5", command)

    def test_rejects_empty_prompt(self):
        with self.assertRaises(ValueError):
            codebuddy_command("  ", session_id="ci", max_turns=1)

    def test_rejects_invalid_session(self):
        with self.assertRaises(ValueError):
            codebuddy_command("task", session_id="-invalid", max_turns=1)

    def test_rejects_invalid_turn_count(self):
        with self.assertRaises(ValueError):
            codebuddy_command("task", session_id="ci", max_turns=0)

    def test_codebuddy_env_keeps_only_required_values(self):
        with patch.dict(
            os.environ,
            {"CODEBUDDY_AUTH_TOKEN": "token", "UNRELATED_SECRET": "nope"},
            clear=True,
        ):
            env = codebuddy_env()
        self.assertEqual(env["CODEBUDDY_AUTH_TOKEN"], "token")
        self.assertNotIn("UNRELATED_SECRET", env)

    def test_codebuddy_env_requires_token(self):
        with patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(SystemExit):
                codebuddy_env()

    def test_positive_int_rejects_non_positive(self):
        with patch.dict(os.environ, {"LIMIT": "0"}, clear=True):
            with self.assertRaises(SystemExit):
                positive_int("LIMIT", 1)

    def test_workspace_must_be_absolute(self):
        with patch.dict(os.environ, {"CODEBUDDY_WORKSPACE": "relative"}, clear=True):
            with self.assertRaises(SystemExit):
                workspace()


if __name__ == "__main__":
    unittest.main()
