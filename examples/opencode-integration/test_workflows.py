# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import sys
import unittest
from contextlib import ExitStack
from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import call, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

import checkpoint_fork_opencode
import e2b
import network_policy
import resume_opencode
import run_opencode


@dataclass
class FakeResult:
    stdout: str = ""
    stderr: str = ""
    exit_code: int = 0


class FakeCommands:
    def __init__(self, *results: FakeResult) -> None:
        self.results = list(results)
        self.calls: list[tuple[str, dict[str, object]]] = []

    def run(
        self,
        command: str,
        *,
        cwd: str | None = None,
        envs: dict[str, str] | None = None,
        timeout: int | float | None = None,
        on_stdout=None,
        on_stderr=None,
        user: str = "root",
    ) -> FakeResult:
        self.calls.append(
            (
                command,
                {
                    "cwd": cwd,
                    "envs": envs,
                    "timeout": timeout,
                    "on_stdout": on_stdout,
                    "on_stderr": on_stderr,
                    "user": user,
                },
            )
        )
        return self.results.pop(0) if self.results else FakeResult()


class FakeSnapshot:
    snapshot_id = "snap-test"


class FakeSandbox:
    def __init__(self, sandbox_id: str, *results: FakeResult) -> None:
        self.sandbox_id = sandbox_id
        self.commands = FakeCommands(*results)
        self.kill_calls = 0
        self.pause_calls = 0
        self.snapshot_calls = 0

    def kill(self) -> None:
        self.kill_calls += 1

    def pause(self) -> str:
        self.pause_calls += 1
        return self.sandbox_id

    def create_snapshot(self, name: str | None = None) -> FakeSnapshot:
        self.snapshot_calls += 1
        self.snapshot_name = name
        return FakeSnapshot()


def run_args(**overrides):
    values = {
        "template": "tpl-test",
        "prompt": "fix the project",
        "workspace": "/workspace",
        "title": "test-title",
        "sandbox_timeout": 1800,
        "exec_timeout": 900,
        "dry_run": False,
        "keep_alive": False,
        "verbose": False,
        "raw": False,
    }
    values.update(overrides)
    return SimpleNamespace(**values)


def resume_args():
    return SimpleNamespace(
        template="tpl-test",
        workspace="/workspace",
        title="resume-title",
        sandbox_timeout=1800,
        exec_timeout=900,
    )


def network_args(**overrides):
    values = {
        "template": "tpl-test",
        "host": None,
        "workspace": "/workspace",
        "prompt": "write marker",
        "title": "egress-title",
        "sandbox_timeout": 1800,
        "exec_timeout": 900,
        "skip_agent": False,
    }
    values.update(overrides)
    return SimpleNamespace(**values)


def checkpoint_args():
    return SimpleNamespace(
        template="tpl-test",
        host=None,
        workspace="/workspace",
        title="checkpoint-title",
        sandbox_timeout=1800,
        exec_timeout=900,
    )


def enter_patches(stack: ExitStack, patches):
    return [stack.enter_context(item) for item in patches]


class WorkflowTestCase(unittest.TestCase):
    def setUp(self) -> None:
        env = patch.dict(
            os.environ,
            {
                "OPENCODE_MODEL": "deepseek/deepseek-chat",
                "DEEPSEEK_API_KEY": "test-secret",
            },
            clear=False,
        )
        env.start()
        self.addCleanup(env.stop)


