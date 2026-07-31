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

    def test_rule_name_can_correlate_one_rollout_audit(self) -> None:
        rules = network_policy.build_rules(
            "real-secret",
            rule_name="mimo_rollout_123",
        )
        self.assertEqual(rules[0].name, "mimo_rollout_123")

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

    @patch.object(network_policy.Sandbox, "create")
    def test_isolated_source_persists_no_credential_rules(self, create) -> None:
        network_policy.create_isolated_sandbox(
            "tpl_123",
            900,
            api_url="https://cube.example.com",
            api_key="cube-key",
        )
        kwargs = create.call_args.kwargs
        self.assertFalse(kwargs["allow_internet_access"])
        self.assertNotIn("network", kwargs)
        self.assertNotIn("env_vars", kwargs)

    @patch.object(network_policy, "run_command")
    def test_verify_ca_bundle_checks_path_inside_sandbox(self, run_command) -> None:
        result = object()
        run_command.return_value = result

        with patch.object(network_policy, "ensure_success") as ensure_success:
            network_policy.verify_ca_bundle(
                object(),
                {"NODE_EXTRA_CA_CERTS": "/etc/cube/ca/cube-root-ca.crt"},
            )

        self.assertEqual(
            run_command.call_args.args[1],
            "test -r /etc/cube/ca/cube-root-ca.crt",
        )
        ensure_success.assert_called_once_with(
            result,
            "verify CubeEgress CA bundle at '/etc/cube/ca/cube-root-ca.crt'",
        )

    @patch.object(network_policy, "run_command")
    def test_secret_boundary_reads_vm_env_without_command_overlay(
        self, run_command
    ) -> None:
        from types import SimpleNamespace

        run_command.return_value = SimpleNamespace(
            exit_code=0,
            stdout="In-VM MIMO_API_KEY: <unset>\n",
            stderr="",
        )

        network_policy.show_secret_boundary(
            object(),
            {"MIMOCODE_HOME": "/root/.mimocode"},
        )

        command = run_command.call_args.args[1]
        kwargs = run_command.call_args.kwargs
        self.assertNotIn("envs", kwargs)
        self.assertNotIn("env", kwargs)
        self.assertIn("printenv MIMO_API_KEY || echo '<unset>'", command)
        self.assertIn("cube-egress-managed-placeholder", command)
        self.assertIn("test ! -f /root/.mimocode/data/auth.json", command)


if __name__ == "__main__":
    unittest.main()
