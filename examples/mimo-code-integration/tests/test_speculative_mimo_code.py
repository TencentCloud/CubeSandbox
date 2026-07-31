# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import base64
import gzip
import hashlib
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

EXAMPLE_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO_ROOT / "sdk" / "python"))
sys.path.insert(0, str(EXAMPLE_DIR))

import speculative_mimo_code as speculative  # noqa: E402

TASK = speculative.load_rollout_task(speculative.DEFAULT_TASK_CONFIG)


def result(stdout: str = "", stderr: str = "", exit_code: int = 0):
    return SimpleNamespace(stdout=stdout, stderr=stderr, exit_code=exit_code)


class FakeSandbox:
    def __init__(self, sandbox_id: str) -> None:
        self.sandbox_id = sandbox_id
        self.killed = False
        self.files = MagicMock()
        self.rollback_calls: list[str] = []

    def kill(self) -> None:
        self.killed = True

    def rollback(self, snapshot_id: str) -> None:
        self.rollback_calls.append(snapshot_id)


class SpeculativePureFunctionTests(unittest.TestCase):
    def test_candidate_count_is_bounded(self) -> None:
        self.assertEqual(speculative.candidate_count("2"), 2)
        self.assertEqual(
            speculative.candidate_count(str(speculative.MAX_CANDIDATES)),
            speculative.MAX_CANDIDATES,
        )
        for invalid in ("0", "1", str(speculative.MAX_CANDIDATES + 1)):
            with self.subTest(invalid=invalid):
                with self.assertRaises(argparse.ArgumentTypeError):
                    speculative.candidate_count(invalid)

    def test_candidate_specs_are_stable_and_unique(self) -> None:
        specs = speculative.build_candidate_specs(5, TASK.strategies)
        self.assertEqual(len(specs), 5)
        self.assertEqual(len({spec.name for spec in specs}), 5)
        self.assertEqual(specs[0].name, "candidate-1-minimal")
        self.assertEqual(specs[4].name, "candidate-5-minimal")

    def test_changed_paths_accept_only_app(self) -> None:
        self.assertEqual(
            speculative.changed_paths_from_output(
                "app.py\napp.py\n",
                TASK.allowed_paths,
            ),
            ("app.py",),
        )
        for output in ("tests/test_app.py\n", "../app.py\n", "/tmp/app.py\n"):
            with self.subTest(output=output):
                with self.assertRaises(ValueError):
                    speculative.changed_paths_from_output(
                        output,
                        TASK.allowed_paths,
                    )

    def test_changed_lines_rejects_binary_or_malformed_numstat(self) -> None:
        self.assertEqual(
            speculative.changed_lines_from_numstat("3\t1\tapp.py\n2\t0\tother.py\n"),
            6,
        )
        for output in ("-\t-\tapp.py\n", "3\tapp.py\n", "x\t1\tapp.py\n"):
            with self.subTest(output=output):
                with self.assertRaises(ValueError):
                    speculative.changed_lines_from_numstat(output)

    def test_patch_must_be_nonempty_bounded_and_have_paths(self) -> None:
        speculative.validate_patch(
            (
                "diff --git a/app.py b/app.py\n"
                "--- a/app.py\n"
                "+++ b/app.py\n"
                "@@ -1 +1 @@\n"
                "-old\n"
                "+new\n"
            ),
            ("app.py",),
            allowed_paths=TASK.allowed_paths,
            max_bytes=100,
        )
        with self.assertRaisesRegex(ValueError, "no patch"):
            speculative.validate_patch(
                "",
                ("app.py",),
                allowed_paths=TASK.allowed_paths,
                max_bytes=100,
            )
        with self.assertRaisesRegex(ValueError, "limit"):
            speculative.validate_patch(
                "x" * 101,
                ("app.py",),
                allowed_paths=TASK.allowed_paths,
                max_bytes=100,
            )
        with self.assertRaisesRegex(ValueError, "no changed paths"):
            speculative.validate_patch(
                "patch",
                (),
                allowed_paths=TASK.allowed_paths,
                max_bytes=100,
            )

    def test_patch_rechecks_final_diff_headers(self) -> None:
        patch_text = (
            "diff --git a/app.py b/app.py\n"
            "--- a/app.py\n"
            "+++ b/app.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
            "diff --git a/tests/test_app.py b/tests/test_app.py\n"
            "--- a/tests/test_app.py\n"
            "+++ b/tests/test_app.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
        )
        with self.assertRaisesRegex(ValueError, "disallowed"):
            speculative.validate_patch(
                patch_text,
                ("app.py",),
                allowed_paths=TASK.allowed_paths,
                max_bytes=4096,
            )

    def test_patch_policy_is_driven_by_task_allowed_paths(self) -> None:
        patch_text = (
            "diff --git a/app.py b/app.py\n"
            "--- a/app.py\n"
            "+++ b/app.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
            "diff --git a/lib/util.py b/lib/util.py\n"
            "--- a/lib/util.py\n"
            "+++ b/lib/util.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
        )
        speculative.validate_patch(
            patch_text,
            ("app.py", "lib/util.py"),
            allowed_paths=("app.py", "lib/util.py"),
            max_bytes=4096,
        )

    def test_patch_rejects_mode_changes(self) -> None:
        patch_text = (
            "diff --git a/app.py b/app.py\n"
            "old mode 100644\n"
            "new mode 100755\n"
            "--- a/app.py\n"
            "+++ b/app.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
        )
        with self.assertRaisesRegex(ValueError, "mode"):
            speculative.validate_patch(
                patch_text,
                ("app.py",),
                allowed_paths=TASK.allowed_paths,
                max_bytes=4096,
            )

    def test_profile_archive_has_an_uncompressed_size_limit(self) -> None:
        encoded = base64.b64encode(gzip.compress(b"x" * 9)).decode()
        with self.assertRaisesRegex(ValueError, "expands beyond"):
            speculative.decode_profile_archive(
                encoded,
                max_uncompressed_bytes=8,
            )

    def test_winner_is_smallest_passing_patch_with_stable_tie_break(self) -> None:
        results = [
            speculative.CandidateResult(
                name="candidate-b",
                sandbox_id="sb-b",
                passed=True,
                changed_lines=4,
                patch="patch-b",
            ),
            speculative.CandidateResult(
                name="candidate-a",
                sandbox_id="sb-a",
                passed=True,
                changed_lines=4,
                patch="patch-a",
            ),
            speculative.CandidateResult(
                name="candidate-smaller-but-failed",
                sandbox_id="sb-c",
                passed=False,
                changed_lines=1,
                patch="patch-c",
                error="tests failed",
            ),
        ]
        self.assertEqual(speculative.choose_winner(results).name, "candidate-a")
        with self.assertRaisesRegex(ValueError, "no candidate"):
            speculative.choose_winner(results[2:])