class RunWorkflowTest(WorkflowTestCase):
    def common_patches(self, sandbox: FakeSandbox):
        return (
            patch.object(run_opencode, "load_local_dotenv"),
            patch.object(run_opencode, "parse_args", return_value=run_args()),
            patch.object(run_opencode, "required", return_value="configured"),
            patch.object(run_opencode, "opencode_model", return_value="deepseek/deepseek-chat"),
            patch.object(run_opencode, "opencode_provider", return_value="deepseek"),
            patch.object(run_opencode, "require_provider_key", return_value="secret"),
            patch.object(run_opencode, "provider_key_name", return_value="DEEPSEEK_API_KEY"),
            patch.object(
                run_opencode,
                "build_opencode_env",
                return_value={"DEEPSEEK_API_KEY": "secret"},
            ),
            patch.object(run_opencode, "setup_dev_sidecar_if_requested"),
            patch.object(run_opencode, "warn_direct_secret_env"),
            patch.object(e2b.Sandbox, "create", return_value=sandbox),
        )

    def test_success_runs_agent_verifies_and_kills(self) -> None:
        sandbox = FakeSandbox("run-sb", FakeResult(), FakeResult(), FakeResult(stdout="OK"))
        patches = self.common_patches(sandbox)
        with ExitStack() as stack:
            entered = enter_patches(stack, patches)
            self.assertEqual(run_opencode.main(), 0)
        create = entered[-1]
        create.assert_called_once_with(template="tpl-test", timeout=1800)
        self.assertEqual(sandbox.kill_calls, 1)
        self.assertEqual(len(sandbox.commands.calls), 3)
        self.assertEqual(
            sandbox.commands.calls[1][1]["envs"],
            {"DEEPSEEK_API_KEY": "secret"},
        )

    def test_agent_failure_still_kills(self) -> None:
        sandbox = FakeSandbox("run-sb", FakeResult(), FakeResult(stderr="failed", exit_code=7))
        patches = self.common_patches(sandbox)
        with ExitStack() as stack:
            enter_patches(stack, patches)
            with self.assertRaisesRegex(SystemExit, "run OpenCode"):
                run_opencode.main()
        self.assertEqual(sandbox.kill_calls, 1)

    def test_create_failure_is_reported_without_cleanup_masking(self) -> None:
        sandbox = FakeSandbox("unused")
        patches = self.common_patches(sandbox)
        patches = patches[:-1] + (
            patch.object(
                e2b.Sandbox,
                "create",
                side_effect=RuntimeError("create failed"),
            ),
        )
        with ExitStack() as stack:
            enter_patches(stack, patches)
            with self.assertRaisesRegex(RuntimeError, "create failed"):
                run_opencode.main()
        self.assertEqual(sandbox.kill_calls, 0)


class ResumeWorkflowTest(WorkflowTestCase):
    def common_patches(self, source: FakeSandbox, resumed: FakeSandbox):
        return (
            patch.object(resume_opencode, "load_local_dotenv"),
            patch.object(resume_opencode, "parse_args", return_value=resume_args()),
            patch.object(resume_opencode, "required", return_value="configured"),
            patch.object(resume_opencode, "opencode_model", return_value="deepseek/deepseek-chat"),
            patch.object(resume_opencode, "opencode_provider", return_value="deepseek"),
            patch.object(resume_opencode, "require_provider_key", return_value="secret"),
            patch.object(resume_opencode, "provider_key_name", return_value="DEEPSEEK_API_KEY"),
            patch.object(
                resume_opencode,
                "build_opencode_env",
                return_value={"DEEPSEEK_API_KEY": "secret"},
            ),
            patch.object(resume_opencode, "setup_dev_sidecar_if_requested"),
            patch.object(resume_opencode, "warn_direct_secret_env"),
            patch.object(resume_opencode.Sandbox, "create", return_value=source),
            patch.object(resume_opencode.Sandbox, "connect", return_value=resumed),
        )

    def test_pause_connect_continue_and_cleanup(self) -> None:
        source = FakeSandbox("resume-sb", FakeResult(), FakeResult())
        resumed = FakeSandbox("resume-sb", FakeResult(stdout="plan"), FakeResult(), FakeResult())
        patches = self.common_patches(source, resumed)
        with ExitStack() as stack:
            entered = enter_patches(stack, patches)
            self.assertEqual(resume_opencode.main(), 0)
        connect = entered[-1]
        self.assertEqual(source.pause_calls, 1)
        connect.assert_called_once_with(sandbox_id="resume-sb")
        self.assertEqual(resumed.kill_calls, 1)

    def test_connect_failure_cleans_paused_handle(self) -> None:
        source = FakeSandbox("resume-sb", FakeResult(), FakeResult())
        resumed = FakeSandbox("unused")
        patches = self.common_patches(source, resumed)
        patches = patches[:-1] + (
            patch.object(
                resume_opencode.Sandbox,
                "connect",
                side_effect=RuntimeError("connect failed"),
            ),
        )
        with ExitStack() as stack:
            enter_patches(stack, patches)
            with self.assertRaisesRegex(RuntimeError, "connect failed"):
                resume_opencode.main()
        self.assertEqual(source.kill_calls, 1)

    def test_second_turn_failure_cleans_resumed_sandbox(self) -> None:
        source = FakeSandbox("resume-sb", FakeResult(), FakeResult())
        resumed = FakeSandbox(
            "resume-sb",
            FakeResult(stdout="plan"),
            FakeResult(stderr="agent failed", exit_code=8),
        )
        patches = self.common_patches(source, resumed)
        with ExitStack() as stack:
            enter_patches(stack, patches)
            with self.assertRaisesRegex(SystemExit, "turn 2"):
                resume_opencode.main()
        self.assertEqual(resumed.kill_calls, 1)


