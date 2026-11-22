#!/usr/bin/env python3

from __future__ import annotations

import argparse
import sys
import unittest
from contextlib import ExitStack
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import cmdline_append as target  # noqa: E402


def make_args(**overrides: object) -> argparse.Namespace:
    values: dict[str, object] = {
        "image": "127.0.0.1:5000/sandbox-code:latest",
        "node": "node-a",
        "master_url": "http://127.0.0.1:8089",
        "expected_memory_mib": 2048,
        "template_build_timeout": 1.0,
        "poll_timeout": 1.0,
        "output": None,
        "runtime_state_root": None,
        "keep_resources": False,
        "expect_kernel": "none",
    }
    values.update(overrides)
    return argparse.Namespace(**values)


class FakeSandbox:
    sandbox_id = "sbx-test"

    def kill(self) -> None:
        pass

    def get_info(self) -> object:
        raise target.harness.SandboxNotFoundError("gone", 404)


class FakeResponse:
    def __init__(self, payload: dict) -> None:
        self._payload = payload
        self.status_code = 200

    def raise_for_status(self) -> None:
        pass

    def json(self) -> dict:
        return self._payload


VALID_CMDLINE = " ".join(
    (
        "root=/dev/pmem0",
        *target.DAMON_PARAMS,
        "damon_reclaim.monitor_region_start=4096",
        f"damon_reclaim.monitor_region_end={2048 << 20}",
    )
)


class CmdlineAppendTests(unittest.TestCase):
    def test_build_template_payload_carries_annotation(self) -> None:
        payload = target.build_template_payload(
            image="img:tag",
            alias="alias-1",
            annotation=list(target.DAMON_PARAMS),
            memory_mib=2048,
            node="node-a",
            request_id="req-1",
        )
        overrides = payload["container_overrides"]
        self.assertEqual(
            overrides["annotations"][target.CMDLINE_ANNOTATION],
            '["damon_reclaim.enabled=Y", "damon_reclaim.min_age=1000000", '
            '"page_reporting.page_reporting_order=6"]',
        )
        # Guest RAM must ride on the container override: a top-level "resources"
        # key is silently dropped, which would fall back to the 2000 MiB default.
        self.assertEqual(overrides["resources"], {"cpu": "500m", "mem": "2048Mi"})
        self.assertNotIn("resources", payload)
        self.assertEqual(payload["distribution_scope"], ["node-a"])
        self.assertNotIn(
            "monitor_region_start", overrides["annotations"][target.CMDLINE_ANNOTATION]
        )

    def test_damon_monitor_region_amd64_above_hole(self) -> None:
        self.assertEqual(
            target.damon_monitor_region(4 << 30, "x86_64"),
            (4096, 5 << 30),
        )

    def test_damon_monitor_region_arm64_below_hole(self) -> None:
        self.assertEqual(
            target.damon_monitor_region(2 << 30, "aarch64"),
            (0x4000_0000, 3 << 30),
        )

    def test_damon_monitor_region_rejects_unknown_arch(self) -> None:
        self.assertIsNone(target.damon_monitor_region(1 << 30, "s390x"))

    def test_assert_guest_cmdline_accepts_valid(self) -> None:
        target.assert_guest_cmdline(
            VALID_CMDLINE,
            want_start=4096,
            want_end=2048 << 20,
        )

    def test_assert_guest_cmdline_rejects_missing_region(self) -> None:
        with self.assertRaisesRegex(AssertionError, "monitor_region_end"):
            target.assert_guest_cmdline(
                VALID_CMDLINE.replace(f"damon_reclaim.monitor_region_end={2048 << 20}", ""),
                want_start=4096,
                want_end=2048 << 20,
            )

    def test_assert_guest_cmdline_rejects_missing_injected_param(self) -> None:
        with self.assertRaisesRegex(AssertionError, "damon_reclaim.enabled=Y"):
            target.assert_guest_cmdline(
                VALID_CMDLINE.replace("damon_reclaim.enabled=Y", ""),
                want_start=4096,
                want_end=2048 << 20,
            )

    def test_assert_guest_kernel_none_skips_probe(self) -> None:
        sandbox = mock.Mock()
        target.assert_guest_kernel(sandbox, "none", 1.0)
        sandbox.commands.run.assert_not_called()

    def test_assert_guest_kernel_damon_probe_passes(self) -> None:
        sandbox = mock.Mock()
        sandbox.commands.run.return_value = mock.Mock(exit_code=0, stderr="")
        target.assert_guest_kernel(sandbox, "damon", 1.0)
        self.assertEqual(
            sandbox.commands.run.call_args.args[0],
            'test "$(cat /sys/module/damon_reclaim/parameters/enabled 2>/dev/null)" = Y',
        )

    def test_assert_guest_kernel_default_rejects_damon_kernel(self) -> None:
        sandbox = mock.Mock()
        # `test ! -e /sys/module/damon_reclaim/parameters/enabled` exits 1 when
        # the DAMON_RECLAIM module parameter is present, i.e. this is not the
        # stock kernel.
        sandbox.commands.run.return_value = mock.Mock(exit_code=1, stderr="")
        with self.assertRaisesRegex(AssertionError, "expect-kernel 'default'"):
            target.assert_guest_kernel(sandbox, "default", 1.0)

    def test_create_template_rejects_non_success_ret(self) -> None:
        with mock.patch.object(
            target.requests,
            "post",
            return_value=FakeResponse({"ret": {"ret_code": 500, "ret_msg": "boom"}}),
        ):
            with self.assertRaisesRegex(RuntimeError, "template create failed"):
                target.create_template_with_annotation("http://127.0.0.1:8089", {})

    def test_wait_for_template_ready_raises_on_failed_status(self) -> None:
        with mock.patch.object(
            target.requests,
            "get",
            return_value=FakeResponse({"ret": {"ret_code": 200}, "job": {"status": "FAILED"}}),
        ):
            with self.assertRaisesRegex(target.harness.FatalProbeError, "template build failed"):
                target.wait_for_template_ready("http://127.0.0.1:8089", "job-1", 1.0)

    def test_main_cleans_up_after_cmdline_assertion_failure(self) -> None:
        sandbox = FakeSandbox()
        with ExitStack() as stack:
            stack.enter_context(
                mock.patch.object(target, "parse_args", return_value=make_args())
            )
            stack.enter_context(mock.patch.object(target.harness, "Config"))
            stack.enter_context(
                mock.patch.object(
                    target, "create_template_with_annotation", return_value=("tpl-1", "job-1")
                )
            )
            stack.enter_context(mock.patch.object(target, "wait_for_template_ready"))
            stack.enter_context(
                mock.patch.object(target.Sandbox, "create", return_value=sandbox)
            )
            stack.enter_context(
                mock.patch.object(target, "wait_for_data_plane", return_value=sandbox)
            )
            stack.enter_context(
                mock.patch.object(
                    target, "read_guest_cmdline", return_value="root=/dev/pmem0"
                )
            )
            cleanup_sandbox = stack.enter_context(
                mock.patch.object(target, "cleanup_sandbox")
            )
            cleanup_template = stack.enter_context(
                mock.patch.object(target, "cleanup_template")
            )
            emit_result = stack.enter_context(mock.patch.object(target, "emit_result"))
            with self.assertRaises(AssertionError):
                target.main()

        cleanup_sandbox.assert_called_once()
        cleanup_template.assert_called_once()
        self.assertEqual(emit_result.call_args.args[0]["status"], "FAIL")


if __name__ == "__main__":
    unittest.main()
