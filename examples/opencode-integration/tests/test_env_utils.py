# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import os
import shlex
import sys
import unittest
from pathlib import Path
from unittest import mock

EXAMPLE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE))

import env_utils


class EnvironmentTests(unittest.TestCase):
    def test_vault_environment_excludes_real_key(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "HY3_API_KEY": "secret-value",
                "HY3_BASE_URL": "https://tokenhub.tencentmaas.com/v1",
                "HY3_MODEL": "hy3",
            },
            clear=True,
        ):
            env = env_utils.build_opencode_env(include_secret=False)
        self.assertNotIn("HY3_API_KEY", env)
        self.assertEqual(env["OPENCODE_DISABLE_MODELS_FETCH"], "1")

    def test_direct_environment_contains_only_requested_secret(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "HY3_API_KEY": "secret-value",
                "UNRELATED_API_KEY": "must-not-leak",
            },
            clear=True,
        ):
            env = env_utils.build_opencode_env(include_secret=True)
        self.assertEqual(env["HY3_API_KEY"], "secret-value")
        self.assertNotIn("UNRELATED_API_KEY", env)

    def test_rejects_unsafe_base_url(self) -> None:
        with (
            mock.patch.dict(
                os.environ,
                {"HY3_BASE_URL": "http://tokenhub.example/v1"},
                clear=True,
            ),
            self.assertRaises(SystemExit),
        ):
            env_utils.hy3_base_url()

    def test_requires_hy3_model(self) -> None:
        with (
            mock.patch.dict(os.environ, {"HY3_MODEL": "other"}, clear=True),
            self.assertRaises(SystemExit),
        ):
            env_utils.require_hy3_model()

    def test_base_url_accepts_one_trailing_slash(self) -> None:
        with mock.patch.dict(
            os.environ,
            {"HY3_BASE_URL": "https://tokenhub.example/v1/"},
            clear=True,
        ):
            self.assertEqual(
                env_utils.hy3_base_url(),
                "https://tokenhub.example/v1",
            )

    def test_base_url_rejects_paths_beyond_v1(self) -> None:
        for path in ("/v1/chat", "/proxy/v1"):
            with (
                self.subTest(path=path),
                mock.patch.dict(
                    os.environ,
                    {"HY3_BASE_URL": f"https://tokenhub.example{path}"},
                    clear=True,
                ),
                self.assertRaises(SystemExit),
            ):
                env_utils.hy3_base_url()

    def test_extracts_host_from_prevalidated_base_url(self) -> None:
        base_url = "https://tokenhub.tencentmaas.com/v1"
        self.assertEqual(
            env_utils.hy3_host(base_url),
            "tokenhub.tencentmaas.com",
        )


class CommandTests(unittest.TestCase):
    def test_command_quotes_prompt_and_session(self) -> None:
        command = env_utils.opencode_command(
            "fix it; echo unsafe",
            session_id="ses_test",
        )
        args = shlex.split(command)
        self.assertEqual(args[0:2], ["opencode", "run"])
        self.assertIn("--pure", args)
        self.assertIn("--auto", args)
        self.assertEqual(args[args.index("--model") + 1], "tokenhub/hy3")
        self.assertEqual(args[args.index("--session") + 1], "ses_test")
        self.assertEqual(args[-1], "fix it; echo unsafe")

    def test_example_config_has_no_literal_key(self) -> None:
        config = json.loads((EXAMPLE / "opencode.v1.json").read_text(encoding="utf-8"))
        options = config["provider"]["tokenhub"]["options"]
        self.assertEqual(options["apiKey"], "{env:HY3_API_KEY}")
        self.assertEqual(config["model"], "tokenhub/hy3")
        self.assertEqual(config["share"], "disabled")

    def test_destructive_command_rules_cover_direct_rm_variants(self) -> None:
        config = json.loads((EXAMPLE / "opencode.v1.json").read_text(encoding="utf-8"))
        rules = config["permission"]["bash"]
        self.assertEqual(next(iter(rules)), "*")
        for pattern in ("rm *", "/bin/rm *", "/usr/bin/rm *", "command rm *"):
            self.assertEqual(rules[pattern], "deny")
        self.assertNotIn("rm -rf *", rules)


if __name__ == "__main__":
    unittest.main()