class NetworkWorkflowTest(WorkflowTestCase):
    def common_patches(self, sandbox: FakeSandbox, args=None):
        return (
            patch.object(network_policy, "load_local_dotenv"),
            patch.object(network_policy, "parse_args", return_value=args or network_args()),
            patch.object(network_policy, "required", return_value="configured"),
            patch.object(network_policy, "require_native_sdk_env"),
            patch.object(network_policy, "opencode_model", return_value="deepseek/deepseek-chat"),
            patch.object(network_policy, "opencode_provider", return_value="deepseek"),
            patch.object(network_policy, "require_provider_key", return_value="real-secret"),
            patch.object(network_policy, "opencode_llm_host", return_value="api.deepseek.com"),
            patch.object(network_policy, "provider_key_name", return_value="DEEPSEEK_API_KEY"),
            patch.object(network_policy, "build_opencode_env", return_value={}),
            patch.object(network_policy, "build_rules", return_value=["rule"]),
            patch.object(network_policy, "create_sandbox", return_value=sandbox),
        )

    def test_security_checks_agent_and_cleanup(self) -> None:
        sandbox = FakeSandbox(
            "network-sb",
            FakeResult(stdout="<unset>\n"),
            FakeResult(stdout=f"{network_policy.PLACEHOLDER_KEY}\n"),
            FakeResult(stdout="403"),
            FakeResult(),
            FakeResult(),
            FakeResult(stdout="OPENCODE_EGRESS_OK"),
        )
        patches = self.common_patches(sandbox)
        with ExitStack() as stack:
            entered = enter_patches(stack, patches)
            self.assertEqual(network_policy.main(), 0)
        create = entered[-1]
        create.assert_called_once_with("tpl-test", ["rule"], 1800)
        self.assertEqual(sandbox.kill_calls, 1)

    def test_reachable_non_llm_host_fails_and_cleans_up(self) -> None:
        sandbox = FakeSandbox(
            "network-sb",
            FakeResult(stdout="<unset>\n"),
            FakeResult(stdout=f"{network_policy.PLACEHOLDER_KEY}\n"),
            FakeResult(stdout="200"),
        )
        patches = self.common_patches(sandbox)
        with ExitStack() as stack:
            enter_patches(stack, patches)
            with self.assertRaisesRegex(SystemExit, "was reachable"):
                network_policy.main()
        self.assertEqual(sandbox.kill_calls, 1)

    def test_agent_failure_still_cleans_restricted_sandbox(self) -> None:
        sandbox = FakeSandbox(
            "network-sb",
            FakeResult(stdout="<unset>\n"),
            FakeResult(stdout=f"{network_policy.PLACEHOLDER_KEY}\n"),
            FakeResult(stdout="403"),
            FakeResult(),
            FakeResult(stderr="agent failed", exit_code=9),
        )
        patches = self.common_patches(sandbox)
        with ExitStack() as stack:
            enter_patches(stack, patches)
            with self.assertRaisesRegex(SystemExit, "restricted egress"):
                network_policy.main()
        self.assertEqual(sandbox.kill_calls, 1)


