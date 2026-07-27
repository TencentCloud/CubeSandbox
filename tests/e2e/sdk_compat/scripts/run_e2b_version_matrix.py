#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run the SDK compatibility suite against several pinned E2B SDK versions.

A single environment can only hold one version of a package, so the matrix is
driven from outside pytest: one virtualenv per requirement line, each running
the same case selection against the same live backend, then a merged table.

    python3 scripts/run_e2b_version_matrix.py --label v0.6.0

Writes reports/e2b-matrix/matrix.md and matrix.json. Requires the same live
environment as a normal e2b-backend run (CUBE_API_URL, CUBE_TEMPLATE_ID,
E2B_API_KEY).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from xml.etree import ElementTree

SUITE_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_VERSIONS_FILE = SUITE_ROOT / "e2b-versions.txt"
DEFAULT_REPORT_DIR = SUITE_ROOT / "reports" / "e2b-matrix"
DEFAULT_WORKDIR = SUITE_ROOT / ".venv-matrix"
DEFAULT_MARKERS = "smoke or p0"
REQUIRED_ENV = ("CUBE_API_URL", "CUBE_TEMPLATE_ID", "E2B_API_KEY")
TRACKED_PACKAGES = ("e2b", "e2b-code-interpreter")


@dataclass
class RowResult:
    spec: str
    installed: dict[str, str] = field(default_factory=dict)
    status: str = "error"
    passed: int = 0
    failed: int = 0
    skipped: int = 0
    duration_s: float = 0.0
    markers: str = ""
    failed_cases: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    detail: str = ""


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument(
        "--versions-file",
        type=Path,
        default=DEFAULT_VERSIONS_FILE,
        help=f"file of pip requirement lines, one row per line (default: {DEFAULT_VERSIONS_FILE.name})",
    )
    parser.add_argument(
        "--spec",
        action="append",
        default=[],
        metavar="REQUIREMENT",
        help="requirement line to run instead of the file; repeatable",
    )
    parser.add_argument("-m", "--markers", default=DEFAULT_MARKERS, help=f"pytest marker expression (default: {DEFAULT_MARKERS!r})")
    parser.add_argument(
        "--pytest-arg",
        action="append",
        default=[],
        help="extra argument passed to pytest, written as --pytest-arg=-x so argparse keeps the leading dash; repeatable",
    )
    parser.add_argument("--label", default="", help="CubeSandbox version the matrix was produced against, e.g. v0.6.0")
    parser.add_argument("--workdir", type=Path, default=DEFAULT_WORKDIR, help="directory holding the per-version virtualenvs")
    parser.add_argument("--report-dir", type=Path, default=DEFAULT_REPORT_DIR, help="directory for matrix.md, matrix.json and per-row reports")
    parser.add_argument("--python", default=sys.executable, help="base interpreter used to create the virtualenvs")
    parser.add_argument("--recreate", action="store_true", help="delete and rebuild the virtualenvs instead of reusing them")
    parser.add_argument("--exit-zero", action="store_true", help="always exit 0, even when a version fails (report-only runs)")
    parser.add_argument("--dry-run", action="store_true", help="print the plan without creating environments or running tests")
    return parser.parse_args(argv)


def read_specs(path: Path) -> list[str]:
    specs: list[str] = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.split("#", 1)[0].strip()
        if line:
            specs.append(line)
    return specs


def slugify(spec: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]+", "-", spec).strip("-")


def venv_python(venv_dir: Path) -> Path:
    if os.name == "nt":
        return venv_dir / "Scripts" / "python.exe"
    return venv_dir / "bin" / "python"


def ensure_venv(venv_dir: Path, base_python: str, recreate: bool) -> Path:
    python = venv_python(venv_dir)
    if recreate and venv_dir.exists():
        shutil.rmtree(venv_dir)
    if not python.exists():
        venv_dir.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run([base_python, "-m", "venv", str(venv_dir)], check=True)
    return python


def pip_install(python: Path, args: list[str]) -> None:
    subprocess.run([str(python), "-m", "pip", "install", "--quiet", "--disable-pip-version-check", *args], check=True)


def installed_versions(python: Path) -> dict[str, str]:
    code = (
        "import json\n"
        "from importlib import metadata\n"
        "out = {}\n"
        f"for name in {list(TRACKED_PACKAGES)!r}:\n"
        "    try:\n"
        "        out[name] = metadata.version(name)\n"
        "    except metadata.PackageNotFoundError:\n"
        "        pass\n"
        "print(json.dumps(out))\n"
    )
    proc = subprocess.run([str(python), "-c", code], capture_output=True, text=True, check=True)
    return json.loads(proc.stdout.strip() or "{}")


def parse_junit(path: Path) -> tuple[int, int, int, list[str]]:
    """Return (passed, failed, skipped, failed_case_ids) from a pytest JUnit XML."""
    root = ElementTree.parse(path).getroot()
    suites = root.iter("testsuite") if root.tag == "testsuites" else [root]
    passed = failed = skipped = 0
    failed_cases: list[str] = []
    for suite in suites:
        for case in suite.iter("testcase"):
            outcome = "passed"
            for child in case:
                if child.tag in ("failure", "error"):
                    outcome = "failed"
                    break
                if child.tag == "skipped":
                    outcome = "skipped"
            if outcome == "failed":
                failed += 1
                name = case.get("name", "?")
                classname = case.get("classname", "")
                failed_cases.append(f"{classname}::{name}" if classname else name)
            elif outcome == "skipped":
                skipped += 1
            else:
                passed += 1
    return passed, failed, skipped, failed_cases


