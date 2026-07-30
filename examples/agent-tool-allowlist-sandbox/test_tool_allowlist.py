# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-only unit tests for tool_allowlist (no cluster, no Sandbox).

Run from this directory:

    python -m unittest test_tool_allowlist.py -v

These tests lock the *documented* threat model: what the gate claims to
refuse, and one explicit non-goal (allowlisted cat is not path confinement).
"""

from __future__ import annotations

from pathlib import Path
import unittest

from tool_allowlist import (
    AllowlistDenied,
    DEFAULT_ALLOWED_BINARIES,
    assert_allowlisted,
    is_allowlisted,
)

ROOT = Path(__file__).resolve().parent
PROFILE = ROOT / "tool-profile.txt"


class ToolAllowlistTests(unittest.TestCase):
    def test_empty_and_whitespace_denied(self) -> None:
        for cmd in ("", "   ", "\t"):
            with self.subTest(cmd=cmd):
                self.assertFalse(is_allowlisted(cmd))
                with self.assertRaises(AllowlistDenied):
                    assert_allowlisted(cmd)

    def test_happy_path_echo(self) -> None:
        cmd = "echo agent-tool-allowlist-ok"
        self.assertTrue(is_allowlisted(cmd))
        self.assertEqual(assert_allowlisted(cmd), cmd)

    def test_cube_tool_wrapper_allowlisted(self) -> None:
        # Preferred image path: host sees cube-tool; guest re-checks profile.
        cmd = "cube-tool echo via-cube-tool"
        self.assertTrue(is_allowlisted(cmd))
        self.assertEqual(assert_allowlisted(cmd), cmd)

    def test_profile_file_matches_default_toolbox(self) -> None:
        lines = {
            line.strip()
            for line in PROFILE.read_text(encoding="utf-8").splitlines()
            if line.strip() and not line.strip().startswith("#")
        }
        self.assertTrue(lines)
        # Profile tools must be host-allowlisted; cube-tool is wrapper-only.
        self.assertTrue(lines <= DEFAULT_ALLOWED_BINARIES)
        self.assertIn("cube-tool", DEFAULT_ALLOWED_BINARIES)
        self.assertNotIn("cube-tool", lines)

    def test_unknown_binary_denied(self) -> None:
        for cmd in ("bash -c id", "curl -s http://example.com", "busybox sh"):
            with self.subTest(cmd=cmd):
                self.assertFalse(is_allowlisted(cmd))
                with self.assertRaises(AllowlistDenied) as ctx:
                    assert_allowlisted(cmd)
                self.assertIn("not on tool allowlist", str(ctx.exception))

    def test_shell_metachar_chaining_denied(self) -> None:
        # Naive first-token checks would accept these because argv0 is echo.
        cases = (
            "echo ok; bash -c id",
            "echo ok | bash",
            "echo ok & bash",
            "echo `id`",
            "echo $HOME",
            "echo ok\nbash",
        )
        for cmd in cases:
            with self.subTest(cmd=cmd):
                self.assertFalse(is_allowlisted(cmd))
                with self.assertRaises(AllowlistDenied) as ctx:
                    assert_allowlisted(cmd)
                self.assertIn("shell metacharacters", str(ctx.exception))

    def test_path_style_argv0_denied(self) -> None:
        for cmd in ("/bin/echo hi", "./echo hi", r"..\echo hi"):
            with self.subTest(cmd=cmd):
                self.assertFalse(is_allowlisted(cmd))
                with self.assertRaises(AllowlistDenied) as ctx:
                    assert_allowlisted(cmd)
                self.assertIn("path-style argv0", str(ctx.exception))

    def test_python3_requires_explicit_flag(self) -> None:
        cmd = "python3 -c 'print(1)'"
        self.assertFalse(is_allowlisted(cmd))
        with self.assertRaises(AllowlistDenied):
            assert_allowlisted(cmd)
        self.assertTrue(is_allowlisted(cmd, enable_code_execution=True))
        self.assertEqual(
            assert_allowlisted(cmd, enable_code_execution=True), cmd
        )

    def test_extra_binaries_require_unsafe_flag(self) -> None:
        cmd = "curl -s https://example.com"
        with self.assertRaises(ValueError):
            is_allowlisted(cmd, extra_binaries={"curl"})
        self.assertTrue(
            is_allowlisted(
                cmd,
                extra_binaries={"curl"},
                allow_unsafe_allowlist_extension=True,
            )
        )

    def test_assert_tracks_is_allowlisted(self) -> None:
        """assert_* must stay a thin wrapper — no divergent accept/deny."""
        samples = (
            "echo ok",
            "bash -c id",
            "echo ok; true",
            "/bin/ls",
            "python3 -c '1'",
            "cat /tmp/x",
        )
        for cmd in samples:
            with self.subTest(cmd=cmd):
                ok = is_allowlisted(cmd)
                if ok:
                    self.assertEqual(assert_allowlisted(cmd), cmd)
                else:
                    with self.assertRaises(AllowlistDenied):
                        assert_allowlisted(cmd)

    def test_non_goal_allowlisted_cat_is_not_confinement(self) -> None:
        # Honesty check: the gate does NOT implement path policy.
        self.assertTrue(is_allowlisted("cat /etc/passwd"))

    def test_residual_redirect_still_first_token_only(self) -> None:
        # Documented residual: '>' is not treated as shell-chaining meta
        # (agent loop writes artifacts with `echo ... > file`). Path policy
        # remains out of scope for this host gate.
        cmd = "echo artifact-ok > /tmp/agent_loop.txt"
        self.assertTrue(is_allowlisted(cmd))
        self.assertEqual(assert_allowlisted(cmd), cmd)

    def test_unbalanced_quotes_denied(self) -> None:
        self.assertFalse(is_allowlisted("echo 'unterminated"))
        with self.assertRaises(AllowlistDenied) as ctx:
            assert_allowlisted("echo 'unterminated")
        self.assertIn("could not be parsed", str(ctx.exception))
        self.assertNotIn("''", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