class CheckpointForkWorkflowTest(WorkflowTestCase):
    def common_patches(self, source: FakeSandbox, forked: FakeSandbox):
        return (
            patch.object(checkpoint_fork_opencode, "load_local_dotenv"),
            patch.object(checkpoint_fork_opencode, "parse_args", return_value=checkpoint_args()),
            patch.object(checkpoint_fork_opencode, "required", return_value="configured"),
            patch.object(checkpoint_fork_opencode, "require_native_sdk_env"),
            patch.object(
                checkpoint_fork_opencode,
                "opencode_model",
                return_value="deepseek/deepseek-chat",
            ),
            patch.object(checkpoint_fork_opencode, "opencode_provider", return_value="deepseek"),
            patch.object(
                checkpoint_fork_opencode,
                "require_provider_key",
                return_value="real-secret",
            ),
            patch.object(
                checkpoint_fork_opencode,
                "opencode_llm_host",
                return_value="api.deepseek.com",
            ),
            patch.object(
                checkpoint_fork_opencode,
                "provider_key_name",
                return_value="DEEPSEEK_API_KEY",
            ),
            patch.object(
                checkpoint_fork_opencode,
                "build_opencode_env",
                return_value={},
            ),
            patch.object(checkpoint_fork_opencode, "build_rules", return_value=["rule"]),
            patch.object(checkpoint_fork_opencode, "verify_key_not_in_vm"),
            patch.object(checkpoint_fork_opencode, "verify_placeholder_env"),
            patch.object(checkpoint_fork_opencode, "verify_non_llm_blocked"),
            patch.object(
                checkpoint_fork_opencode,
                "create_sandbox",
                side_effect=[source, forked],
            ),
            patch.object(checkpoint_fork_opencode.Sandbox, "delete_snapshot"),
        )

    def test_snapshot_fork_continue_and_cleanup(self) -> None:
        source = FakeSandbox(
            "source-sb",
            FakeResult(),
            FakeResult(),
            FakeResult(stdout="OPENCODE_CHECKPOINT_READY"),
        )
        forked = FakeSandbox("forked-sb", FakeResult(stdout="plan"), FakeResult(), FakeResult())
        patches = self.common_patches(source, forked)
        with ExitStack() as stack:
            entered = enter_patches(stack, patches)
            self.assertEqual(checkpoint_fork_opencode.main(), 0)
        create = entered[14]
        delete = entered[15]
        self.assertEqual(source.snapshot_calls, 1)
        self.assertEqual(
            create.call_args_list,
            [
                call("tpl-test", ["rule"], 1800),
                call("snap-test", ["rule"], 1800),
            ],
        )
        entered[9].assert_called_once_with(include_secrets=False)
        secure_env = {
            "DEEPSEEK_API_KEY": checkpoint_fork_opencode.PLACEHOLDER_KEY
        }
        self.assertEqual(
            entered[11].call_args_list,
            [
                call(source, "DEEPSEEK_API_KEY"),
                call(forked, "DEEPSEEK_API_KEY"),
            ],
        )
        self.assertEqual(
            entered[12].call_args_list,
            [
                call(source, "DEEPSEEK_API_KEY", secure_env),
                call(forked, "DEEPSEEK_API_KEY", secure_env),
            ],
        )
        self.assertEqual(
            entered[13].call_args_list,
            [call(source), call(forked)],
        )
        self.assertEqual(source.kill_calls, 1)
        self.assertEqual(forked.kill_calls, 1)
        delete.assert_called_once_with("snap-test")

    def test_fork_creation_failure_cleans_source_and_checkpoint(self) -> None:
        source = FakeSandbox(
            "source-sb",
            FakeResult(),
            FakeResult(),
            FakeResult(stdout="OPENCODE_CHECKPOINT_READY"),
        )
        fork_error = RuntimeError("fork create failed")
        patches = self.common_patches(source, fork_error)
        with ExitStack() as stack:
            entered = enter_patches(stack, patches)
            with self.assertRaisesRegex(RuntimeError, "fork create failed"):
                checkpoint_fork_opencode.main()
        delete = entered[15]
        self.assertEqual(source.kill_calls, 1)
        delete.assert_called_once_with("snap-test")

    def test_reused_source_id_rejects_fork_and_cleans_everything(self) -> None:
        source = FakeSandbox(
            "same-sb",
            FakeResult(),
            FakeResult(),
            FakeResult(stdout="OPENCODE_CHECKPOINT_READY"),
        )
        forked = FakeSandbox("same-sb")
        patches = self.common_patches(source, forked)

        with ExitStack() as stack:
            entered = enter_patches(stack, patches)
            with self.assertRaisesRegex(RuntimeError, "reused the source sandbox ID"):
                checkpoint_fork_opencode.main()

        self.assertEqual(source.kill_calls, 1)
        self.assertEqual(forked.kill_calls, 1)
        entered[15].assert_called_once_with("snap-test")


if __name__ == "__main__":
    unittest.main()
