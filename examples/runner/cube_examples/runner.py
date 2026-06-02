"""Execution engine: run an example's setup + steps and assert outcomes."""

from __future__ import annotations

import dataclasses
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from .manifest import Manifest, Step

# Created lazily: a directory containing a `python` shim that points at the
# interpreter currently running the runner. Many example READMEs invoke
# `python xxx.py`, but some hosts only ship `python3`. Rather than rewriting
# every manifest, we make `python` resolvable on PATH for the duration of a run.
_PYTHON_SHIM_DIR: str | None = None


def _python_shim_dir() -> str:
    """Return a dir with a `python` shim if `python` is not already on PATH."""
    global _PYTHON_SHIM_DIR
    if _PYTHON_SHIM_DIR is not None:
        return _PYTHON_SHIM_DIR
    from shutil import which

    if which("python"):
        _PYTHON_SHIM_DIR = ""
        return ""
    shim_dir = tempfile.mkdtemp(prefix="cube-examples-shim-")
    shim_path = Path(shim_dir) / "python"
    shim_path.write_text(
        f'#!/bin/sh\nexec "{sys.executable}" "$@"\n', encoding="utf-8"
    )
    shim_path.chmod(0o755)
    _PYTHON_SHIM_DIR = shim_dir
    return shim_dir


@dataclasses.dataclass
class StepResult:
    name: str
    command: str
    exit_code: int | None
    duration_s: float
    passed: bool
    failures: list[str] = dataclasses.field(default_factory=list)
    stdout_tail: str = ""
    stderr_tail: str = ""
    timed_out: bool = False


@dataclasses.dataclass
class ExampleResult:
    name: str
    path: str
    tags: list[str]
    status: str  # passed | failed | skipped | error
    duration_s: float
    steps: list[StepResult] = dataclasses.field(default_factory=list)
    message: str = ""

    @property
    def passed(self) -> bool:
        return self.status == "passed"


def _tail(text: str, max_chars: int = 4000) -> str:
    text = text or ""
    if len(text) <= max_chars:
        return text
    return "... [truncated] ...\n" + text[-max_chars:]


def _run_command(
    command: str,
    cwd: Path,
    env: dict[str, str],
    timeout: int,
) -> tuple[int | None, str, str, bool]:
    """Run a shell command, returning (exit_code, stdout, stderr, timed_out)."""
    try:
        proc = subprocess.run(
            command,
            shell=True,
            cwd=str(cwd),
            env=env,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        return proc.returncode, proc.stdout, proc.stderr, False
    except subprocess.TimeoutExpired as exc:
        out = exc.stdout or ""
        err = exc.stderr or ""
        if isinstance(out, bytes):
            out = out.decode("utf-8", "replace")
        if isinstance(err, bytes):
            err = err.decode("utf-8", "replace")
        return None, out, err, True


def _assert_step(step: Step, exit_code: int | None, stdout: str, timed_out: bool) -> list[str]:
    failures: list[str] = []
    if timed_out:
        failures.append("step timed out")
        return failures
    if step.expect_exit is not None and exit_code != step.expect_exit:
        failures.append(f"exit code {exit_code} != expected {step.expect_exit}")
    for needle in step.expect_stdout_contains:
        if needle not in stdout:
            failures.append(f"stdout missing expected text: {needle!r}")
    for needle in step.expect_stdout_not_contains:
        if needle in stdout:
            failures.append(f"stdout contains forbidden text: {needle!r}")
    return failures


def run_example(
    manifest: Manifest,
    base_env: dict[str, str],
    *,
    verbose: bool = False,
    run_setup: bool = True,
) -> ExampleResult:
    """Run a single example end-to-end."""
    start = time.time()
    env = {**os.environ, **base_env, **manifest.env}
    # Ensure `python` resolves to the runner's interpreter when only `python3`
    # exists on the host.
    shim = _python_shim_dir()
    if shim:
        env["PATH"] = shim + os.pathsep + env.get("PATH", "")

    result = ExampleResult(
        name=manifest.name,
        path=str(manifest.path),
        tags=manifest.tags,
        status="passed",
        duration_s=0.0,
    )

    if manifest.skip:
        result.status = "skipped"
        result.message = manifest.skip_reason or "skipped by manifest"
        result.duration_s = round(time.time() - start, 2)
        return result

    # Setup phase (e.g. pip install). Failures here mark the example as error.
    if run_setup:
        for setup_cmd in manifest.setup:
            if verbose:
                print(f"    [setup] {setup_cmd}")
            code, out, err, timed_out = _run_command(
                setup_cmd, manifest.path, env, timeout=manifest.timeout
            )
            if timed_out or code != 0:
                result.status = "error"
                result.message = f"setup failed: {setup_cmd!r} (exit={code}, timeout={timed_out})"
                result.steps.append(
                    StepResult(
                        name="setup",
                        command=setup_cmd,
                        exit_code=code,
                        duration_s=0.0,
                        passed=False,
                        failures=[result.message],
                        stdout_tail=_tail(out),
                        stderr_tail=_tail(err),
                        timed_out=timed_out,
                    )
                )
                result.duration_s = round(time.time() - start, 2)
                return result

    # Step phase.
    for step in manifest.steps:
        step_timeout = step.timeout or manifest.timeout
        if verbose:
            print(f"    [step] {step.name}: {step.run}")
        t0 = time.time()
        code, out, err, timed_out = _run_command(step.run, manifest.path, env, step_timeout)
        dur = time.time() - t0
        failures = _assert_step(step, code, out, timed_out)
        passed = not failures
        if step.allow_failure and not passed:
            # Record but do not fail the example.
            passed = True
            failures = [f"(allowed failure) {f}" for f in failures]
        result.steps.append(
            StepResult(
                name=step.name,
                command=step.run,
                exit_code=code,
                duration_s=round(dur, 2),
                passed=passed,
                failures=failures,
                stdout_tail=_tail(out),
                stderr_tail=_tail(err),
                timed_out=timed_out,
            )
        )
        if not passed:
            result.status = "failed"

    result.duration_s = round(time.time() - start, 2)
    return result
