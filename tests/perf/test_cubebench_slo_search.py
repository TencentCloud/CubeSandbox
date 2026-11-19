import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from cubebench_slo_search import create_results_directory, plan, run_search


PERF = Path(__file__).resolve().parent
SEARCH = "cubebench_slo_search.py"


def reading(concurrency, p95_ms, errors=0):
    return dict(concurrency=concurrency, requests=500, errors=errors, p95_ms=p95_ms)


class SearchTest(unittest.TestCase):
    def test_coarse_refine_and_confirmation(self):
        rows = []
        phases = set()
        while (case := plan(rows)["next_case"]) is not None:
            self.assertLess(len(rows), 48)
            phases.add(case["phase"])
            c = case["concurrency"]
            rows.append(reading(c, 90 + c / 3))
        first, = plan(rows)["results"]
        self.assertIn("confirmation", phases)
        self.assertIn("refine", phases)
        self.assertEqual(first["status"], "confirmed_boundary")
        self.assertEqual((first["confirmed_concurrency"], first["failing_concurrency"]), (30, 31))
        self.assertEqual(len(first["passing_p95_ms"]), 3)
        self.assertEqual(len(first["failing_p95_ms"]), 3)

    def test_confirmation_tolerates_an_outlier_on_either_side(self):
        traces = {30: iter([99, 130, 99]), 31: iter([110, 80, 110])}
        rows = []
        while (case := plan(rows)["next_case"]) is not None:
            self.assertLess(len(rows), 48)
            c = case["concurrency"]
            value = next(traces[c]) if c in traces else (99 if c < 30 else 130)
            rows.append(reading(c, value))
        verdict, = plan(rows)["results"]
        self.assertEqual(verdict["status"], "confirmed_boundary")
        self.assertEqual(verdict["confirmed_concurrency"], 30)
        self.assertEqual(verdict["passing_p95_ms"], [99, 130, 99])
        self.assertEqual(verdict["failing_p95_ms"], [110, 80, 110])

    def test_failure_at_minimum_requires_three_rounds(self):
        rows = [reading(1, 190)]
        for value in (90, 150):
            self.assertEqual(plan(rows)["next_case"]["concurrency"], 1)
            rows.append(reading(1, value))
        self.assertIsNone(plan(rows)["next_case"])
        verdict, = plan(rows)["results"]
        self.assertEqual(verdict["status"], "lowest_concurrency_exceeded_limit")
        self.assertEqual(verdict["failing_p95_ms"], [190, 90, 150])

    def test_failed_confirmation_finds_lower_stable_boundary(self):
        rows = [reading(29, 98), reading(30, 99), reading(31, 110), reading(30, 101)]
        self.assertEqual(plan(rows)["next_case"]["concurrency"], 30)
        rows.append(reading(30, 101))
        self.assertEqual(plan(rows)["next_case"]["concurrency"], 29)
        result, = plan(rows + [reading(29, 99)] * 2)["results"]
        self.assertEqual((result["confirmed_concurrency"], result["failing_concurrency"]), (29, 30))
        self.assertEqual(result["status"], "confirmed_boundary")

    def test_non_monotonic_observations_stop_the_search(self):
        rows = [reading(16, 102), reading(17, 99)]
        self.assertEqual(plan(rows)["next_case"]["phase"], "confirmation")
        result = plan(rows * 3)
        self.assertEqual(result["results"][0]["status"], "unstable_or_non_monotonic")
        self.assertIsNone(result["next_case"])

    def test_latency_limit_is_inclusive(self):
        verdict, = plan([reading(16, 100), reading(17, 100.01)])["results"]
        self.assertEqual(verdict["passing_concurrency"], 16)
        self.assertEqual(verdict["failing_concurrency"], 17)

    def test_successful_measurement_requires_p95(self):
        with self.assertRaisesRegex(KeyError, "p95_ms"):
            plan([dict(concurrency=16, requests=500, errors=0, p99_ms=80)])

    def test_search_probes_the_adjacent_integer_before_confirmation(self):
        rows = [reading(72, 100), reading(76, 105.27)]
        case = plan(rows)["next_case"]
        self.assertEqual((case["concurrency"], case["phase"]), (73, "refine"))
        rows.append(reading(73, 100.5))
        self.assertEqual(plan(rows)["next_case"]["concurrency"], 72)
        rows.extend([reading(72, 99.99)] * 2)
        self.assertEqual(plan(rows)["next_case"]["concurrency"], 73)
        result, = plan(rows + [reading(73, 101)] * 2)["results"]
        self.assertEqual(result["status"], "confirmed_boundary")

    def test_history_predicts_first_probe_but_cannot_confirm_it(self):
        history = [reading(20, 80), reading(40, 120)]
        result = plan([], history)
        self.assertEqual(result["next_case"]["concurrency"], 30)
        self.assertIsNone(result["results"][0]["passing_concurrency"])
        rows = [reading(30, 100)]
        self.assertEqual(plan(rows, history)["next_case"]["concurrency"], 31)
        rows.append(reading(31, 101))
        history += [reading(30, 100)] * 3
        self.assertEqual(plan(rows, history)["next_case"]["phase"], "confirmation")

    def test_stale_prior_does_not_exclude_the_live_boundary(self):
        history = [reading(150, 80), reading(300, 110)]
        rows = []
        while (case := plan(rows, history)["next_case"]) is not None:
            self.assertLess(len(rows), 48)
            c = case["concurrency"]
            rows.append(reading(c, 50 + c))
        result, = plan(rows, history)["results"]
        self.assertEqual(result["confirmed_concurrency"], 50)
        self.assertEqual(result["failing_concurrency"], 51)

    def test_stale_low_prior_does_not_limit_each_step_to_the_last_interval(self):
        history = [reading(69, 97), reading(71, 101)]
        rows = []
        while (case := plan(rows, history)["next_case"]) is not None:
            c = case["concurrency"]
            rows.append(reading(c, 100 + (c - 400) / 8))
        result, = plan(rows, history)["results"]
        self.assertEqual(result["status"], "confirmed_boundary")
        self.assertEqual(result["confirmed_concurrency"], 400)

    def test_spending_the_case_budget_does_not_prove_a_boundary(self):
        rows = [reading(40, 98)] * 45 + [reading(41, 100.5)] * 2
        self.assertEqual(plan(rows)["next_case"]["concurrency"], 41)
        for last, status, boundary in ((reading(41, 110), "confirmed_boundary", 40),
                                       (reading(40, 99), "case_budget_exhausted", None)):
            with self.subTest(status=status):
                search = plan(rows + [last])
                self.assertIsNone(search["next_case"])
                self.assertEqual(search["results"][0]["status"], status)
                self.assertEqual(search["results"][0]["confirmed_concurrency"], boundary)

    def test_cap_is_lower_bound(self):
        rows = [reading(500, 100), reading(500, 130)]
        self.assertEqual(plan(rows)["next_case"]["concurrency"], 500)
        result = plan(rows + [reading(500, 99)])
        self.assertIsNone(result["next_case"])
        self.assertEqual(result["results"][0]["status"], "lower_bound_at_cap")

    def test_operation_error_stops_search(self):
        result = plan([reading(16, None, errors=1)])
        self.assertIsNone(result["next_case"])
        self.assertEqual(result["results"][0]["status"], "operation_error")


class SearchCommandTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.env = {
            "PATH": os.environ["PATH"],
            "PYTHON_BIN": sys.executable,
            "TMPDIR": str(self.root),
            "OUTPUT_MD": str(self.root / "capacity report.md"),
            "CUBE_BENCH_SLO_SETTLE": "0",
        }

    def test_same_second_searches_get_distinct_timestamped_directories(self):
        name = "cubebench-search-20261119_154520"
        with patch("cubebench_slo_search.tempfile.gettempdir", return_value=str(self.root)), \
                patch("cubebench_slo_search.time.strftime", return_value=name) as stamp:
            directories = [create_results_directory() for _ in range(3)]
        stamp.assert_called_with("cubebench-search-%Y%m%d_%H%M%S")
        self.assertEqual(directories, [self.root.resolve() / (name + suffix) for suffix in ("", "-1", "-2")])
        self.assertTrue(all(path.is_dir() for path in directories))

    def invoke(self, command, *args, stdin=""):
        return subprocess.run(
            [*command, *args], cwd=self.root, env=self.env, input=stdin,
            capture_output=True, text=True, timeout=15,
        )

    def preview(self, *args):
        result = self.invoke(["bash", str(PERF / "cubebench.sh"), "search"], "--preview", *args)
        self.assertEqual(result.returncode, 0, result.stderr)
        return [(section, json.loads(row)["concurrency"])
                for section, row in (line.split(maxsplit=1) for line in result.stdout.splitlines())]

    def history(self, section, points):
        directory = self.root / "prior" / section
        directory.mkdir(parents=True)
        records = [dict(concurrency=c, p95_ms=p95, requests=500, errors=0) for c, p95 in points]
        (directory / "history.json").write_text(json.dumps(records))
        return str(directory.parent)

    def test_preview_all_chapter_and_single(self):
        self.assertEqual(self.preview(), [("3.2", 16), ("4.3", 16), ("4.6", 16)])
        self.assertEqual(self.preview("-y", "4", "4.3"), [("4.3", 16), ("4.6", 16)])
        self.assertEqual(self.preview("4.6"), [("4.6", 16)])
        self.assertFalse(Path(self.env["OUTPUT_MD"]).exists())

    def test_planner_cli_accepts_stdin_and_history(self):
        directory = Path(self.history("3.2", [(20, 80), (40, 120)]))
        command = [sys.executable, str(PERF / SEARCH)]
        result = self.invoke(command, "--history", str(directory / "3.2" / "history.json"), stdin="[]")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["next_case"]["concurrency"], 30)

    def test_preview_with_history(self):
        prior = self.history("3.2", [(20, 80), (40, 120)])
        self.assertEqual(self.preview("--history", prior, "3.2"), [("3.2", 30)])

    def mock_executor(self):
        # Use a fixture in place of the real cluster-facing shell.
        shutil.copy(PERF / SEARCH, self.root)
        (self.root / "cubebench.sh").write_text(
            'set -eu\n'
            '[[ "$1 $2" == "run -y" ]]\n'
            'echo "$CUBE_BENCH_SLO_CONCURRENCY $CUBE_MASTER_IP" >> "$FIXTURES/calls"\n'
            'cp "$FIXTURES/$3-c${CUBE_BENCH_SLO_CONCURRENCY}.json" "$CUBE_BENCH_SLO_OUTPUT"\n'
        )
        self.env.update(FIXTURES=str(self.root), CUBE_MASTER_IP="192.0.2.1")
        return [sys.executable, str(self.root / SEARCH), "run"]

    def create_raw(self, c, p95, summary_p95=None):
        rows = [dict(seq=i, create_ms=p95) for i in range(1, 501)]
        doc = {
            "config": dict(concurrency=c, mode="create-only", total=500, warmup=0, template="tpl-test"),
            "summary": dict(successful=500, errors=0),
            "create": {"count": 500, "p95": p95 if summary_p95 is None else summary_p95},
            "raw": rows,
        }
        (self.root / f"3.2-c{c}.json").write_text(json.dumps(doc))

    def test_rejects_invalid_input_before_measurement(self):
        runner = self.mock_executor()
        self.env["CUBE_TEMPLATE_ID"] = "tpl-test"
        for args in [("--typo",), ("3.3",), ("-y", "4.3"), ("-y", "--history", "missing", "3.2"),
                     ("-y", "--settle", "-1"), ("-y", "--settle", "nan"), ("-y", "--settle", "inf")]:
            with self.subTest(args=args):
                result = self.invoke(runner, *args)
                self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "calls").exists())

    def test_invalid_settle_environment_can_be_overridden(self):
        self.env["CUBE_BENCH_SLO_SETTLE"] = "invalid"
        command = ["bash", str(PERF / "cubebench.sh"), "search", "--preview", "3.2"]
        result = self.invoke(command)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--settle", result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertEqual(self.invoke(command, "--settle", "0").returncode, 0)

    def test_settle_only_between_rounds_of_the_same_section(self):
        prior = self.history("4.3", [(1, 150)])
        self.history("4.6", [(1, 150)])
        env = dict(CUBE_TEMPLATE_ID="tpl-test", CUBE_SNAPSHOT_ID="snap-test")
        for options, env_value, seconds in [((), None, 5), ((), "1.5", 1.5), (("--settle", "0"), "5", 0)]:
            with self.subTest(options=options, env=env_value):
                if env_value is not None:
                    env["CUBE_BENCH_SLO_SETTLE"] = env_value
                events = []

                def measure(section, concurrency, directory):
                    events.append(section)
                    return reading(concurrency, 150)

                with patch.dict(os.environ, env, clear=True), \
                        patch("cubebench_slo_search.tempfile.gettempdir", return_value=str(self.root)), \
                        patch("cubebench_slo_search.time.sleep", side_effect=events.append), \
                        patch("cubebench_slo_search.run_round", side_effect=measure), patch("builtins.print"):
                    run_search(["-y", "--history", prior, *options, "4"])
                expected = []
                for section in ("4.3", "4.6"):
                    expected.extend([section, seconds, section, seconds, section] if seconds else [section] * 3)
                self.assertEqual(events, expected)

    def test_live_search_requires_confirmation(self):
        runner = self.mock_executor()
        self.env["CUBE_TEMPLATE_ID"] = "tpl-test"
        self.invoke(runner, "3.2", stdin="no\n")
        self.assertFalse((self.root / "calls").exists())

    def test_search_records_fresh_rounds_and_writes_report(self):
        runner = self.mock_executor()
        self.env["CUBE_TEMPLATE_ID"] = "tpl-test"
        prior = self.history("3.2", [(1, 100)] * 3 + [(2, 110)])
        self.create_raw(1, 100)
        self.create_raw(2, 110)
        result = self.invoke(runner, "-y", "--history", prior, "3.2")
        self.assertEqual(result.returncode, 0, result.stderr)
        rounds = [1, 2, 1, 1, 2, 2]
        self.assertEqual((self.root / "calls").read_text().splitlines(), [f"{c} 192.0.2.1" for c in rounds])
        measured, = self.root.glob("cubebench-search-[0-9]*/3.2/history.json")
        self.assertEqual([r["concurrency"] for r in json.loads(measured.read_text())], rounds)
        curve = measured.with_name("curve.csv").read_text().splitlines()
        self.assertEqual(curve[0], "round,concurrency,p95_ms")
        self.assertEqual([list(map(float, row.split(","))) for row in curve[1:]],
                         [[number, c, 100 if c == 1 else 110] for number, c in enumerate(rounds, 1)])
        verdict = json.loads(measured.with_name("verdict.json").read_text())
        self.assertEqual((verdict["status"], verdict["confirmed_concurrency"]), ("confirmed_boundary", 1))
        self.assertIn("3.2", Path(self.env["OUTPUT_MD"]).read_text())

    def create_chapter_four_fixtures(self):
        def operation(p95):
            return dict(summary=dict(requests=500, successful=500, errors=0, skipped=0, latency_samples=500, p95=p95),
                        raw=[dict(seq=i, round=1, latency_ms=p95, error=None) for i in range(1, 501)])

        config = dict(concurrency=1, requests_per_round=500, rounds=1, warmup=True,
                      percentile_method="nearest-rank", snapshot_id="snap-test", template_id="tpl-test")
        header = dict(config=config, valid=True, status="complete", failure=None, cleanup_errors=[])
        create = operation(111)
        for row in create["raw"]:
            row["sandbox_id"] = f"sb-{row['seq']}"
        warmup = {"raw": [dict(seq=1, latency_ms=4, error=None, sandbox_id="sb-warm")]}
        (self.root / "4.3-c1.json").write_text(json.dumps(dict(header, **create, warmup_diagnostic=warmup)))
        operations = dict(pause=operation(88), resume=operation(144))
        (self.root / "4.6-c1.json").write_text(json.dumps(
            dict(header, operations=operations, warmup_diagnostic=dict(pause=warmup, resume=warmup))))

    def test_chapter_four_records_snapshot_create_and_resume_p95(self):
        runner = self.mock_executor()
        self.env.update(CUBE_TEMPLATE_ID="tpl-test", CUBE_SNAPSHOT_ID="snap-test")
        prior = self.history("4.3", [(1, 150)])
        self.history("4.6", [(1, 150)])
        self.create_chapter_four_fixtures()
        result = self.invoke(runner, "-y", "4", "--history", prior)
        self.assertEqual(result.returncode, 0, result.stderr)
        for section, expected in (("4.3", 111), ("4.6", 144)):
            history, = self.root.glob(f"cubebench-search-[0-9]*/{section}/history.json")
            self.assertEqual([row["p95_ms"] for row in json.loads(history.read_text())], [expected] * 3)

    def test_failed_section_keeps_other_results_and_previous_latest(self):
        runner = self.mock_executor()
        self.env.update(CUBE_TEMPLATE_ID="tpl-test", CUBE_SNAPSHOT_ID="snap-test")
        prior = Path(self.history("4.6", [(1, 150)]))
        self.create_chapter_four_fixtures()
        latest = self.root / "cubebench-search-latest"
        latest.symlink_to(prior, target_is_directory=True)
        for issue, expected in (("missing history", "history.json"), ("missing snapshot", "CUBE_SNAPSHOT_ID"),
                                ("failed round", "benchmark exited")):
            with self.subTest(issue=issue):
                if issue == "missing snapshot":
                    self.history("4.3", [(1, 150)])
                    del self.env["CUBE_SNAPSHOT_ID"]
                if issue == "failed round":
                    self.env["CUBE_SNAPSHOT_ID"] = "snap-test"
                    (self.root / "4.3-c1.json").unlink()
                result = self.invoke(runner, "-y", "--history", str(prior), "4")
                self.assertNotEqual(result.returncode, 0)
                report = Path(self.env["OUTPUT_MD"]).read_text()
                self.assertIn("4.3: FAILED", report)
                self.assertIn(expected, report)
                self.assertIn("4.6: lowest_concurrency_exceeded_limit", report)
                self.assertEqual(latest.resolve(), prior.resolve())

    def test_latest_bootstrap_refresh_and_failed_run(self):
        runner = self.mock_executor()
        self.env["CUBE_TEMPLATE_ID"] = "tpl-test"
        link = self.root / "cubebench-search-latest"
        self.assertEqual(self.preview("--history", "latest", "3.2"), [("3.2", 16)])
        self.assertFalse(link.is_symlink())
        for c in (16, 8, 4, 2, 1):
            self.create_raw(c, 150)
        for history_args in (["--history", "latest"], ["--no-history"]):
            result = self.invoke(runner, "-y", "3.2", *history_args)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(link.is_symlink())
            if history_args[0] == "--history":
                first = link.resolve()
            else:
                self.assertNotEqual(first, link.resolve())
        self.assertEqual(self.preview("--history", "latest"), [("3.2", 1), ("4.3", 16), ("4.6", 16)])
        self.assertEqual(self.preview(), self.preview("--history", "latest"))
        self.assertEqual(self.preview("--no-history", "3.2"), [("3.2", 16)])
        previous = link.resolve()
        self.create_raw(1, 150, summary_p95=50)
        self.assertNotEqual(self.invoke(runner, "-y", "3.2").returncode, 0)
        self.assertEqual(link.resolve(), previous)

    def test_latest_does_not_overwrite_existing_file(self):
        runner = self.mock_executor()
        self.env["CUBE_TEMPLATE_ID"] = "tpl-test"
        link = self.root / "cubebench-search-latest"
        link.write_text("keep")
        self.assertNotEqual(self.invoke(runner, "-y", "3.2").returncode, 0)
        self.assertEqual(link.read_text(), "keep")
        self.assertFalse((self.root / "calls").exists())

    def test_inconsistent_raw_p95_stops_before_next_round(self):
        runner = self.mock_executor()
        self.env["CUBE_TEMPLATE_ID"] = "tpl-test"
        self.create_raw(16, 110, summary_p95=50)
        result = self.invoke(runner, "-y", "3.2")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / "calls").read_text().splitlines(), ["16 192.0.2.1"])
        self.assertEqual(len(list(self.root.glob("cubebench-search-[0-9]*/3.2/*/raw.json"))), 1)
        self.assertFalse(list(self.root.glob("cubebench-search-[0-9]*/3.2/verdict.json")))


if __name__ == "__main__":
    unittest.main()