class SpeculativeLifecycleTests(unittest.TestCase):
    @patch.object(speculative, "run_command")
    def test_seed_project_copies_external_fixture_and_checks_baseline(
        self, run_command
    ) -> None:
        sandbox = FakeSandbox("source")
        run_command.side_effect = [
            result(),  # create fixture directories
            result(),  # initialize and commit Git baseline
            result(exit_code=1),  # expected failing baseline
        ]

        speculative.seed_project(sandbox, TASK, "/workspace")

        written_paths = {
            call.args[0] for call in sandbox.files.write.call_args_list
        }
        self.assertIn("/workspace/app.py", written_paths)
        self.assertIn("/workspace/tests/test_app.py", written_paths)
        self.assertEqual(run_command.call_args_list[1].kwargs["cwd"], "/workspace")
        self.assertIn(TASK.test_command, run_command.call_args_list[2].args[1])

    def test_candidate_creation_returns_all_sorted(self) -> None:
        specs = [
            speculative.CandidateSpec("candidate-b", "b"),
            speculative.CandidateSpec("candidate-a", "a"),
        ]
        created = speculative.create_candidate_sandboxes(
            specs,
            lambda spec: FakeSandbox(f"sb-{spec.name}"),
            concurrency=2,
        )
        self.assertEqual(
            [spec.name for spec, _sandbox in created],
            ["candidate-a", "candidate-b"],
        )

    @patch.object(speculative, "evaluate_candidate")
    def test_candidate_failure_does_not_hide_success(
        self, evaluate_candidate
    ) -> None:
        candidates = [
            (speculative.CandidateSpec("candidate-a", "a"), FakeSandbox("a")),
            (speculative.CandidateSpec("candidate-b", "b"), FakeSandbox("b")),
        ]

        def evaluate(spec, sandbox, **_kwargs):
            if spec.name == "candidate-a":
                raise RuntimeError("candidate failed")
            return speculative.CandidateResult(
                name=spec.name,
                sandbox_id=sandbox.sandbox_id,
                session_id="child-b",
                passed=True,
                changed_lines=3,
                patch="patch-b",
            )

        evaluate_candidate.side_effect = evaluate
        results = speculative.evaluate_candidates(
            candidates,
            task=TASK,
            parent_session_id="parent",
            continuity_token="TOKEN",
            workspace="/workspace",
            envs={},
            timeout=30,
            max_patch_bytes=4096,
            concurrency=2,
        )

        self.assertEqual(
            [item.name for item in results],
            ["candidate-a", "candidate-b"],
        )
        self.assertIn("candidate failed", results[0].error)
        self.assertEqual(speculative.choose_winner(results).name, "candidate-b")

    def test_partial_candidate_creation_cleans_successes(self) -> None:
        specs = [
            speculative.CandidateSpec("good", "good"),
            speculative.CandidateSpec("bad", "bad"),
        ]
        good = FakeSandbox("sb-good")

        def create_one(spec):
            if spec.name == "bad":
                raise RuntimeError("create failed")
            return good

        with self.assertRaisesRegex(RuntimeError, "create failed"):
            speculative.create_candidate_sandboxes(
                specs,
                create_one,
                concurrency=2,
            )
        self.assertTrue(good.killed)

    def test_candidate_creation_reports_cleanup_failure_with_resource_id(self) -> None:
        specs = [
            speculative.CandidateSpec("good", "good"),
            speculative.CandidateSpec("bad", "bad"),
        ]
        leaked = FakeSandbox("sb-leaked")
        leaked.kill = MagicMock(side_effect=RuntimeError("cannot kill"))

        def create_one(spec):
            if spec.name == "bad":
                raise RuntimeError("create failed")
            return leaked

        with self.assertRaisesRegex(RuntimeError, "sandbox sb-leaked"):
            speculative.create_candidate_sandboxes(
                specs,
                create_one,
                concurrency=2,
            )

    def test_partial_creation_checkpoints_success_before_cleanup(self) -> None:
        specs = [
            speculative.CandidateSpec("good", "good"),
            speculative.CandidateSpec("bad", "bad"),
        ]
        recorded: list[str] = []

        def create_one(spec):
            if spec.name == "bad":
                raise RuntimeError("create failed")
            return FakeSandbox("sb-good")

        with self.assertRaises(RuntimeError):
            speculative.create_candidate_sandboxes(
                specs,
                create_one,
                concurrency=2,
                on_created=lambda _spec, sandbox: recorded.append(
                    sandbox.sandbox_id
                ),
            )
        self.assertEqual(recorded, ["sb-good"])

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_parent_plan_requires_clean_token_free_workspace(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "parent"}],
        )
        run_command.side_effect = [result(exit_code=1), result(stdout="")]
        session_id = speculative.run_parent_plan(
            FakeSandbox("source"),
            task=TASK,
            workspace="/workspace",
            token="TOKEN",
            envs={},
            timeout=30,
        )
        self.assertEqual(session_id, "parent")
        self.assertIn("--exclude-dir=.git", run_command.call_args_list[0].args[1])

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_parent_plan_reports_unexpected_workspace_edits(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "parent"}],
        )
        run_command.side_effect = [
            result(exit_code=1),
            result(stdout=" M app.py\n"),
        ]
        with self.assertRaisesRegex(SystemExit, "git_status=M app.py"):
            speculative.run_parent_plan(
                FakeSandbox("source"),
                task=TASK,
                workspace="/workspace",
                token="TOKEN",
                envs={},
                timeout=30,
            )

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_parent_plan_does_not_treat_token_scan_error_as_absent(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "parent"}],
        )
        run_command.side_effect = [
            result(stderr="permission denied", exit_code=2),
            result(stdout=""),
        ]
        with self.assertRaisesRegex(SystemExit, "scan the parent workspace"):
            speculative.run_parent_plan(
                FakeSandbox("source"),
                task=TASK,
                workspace="/workspace",
                token="TOKEN",
                envs={},
                timeout=30,
            )

    @patch.object(speculative, "run_command")
    def test_transfers_parent_session_into_snapshot_source(self, run_command) -> None:
        planner = FakeSandbox("planner")
        source = FakeSandbox("source")
        encoded = base64.b64encode(gzip.compress(b"profile data")).decode()
        run_command.side_effect = [
            result(stdout=encoded),
            result(),
        ]
        speculative.transfer_mimo_home(
            planner,
            source,
            "/root/.mimocode",
            real_secret="real-secret",
        )
        source.files.write.assert_called_once_with(
            "/tmp/cube-mimo-home.tar.gz.b64",
            encoded,
        )
        self.assertIn("tar -C /root -xzf -", run_command.call_args_list[1].args[1])

    @patch.object(speculative, "run_command")
    def test_rejects_parent_profile_containing_real_key(self, run_command) -> None:
        encoded = base64.b64encode(
            gzip.compress(b"config real-secret")
        ).decode()
        run_command.return_value = result(stdout=encoded)
        with self.assertRaisesRegex(ValueError, "persisted"):
            speculative.transfer_mimo_home(
                FakeSandbox("planner"),
                FakeSandbox("source"),
                "/root/.mimocode",
                real_secret="real-secret",
            )

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_candidate_fork_checks_context_tests_and_patch(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "child-session"}],
        )
        patch_text = (
            "diff --git a/app.py b/app.py\n"
            "--- a/app.py\n"
            "+++ b/app.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
        )
        candidate = FakeSandbox("sb-child")
        candidate.files.read.return_value = patch_text
        run_command.side_effect = [
            result(),  # continuity report
            result(stdout="tests passed"),  # fixed acceptance tests
            result(),  # git add -N
            result(stdout="app.py\n"),  # changed paths
            result(stdout="3\t1\tapp.py\n"),  # numstat
            result(
                stdout=(
                    f"{len(patch_text.encode())}\n"
                    f"{hashlib.sha256(patch_text.encode()).hexdigest()}\n"
                )
            ),  # bounded patch export
            result(),  # git apply parser and check
        ]
        spec = speculative.CandidateSpec("candidate-1-minimal", "minimal")

        actual = speculative.evaluate_candidate(
            spec,
            candidate,
            task=TASK,
            parent_session_id="parent-session",
            continuity_token="TOKEN",
            workspace="/workspace",
            envs={"MIMO_API_KEY": "placeholder"},
            timeout=30,
            max_patch_bytes=4096,
        )

        self.assertTrue(actual.passed)
        self.assertEqual(actual.session_id, "child-session")
        self.assertEqual(actual.changed_paths, ("app.py",))
        self.assertEqual(actual.changed_lines, 4)
        self.assertEqual(actual.patch, patch_text)
        command = run_mimo_command.call_args.args[1]
        self.assertIn("--session parent-session", command)
        self.assertIn("--fork", command)

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_candidate_rejects_nonforked_parent_session(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "same-session"}],
        )
        with self.assertRaisesRegex(ValueError, "did not create"):
            speculative.evaluate_candidate(
                speculative.CandidateSpec("candidate", "strategy"),
                FakeSandbox("sb"),
                task=TASK,
                parent_session_id="same-session",
                continuity_token="TOKEN",
                workspace="/workspace",
                envs={},
                timeout=30,
                max_patch_bytes=4096,
            )
        run_command.assert_not_called()

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_failed_candidate_tests_do_not_export_patch(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "child"}],
        )
        run_command.side_effect = [
            result(),  # continuity
            result(stderr="failure", exit_code=1),  # tests
        ]
        actual = speculative.evaluate_candidate(
            speculative.CandidateSpec("candidate", "strategy"),
            FakeSandbox("sb"),
            task=TASK,
            parent_session_id="parent",
            continuity_token="TOKEN",
            workspace="/workspace",
            envs={},
            timeout=30,
            max_patch_bytes=4096,
        )
        self.assertFalse(actual.passed)
        self.assertEqual(actual.error, "acceptance tests failed")
        self.assertEqual(run_command.call_count, 2)

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_candidate_rejects_patch_changed_after_validation(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [{"type": "step_finish", "sessionID": "child"}],
        )
        patch_text = (
            "diff --git a/app.py b/app.py\n"
            "--- a/app.py\n"
            "+++ b/app.py\n"
            "@@ -1 +1 @@\n"
            "-old\n"
            "+new\n"
        )
        candidate = FakeSandbox("sb")
        candidate.files.read.return_value = patch_text
        run_command.side_effect = [
            result(),  # continuity
            result(stdout="tests passed"),
            result(),  # git add -N
            result(stdout="app.py\n"),
            result(stdout="1\t1\tapp.py\n"),
            result(stdout=f"{len(patch_text.encode())}\n{'0' * 64}\n"),
            result(),  # in-sandbox parser
        ]
        with self.assertRaisesRegex(ValueError, "changed while"):
            speculative.evaluate_candidate(
                speculative.CandidateSpec("candidate", "strategy"),
                candidate,
                task=TASK,
                parent_session_id="parent",
                continuity_token="TOKEN",
                workspace="/workspace",
                envs={},
                timeout=30,
                max_patch_bytes=4096,
            )

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_missing_continuity_report_gets_one_session_retry(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.side_effect = [
            (
                result(),
                [{"type": "step_finish", "sessionID": "child"}],
            ),
            (
                result(),
                [{"type": "step_finish", "sessionID": "child"}],
            ),
        ]
        run_command.side_effect = [
            result(exit_code=2),  # report missing
            result(),  # retry writes the report
            result(exit_code=1, stderr="tests failed"),
        ]
        actual = speculative.evaluate_candidate(
            speculative.CandidateSpec("candidate", "strategy"),
            FakeSandbox("sb"),
            task=TASK,
            parent_session_id="parent",
            continuity_token="TOKEN",
            workspace="/workspace",
            envs={},
            timeout=30,
            max_patch_bytes=4096,
        )
        self.assertEqual(actual.error, "acceptance tests failed")
        retry_command = run_mimo_command.call_args_list[1].args[1]
        self.assertIn("--session child", retry_command)
        self.assertNotIn("--fork", retry_command)

    @patch.object(speculative, "run_command")
    @patch.object(speculative, "run_mimo_command")
    def test_continuity_marker_can_replace_missing_report(
        self, run_mimo_command, run_command
    ) -> None:
        run_mimo_command.return_value = (
            result(),
            [
                {
                    "type": "text",
                    "sessionID": "child",
                    "text": "CONTINUITY=TOKEN",
                }
            ],
        )
        run_command.side_effect = [
            result(exit_code=2),
            result(exit_code=1, stderr="tests failed"),
        ]
        actual = speculative.evaluate_candidate(
            speculative.CandidateSpec("candidate", "strategy"),
            FakeSandbox("sb"),
            task=TASK,
            parent_session_id="parent",
            continuity_token="TOKEN",
            workspace="/workspace",
            envs={},
            timeout=30,
            max_patch_bytes=4096,
        )
        self.assertEqual(actual.error, "acceptance tests failed")
        run_mimo_command.assert_called_once()

    @patch.object(speculative, "run_command")
    def test_promotion_requires_apply_and_source_validation(self, run_command) -> None:
        source = FakeSandbox("source")
        winner = speculative.CandidateResult(
            name="winner",
            sandbox_id="candidate",
            passed=True,
            patch="patch text",
        )
        run_command.side_effect = [result(), result()]

        self.assertTrue(
            speculative.promote_winner(
                source,
                winner,
                task=TASK,
                workspace="/workspace",
                force_validation_failure=False,
            )
        )
        source.files.write.assert_called_once_with(
            speculative.PATCH_PATH,
            "patch text",
        )
        self.assertIn("git apply --check", run_command.call_args_list[0].args[1])
        validation = run_command.call_args_list[1].args[1]
        self.assertIn(TASK.test_command, validation)
        self.assertIn("tail -c 4000", validation)

    @patch.object(speculative, "run_command")
    def test_forced_promotion_failure_returns_false(self, run_command) -> None:
        source = FakeSandbox("source")
        run_command.side_effect = [result(), result(exit_code=1)]
        winner = speculative.CandidateResult(
            name="winner",
            sandbox_id="candidate",
            passed=True,
            patch="patch",
        )
        self.assertFalse(
            speculative.promote_winner(
                source,
                winner,
                task=TASK,
                workspace="/workspace",
                force_validation_failure=True,
            )
        )
        self.assertEqual(run_command.call_args_list[1].args[1], "false")

    @patch.object(speculative, "run_command", return_value=result())
    def test_rollback_verifies_clean_source(self, run_command) -> None:
        source = FakeSandbox("source")
        speculative.rollback_source(source, "snapshot-1", "/workspace")
        self.assertEqual(source.rollback_calls, ["snapshot-1"])
        self.assertIn(
            "git -C /workspace status",
            run_command.call_args.args[1],
        )

    @patch.object(speculative.time, "sleep")
    @patch.object(speculative, "run_command")
    def test_rollback_retries_transient_data_plane_failure(
        self, run_command, sleep
    ) -> None:
        run_command.side_effect = [RuntimeError("502"), result()]
        source = FakeSandbox("source")
        speculative.rollback_source(source, "snapshot-1", "/workspace")
        self.assertEqual(run_command.call_count, 2)
        sleep.assert_called_once_with(1)

    def test_cleanup_reports_failures_without_skipping_siblings(self) -> None:
        good = FakeSandbox("good")
        bad = FakeSandbox("bad")
        bad.kill = MagicMock(side_effect=RuntimeError("cannot kill"))
        errors = speculative.cleanup_sandboxes([bad, good])
        self.assertTrue(good.killed)
        self.assertEqual(len(errors), 1)
        self.assertIn("bad", errors[0])

    @patch.object(speculative.time, "sleep")
    @patch.object(speculative.Sandbox, "delete_snapshot")
    def test_snapshot_delete_retries_active_runtime_refs(
        self, delete_snapshot, sleep
    ) -> None:
        delete_snapshot.side_effect = [
            RuntimeError("snapshot still has 1 active runtime ref(s)"),
            None,
        ]
        speculative.delete_snapshot_with_retry(
            "snapshot",
            config=object(),
            attempts=2,
            delay=0.25,
        )
        self.assertEqual(delete_snapshot.call_count, 2)
        sleep.assert_called_once_with(0.25)

    def test_evidence_omits_patch_and_can_be_written(self) -> None:
        candidate = speculative.CandidateResult(
            name="candidate",
            sandbox_id="sb",
            passed=True,
            patch="secret patch body",
            test_output="ok",
        )
        payload = {"candidates": [candidate.evidence()]}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "nested" / "evidence.json"
            speculative.write_evidence(str(path), payload)
            text = path.read_text(encoding="utf-8")
        self.assertNotIn("secret patch body", text)
        self.assertIn('"candidate"', text)


if __name__ == "__main__":
    unittest.main()
