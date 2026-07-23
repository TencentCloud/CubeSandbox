# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

EXAMPLE_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE_DIR))

import env_utils  # noqa: E402


class EnvUtilsTests(unittest.TestCase):
    def test_config_uses_mimo_platform_without_embedding_secret(self) -> None:
        with patch.dict(os.environ, {"MIMO_API_KEY": "real-secret"}, clear=True):
            config_text = env_utils.build_mimo_config()
        config = json.loads(config_text)
        provider = config["provider"]["mimo"]

        self.assertEqual(config["model"], "mimo/mimo-v2.5-pro")
        self.assertEqual(
            provider["options"]["baseURL"], "https://api.xiaomimimo.com/v1"
        )
        self.assertEqual(
            provider["options"]["headers"]["api-key"], "{env:MIMO_API_KEY}"
        )
        self.assertNotIn("real-secret", config_text)
        self.assertEqual(config["share"], "disabled")

    def test_direct_environment_contains_only_active_secret(self) -> None:
        with patch.dict(os.environ, {"MIMO_API_KEY": "key-123"}, clear=True):
            env = env_utils.build_mimo_env(include_secret=True)

        self.assertEqual(env["MIMO_API_KEY"], "key-123")
        self.assertEqual(env["MIMOCODE_HOME"], "/root/.mimocode")
        self.assertEqual(env["MIMOCODE_ENABLE_ANALYSIS"], "false")

    def test_vault_environment_omits_secret(self) -> None:
        with patch.dict(os.environ, {"MIMO_API_KEY": "key-123"}, clear=True):
            env = env_utils.build_mimo_env(include_secret=False)

        self.assertNotIn("MIMO_API_KEY", env)
        self.assertNotIn("key-123", env["MIMOCODE_CONFIG_CONTENT"])

    def test_home_must_be_absolute(self) -> None:
        with patch.dict(os.environ, {"MIMOCODE_HOME": "relative"}, clear=True):
            with self.assertRaisesRegex(SystemExit, "must be an absolute path"):
                env_utils.mimocode_home()

    def test_model_must_use_mimo_provider(self) -> None:
        with patch.dict(os.environ, {"MIMO_MODEL": "openai/gpt"}, clear=True):
            with self.assertRaisesRegex(SystemExit, "mimo/<model-id>"):
                env_utils.mimo_model()

    def test_command_contains_session_agent_and_quoted_prompt(self) -> None:
        with patch.dict(os.environ, {}, clear=True):
            command = env_utils.mimo_command(
                "write 'hello world'",
                session_id="ses_123",
                agent="compose",
            )
        args = shlex.split(command)

        self.assertEqual(args[:2], ["mimo", "run"])
        self.assertIn("--format", args)
        self.assertIn("--session", args)
        self.assertIn("ses_123", args)
        self.assertIn("--agent", args)
        self.assertIn("compose", args)
        self.assertIn("--dangerously-skip-permissions", args)
        self.assertEqual(args[-1], "write 'hello world'")

    def test_mimo_inject_uses_api_key_header(self) -> None:
        self.assertEqual(
            env_utils.mimo_inject("secret"),
            [{"header": "api-key", "secret": "secret", "format": "${SECRET}"}],
        )

    def test_command_can_keep_permission_prompts_enabled(self) -> None:
        with patch.dict(os.environ, {}, clear=True):
            args = shlex.split(env_utils.mimo_command("inspect", dangerous=False))
        self.assertNotIn("--dangerously-skip-permissions", args)

    def test_workspace_must_be_absolute(self) -> None:
        with patch.dict(os.environ, {"MIMO_WORKSPACE": "relative"}, clear=True):
            with self.assertRaisesRegex(SystemExit, "must be an absolute path"):
                env_utils.mimo_workspace()

    def test_timeout_must_be_positive(self) -> None:
        with patch.dict(os.environ, {"TIMEOUT": "0"}, clear=True):
            with self.assertRaisesRegex(SystemExit, "greater than zero"):
                env_utils.int_env("TIMEOUT", 10)

        with self.assertRaisesRegex(argparse.ArgumentTypeError, "greater than zero"):
            env_utils.positive_int("-1")


if __name__ == "__main__":
    unittest.main()
