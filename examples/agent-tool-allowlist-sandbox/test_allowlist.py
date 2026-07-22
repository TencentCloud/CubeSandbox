# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Local unit tests for allowlist.py — no sandbox / API required."""

from __future__ import annotations

import unittest
from pathlib import Path
from unittest.mock import MagicMock

from allowlist import AllowlistDenied, assert_allowlisted, is_allowlisted

ROOT = Path(__file__).resolve().parent


class AllowlistTests(unittest.TestCase):
    def test_allow_echo(self) -> None:
        self.assertTrue(is_allowlisted("echo hello"))
        self.assertEqual(assert_allowlisted("echo hello"), "echo hello")

    def test_deny_bash(self) -> None:
        self.assertFalse(is_allowlisted("bash -c 'echo hi'"))
        with self.assertRaises(AllowlistDenied):
            assert_allowlisted("bash -c 'echo hi'")

    def test_empty_command(self) -> None:
        with self.assertRaises(AllowlistDenied):
            is_allowlisted("")
        with self.assertRaises(AllowlistDenied):
            assert_allowlisted("   ")

    def test_path_style_binary_rejected(self) -> None:
        self.assertFalse(is_allowlisted("/bin/bash -c id"))
        # Single quotes preserve backslashes through POSIX shlex parsing, so
        # this exercises the explicit Windows path-separator rejection branch.
        self.assertFalse(is_allowlisted(r"'C:\Windows\System32\cmd.exe'"))
        with self.assertRaises(AllowlistDenied):
            assert_allowlisted("/usr/bin/python3 -c 'print(1)'")

    def test_case_sensitive_exact_name(self) -> None:
        # Allowlist stores lowercase names; Echo is not the same token.
        self.assertFalse(is_allowlisted("Echo hello"))
        self.assertTrue(is_allowlisted("echo hello"))

    def test_shlex_injection_style_first_token(self) -> None:
        # Only the first argv token matters; trailing payload does not expand the gate.
        self.assertTrue(is_allowlisted("echo '; rm -rf /'"))
        self.assertFalse(is_allowlisted("curl http://example.com"))
        # Quoted path-like first token still contains '/' → denied.
        self.assertFalse(is_allowlisted("'/bin/echo' ok"))

    def test_custom_allowlist(self) -> None:
        self.assertTrue(is_allowlisted("curl https://x", allowed_binaries={"curl"}))
        self.assertFalse(is_allowlisted("echo hi", allowed_binaries={"curl"}))

    def test_deny_path_never_calls_sandbox_create(self) -> None:
        """Host gate failure must leave Sandbox.create uncalled (call_count == 0)."""
        mock_create = MagicMock()

        def run_tool_through_host_gate(command: str) -> None:
            # Mirrors run_allowlisted.py ordering: gate, then create.
            assert_allowlisted(command)
            mock_create(template="unused")

        with self.assertRaises(AllowlistDenied):
            run_tool_through_host_gate("bash -c 'curl http://example.com'")
        mock_create.assert_not_called()
        self.assertEqual(mock_create.call_count, 0)

    def test_run_denied_script_has_no_sandbox_create(self) -> None:
        """Static guard: the deny demo must not call Sandbox.create."""
        source = (ROOT / "run_denied.py").read_text(encoding="utf-8")
        self.assertNotIn("Sandbox.create", source)

    def test_allow_scripts_stack_airgap_egress(self) -> None:
        """Static guard: allow paths set network-policy Mode 1 airgap."""
        for name in ("run_allowlisted.py", "run_allowlisted_sidecar.py"):
            source = (ROOT / name).read_text(encoding="utf-8")
            self.assertIn("Sandbox.create", source)
            self.assertIn("allow_internet_access=False", source)


if __name__ == "__main__":
    unittest.main()
