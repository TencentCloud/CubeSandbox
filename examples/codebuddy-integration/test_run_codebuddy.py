import os
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import run_codebuddy


class CodeBuddyRunnerTest(unittest.TestCase):
    def test_required_env_reports_all_missing_keys(self):
        with patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(SystemExit) as ctx:
                run_codebuddy.require_env(["E2B_API_URL", "CODEBUDDY_API_KEY"])

        self.assertEqual(
            str(ctx.exception),
            "Missing required environment variables: E2B_API_URL, CODEBUDDY_API_KEY",
        )

    def test_build_codebuddy_script_contains_safe_runtime_defaults(self):
        script = run_codebuddy.build_codebuddy_script(
            prompt="Inspect /tmp/codebuddy-demo and run python3 hello.py",
            output_format="text",
            config_dir="/workspace/.codebuddy",
            permission_mode="bypassPermissions",
        )

        self.assertIn("export DISABLE_AUTOUPDATER=1", script)
        self.assertIn("export CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy", script)
        self.assertIn("codebuddy --version", script)
        self.assertIn("codebuddy -p", script)
        self.assertIn("--output-format text", script)
        self.assertIn("--permission-mode bypassPermissions", script)
        self.assertIn("python3 hello.py", script)

    def test_print_result_fails_when_exit_code_is_missing(self):
        result = SimpleNamespace(stdout="", stderr="")

        with self.assertRaises(SystemExit) as ctx:
            run_codebuddy.print_result(result)

        self.assertEqual(
            str(ctx.exception),
            "Sandbox command did not report an exit code",
        )


if __name__ == "__main__":
    unittest.main()
