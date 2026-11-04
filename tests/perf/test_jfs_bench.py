"""Offline checks for measurement validity and cluster-state restoration."""

import types
import tempfile
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import jfs_bench_common as common
import cubebench_jfs_local as bench
import cubebench_jfs_report as report


class BenchTests(unittest.TestCase):
    def test_report_renders_all_tables(self):
        def result(prefix, origin="", target=""):
            cells = {}
            metrics = {
                "template": ["create_ms"], "snapshot": ["snapshot_ms", "snapshot_share_ms", "snapshot_total_ready_ms"],
                "fromsnap-local": ["create_ms"], "pause-local": ["pause_ms", "pause_share_ms", "pause_total_ready_ms", "resume_ms"],
                "fromsnap-cold": ["create_ms"], "fromsnap-warm": ["create_ms"],
                "pause-cross": ["pause_ms", "pause_share_ms", "pause_total_ready_ms", "resume_ms"],
            }
            for section, names in metrics.items():
                if (prefix == "local") != (section in {"template", "snapshot", "fromsnap-local", "pause-local"}):
                    continue
                for c in (1, 5, 10):
                    cells[f"{section}_c{c}"] = {"summary": {"valid": True, "metrics": {n: {"avg": c} for n in names}}}
            return {"suite": "lifecycle", "config": {"origin": origin, "target": target}, "cells": cells}

        rendered = report.render(result("local"), result("cross", "c2", "c3"))
        self.assertIn("### 4.1 模板启动与快照", rendered)
        self.assertIn("| c2→c3 首次启动 | 1.0 ms | 5.0 ms | 10.0 ms |", rendered)

    def test_control_helpers_run_locally(self):
        result = types.SimpleNamespace(returncode=0, stdout="ok\n", stderr="")
        with patch.object(common.subprocess, "run", return_value=result) as run:
            self.assertEqual(common.control("true"), "ok")
        self.assertEqual(run.call_args.args[0], "true")
        self.assertTrue(run.call_args.kwargs["shell"])

    def test_nearest_rank_and_incomplete_cells(self):
        rows = [{"create_ms": n} for n in range(1, 11)]
        summary = bench.summarize(rows, 10)
        self.assertTrue(summary["valid"])
        self.assertEqual(summary["metrics"]["create_ms"]["p95"], 10)
        self.assertEqual(summary["metrics"]["create_ms"]["avg"], 5.5)
        self.assertFalse(bench.summarize(rows[:5], 10)["valid"])
        rows[-1]["error"] = "wrong target"
        self.assertIsNone(bench.summarize(rows, 10)["metrics"]["create_ms"]["avg"])

    def test_restore_only_nodes_changed_even_on_partial_failure(self):
        calls = []

        def isolate(nodes, on):
            calls.append((nodes, on))
            if nodes == ["b"] and on:
                raise RuntimeError("connection failed")

        with (
            patch.object(common, "NODE_IPS", dict(a="1", b="2", c="3", d="4")),
            patch.object(common, "isolated", return_value={"d"}),
            patch.object(common, "isolate", side_effect=isolate),
        ):
            with self.assertRaises(RuntimeError):
                with common.target_only("c", True):
                    self.fail("partial failure should not enter body")
        self.assertEqual(
            calls, [(["a"], True), (["b"], True), (["a"], False), (["b"], False)]
        )

    def test_failed_remote_never_ready(self):
        with self.assertRaises(RuntimeError):
            common.wait_ready(lambda: {"remote_status": "failed"})

    def test_snapshot_cleanup_when_readiness_fails(self):
        sb = types.SimpleNamespace(
            create_snapshot=lambda **kw: types.SimpleNamespace(snapshot_id="s"),
            kill=lambda: None,
        )
        deleted = []
        sdk = types.SimpleNamespace(delete_snapshot=lambda sid: deleted.append(sid))
        b = bench.Bench(types.SimpleNamespace(template="t", origin="a", backend="jfs"))
        with (
            patch.object(bench, "create_on", return_value=sb),
            patch.object(bench, "Sandbox", sdk),
            patch.object(bench, "wait_ready", side_effect=TimeoutError),
        ):
            row = b.sample("snapshot")
        self.assertIn("error", row)
        self.assertEqual(deleted, ["s"])

    def test_cleanup_errors_invalidate_sample(self):
        rows = [{"pause_ms": 1, "cleanup_errors": ["delete failed"]}]
        self.assertFalse(bench.summarize(rows, 1)["valid"])

    def test_setup_completes_before_concurrent_snapshots(self):
        created, killed, deleted = [], [], []

        def setup(template, origin):
            index = len(created)
            created.append(index)

            def snap(**kwargs):
                self.assertEqual(len(created), 5)
                return types.SimpleNamespace(snapshot_id=f"s{index}")

            return types.SimpleNamespace(
                create_snapshot=snap, kill=lambda: killed.append(index)
            )

        b = bench.Bench(types.SimpleNamespace(template="t", origin="a", backend="xfs"))
        with (
            patch.object(bench, "create_on", side_effect=setup),
            patch.object(
                bench,
                "Sandbox",
                types.SimpleNamespace(delete_snapshot=lambda sid: deleted.append(sid)),
            ),
        ):
            rows = b.snapshot_batch(5)
        self.assertTrue(bench.summarize(rows, 5)["valid"])
        self.assertEqual(len(killed), 5)
        self.assertEqual(len(deleted), 5)

    def test_lifecycle_json_smoke(self):
        counter = iter(range(100))

        class FakeSandbox:
            def __init__(self):
                self.sandbox_id = f"sb-{next(counter)}"

            def create_snapshot(self, **kwargs):
                return types.SimpleNamespace(snapshot_id=f"snap-{next(counter)}")

            def pause(self, **kwargs):
                pass

            def kill(self):
                pass

        sdk = types.SimpleNamespace(
            create=lambda *a, **kw: FakeSandbox(),
            connect=lambda *a, **kw: None,
            delete_snapshot=lambda sid: None,
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.json"
            args = types.SimpleNamespace(
                template="tpl",
                backend="jfs",
                origin="a",
                target="b",
                out=str(path),
                samples=2,
                warmups=1,
                round_gap=0,
                gap_range=None,
                s3_serial_pause=False,
                allow_node_isolation=False,
            )
            result = {"suite": "lifecycle", "cells": {}}
            b = bench.Bench(args)
            with (
                patch.object(bench, "Sandbox", sdk),
                patch.object(bench, "NODE_IPS", {"a": "1", "b": "2"}),
                patch.object(bench, "create_on", side_effect=lambda *a: FakeSandbox()),
                patch.object(bench, "wait_ready", return_value=0.0),
                patch.object(
                    b, "verify", side_effect=lambda sb, node, row: row.update(node=node)
                ),
            ):
                for section in (
                    "template",
                    "snapshot",
                    "fromsnap-local",
                    "fromsnap-cold",
                    "fromsnap-warm",
                    "pause-local",
                ):
                    b.cell(section, 2, result)
            saved = json.loads(path.read_text())
            self.assertTrue(all(c["summary"]["valid"] for c in saved["cells"].values()))
            self.assertNotIn("_sandbox", path.read_text())
            self.assertEqual(len(saved["cells"]["fromsnap-cold_c2"]["warmups"]), 0)


if __name__ == "__main__":
    unittest.main()
