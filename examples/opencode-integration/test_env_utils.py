# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import unittest
from unittest.mock import patch

import env_utils


BASE_ENV = {
    "CUBE_API_URL": "http://cube.test:3000/",
    "CUBE_TEMPLATE_ID": "tpl-test",
    "OPENCODE_PROVIDER": "anthropic",
    "OPENCODE_MODEL": "anthropic/test-model",
    "ANTHROPIC_API_KEY": "anthropic-secret",
}


class EnvUtilsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.env = patch.dict(os.environ, BASE_ENV, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_provider_config_uses_expected_anthropic_defaults(self) -> None:
        config = env_utils.provider_config()

        self.assertEqual(config.name, "anthropic")
        self.assertEqual(config.model, "anthropic/test-model")
        self.assertEqual(config.host, "api.anthropic.com")
        self.assertEqual(config.key_env, "ANTHROPIC_API_KEY")
        self.assertEqual(config.auth_header, "x-api-key")
        self.assertNotIn("anthropic-secret", repr(config))

    def test_provider_host_accepts_a_full_url(self) -> None:
        os.environ["OPENCODE_LLM_HOST"] = "https://Gateway.Example.com/v1/messages"

        self.assertEqual(env_utils.provider_config().host, "gateway.example.com")

    def test_provider_rejects_unknown_provider(self) -> None:
        os.environ["OPENCODE_PROVIDER"] = "unknown"
        os.environ["OPENCODE_MODEL"] = "unknown/model"

        with self.assertRaisesRegex(SystemExit, "Unsupported OPENCODE_PROVIDER"):
            env_utils.provider_config()

    def test_provider_rejects_invalid_or_mismatched_model(self) -> None:
        for model, message in (
            ("missing-prefix", "provider/model"),
            ("openai/test-model", "does not match"),
        ):
            with self.subTest(model=model):
                os.environ["OPENCODE_MODEL"] = model
                with self.assertRaisesRegex(SystemExit, message):
                    env_utils.provider_config()

    def test_provider_can_be_loaded_without_secret_for_validation(self) -> None:
        del os.environ["ANTHROPIC_API_KEY"]

        config = env_utils.provider_config(require_secret=False)

        self.assertEqual(config.secret, "")

    def test_direct_environment_contains_only_the_active_provider_secret(self) -> None:
        os.environ["OPENAI_API_KEY"] = "must-not-leak"

        command_env = env_utils.build_opencode_env(env_utils.provider_config())

        self.assertEqual(command_env["ANTHROPIC_API_KEY"], "anthropic-secret")
        self.assertNotIn("OPENAI_API_KEY", command_env)
        self.assertEqual(command_env["OPENCODE_DISABLE_AUTOUPDATE"], "true")

    def test_vault_environment_uses_placeholder_and_node_ca(self) -> None:
        os.environ["OPENCODE_PLACEHOLDER_KEY"] = "placeholder"
        os.environ["OPENCODE_NODE_CA_BUNDLE"] = "/custom/ca.pem"

        command_env = env_utils.build_opencode_env(
            env_utils.provider_config(), include_secret=False
        )

        self.assertEqual(command_env["ANTHROPIC_API_KEY"], "placeholder")
        self.assertEqual(command_env["NODE_EXTRA_CA_CERTS"], "/custom/ca.pem")
        self.assertNotIn("anthropic-secret", command_env.values())

    def test_inline_config_is_forwarded_without_unrelated_host_variables(self) -> None:
        os.environ["OPENCODE_CONFIG_CONTENT"] = '{"model":"anthropic/test-model"}'
        os.environ["UNRELATED_TOKEN"] = "do-not-forward"

        command_env = env_utils.build_opencode_env(env_utils.provider_config())

        self.assertIn("OPENCODE_CONFIG_CONTENT", command_env)
        self.assertNotIn("UNRELATED_TOKEN", command_env)

    def test_run_config_normalizes_url_and_reads_timeouts(self) -> None:
        os.environ["OPENCODE_WORKSPACE"] = "/work"
        os.environ["OPENCODE_SANDBOX_TIMEOUT"] = "1200"
        os.environ["OPENCODE_EXEC_TIMEOUT"] = "300"

        config = env_utils.run_config()

        self.assertEqual(config.api_url, "http://cube.test:3000")
        self.assertEqual(config.workspace, "/work")
        self.assertEqual(config.sandbox_timeout, 1200)
        self.assertEqual(config.exec_timeout, 300)

    def test_int_env_rejects_non_integer_and_non_positive_values(self) -> None:
        for value, message in (("abc", "must be an integer"), ("0", "at least 1")):
            with self.subTest(value=value):
                os.environ["OPENCODE_EXEC_TIMEOUT"] = value
                with self.assertRaisesRegex(SystemExit, message):
                    env_utils.run_config()

    def test_provider_inject_uses_raw_anthropic_and_bearer_openai_headers(self) -> None:
        anthropic = env_utils.provider_inject(env_utils.provider_config())
        self.assertEqual(anthropic[0]["header"], "x-api-key")
        self.assertEqual(anthropic[0]["format"], "${SECRET}")

        os.environ.update(
            {
                "OPENCODE_PROVIDER": "openai",
                "OPENCODE_MODEL": "openai/test-model",
                "OPENAI_API_KEY": "openai-secret",
            }
        )
        openai = env_utils.provider_inject(env_utils.provider_config())
        self.assertEqual(openai[0]["header"], "Authorization")
        self.assertEqual(openai[0]["format"], "Bearer ${SECRET}")

    def test_redact_secrets_handles_known_and_explicit_values(self) -> None:
        os.environ["OPENAI_API_KEY"] = "a-longer-openai-secret"

        text = env_utils.redact_secrets(
            "anthropic-secret and a-longer-openai-secret"
        )
        explicit = env_utils.redact_secrets("custom-secret", ("custom-secret",))

        self.assertEqual(text, "<redacted> and <redacted>")
        self.assertEqual(explicit, "<redacted>")


if __name__ == "__main__":
    unittest.main()
