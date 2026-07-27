# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import sys
import unittest
from importlib.util import module_from_spec, spec_from_file_location
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

EXAMPLE_DIR = Path(__file__).resolve().parents[1]
COMMON_SPEC = spec_from_file_location("deno_sandbox_common", EXAMPLE_DIR / "common.py")
if COMMON_SPEC is None or COMMON_SPEC.loader is None:
    raise RuntimeError("Could not load the Deno sandbox common.py module")
common = module_from_spec(COMMON_SPEC)
COMMON_SPEC.loader.exec_module(common)

RESUME_SPEC = spec_from_file_location(
    "deno_sandbox_resume_example", EXAMPLE_DIR / "resume_example.py"
)
if RESUME_SPEC is None or RESUME_SPEC.loader is None:
    raise RuntimeError("Could not load the Deno sandbox resume_example.py module")
resume_example = module_from_spec(RESUME_SPEC)
with patch.dict(sys.modules, {"common": common}):
    RESUME_SPEC.loader.exec_module(resume_example)


class FakeSandbox:
    sandbox_id = "sandbox-123"
    traffic_access_token = "traffic-token-123"

    @staticmethod
    def get_host(port: int) -> str:
        return f"{port}-sandbox-123.cube.test"


class FakeCommands:
    def __init__(self) -> None:
        self.command = ""

    def run(self, command: str, **_kwargs: object) -> SimpleNamespace:
        self.command = command
        return SimpleNamespace(exit_code=0, stdout=f"{'a' * 64}\n", stderr="")


class FakeCommandSandbox:
    def __init__(self) -> None:
        self.commands = FakeCommands()


class CommonTests(unittest.TestCase):
    def test_public_url_uses_requested_port_and_path(self) -> None:
        self.assertEqual(
            common.public_url(FakeSandbox(), "/counter", 8000),
            "https://8000-sandbox-123.cube.test/counter",
        )

    def test_traffic_headers_require_the_per_sandbox_token(self) -> None:
        self.assertEqual(
            common.traffic_headers(FakeSandbox()),
            {"e2b-traffic-access-token": "traffic-token-123"},
        )
        with self.assertRaisesRegex(RuntimeError, "no traffic_access_token"):
            common.traffic_headers(SimpleNamespace(traffic_access_token=""))

    def test_public_access_check_requires_unauthorized_status(self) -> None:
        with patch.object(
            common.requests,
            "get",
            return_value=SimpleNamespace(status_code=403),
        ):
            self.assertEqual(common.assert_public_access_restricted(FakeSandbox()), 403)

        with (
            patch.object(
                common.requests,
                "get",
                return_value=SimpleNamespace(status_code=200),
            ),
            self.assertRaisesRegex(RuntimeError, "accepted an unauthenticated"),
        ):
            common.assert_public_access_restricted(FakeSandbox())

    def test_public_egress_check_targets_a_public_tcp_endpoint(self) -> None:
        sandbox = FakeCommandSandbox()

        self.assertEqual(common.assert_public_egress_blocked(sandbox), "a" * 64)
        self.assertIn("/dev/tcp/127.0.0.1/49983", sandbox.commands.command)
        self.assertIn("/dev/tcp/1.1.1.1/80", sandbox.commands.command)
        self.assertIn("timeout 5", sandbox.commands.command)
        self.assertIn("bash is required", sandbox.commands.command)
        self.assertIn("timeout is required", sandbox.commands.command)
        self.assertIn("local envd port 49983", sandbox.commands.command)

    def test_sandbox_identifier_rejects_missing_id(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "no sandbox_id"):
            common.sandbox_identifier(SimpleNamespace(sandbox_id=""))

    def test_resume_example_kills_sandbox_if_identifier_is_invalid(self) -> None:
        sandbox = SimpleNamespace(sandbox_id="", kill=Mock())
        args = SimpleNamespace(template="tpl-deno", timeout=600, poll_timeout=60)

        with (
            patch("builtins.print"),
            patch.object(resume_example, "load_environment"),
            patch.object(resume_example, "parse_args", return_value=args),
            patch.object(resume_example, "required", return_value="configured"),
            patch.object(resume_example.Sandbox, "create", return_value=sandbox),
            self.assertRaisesRegex(RuntimeError, "no sandbox_id"),
        ):
            resume_example.main()

        sandbox.kill.assert_called_once_with()

    def test_required_rejects_empty_value(self) -> None:
        with (
            patch.dict(os.environ, {"CUBE_TEST_REQUIRED": ""}, clear=False),
            self.assertRaisesRegex(SystemExit, "CUBE_TEST_REQUIRED"),
        ):
            common.required("CUBE_TEST_REQUIRED")

    def test_sandbox_create_options_are_secure_by_default(self) -> None:
        self.assertEqual(
            common.sandbox_create_options("tpl-deno", 300),
            {
                "template": "tpl-deno",
                "timeout": 300,
                "allow_internet_access": False,
                "network": {"allow_public_traffic": False},
            },
        )

    def test_ensure_success_includes_process_output(self) -> None:
        result = SimpleNamespace(exit_code=7, stdout="out", stderr="err")
        with self.assertRaisesRegex(RuntimeError, "exit=7") as raised:
            common.ensure_success(result, "demo")
        self.assertIn("out", str(raised.exception))
        self.assertIn("err", str(raised.exception))

    def test_ensure_success_rejects_result_without_exit_code(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "returned no exit_code"):
            common.ensure_success(SimpleNamespace(), "demo")

    def test_cache_fingerprint_uses_the_fixed_cache_path(self) -> None:
        sandbox = FakeCommandSandbox()

        self.assertEqual(common.cache_fingerprint(sandbox), "a" * 64)
        self.assertIn(common.DENO_CACHE_DIR, sandbox.commands.command)
        self.assertNotIn("$DENO_DIR", sandbox.commands.command)
        self.assertIn("-print -quit | grep -q .", sandbox.commands.command)


if __name__ == "__main__":
    unittest.main()
