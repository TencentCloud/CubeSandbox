"""Exercise the SDK workloads with a local Sandbox double."""
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import textwrap
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch

import cubebench_slo_workload as bench


class SandboxFixture:
    def __init__(self):
        self.templates = []
        self.active = set()
        self.peak = 0
        self.resumed = 0
        self.lock = threading.Lock()

    def create(self, *, template):
        owner = self

        class Sandbox:
            paused = False

            def pause(self):
                self.paused = True

            def resume(self):
                assert self.paused
                self.paused = False
                with owner.lock:
                    owner.resumed += 1

            def kill(self):
                owner.active.remove(self.sandbox_id)

        with self.lock:
            self.templates.append(template)
            sandbox = Sandbox()
            sandbox.sandbox_id = f"fixture-{len(self.templates)}"
            self.active.add(sandbox.sandbox_id)
            self.peak = max(self.peak, len(self.active))
        return sandbox


class WorkloadTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.path = self.directory / "samples.json"
        self.fixture = SandboxFixture()

    def args(self, section, *extra):
        with patch.dict(os.environ, CUBE_TEMPLATE_ID="template", CUBE_SNAPSHOT_ID="snapshot"):
            return bench.parse_args([section, "-c", "3", "-n", "2", "--requests", "7",
                                     "--output", str(self.path), "-s", "0", *extra])

    def report(self):
        return json.loads(self.path.read_text())

    def test_config_requires_ready_source_and_enough_requests(self):
        for section, key in [("4.3", "CUBE_SNAPSHOT_ID"), ("4.6", "CUBE_TEMPLATE_ID")]:
            with self.subTest(section=section):
                with patch.dict(os.environ, clear=True), contextlib.redirect_stderr(io.StringIO()) as err:
                    with self.assertRaises(SystemExit):
                        bench.parse_args([section, "--output", str(self.path)])
                    self.assertIn(key, err.getvalue())
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    self.args(section, "-c", "8")
        self.assertEqual(self.args("4.3", "--snapshot", "override").source, "override")

    def test_bounded_batches_warmup_per_round_sequences_and_source(self):
        for section, source in [("4.3", "snapshot"), ("4.6", "template")]:
            with self.subTest(section=section):
                fixture = SandboxFixture()
                self.assertEqual(bench.run(self.args(section), fixture.create), 0)
                doc = self.report()
                self.assertTrue(doc["valid"])
                self.assertEqual(doc["status"], "complete")
                self.assertEqual(doc["config"]["percentile_method"], "nearest-rank")
                self.assertEqual(doc["progress"], dict(round=2, offset=7, completed_rounds=2))
                self.assertFalse(fixture.active)
                self.assertEqual(fixture.peak, 3)
                self.assertEqual(fixture.templates, [source] * 21)  # 3 warmup + 2 full rounds of 9
                if section == "4.3":
                    groups = [doc]
                    warm = doc["warmup_diagnostic"]["raw"]
                    self.assertEqual(doc["config"]["snapshot_id"], source)
                    self.assertEqual(len(warm), 3)
                    self.assertTrue({r["sandbox_id"] for r in warm}.isdisjoint(
                        r["sandbox_id"] for r in doc["raw"]))
                    self.assertEqual(len({r["sandbox_id"] for r in doc["raw"]}), 14)
                else:
                    groups = list(doc["operations"].values())
                    self.assertEqual(set(doc["operations"]), {"pause", "resume"})
                    self.assertEqual(len(doc["warmup_diagnostic"]["resume"]["raw"]), 3)
                    self.assertEqual(fixture.resumed, 21)
                for group in groups:
                    self.assertEqual(group["padding_count"], 4)
                    self.assertEqual(group["padding_errors"], [])
                    expected = [(group["summary"], 18, 14), (group["rounds"][0], 9, 7),
                                (group["rounds"][1], 9, 7)]
                    for stats, issued, samples in expected:
                        self.assertEqual(stats["requests"], issued)
                        self.assertEqual(stats["successful"], issued)
                        self.assertEqual(stats["errors"], 0)
                        self.assertEqual(stats["skipped"], 0)
                        self.assertEqual(stats["latency_samples"], samples)
                        self.assertAlmostEqual(stats["throughput_qps"], issued * 1000 / stats["wall_ms"])
                        self.assertAlmostEqual(stats["amortized_ms"], stats["wall_ms"] / issued)
                    self.assertEqual([(r["round"], r["seq"]) for r in group["raw"]],
                                     [(n, i) for n in (1, 2) for i in range(1, 8)])

    def test_nearest_rank_uses_only_successful_sdk_latencies(self):
        values = [dict(error=None, latency_ms=i) for i in range(1, 101)]
        values += [dict(error="failed", latency_ms=10000)]
        summary = bench.summarize(values, 2000)
        self.assertEqual((summary["p95"], summary["p99"], summary["errors"]), (95, 99, 1))
        self.assertEqual(summary["throughput_qps"], 50)

    def test_failed_padding_create_invalidates_round(self):
        issued = 0

        def create(**kwargs):
            nonlocal issued
            issued += 1
            if issued == 5:
                raise RuntimeError("create quota")
            return self.fixture.create(**kwargs)

        args = self.args("4.3", "-n", "1", "--requests", "4", "--no-warmup")
        # Serialize the fake SDK calls so the fifth call is always the first padding slot.
        with patch.object(bench, "ThreadPoolExecutor", lambda max_workers: ThreadPoolExecutor(1)):
            self.assertEqual(bench.run(args, create), 1)
        doc = self.report()
        self.assertFalse(doc["valid"])
        self.assertFalse(self.fixture.active)
        self.assertEqual([r["seq"] for r in doc["raw"]], [1, 2, 3, 4])
        self.assertEqual(doc["padding_errors"], [dict(seq=5, round=1, error="create quota")])
        self.assertEqual(doc["summary"]["successful"], 5)
        self.assertEqual(doc["summary"]["errors"], 1)
        self.assertEqual(doc["rounds"][0]["errors"], 1)

    def test_failed_padding_pause_invalidates_round_and_marks_resume_skipped(self):
        def create(**kwargs):
            sb = self.fixture.create(**kwargs)
            if sb.sandbox_id == "fixture-5":
                def fail():
                    raise RuntimeError("pause failed")
                sb.pause = fail
            return sb

        args = self.args("4.6", "-n", "1", "--requests", "4", "--no-warmup")
        self.assertEqual(bench.run(args, create), 1)
        doc = self.report()
        self.assertFalse(doc["valid"])
        self.assertFalse(self.fixture.active)
        self.assertEqual(self.fixture.resumed, 5)
        pause, resume = (doc["operations"][name] for name in ("pause", "resume"))
        self.assertEqual(pause["summary"]["requests"], 6)
        self.assertEqual(pause["summary"]["errors"], 1)
        self.assertEqual(resume["summary"]["requests"], 5)
        self.assertEqual(resume["summary"]["errors"], 0)
        self.assertEqual(resume["summary"]["skipped"], 1)
        self.assertTrue(resume["padding_errors"][0]["skipped"])
        for group in (pause, resume):
            self.assertEqual(group["summary"]["latency_samples"], 4)
            self.assertEqual(group["padding_errors"][0]["seq"], 5)

    def test_failed_pause_skips_resume_and_cleans_every_sandbox(self):
        def create(**kwargs):
            sb = self.fixture.create(**kwargs)
            def fail():
                raise RuntimeError("pause failed")
            sb.pause = fail
            return sb

        self.assertEqual(bench.run(self.args("4.6", "--no-warmup"), create), 1)
        doc = self.report()
        self.assertFalse(doc["valid"])
        self.assertFalse(self.fixture.active)
        self.assertEqual(self.fixture.resumed, 0)
        self.assertEqual(doc["operations"]["pause"]["summary"]["errors"], 18)
        self.assertEqual(doc["operations"]["resume"]["summary"]["requests"], 0)
        self.assertEqual(doc["operations"]["resume"]["summary"]["skipped"], 18)
        for group in doc["operations"].values():
            self.assertIsNone(group["summary"]["p95"])

    def test_create_metadata_error_is_not_a_valid_latency(self):
        def create(**kwargs):
            class MissingID:
                sandbox_id = "bad id"
                def kill(self):
                    killed.append(self)
            return MissingID()
        killed = []
        self.assertEqual(bench.run(self.args("4.3", "--no-warmup"), create), 1)
        self.assertEqual(len(killed), 18)
        self.assertEqual(self.report()["summary"]["errors"], 18)
        self.assertIsNone(self.report()["summary"]["p95"])

    def test_failed_warmup_remains_diagnostic_only(self):
        def create(**kwargs):
            raise RuntimeError("create failed")

        with self.assertRaisesRegex(RuntimeError, "warmup"):
            bench.run(self.args("4.3"), create)
        doc = self.report()
        self.assertEqual(doc["failure"]["stage"], "warmup")
        self.assertEqual(len(doc["warmup_diagnostic"]["raw"]), 3)
        self.assertEqual(doc["summary"]["requests"], 0)

    def test_preparation_failure_preserves_checkpoint_and_primary_error(self):
        def create(**kwargs):
            if len(self.fixture.templates) == 4:
                earlier = self.report()
                self.assertEqual(earlier["status"], "running")
                self.assertEqual(len(earlier["operations"]["pause"]["raw"]), 3)
                raise RuntimeError("primary preparation failure")
            sb = self.fixture.create(**kwargs)
            if len(self.fixture.templates) == 4:
                def bad_cleanup():
                    signal.raise_signal(signal.SIGTERM)
                    raise RuntimeError("cleanup failure")
                sb.kill = bad_cleanup
            return sb

        with self.assertRaisesRegex(RuntimeError, "primary preparation failure"):
            bench.run(self.args("4.6", "--no-warmup"), create)
        doc = self.report()
        self.assertEqual(doc["failure"]["message"], "primary preparation failure")
        self.assertEqual(doc["cleanup_errors"], ["cleanup failure"])
        self.assertEqual(doc["operations"]["pause"]["summary"]["successful"], 3)

    def test_cleanup_failure_preserves_current_batch(self):
        def create(**kwargs):
            sb = self.fixture.create(**kwargs)
            def fail_kill():
                raise RuntimeError("kill failed")
            sb.kill = fail_kill
            return sb

        for section in ("4.3", "4.6"):
            with self.subTest(section=section), self.assertRaisesRegex(RuntimeError, "cleanup failed"):
                bench.run(self.args(section, "--no-warmup"), create)
            doc = self.report()
            self.assertEqual(doc["status"], "failed")
            self.assertEqual(doc["cleanup_errors"], ["kill failed"] * 3)
            groups = [doc] if section == "4.3" else doc["operations"].values()
            for group in groups:
                self.assertEqual(group["summary"]["successful"], 3)
                self.assertFalse(group["rounds"])

    def test_interrupted_resume_keeps_completed_calls_and_sequence_numbers(self):
        barrier = threading.Barrier(3)
        def create(**kwargs):
            sb = self.fixture.create(**kwargs)
            def resume():
                barrier.wait(timeout=3)
                if sb.sandbox_id == "fixture-2":
                    raise KeyboardInterrupt("resume interrupted")
            sb.resume = resume
            return sb

        with self.assertRaisesRegex(KeyboardInterrupt, "resume interrupted"):
            bench.run(self.args("4.6", "--no-warmup"), create)
        doc = self.report()
        self.assertEqual(doc["failure"]["type"], "KeyboardInterrupt")
        self.assertEqual(doc["operations"]["pause"]["summary"]["successful"], 3)
        self.assertEqual([r["seq"] for r in doc["operations"]["resume"]["raw"]], [1, 3])
        self.assertFalse(self.fixture.active)

    def test_atomic_checkpoint_preserves_last_complete_write(self):
        bench.save_report(self.path, {"prior": True})
        with patch("os.replace", side_effect=OSError("replace failed")), self.assertRaises(OSError):
            bench.save_report(self.path, {"next": True})
        self.assertEqual(self.report(), {"prior": True})
        self.assertFalse(list(self.directory.glob("*.tmp")))

    def test_sigterm_drains_created_objects_and_saves_partial_rows(self):
        code = textwrap.dedent("""
            import os, signal, sys, threading, time
            from pathlib import Path
            from cubebench_slo_workload import parse_args, run, terminate

            barrier = threading.Barrier(2)
            class Fake:
                def __init__(self):
                    self.sandbox_id = str(id(self))
                def kill(self):
                    print(self.sandbox_id, flush=True)
                    time.sleep(0.05)
            def create(**kwargs):
                sandbox = Fake()
                if barrier.wait(timeout=3) == 0:
                    for delay in (0.03, 0.06, 0.15):
                        threading.Timer(delay, lambda: os.kill(os.getpid(), signal.SIGTERM)).start()
                time.sleep(0.12)
                return sandbox
            signal.signal(signal.SIGTERM, terminate)
            run(parse_args(), create)
        """)
        result = subprocess.run(
            [sys.executable, "-c", code, "4.3", "-c", "2", "--requests", "2", "-n", "1",
             "--snapshot", "snap", "--no-warmup", "-s", "0", "--output", str(self.path)],
            cwd=Path(__file__).parent, text=True, capture_output=True, timeout=6,
        )
        self.assertEqual(result.returncode, 128 + signal.SIGTERM, result.stderr)
        doc = self.report()
        self.assertEqual(doc["failure"]["type"], "SystemExit")
        self.assertEqual(doc["failure"]["message"], str(128 + signal.SIGTERM))
        self.assertEqual(doc["status"], "failed")
        self.assertEqual(doc["summary"]["successful"], 2)
        self.assertEqual({r["sandbox_id"] for r in doc["raw"]}, set(result.stdout.splitlines()))


if __name__ == "__main__":
    unittest.main()
