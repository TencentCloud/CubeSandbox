# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import sys
import unittest
from dataclasses import dataclass
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

import egress_policy


@dataclass
class FakeResult:
    stdout: str = ""
    stderr: str = ""
    exit_code: int = 0


class NetworkPolicyTest(unittest.TestCase):
    def test_requires_native_cubesandbox_sdk_env(self) -> None:
        env = {
            "E2B_API_URL": "http://127.0.0.1:3000",
            "E2B_API_KEY": "e2b_000000",
        }
        with patch.dict(os.environ, env, clear=True):
            with self.assertRaisesRegex(SystemExit, "CUBE_API_URL"):
                egress_policy.require_native_sdk_env()

    def test_accepts_native_cubesandbox_sdk_env(self) -> None:
        env = {
            "CUBE_API_URL": "http://127.0.0.1:3000",
            "CUBE_PROXY_NODE_IP": "127.0.0.1",
        }
        with patch.dict(os.environ, env, clear=True):
            egress_policy.require_native_sdk_env()

    def test_key_must_be_absent_from_global_vm_environment(self) -> None:
        with patch.object(
            egress_policy, "run_command", return_value=FakeResult(stdout="<unset>\n")
        ):
            egress_policy.verify_key_not_in_vm(object(), "OPENAI_API_KEY")

        with patch.object(
            egress_policy, "run_command", return_value=FakeResult(stdout="unexpected\n")
        ):
            with self.assertRaisesRegex(SystemExit, "global environment"):
                egress_policy.verify_key_not_in_vm(object(), "OPENAI_API_KEY")

    def test_agent_command_must_receive_only_placeholder(self) -> None:
        envs = {"OPENAI_API_KEY": egress_policy.PLACEHOLDER_KEY}
        with patch.object(
            egress_policy,
            "run_command",
            return_value=FakeResult(stdout=f"{egress_policy.PLACEHOLDER_KEY}\n"),
        ) as run:
            egress_policy.verify_placeholder_env(object(), "OPENAI_API_KEY", envs)
        self.assertEqual(run.call_args.kwargs["envs"], envs)

        with patch.object(
            egress_policy, "run_command", return_value=FakeResult(stdout="wrong\n")
        ):
            with self.assertRaisesRegex(SystemExit, "placeholder"):
                egress_policy.verify_placeholder_env(
                    object(), "OPENAI_API_KEY", envs
                )

    def test_non_llm_check_accepts_block_and_rejects_reachability(self) -> None:
        for status in ("403\n", "000blocked\n"):
            with self.subTest(status=status):
                with patch.object(
                    egress_policy,
                    "run_command",
                    return_value=FakeResult(stdout=status),
                ):
                    egress_policy.verify_non_llm_blocked(object())

        with patch.object(
            egress_policy, "run_command", return_value=FakeResult(stdout="200")
        ):
            with self.assertRaisesRegex(SystemExit, "was reachable"):
                egress_policy.verify_non_llm_blocked(object())

    def test_create_sandbox_keeps_default_deny_enabled(self) -> None:
        with patch.object(egress_policy.Sandbox, "create", return_value="sandbox") as create:
            result = egress_policy.create_sandbox("tpl-test", ["rule"], 1800)
        self.assertEqual(result, "sandbox")
        create.assert_called_once_with(
            template="tpl-test",
            allow_internet_access=False,
            network={"rules": ["rule"]},
            timeout=1800,
        )


if __name__ == "__main__":
    unittest.main()
