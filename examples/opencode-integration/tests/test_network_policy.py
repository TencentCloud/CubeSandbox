# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import unittest
from types import SimpleNamespace
from unittest.mock import patch

import network_policy


class NetworkPolicyTests(unittest.TestCase):
    def test_rule_allows_only_hy3_host_and_injects_bearer_key(self) -> None:
        rule = network_policy.build_rules("tokenhub.tencentmaas.com", "test-secret")[0]

        self.assertEqual(rule.match.sni, "tokenhub.tencentmaas.com")
        self.assertEqual(rule.match.host, "tokenhub.tencentmaas.com")
        self.assertTrue(rule.action.allow)
        self.assertEqual(rule.action.audit, "metadata")
        self.assertEqual(len(rule.action.inject), 1)
        self.assertEqual(rule.action.inject[0].header, "Authorization")
        self.assertEqual(rule.action.inject[0].secret, "test-secret")
        self.assertEqual(rule.action.inject[0].format, "Bearer ${SECRET}")

    def test_unrelated_host_must_be_blocked(self) -> None:
        result = SimpleNamespace(stdout="200", exit_code=0)
        with (
            patch.object(network_policy, "run_command", return_value=result),
            self.assertRaisesRegex(SystemExit, "Default-deny verification failed"),
        ):
            network_policy.show_unrelated_host_is_blocked(object())

    def test_placeholder_is_required_inside_vm(self) -> None:
        result = SimpleNamespace(stdout="real-secret", exit_code=0)
        with (
            patch.object(network_policy, "run_command", return_value=result),
            self.assertRaisesRegex(SystemExit, "Expected only"),
        ):
            network_policy.show_key_is_placeholder(object())


if __name__ == "__main__":
    unittest.main()
