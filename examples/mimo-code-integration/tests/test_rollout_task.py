# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

EXAMPLE_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EXAMPLE_DIR))

from rollout_task import load_rollout_task  # noqa: E402


DEFAULT_TASK = (
    EXAMPLE_DIR / "fixtures" / "normalize-slug" / "task.json"
)


class RolloutTaskTests(unittest.TestCase):
    def test_default_fixture_is_external_and_complete(self) -> None:
        task = load_rollout_task(DEFAULT_TASK)
        paths = {path for path, _content in task.fixture_files()}
        self.assertEqual(task.name, "normalize-slug")
        self.assertEqual(task.allowed_paths, ("app.py",))
        self.assertEqual(task.test_timeout_seconds, 120)
        self.assertEqual(task.strategies[0][0], "minimal")
        self.assertIn("app.py", paths)
        self.assertIn("tests/test_app.py", paths)

    def test_task_builds_parent_and_candidate_prompts(self) -> None:
        task = load_rollout_task(DEFAULT_TASK)
        token = "SECRET-CONTINUITY-123"
        parent = task.parent_prompt(token)
        candidate = task.candidate_prompt(
            candidate_name="candidate-a",
            strategy="minimal",
            workspace="/workspace",
            report_path="/tmp/report",
        )
        self.assertIn(token, parent)
        self.assertIn(task.planning_instructions, parent)
        self.assertIn(task.implementation_instructions, candidate)
        self.assertIn(task.test_command, candidate)
        self.assertIn("app.py", candidate)
        self.assertNotIn(token, candidate)

    def test_allowed_path_must_exist_in_fixture(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "project").mkdir()
            (root / "project" / "app.py").write_text("pass\n", encoding="utf-8")
            payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
            payload["allowed_paths"] = ["missing.py"]
            config = root / "task.json"
            config.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "already exist"):
                load_rollout_task(config)

    def test_test_timeout_must_be_positive(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "project").mkdir()
            (root / "project" / "app.py").write_text("pass\n", encoding="utf-8")
            payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
            payload["test_timeout_seconds"] = 0
            config = root / "task.json"
            config.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "positive integer"):
                load_rollout_task(config)

    def test_empty_fixture_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "project").mkdir()
            payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
            config = root / "task.json"
            config.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "empty"):
                load_rollout_task(config)

    def test_fixture_symlinks_and_git_metadata_are_rejected(self) -> None:
        for forbidden in ("symlink", "git"):
            with self.subTest(forbidden=forbidden):
                with tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    project = root / "project"
                    project.mkdir()
                    app = project / "app.py"
                    app.write_text("pass\n", encoding="utf-8")
                    if forbidden == "symlink":
                        (project / "link.py").symlink_to(app)
                        message = "symlinks"
                    else:
                        (project / ".git").mkdir()
                        (project / ".git" / "config").write_text(
                            "metadata\n",
                            encoding="utf-8",
                        )
                        message = "Git metadata"
                    payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
                    config = root / "task.json"
                    config.write_text(json.dumps(payload), encoding="utf-8")
                    with self.assertRaisesRegex(ValueError, message):
                        load_rollout_task(config)

    def test_fixture_file_and_byte_limits_are_enforced(self) -> None:
        for limit in ("files", "bytes"):
            with self.subTest(limit=limit):
                with tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    project = root / "project"
                    project.mkdir()
                    app = project / "app.py"
                    app.write_text("pass\n", encoding="utf-8")
                    if limit == "files":
                        for index in range(64):
                            (project / f"extra-{index}.txt").write_text(
                                "x",
                                encoding="utf-8",
                            )
                    else:
                        app.write_text("x" * (1024 * 1024 + 1), encoding="utf-8")
                    payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
                    config = root / "task.json"
                    config.write_text(json.dumps(payload), encoding="utf-8")
                    with self.assertRaisesRegex(ValueError, "exceeds"):
                        load_rollout_task(config)

    def test_non_utf8_fixture_files_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            project = root / "project"
            project.mkdir()
            (project / "app.py").write_text("pass\n", encoding="utf-8")
            (project / "binary.dat").write_bytes(b"\xff\xfe binary")
            payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
            config = root / "task.json"
            config.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "UTF-8"):
                load_rollout_task(config)

    def test_unsafe_or_duplicate_paths_are_rejected(self) -> None:
        for allowed_paths in (
            ["../app.py"],
            ["/app.py"],
            ["app;rm.py"],
            ["app.py", "app.py"],
        ):
            with self.subTest(allowed_paths=allowed_paths):
                with tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    (root / "project").mkdir()
                    (root / "project" / "app.py").write_text(
                        "pass\n",
                        encoding="utf-8",
                    )
                    payload = json.loads(DEFAULT_TASK.read_text(encoding="utf-8"))
                    payload["allowed_paths"] = allowed_paths
                    config = root / "task.json"
                    config.write_text(json.dumps(payload), encoding="utf-8")
                    with self.assertRaises(ValueError):
                        load_rollout_task(config)


if __name__ == "__main__":
    unittest.main()
