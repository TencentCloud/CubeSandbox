#!/usr/bin/env python3

from __future__ import annotations

import argparse
import sys
import tempfile
import unittest
from contextlib import ExitStack
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import free_page_reporting as target  # noqa: E402


def test_args(**overrides: object) -> argparse.Namespace:
    values: dict[str, object] = {
        "template": "tpl-existing",
        "image": None,
        "node": "node-a",
        "template_build_timeout": 1.0,
        "load_memory_mib": 1536,
        "baseline_limit_mib": 512,
        "peak_delta_mib": 1024,
        "release_slack_mib": 384,
        "poll_timeout": 1.0,
        "output": None,
        "runtime_state_root": None,
        "keep_resources": False,
    }
    values.update(overrides)
    return argparse.Namespace(**values)


class FakeSandbox:
    sandbox_id = "sbx-test"

    def kill(self) -> None:
        pass

    def get_info(self) -> object:
        raise target.SandboxNotFoundError("gone", 404)

    def pause(self, **_kwargs: object) -> None:
        pass


class FreePageReportingRunnerTests(unittest.TestCase):
    def test_default_runtime_state_roots_use_containerd_task_layout(self) -> None:
        self.assertEqual(
            target.DEFAULT_RUNTIME_STATE_ROOTS,
            (
                Path("/data/cubelet/state/io.containerd.runtime.v2.task/default"),
                Path("/data/cubelet/root/io.containerd.runtime.v2.task/default"),
            ),
        )

    def test_pidfile_requires_matching_sandbox_shim(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pidfile = Path(directory) / "vmm.pid"
            pidfile.write_text("123", encoding="utf-8")
            with mock.patch.object(target, "is_sandbox_shim_pid", return_value=False):
                self.assertIsNone(target.read_live_pid(pidfile, "sbx-test"))
            with mock.patch.object(target, "is_sandbox_shim_pid", return_value=True):
                self.assertEqual(target.read_live_pid(pidfile, "sbx-test"), 123)

    def test_reclaim_cycle_waits_for_fully_touched_workload_before_release(self) -> None:
        events: list[str] = []
        sandbox = FakeSandbox()

        def run_command(_sandbox: object, command: str, _timeout: float) -> None:
            if command.startswith("touch "):
                events.append("release")

        with ExitStack() as stack:
            stack.enter_context(
                mock.patch.object(target, "wait_for", side_effect=[100 * 1024, 150 * 1024])
            )
            stack.enter_context(
                mock.patch.object(
                    target,
                    "wait_for_guest_marker",
                    side_effect=lambda *_args: events.append("ready"),
                )
            )
            stack.enter_context(
                mock.patch.object(
                    target,
                    "process_rss_kib",
                    side_effect=lambda _pid: events.append("peak") or 1200 * 1024,
                )
            )
            stack.enter_context(
                mock.patch.object(target, "run_guest_command", side_effect=run_command)
            )

            target.run_reclaim_cycle(sandbox, 123, "cold boot", test_args())

        self.assertLess(events.index("ready"), events.index("peak"))
        self.assertLess(events.index("peak"), events.index("release"))

    def test_cleanup_sandbox_waits_for_all_node_local_state(self) -> None:
        sandbox = FakeSandbox()
        with tempfile.TemporaryDirectory() as directory:
            roots = (Path(directory),)
            with mock.patch.object(target, "find_shim_pid", return_value=None):
                target.cleanup_sandbox(sandbox, roots, timeout=0.1)

    def test_incomplete_template_build_response_still_cleans_template(self) -> None:
        build = target.TemplateBuild(
            build_id="",
            template_id="tpl-created",
            status="PENDING",
        )
        with ExitStack() as stack:
            stack.enter_context(
                mock.patch.object(
                    target, "parse_args", return_value=test_args(image="image")
                )
            )
            stack.enter_context(mock.patch.object(target, "Config"))
            stack.enter_context(
                mock.patch.object(target, "start_template_build", return_value=build)
            )
            cleanup_template = stack.enter_context(
                mock.patch.object(target, "cleanup_template")
            )
            stack.enter_context(mock.patch.object(target, "emit_result"))
            with self.assertRaisesRegex(RuntimeError, "missing build ID"):
                target.main()
        cleanup_template.assert_called_once_with(
            "tpl-created", mock.ANY, 1.0
        )

    def test_early_failure_emits_fail_status(self) -> None:
        with ExitStack() as stack:
            stack.enter_context(
                mock.patch.object(target, "parse_args", return_value=test_args())
            )
            stack.enter_context(mock.patch.object(target, "Config"))
            stack.enter_context(
                mock.patch.object(
                    target.Sandbox,
                    "create",
                    side_effect=RuntimeError("create failed"),
                )
            )
            emit_result = stack.enter_context(
                mock.patch.object(target, "emit_result")
            )
            with self.assertRaisesRegex(RuntimeError, "create failed"):
                target.main()

        emitted = emit_result.call_args.args[0]
        self.assertEqual(emitted["status"], "FAIL")

    def test_terminal_template_build_failure_is_not_retried(self) -> None:
        failed = mock.Mock(
            status="FAILED",
            phase="BUILD_IMAGE",
            error_message="image pull failed",
            message="",
        )
        with mock.patch.object(
            target.Template, "get_build_status", return_value=failed
        ) as get_status:
            with mock.patch.object(target.time, "sleep") as sleep:
                with self.assertRaisesRegex(
                    target.FatalProbeError, "image pull failed"
                ):
                    target.wait_for_template_build(
                        "tpl-created",
                        "build-created",
                        test_args(template_build_timeout=900.0),
                        mock.sentinel.config,
                    )

        get_status.assert_called_once()
        sleep.assert_not_called()

    def test_cleanup_failure_turns_success_into_failure(self) -> None:
        sandbox = FakeSandbox()
        with ExitStack() as stack:
            stack.enter_context(
                mock.patch.object(target, "parse_args", return_value=test_args())
            )
            stack.enter_context(mock.patch.object(target, "Config"))
            stack.enter_context(
                mock.patch.object(target.Sandbox, "create", return_value=sandbox)
            )
            stack.enter_context(
                mock.patch.object(target.Sandbox, "connect", return_value=sandbox)
            )
            stack.enter_context(
                mock.patch.object(target, "wait_for_data_plane", return_value=sandbox)
            )
            stack.enter_context(
                mock.patch.object(target, "wait_for_vmm_pid", side_effect=[101, 202])
            )
            stack.enter_context(
                mock.patch.object(target, "run_reclaim_cycle", return_value={})
            )
            stack.enter_context(
                mock.patch.object(
                    target,
                    "cleanup_sandbox",
                    side_effect=RuntimeError("cleanup failed"),
                )
            )
            emit_result = stack.enter_context(
                mock.patch.object(target, "emit_result")
            )
            with self.assertRaisesRegex(RuntimeError, "sandbox: cleanup failed"):
                target.main()
        emitted = emit_result.call_args.args[0]
        self.assertEqual(emitted["status"], "FAIL")
        self.assertEqual(emitted["cleanup_errors"], ["sandbox: cleanup failed"])


if __name__ == "__main__":
    unittest.main()
