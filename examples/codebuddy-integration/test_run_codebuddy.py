import io
import os
import sys
import unittest
from contextlib import redirect_stderr, redirect_stdout
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

    def test_cli_timeout_rejects_non_positive_values(self):
        for value in ("0", "-5"):
            with self.subTest(value=value):
                with patch.object(
                    sys, "argv", ["run_codebuddy.py", "--timeout", value]
                ):
                    with redirect_stderr(io.StringIO()):
                        with self.assertRaises(SystemExit) as ctx:
                            run_codebuddy.parse_args()

                self.assertEqual(ctx.exception.code, 2)

    def test_help_does_not_validate_timeout_env(self):
        with patch.dict(os.environ, {"CUBE_SANDBOX_TIMEOUT": "0"}, clear=False):
            with patch.object(sys, "argv", ["run_codebuddy.py", "--help"]):
                with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as ctx:
                        run_codebuddy.parse_args()

        self.assertEqual(ctx.exception.code, 0)

    def test_sandbox_env_uses_explicit_api_key(self):
        with patch.dict(os.environ, {}, clear=True):
            env = run_codebuddy.sandbox_env(
                api_key="codebuddy_test_key",
                config_dir="/workspace/.codebuddy",
            )

        self.assertEqual(env["CODEBUDDY_API_KEY"], "codebuddy_test_key")
        self.assertEqual(env["CODEBUDDY_CONFIG_DIR"], "/workspace/.codebuddy")

    def test_sandbox_env_does_not_forward_proxy_by_default(self):
        host_env = {
            "HTTP_PROXY": "http://user:pass@proxy.example:8080",
            "HTTPS_PROXY": "https://alice:secret@proxy.example:8443",
            "NO_PROXY": "localhost,127.0.0.1",
        }
        with patch.dict(os.environ, host_env, clear=True):
            env = run_codebuddy.sandbox_env(
                api_key="codebuddy_test_key",
                config_dir="/workspace/.codebuddy",
            )

        self.assertNotIn("HTTP_PROXY", env)
        self.assertNotIn("HTTPS_PROXY", env)
        self.assertNotIn("NO_PROXY", env)

    def test_sandbox_env_strips_proxy_credentials_when_forwarding_is_enabled(self):
        host_env = {
            "CODEBUDDY_FORWARD_PROXY_ENV": "1",
            "HTTP_PROXY": "http://user:pass@proxy.example:8080",
            "HTTPS_PROXY": "https://alice:secret@proxy.example:8443",
            "NO_PROXY": "localhost,127.0.0.1",
        }
        with patch.dict(os.environ, host_env, clear=True):
            env = run_codebuddy.sandbox_env(
                api_key="codebuddy_test_key",
                config_dir="/workspace/.codebuddy",
            )

        self.assertEqual(env["HTTP_PROXY"], "http://proxy.example:8080")
        self.assertEqual(env["HTTPS_PROXY"], "https://proxy.example:8443")
        self.assertEqual(env["NO_PROXY"], "localhost,127.0.0.1")


if __name__ == "__main__":
    unittest.main()
