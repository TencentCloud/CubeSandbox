# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Local unit tests for allowlist.py — no sandbox / API required."""

from __future__ import annotations

import unittest

from allowlist import AllowlistDenied, assert_allowlisted, is_allowlisted


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


if __name__ == "__main__":
    unittest.main()
