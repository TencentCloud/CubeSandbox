# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import sys
import unittest
from pathlib import Path
from unittest.mock import patch

EXAMPLE_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO_ROOT / "sdk" / "python"))
sys.path.insert(0, str(EXAMPLE_DIR))

import network_policy  # noqa: E402


class NetworkPolicyTests(unittest.TestCase):
    def test_rule_is_exact_host_allow_with_api_key_injection(self) -> None:
        rules = network_policy.build_rules("real-secret")
        self.assertEqual(len(rules), 1)
        self.assertEqual(
            rules[0].to_wire(),
            {
                "name": "allow_mimo_platform",
                "match": {
                    "sni": "api.xiaomimimo.com",
                    "host": "api.xiaomimimo.com",
                    "scheme": "https",
                },
                "action": {
                    "allow": True,
                    "audit": "metadata",
                    "inject": [
                        {
                            "header": "api-key",
                            "secret": "real-secret",
                            "format": "${SECRET}",
                        }
                    ],
                },
            },
        )

    @patch.object(network_policy.Sandbox, "create")
    def test_create_is_default_deny_and_uses_explicit_config(self, create) -> None:
        expected = object()
        create.return_value = expected

        actual = network_policy.create_sandbox(
            "tpl_123",
            "real-secret",
            900,
            api_url="https://cube.example.com",
            api_key="cube-key",
        )

        self.assertIs(actual, expected)
        kwargs = create.call_args.kwargs
        self.assertFalse(kwargs["allow_internet_access"])
        self.assertEqual(kwargs["template"], "tpl_123")
        self.assertEqual(kwargs["timeout"], 900)
        self.assertEqual(kwargs["config"].api_url, "https://cube.example.com")
        self.assertEqual(kwargs["config"].api_key, "cube-key")
        self.assertEqual(
            kwargs["network"]["rules"][0].action.inject[0].secret, "real-secret"
        )

    @patch.object(network_policy.Sandbox, "create")
    def test_create_supports_sdk_without_api_key_config_parameter(self, create) -> None:
        class LegacyConfig:
            def __init__(self, *, api_url: str, template_id: str) -> None:
                self.api_url = api_url
                self.template_id = template_id

        expected = object()
        create.return_value = expected

        with patch.object(network_policy, "Config", LegacyConfig):
            actual = network_policy.create_sandbox(
                "tpl_123",
                "real-secret",
                900,
                api_url="https://cube.example.com",
                api_key="cube-key",
            )

        self.assertIs(actual, expected)
        self.assertEqual(
            create.call_args.kwargs["config"].api_url, "https://cube.example.com"
        )
        self.assertEqual(create.call_args.kwargs["config"].template_id, "tpl_123")


if __name__ == "__main__":
    unittest.main()