def run_row(spec: str, args: argparse.Namespace) -> RowResult:
    row = RowResult(spec=spec, markers=args.markers)
    slug = slugify(spec)
    venv_dir = args.workdir / slug
    row_report_dir = args.report_dir / slug
    row_report_dir.mkdir(parents=True, exist_ok=True)
    junit_path = row_report_dir / "junit.xml"
    if junit_path.exists():
        junit_path.unlink()

    print(f"\n=== {spec} ===", flush=True)
    started = time.monotonic()
    try:
        python = ensure_venv(venv_dir, args.python, args.recreate)
        pip_install(python, ["-r", str(SUITE_ROOT / "requirements.txt")])
        pip_install(python, spec.split())
        row.installed = installed_versions(python)
    except subprocess.CalledProcessError as exc:
        row.status = "install-failed"
        row.detail = f"{exc.cmd} exited {exc.returncode}"
        row.duration_s = round(time.monotonic() - started, 1)
        print(f"install failed: {row.detail}", file=sys.stderr, flush=True)
        return row

    markers = args.markers
    if "e2b-code-interpreter" not in row.installed:
        markers = f"({markers}) and not run_code"
        row.notes.append("run_code deselected: e2b-code-interpreter not installed by this row")
    row.markers = markers

    env = dict(os.environ)
    env["SDK_E2E_BACKENDS"] = "e2b"
    env["SDK_E2E_REPORT_DIR"] = str(row_report_dir / "events")
    command = [
        str(python),
        "-m",
        "pytest",
        "--run-e2e",
        "--sdk-e2e-backends=e2b",
        "-m",
        markers,
        f"--junit-xml={junit_path}",
        *args.pytest_arg,
    ]
    print(f"$ {' '.join(command)}", flush=True)
    proc = subprocess.run(command, cwd=str(SUITE_ROOT), env=env)
    row.duration_s = round(time.monotonic() - started, 1)

    if not junit_path.exists():
        row.status = "error"
        row.detail = f"pytest exited {proc.returncode} without writing a report"
        return row

    row.passed, row.failed, row.skipped, row.failed_cases = parse_junit(junit_path)
    if row.failed:
        row.status = "fail"
    elif row.passed == 0:
        row.status = "no-coverage"
        row.detail = "no case produced a result — all skipped, or none selected"
    else:
        row.status = "pass"
    return row


STATUS_CELL = {
    "pass": "pass",
    "fail": "FAIL",
    "install-failed": "install failed",
    "no-coverage": "no coverage",
    "error": "error",
}


def render_markdown(rows: list[RowResult], args: argparse.Namespace, generated_at: str) -> str:
    lines = [
        "# E2B SDK compatibility matrix",
        "",
        f"- CubeSandbox: {args.label or '_unspecified — pass --label_'}",
        f"- Generated: {generated_at}",
        f"- Selection: `-m \"{args.markers}\"`",
        "- Backend: `e2b`",
        "",
        "| Requirement | e2b | e2b-code-interpreter | Result | Passed | Failed | Skipped | Duration | Notes |",
        "| --- | --- | --- | --- | ---: | ---: | ---: | ---: | --- |",
    ]
    for row in rows:
        notes = "; ".join([*row.notes, row.detail] if row.detail else row.notes) or "—"
        lines.append(
            "| `{spec}` | {e2b} | {ci} | {status} | {passed} | {failed} | {skipped} | {duration}s | {notes} |".format(
                spec=row.spec,
                e2b=row.installed.get("e2b", "—"),
                ci=row.installed.get("e2b-code-interpreter", "—"),
                status=STATUS_CELL.get(row.status, row.status),
                passed=row.passed,
                failed=row.failed,
                skipped=row.skipped,
                duration=row.duration_s,
                notes=notes,
            )
        )

    failing = [row for row in rows if row.failed_cases]
    if failing:
        lines += ["", "## Failing cases", ""]
        for row in failing:
            lines.append(f"### `{row.spec}`")
            lines += [f"- `{case}`" for case in row.failed_cases]
            lines.append("")
    lines.append("")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    specs = args.spec or read_specs(args.versions_file)
    if not specs:
        print(f"no requirement lines found in {args.versions_file}", file=sys.stderr)
        return 2

    if args.dry_run:
        print(f"markers: {args.markers}")
        print(f"workdir: {args.workdir}")
        print(f"reports: {args.report_dir}")
        for spec in specs:
            print(f"  {spec}  ->  {args.workdir / slugify(spec)}")
        return 0

    missing = [name for name in REQUIRED_ENV if not os.environ.get(name)]
    if missing:
        print(f"missing required environment: {', '.join(missing)}", file=sys.stderr)
        return 2

    args.report_dir.mkdir(parents=True, exist_ok=True)
    rows = [run_row(spec, args) for spec in specs]
    generated_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    matrix_md = args.report_dir / "matrix.md"
    matrix_md.write_text(render_markdown(rows, args, generated_at), encoding="utf-8")
    (args.report_dir / "matrix.json").write_text(
        json.dumps(
            {
                "cubesandbox": args.label,
                "generated_at": generated_at,
                "markers": args.markers,
                "backend": "e2b",
                "rows": [asdict(row) for row in rows],
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )

    print("\n" + matrix_md.read_text(encoding="utf-8"))
    print(f"wrote {matrix_md} and {args.report_dir / 'matrix.json'}")

    if args.exit_zero:
        return 0
    return 0 if all(row.status == "pass" for row in rows) else 1


if __name__ == "__main__":
    raise SystemExit(main())
