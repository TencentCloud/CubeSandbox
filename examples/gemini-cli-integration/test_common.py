# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from common import gemini_command, shell_join


class GeminiCommandTests(unittest.TestCase):
    def test_command_quotes_prompt_and_model(self):
        command = gemini_command("write 'result.md'", model="gemini-2.5-flash", approve_all=False)
        self.assertIn("--prompt", command)
        self.assertIn("gemini-2.5-flash", command)
        self.assertNotIn("--yolo", command)

    def test_command_requires_explicit_approval_for_yolo(self):
        command = gemini_command("write a file", model=None, approve_all=True)
        self.assertTrue(command.endswith("--yolo"))

    def test_shell_join_omits_empty_commands(self):
        self.assertEqual(
            shell_join("cd /workspace", "", "gemini --version"),
            "cd /workspace && gemini --version",
        )


if __name__ == "__main__":
    unittest.main()
