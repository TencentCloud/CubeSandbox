#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run snapshot-isolated MiMo session forks and promote the best patch."""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import secrets
import shlex
import subprocess
import sys
import tempfile
import time
import zlib
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
from typing import Any, Callable, Iterable

from cubesandbox import Sandbox

from _mimo_common import (
    ensure_success,
    run_command,
    run_mimo_command,
    sandbox_identifier,
    session_id_from_events,
)
from env_utils import (
    MIMO_API_KEY_ENV,
    build_mimo_env,
    int_env,
    load_local_dotenv,
    mimo_command,
    mimo_workspace,
    positive_int,
    required,
    shell_join,
)
from network_policy import (
    DEFAULT_NODE_CA_BUNDLE,
    PLACEHOLDER_KEY,
    build_config,
    create_isolated_sandbox,
    create_sandbox,
    show_secret_boundary,
    verify_ca_bundle,
)
from rollout_task import RolloutTask, load_rollout_task

REPORT_PATH = "/tmp/cube-mimo-candidate.txt"
PATCH_PATH = "/tmp/cube-mimo-winner.patch"
CANDIDATE_PATCH_PATH = "/tmp/cube-mimo-candidate.patch"
DEFAULT_TASK_CONFIG = (
    Path(__file__).with_name("fixtures") / "normalize-slug" / "task.json"
)
MAX_CANDIDATES = 8
DEFAULT_MAX_PATCH_BYTES = 256 * 1024
MAX_MIMO_HOME_ARCHIVE_BYTES = 16 * 1024 * 1024
MAX_MIMO_HOME_UNCOMPRESSED_BYTES = 64 * 1024 * 1024

def bounded_test_command(test_command: str, log_path: str) -> str:
    """Keep candidate-controlled test output out of host memory."""
    quoted = shlex.quote(log_path)
    return (
        f"{{ {test_command}; }} > {quoted} 2>&1; "
        f"status=$?; tail -c 4000 {quoted}; exit $status"
    )


@dataclass(frozen=True)
class CandidateSpec:
    name: str
    strategy: str


@dataclass
class CandidateResult:
    name: str
    sandbox_id: str
    session_id: str = ""
    passed: bool = False
    changed_lines: int = 0
    changed_paths: tuple[str, ...] = ()
    patch: str = ""
    test_output: str = ""
    error: str = ""

    def evidence(self) -> dict[str, Any]:
        data = asdict(self)
        data.pop("patch", None)
        data["test_output"] = self.test_output[-4000:]
        return data


def candidate_count(raw: str) -> int:
    value = positive_int(raw)
    if value < 2 or value > MAX_CANDIDATES:
        raise argparse.ArgumentTypeError(
            f"candidate count must be between 2 and {MAX_CANDIDATES}"
        )
    return value


def build_candidate_specs(
    count: int,
    strategies: tuple[tuple[str, str], ...],
) -> list[CandidateSpec]:
    return [
        CandidateSpec(
            name=f"candidate-{index + 1}-{strategies[index % len(strategies)][0]}",
            strategy=strategies[index % len(strategies)][1],
        )
        for index in range(count)
    ]


def changed_paths_from_output(
    output: str,
    allowed_paths: tuple[str, ...],
) -> tuple[str, ...]:
    paths: list[str] = []
    for raw in output.splitlines():
        if not raw:
            continue
        path = PurePosixPath(raw)
        if path.is_absolute() or ".." in path.parts:
            raise ValueError(f"unsafe changed path: {raw!r}")
        normalized = path.as_posix()
        if normalized not in allowed_paths:
            raise ValueError(
                f"candidate changed disallowed path {normalized!r}; "
                f"allowed: {allowed_paths}"
            )
        paths.append(normalized)
    return tuple(sorted(set(paths)))


def changed_lines_from_numstat(output: str) -> int:
    total = 0
    for line in output.splitlines():
        if not line:
            continue
        parts = line.split("\t", 2)
        if len(parts) != 3 or not parts[0].isdigit() or not parts[1].isdigit():
            raise ValueError(f"unsupported git numstat line: {line!r}")
        total += int(parts[0]) + int(parts[1])
    return total


def validate_patch(
    patch: str,
    changed_paths: tuple[str, ...],
    *,
    allowed_paths: tuple[str, ...],
    max_bytes: int,
) -> None:
    if not patch.strip():
        raise ValueError("candidate produced no patch")
    size = len(patch.encode("utf-8"))
    if size > max_bytes:
        raise ValueError(f"candidate patch is {size} bytes; limit is {max_bytes}")
    if not changed_paths:
        raise ValueError("candidate patch has no changed paths")
    with tempfile.TemporaryDirectory(prefix="cube-mimo-parse-") as directory:
        parsed = subprocess.run(
            ["git", "apply", "--numstat", "-z"],
            input=patch.encode("utf-8"),
            cwd=directory,
            check=False,
            capture_output=True,
        )
    if parsed.returncode != 0:
        raise ValueError("Git could not parse the final candidate patch")
    records = [record for record in parsed.stdout.split(b"\0") if record]
    if not records:
        raise ValueError("final patch must modify at least one file")
    parsed_paths: list[str] = []
    for record in records:
        fields = record.split(b"\t", 2)
        if (
            len(fields) != 3
            or not fields[0].isdigit()
            or not fields[1].isdigit()
        ):
            raise ValueError("Git parsed a binary or malformed patch")
        try:
            parsed_path = fields[2].decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("Git parsed a non-UTF-8 patch destination") from exc
        if parsed_path not in allowed_paths:
            raise ValueError(
                f"Git parsed disallowed patch destination {parsed_path!r}"
            )
        parsed_paths.append(parsed_path)
    if tuple(sorted(parsed_paths)) != tuple(sorted(changed_paths)):
        raise ValueError("parsed patch paths do not match the candidate worktree")
    lines = patch.splitlines()
    diff_headers = [line for line in lines if line.startswith("diff --git ")]
    expected_headers = [
        f"diff --git a/{path} b/{path}" for path in sorted(changed_paths)
    ]
    if sorted(diff_headers) != expected_headers:
        raise ValueError("final patch contains a disallowed or ambiguous path")
    for path in changed_paths:
        if f"--- a/{path}" not in lines or f"+++ b/{path}" not in lines:
            raise ValueError(
                f"final patch does not modify the existing file {path!r}"
            )
    forbidden = (
        "old mode ",
        "new mode ",
        "deleted file mode ",
        "new file mode ",
        "rename from ",
        "rename to ",
        "copy from ",
        "copy to ",
        "GIT binary patch",
        "Binary files ",
    )
    if any(line.startswith(forbidden) for line in lines):
        raise ValueError("final patch contains a mode, rename, or binary change")


def validate_exported_patch(
    sandbox: Any,
    workspace: str,
    allowed_paths: tuple[str, ...],
) -> None:
    """Ask Git to parse the final patch and enforce the task's path policy."""
    command = f"""python3 - <<'PY'
import subprocess

patch = {CANDIDATE_PATCH_PATH!r}
allowed = set({allowed_paths!r})
result = subprocess.run(
    ["git", "apply", "--numstat", "-z", patch],
    cwd={workspace!r},
    check=True,
    capture_output=True,
)
records = [record for record in result.stdout.split(b"\\0") if record]
if not records:
    raise SystemExit("patch must contain at least one file")
seen = set()
for record in records:
    fields = record.split(b"\\t", 2)
    if len(fields) != 3 or not fields[0].isdigit() or not fields[1].isdigit():
        raise SystemExit("binary or malformed patches are not allowed")
    path = fields[2].decode("utf-8")
    if path not in allowed:
        raise SystemExit(f"patch destination is not allowed: {{path}}")
    if path in seen:
        raise SystemExit(f"patch contains duplicate destination: {{path}}")
    seen.add(path)
subprocess.run(
    ["git", "apply", "--check", "--reverse", patch],
    cwd={workspace!r},
    check=True,
)
PY"""
    result = run_command(sandbox, command, cwd=workspace, timeout=60)
    ensure_success(result, "parse and validate the final candidate patch")


def choose_winner(results: Iterable[CandidateResult]) -> CandidateResult:
    eligible = [
        result
        for result in results
        if result.passed and not result.error and result.patch
    ]
    if not eligible:
        raise ValueError("no candidate passed tests and patch validation")
    return min(eligible, key=lambda result: (result.changed_lines, result.name))


def create_candidate_sandboxes(
    specs: list[CandidateSpec],
    create_one: Callable[[CandidateSpec], Any],
    *,
    concurrency: int,
    on_created: Callable[[CandidateSpec, Any], None] | None = None,
) -> list[tuple[CandidateSpec, Any]]:
    """Create every candidate or clean up all partial successes."""
    pool = ThreadPoolExecutor(max_workers=min(concurrency, len(specs)))
    futures = {pool.submit(create_one, spec): spec for spec in specs}
    primary_error: BaseException | None = None
    try:
        for future in as_completed(futures):
            try:
                future.result()
            except BaseException as exc:  # noqa: BLE001 - drain before cleanup
                primary_error = exc
                for pending in futures:
                    pending.cancel()
                break
    except BaseException as exc:  # noqa: BLE001 - SIGINT must still clean up
        primary_error = exc
        for pending in futures:
            pending.cancel()
    finally:
        pool.shutdown(wait=True, cancel_futures=True)

    created: list[tuple[CandidateSpec, Any]] = []
    if primary_error is None:
        for future, spec in futures.items():
            try:
                sandbox = future.result()
                created.append((spec, sandbox))
                if on_created is not None:
                    on_created(spec, sandbox)
            except BaseException as exc:  # noqa: BLE001 - capture late failures
                primary_error = exc
                break

    if primary_error is not None:
        for future, spec in futures.items():
            if future.cancelled() or not future.done():
                continue
            try:
                sandbox = future.result()
            except BaseException:
                continue
            if all(existing is not sandbox for _, existing in created):
                created.append((spec, sandbox))
                if on_created is not None:
                    on_created(spec, sandbox)
        cleanup_errors = cleanup_sandboxes(sandbox for _, sandbox in created)
        cleanup_suffix = (
            "; cleanup failures: " + "; ".join(cleanup_errors)
            if cleanup_errors
            else ""
        )
        raise RuntimeError(
            f"failed to create all candidate sandboxes: {primary_error}"
            f"{cleanup_suffix}"
        ) from primary_error
    return sorted(created, key=lambda item: item[0].name)


def seed_project(sandbox: Any, task: RolloutTask, workspace: str) -> None:
    """Copy a task fixture into one sandbox and commit its fixed baseline."""
    fixture_files = task.fixture_files()
    directories = {
        PurePosixPath(workspace),
        *(
            PurePosixPath(workspace) / PurePosixPath(relative).parent
            for relative, _content in fixture_files
        ),
    }
    prepare = run_command(
        sandbox,
        "mkdir -p -- "
        + " ".join(shlex.quote(path.as_posix()) for path in sorted(directories)),
        timeout=60,
    )
    ensure_success(prepare, "prepare the rollout task workspace")

    root = PurePosixPath(workspace)
    for relative, content in fixture_files:
        sandbox.files.write((root / relative).as_posix(), content)

    command = shell_join(
        "git init -q",
        "git add -- .",
        (
            "git -c user.name='CubeSandbox Example' "
            "-c user.email='example@invalid' "
            "commit -qm 'seed speculative rollout fixture'"
        ),
    )
    result = run_command(sandbox, command, cwd=workspace, timeout=60)
    ensure_success(result, "seed the speculative rollout fixture")
    baseline = run_command(
        sandbox,
        bounded_test_command(task.test_command, "/tmp/cube-mimo-baseline-tests.log"),
        cwd=workspace,
        timeout=task.test_timeout_seconds,
    )
    baseline_passed = getattr(baseline, "exit_code", None) == 0
    if task.expect_baseline_failure == baseline_passed:
        expectation = "fail" if task.expect_baseline_failure else "pass"
        raise SystemExit(
            f"task {task.name!r} baseline tests must {expectation} before rollout"
        )


def run_parent_plan(
    sandbox: Any,
    *,
    task: RolloutTask,
    workspace: str,
    token: str,
    envs: dict[str, str],
    timeout: int,
) -> str:
    prompt = task.parent_prompt(token)
    command = mimo_command(
        prompt,
        workspace=workspace,
        agent="build",
        dangerous=False,
    )
    result, events = run_mimo_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=timeout,
    )
    ensure_success(result, "run the parent MiMo planning session")
    session_id = session_id_from_events(events)
    token_check = run_command(
        sandbox,
        (
            f"grep -R -Fq --exclude-dir=.git -- {shlex.quote(token)} "
            f"{shlex.quote(workspace)}"
        ),
        timeout=60,
    )
    status = run_command(
        sandbox,
        f"git -C {shlex.quote(workspace)} status --porcelain --untracked-files=all",
        timeout=60,
    )
    ensure_success(status, "inspect the parent workspace status")
    dirty = getattr(status, "stdout", "").strip()
    token_exit = getattr(token_check, "exit_code", None)
    if token_exit not in (0, 1):
        ensure_success(token_check, "scan the parent workspace for the token")
    if token_exit == 0 or dirty:
        raise SystemExit(
            "Parent planning session must not persist its continuity token or edit "
            f"the workspace. token_found={token_exit == 0}; "
            f"git_status={dirty or '<clean>'}"
        )
    return session_id


def decode_profile_archive(
    encoded: str,
    *,
    max_uncompressed_bytes: int = MAX_MIMO_HOME_UNCOMPRESSED_BYTES,
) -> bytes:
    try:
        compressed = base64.b64decode(encoded, validate=True)
        decompressor = zlib.decompressobj(16 + zlib.MAX_WBITS)
        archive = decompressor.decompress(
            compressed,
            max_uncompressed_bytes + 1,
        )
    except (ValueError, zlib.error) as exc:
        raise ValueError("parent MiMo session archive is invalid") from exc
    if (
        len(archive) > max_uncompressed_bytes
        or decompressor.unconsumed_tail
        or not decompressor.eof
    ):
        raise ValueError("parent MiMo session archive expands beyond the limit")
    archive += decompressor.flush()
    if len(archive) > max_uncompressed_bytes:
        raise ValueError("parent MiMo session archive expands beyond the limit")
    return archive


def transfer_mimo_home(
    planner: Any,
    source: Any,
    mimo_home: str,
    *,
    real_secret: str,
) -> None:
    """Copy the parent session into a credential-free snapshot source."""
    home = PurePosixPath(mimo_home)
    if not home.is_absolute() or home.name in ("", ".", ".."):
        raise ValueError(f"invalid MIMOCODE_HOME: {mimo_home!r}")
    archive = run_command(
        planner,
        (
            f"tar -C {shlex.quote(home.parent.as_posix())} -czf - "
            f"{shlex.quote(home.name)} | base64 -w 0"
        ),
        timeout=120,
    )
    ensure_success(archive, "export the parent MiMo session")
    encoded = getattr(archive, "stdout", "")
    if not encoded:
        raise ValueError("parent MiMo session archive is empty")
    if len(encoded.encode("ascii", "strict")) > MAX_MIMO_HOME_ARCHIVE_BYTES:
        raise ValueError("parent MiMo session archive exceeds the transfer limit")
    archive_bytes = decode_profile_archive(encoded)
    if real_secret.encode() in archive_bytes:
        raise ValueError("real MiMo key was persisted in the parent profile")

    archive_path = "/tmp/cube-mimo-home.tar.gz.b64"
    source.files.write(archive_path, encoded)
    restore = run_command(
        source,
        shell_join(
            f"mkdir -p {shlex.quote(home.parent.as_posix())}",
            (
                f"base64 -d {archive_path} | "
                f"tar -C {shlex.quote(home.parent.as_posix())} -xzf -"
            ),
            f"rm -f {archive_path}",
        ),
        timeout=120,
    )
    ensure_success(restore, "import the parent MiMo session into the source")


def candidate_prompt(
    spec: CandidateSpec,
    task: RolloutTask,
    workspace: str,
) -> str:
    return task.candidate_prompt(
        candidate_name=spec.name,
        strategy=spec.strategy,
        workspace=workspace,
        report_path=REPORT_PATH,
    )


def events_contain_text(events: list[dict[str, Any]], text: str) -> bool:
    return text in json.dumps(events, ensure_ascii=False)


def evaluate_candidate(
    spec: CandidateSpec,
    sandbox: Any,
    *,
    task: RolloutTask,
    parent_session_id: str,
    continuity_token: str,
    workspace: str,
    envs: dict[str, str],
    timeout: int,
    max_patch_bytes: int,
) -> CandidateResult:
    sandbox_id = sandbox_identifier(sandbox)
    command = mimo_command(
        candidate_prompt(spec, task, workspace),
        workspace=workspace,
        session_id=parent_session_id,
        fork=True,
        agent="build",
    )
    result, events = run_mimo_command(
        sandbox,
        command,
        cwd=workspace,
        envs=envs,
        timeout=timeout,
    )
    ensure_success(result, f"run MiMo fork for {spec.name}")
    child_session_id = session_id_from_events(events)
    if child_session_id == parent_session_id:
        raise ValueError(f"{spec.name} did not create a child MiMo session")
    continuity_marker = f"CONTINUITY={continuity_token}"
    continuity_in_events = events_contain_text(events, continuity_marker)

    report = run_command(
        sandbox,
        shell_join(
            f"grep -Fxq {shlex.quote(f'CANDIDATE={spec.name}')} {REPORT_PATH}",
            f"grep -Fxq {shlex.quote(f'TOKEN={continuity_token}')} {REPORT_PATH}",
        ),
        timeout=60,
    )
    if getattr(report, "exit_code", None) != 0 and not continuity_in_events:
        retry_prompt = (
            f"You omitted the required continuity report. Without changing any "
            f"repository file, write {REPORT_PATH} with exactly two lines: "
            f"CANDIDATE={spec.name} and TOKEN=<the continuity token from the "
            "parent conversation>, then reply with CONTINUITY=<that token>. "
            "Do not read the token from files."
        )
        retry_command = mimo_command(
            retry_prompt,
            workspace=workspace,
            session_id=child_session_id,
            agent="build",
        )
        retry_result, retry_events = run_mimo_command(
            sandbox,
            retry_command,
            cwd=workspace,
            envs=envs,
            timeout=timeout,
        )
        ensure_success(retry_result, f"retry the continuity report for {spec.name}")
        continuity_in_events = events_contain_text(
            retry_events,
            continuity_marker,
        )
        report = run_command(
            sandbox,
            shell_join(
                f"grep -Fxq {shlex.quote(f'CANDIDATE={spec.name}')} {REPORT_PATH}",
                f"grep -Fxq {shlex.quote(f'TOKEN={continuity_token}')} {REPORT_PATH}",
            ),
            timeout=60,
        )
    if getattr(report, "exit_code", None) != 0 and not continuity_in_events:
        ensure_success(report, f"verify conversation continuity for {spec.name}")

    tests = run_command(
        sandbox,
        bounded_test_command(
            task.test_command,
            "/tmp/cube-mimo-candidate-tests.log",
        ),
        cwd=workspace,
        timeout=task.test_timeout_seconds,
    )
    test_output = (
        f"{getattr(tests, 'stdout', '')}\n{getattr(tests, 'stderr', '')}".strip()
    )
    if getattr(tests, "exit_code", None) != 0:
        return CandidateResult(
            name=spec.name,
            sandbox_id=sandbox_id,
            session_id=child_session_id,
            test_output=test_output,
            error="acceptance tests failed",
        )

    prepare = run_command(
        sandbox,
        "git add -N -- .",
        cwd=workspace,
        timeout=60,
    )
    ensure_success(prepare, f"prepare patch paths for {spec.name}")
    names = run_command(
        sandbox,
        "git diff --name-only HEAD -- .",
        cwd=workspace,
        timeout=60,
    )
    ensure_success(names, f"list changed paths for {spec.name}")
    changed_paths = changed_paths_from_output(
        getattr(names, "stdout", ""),
        task.allowed_paths,
    )
    numstat = run_command(
        sandbox,
        "git diff --numstat HEAD -- .",
        cwd=workspace,
        timeout=60,
    )
    ensure_success(numstat, f"score changed lines for {spec.name}")
    changed_lines = changed_lines_from_numstat(getattr(numstat, "stdout", ""))
    diff = run_command(
        sandbox,
        (
            f"git diff --binary --no-ext-diff HEAD -- . > {CANDIDATE_PATCH_PATH} "
            f"&& stat -c '%s' {CANDIDATE_PATCH_PATH} "
            f"&& sha256sum {CANDIDATE_PATCH_PATH} | cut -d' ' -f1"
        ),
        cwd=workspace,
        timeout=60,
    )
    ensure_success(diff, f"export patch for {spec.name}")
    metadata = getattr(diff, "stdout", "").splitlines()
    if len(metadata) != 2:
        raise ValueError(f"could not determine patch metadata for {spec.name}")
    size_text, expected_hash = metadata
    if not size_text.isdigit():
        raise ValueError(f"could not determine patch size for {spec.name}")
    if len(expected_hash) != 64 or any(
        character not in "0123456789abcdef" for character in expected_hash
    ):
        raise ValueError(f"could not determine patch hash for {spec.name}")
    if int(size_text) > max_patch_bytes:
        raise ValueError(
            f"candidate patch is {size_text} bytes; limit is {max_patch_bytes}"
        )
    validate_exported_patch(sandbox, workspace, task.allowed_paths)
    patch = sandbox.files.read(CANDIDATE_PATCH_PATH)
    actual_hash = hashlib.sha256(patch.encode("utf-8")).hexdigest()
    if actual_hash != expected_hash:
        raise ValueError("candidate patch changed while it was being validated")
    validate_patch(
        patch,
        changed_paths,
        allowed_paths=task.allowed_paths,
        max_bytes=max_patch_bytes,
    )

    return CandidateResult(
        name=spec.name,
        sandbox_id=sandbox_id,
        session_id=child_session_id,
        passed=True,
        changed_lines=changed_lines,
        changed_paths=changed_paths,
        patch=patch,
        test_output=test_output,
    )


def failed_candidate(
    spec: CandidateSpec,
    sandbox: Any,
    exc: BaseException,
) -> CandidateResult:
    return CandidateResult(
        name=spec.name,
        sandbox_id=sandbox_identifier(sandbox),
        error=f"{type(exc).__name__}: {exc}"[:1000],
    )


def evaluate_candidates(
    candidates: list[tuple[CandidateSpec, Any]],
    *,
    task: RolloutTask,
    parent_session_id: str,
    continuity_token: str,
    workspace: str,
    envs: dict[str, str],
    timeout: int,
    max_patch_bytes: int,
    concurrency: int,
) -> list[CandidateResult]:
    results: list[CandidateResult] = []
    pool = ThreadPoolExecutor(max_workers=min(concurrency, len(candidates)))
    futures = {
        pool.submit(
            evaluate_candidate,
            spec,
            sandbox,
            task=task,
            parent_session_id=parent_session_id,
            continuity_token=continuity_token,
            workspace=workspace,
            envs=envs,
            timeout=timeout,
            max_patch_bytes=max_patch_bytes,
        ): (spec, sandbox)
        for spec, sandbox in candidates
    }
    try:
        for future in as_completed(futures):
            spec, sandbox = futures[future]
            try:
                results.append(future.result())
            except BaseException as exc:  # noqa: BLE001 - one branch may fail
                results.append(failed_candidate(spec, sandbox, exc))
    except BaseException:
        for future in futures:
            future.cancel()
        pool.shutdown(wait=False, cancel_futures=True)
        raise
    else:
        pool.shutdown(wait=True)
    return sorted(results, key=lambda result: result.name)


def promote_winner(
    source: Any,
    winner: CandidateResult,
    *,
    task: RolloutTask,
    workspace: str,
    force_validation_failure: bool,
) -> bool:
    source.files.write(PATCH_PATH, winner.patch)
    apply_patch = run_command(
        source,
        shell_join(
            f"git apply --check {PATCH_PATH}",
            f"git apply {PATCH_PATH}",
        ),
        cwd=workspace,
        timeout=60,
    )
    ensure_success(apply_patch, f"promote patch from {winner.name}")
    validation_command = (
        "false"
        if force_validation_failure
        else bounded_test_command(
            task.test_command,
            "/tmp/cube-mimo-promotion-tests.log",
        )
    )
    validation = run_command(
        source,
        validation_command,
        cwd=workspace,
        timeout=task.test_timeout_seconds,
    )
    return getattr(validation, "exit_code", None) == 0


def rollback_source(source: Any, snapshot_id: str, workspace: str) -> None:
    source.rollback(snapshot_id)
    check = None
    for attempt in range(1, 6):
        try:
            check = run_command(
                source,
                (
                    f"git -C {shlex.quote(workspace)} status --porcelain "
                    f"> /tmp/cube-mimo-source-status && "
                    f"test ! -s /tmp/cube-mimo-source-status"
                ),
                timeout=60,
            )
            break
        except Exception:
            if attempt == 5:
                raise
            time.sleep(1)
    assert check is not None
    ensure_success(check, "verify rollback restored the clean baseline")


def write_evidence(path: str | None, evidence: dict[str, Any]) -> None:
    text = json.dumps(evidence, indent=2, sort_keys=True) + "\n"
    if path:
        destination = Path(path)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(text, encoding="utf-8")
        print(f"Evidence written to {destination}")
    else:
        print("\n--- speculative rollout evidence ---")
        print(text, end="")


def checkpoint_evidence(path: str | None, evidence: dict[str, Any]) -> None:
    if not path:
        return
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(
        json.dumps(evidence, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def cleanup_sandboxes(sandboxes: Iterable[Any]) -> list[str]:
    items = list(sandboxes)
    errors: list[str] = []
    with ThreadPoolExecutor(max_workers=min(4, len(items) or 1)) as pool:
        futures = {pool.submit(sandbox.kill): sandbox for sandbox in items}
        for future in as_completed(futures):
            sandbox = futures[future]
            sandbox_id = sandbox_identifier(sandbox)
            try:
                future.result()
                print(f"Sandbox {sandbox_id} killed.")
            except Exception as exc:
                errors.append(f"sandbox {sandbox_id}: {exc}")
    return errors


def delete_snapshot_with_retry(
    snapshot_id: str,
    *,
    config: Any,
    attempts: int = 10,
    delay: float = 1.0,
) -> None:
    """Wait for asynchronous sandbox teardown before deleting a snapshot."""
    for attempt in range(1, attempts + 1):
        try:
            Sandbox.delete_snapshot(snapshot_id, config=config)
            return
        except Exception as exc:
            transient = "active runtime ref" in str(exc).lower()
            if not transient or attempt == attempts:
                raise
            time.sleep(delay)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Fork one MiMo planning session into snapshot-isolated candidates, "
            "test them, and promote the smallest passing patch."
        )
    )
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--workspace", default=mimo_workspace())
    parser.add_argument(
        "--task",
        default=str(DEFAULT_TASK_CONFIG),
        help="Path to a rollout task.json with a sibling project/ fixture.",
    )
    parser.add_argument("--candidates", type=candidate_count, default=2)
    parser.add_argument("--concurrency", type=positive_int, default=2)
    parser.add_argument(
        "--sandbox-timeout",
        type=positive_int,
        default=int_env("MIMO_SANDBOX_TIMEOUT", 1800),
    )
    parser.add_argument(
        "--exec-timeout",
        type=positive_int,
        default=int_env("MIMO_AGENT_EXEC_TIMEOUT", 900),
    )
    parser.add_argument(
        "--max-patch-bytes",
        type=positive_int,
        default=DEFAULT_MAX_PATCH_BYTES,
    )
    parser.add_argument("--evidence-file")
    parser.add_argument("--force-promotion-failure", action="store_true")
    parser.add_argument("--raw", action="store_true")
    return parser.parse_args()


def run_rollout(args: argparse.Namespace, task: RolloutTask) -> int:
    """Execute the reusable dual-fork rollout lifecycle for one task."""
    if args.concurrency < args.candidates:
        raise SystemExit(
            "--concurrency must be at least --candidates so candidate MicroVMs "
            "do not sit idle while consuming cluster capacity"
        )

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    api_url = required("E2B_API_URL")
    api_key = required("E2B_API_KEY")
    secret = required(MIMO_API_KEY_ENV)
    egress_rule_name = f"mimo_rollout_{secrets.token_hex(6)}"
    config = build_config(template_id, api_url=api_url, api_key=api_key)
    envs = build_mimo_env(include_secret=False)
    envs[MIMO_API_KEY_ENV] = PLACEHOLDER_KEY
    envs["NODE_EXTRA_CA_CERTS"] = os.environ.get(
        "MIMO_NODE_EXTRA_CA_CERTS", DEFAULT_NODE_CA_BUNDLE
    )

    planner = None
    source = None
    snapshot_id = ""
    candidates: list[tuple[CandidateSpec, Any]] = []
    cleanup_errors: list[str] = []
    primary_error: BaseException | None = None
    evidence: dict[str, Any] = {
        "started_at": datetime.now(timezone.utc).isoformat(),
        "candidate_count": args.candidates,
        "egress_rule_name": egress_rule_name,
        "task": {
            "name": task.name,
            "summary": task.summary,
            "test_command": task.test_command,
            "test_timeout_seconds": task.test_timeout_seconds,
            "allowed_paths": list(task.allowed_paths),
            "strategies": [name for name, _instructions in task.strategies],
        },
        "outcome": "started",
    }
    try:
        source = create_isolated_sandbox(
            template_id,
            args.sandbox_timeout,
            api_url=api_url,
            api_key=api_key,
        )
        evidence["source_sandbox_id"] = sandbox_identifier(source)
        checkpoint_evidence(args.evidence_file, evidence)
        print(f"Source sandbox ready: {evidence['source_sandbox_id']}")
        verify_ca_bundle(source, envs)
        show_secret_boundary(source, envs)
        seed_project(source, task, args.workspace)

        planner = create_sandbox(
            template_id,
            secret,
            args.sandbox_timeout,
            api_url=api_url,
            api_key=api_key,
            rule_name=egress_rule_name,
        )
        evidence["planner_sandbox_id"] = sandbox_identifier(planner)
        checkpoint_evidence(args.evidence_file, evidence)
        print(f"Credentialed planner ready: {evidence['planner_sandbox_id']}")
        verify_ca_bundle(planner, envs)
        show_secret_boundary(planner, envs)
        seed_project(planner, task, args.workspace)

        continuity_token = f"CUBE-MIMO-FORK-{secrets.token_hex(8).upper()}"
        parent_session_id = run_parent_plan(
            planner,
            task=task,
            workspace=args.workspace,
            token=continuity_token,
            envs=envs,
            timeout=args.exec_timeout,
        )
        evidence["parent_session_id"] = parent_session_id
        checkpoint_evidence(args.evidence_file, evidence)
        print(f"Parent MiMo session: {parent_session_id}")

        transfer_mimo_home(
            planner,
            source,
            envs["MIMOCODE_HOME"],
            real_secret=secret,
        )
        planner_cleanup = cleanup_sandboxes([planner])
        if planner_cleanup:
            raise RuntimeError(
                "failed to clean credentialed planner: " + "; ".join(planner_cleanup)
            )
        planner = None
        evidence["snapshot_secret_boundary"] = "credential-free source request"

        snapshot = source.create_snapshot()
        snapshot_id = snapshot.snapshot_id
        evidence["snapshot_id"] = snapshot_id
        checkpoint_evidence(args.evidence_file, evidence)
        print(f"Baseline snapshot: {snapshot_id}")

        specs = build_candidate_specs(args.candidates, task.strategies)
        evidence["candidates"] = []
        checkpoint_evidence(args.evidence_file, evidence)

        def record_created_candidate(spec: CandidateSpec, sandbox: Any) -> None:
            evidence["candidates"].append(
                {
                    "name": spec.name,
                    "sandbox_id": sandbox_identifier(sandbox),
                }
            )
            checkpoint_evidence(args.evidence_file, evidence)

        candidates = create_candidate_sandboxes(
            specs,
            lambda _spec: create_sandbox(
                snapshot_id,
                secret,
                args.sandbox_timeout,
                api_url=api_url,
                api_key=api_key,
                rule_name=egress_rule_name,
            ),
            concurrency=args.concurrency,
            on_created=record_created_candidate,
        )
        print(f"Created {len(candidates)} snapshot-isolated candidates.")

        results = evaluate_candidates(
            candidates,
            task=task,
            parent_session_id=parent_session_id,
            continuity_token=continuity_token,
            workspace=args.workspace,
            envs=envs,
            timeout=args.exec_timeout,
            max_patch_bytes=args.max_patch_bytes,
            concurrency=args.concurrency,
        )
        candidate_cleanup = cleanup_sandboxes(
            sandbox for _, sandbox in candidates
        )
        if candidate_cleanup:
            raise RuntimeError(
                "failed to clean evaluated candidates: "
                + "; ".join(candidate_cleanup)
            )
        candidates = []
        evidence["candidates"] = [result.evidence() for result in results]
        for result in results:
            status = "passed" if result.passed else f"rejected: {result.error}"
            print(
                f"{result.name} ({result.sandbox_id}, "
                f"{result.session_id or 'no session'}): "
                f"{status}; changed_lines={result.changed_lines}"
            )

        child_ids = [result.session_id for result in results if result.session_id]
        if len(child_ids) != len(set(child_ids)):
            raise ValueError("candidate MiMo child session IDs are not unique")
        winner = choose_winner(results)
        evidence["winner"] = winner.name
        print(f"Selected winner: {winner.name} ({winner.changed_lines} changed lines)")

        promoted = promote_winner(
            source,
            winner,
            task=task,
            workspace=args.workspace,
            force_validation_failure=args.force_promotion_failure,
        )
        if not promoted:
            rollback_source(source, snapshot_id, args.workspace)
            evidence["outcome"] = "rolled_back"
            write_evidence(args.evidence_file, evidence)
            if args.force_promotion_failure:
                print("CUBE_MIMO_ROLLBACK_OK")
                return 0
            raise SystemExit("winner failed source validation; source was rolled back")

        evidence["outcome"] = "promoted"
        write_evidence(args.evidence_file, evidence)
        print("CUBE_MIMO_PROMOTION_OK")
        return 0
    except BaseException as exc:
        primary_error = exc
        raise
    finally:
        cleanup_errors.extend(cleanup_sandboxes(sandbox for _, sandbox in candidates))
        if planner is not None:
            cleanup_errors.extend(cleanup_sandboxes([planner]))
        if source is not None:
            cleanup_errors.extend(cleanup_sandboxes([source]))
        if snapshot_id:
            try:
                delete_snapshot_with_retry(snapshot_id, config=config)
                print(f"Snapshot {snapshot_id} deleted.")
            except Exception as exc:
                cleanup_errors.append(f"snapshot {snapshot_id}: {exc}")
        if cleanup_errors:
            message = "Cleanup failures: " + "; ".join(cleanup_errors)
            if primary_error is not None:
                print(f"Warning: {message}", file=sys.stderr)
            else:
                raise SystemExit(message)


def main() -> int:
    """Load one task profile and invoke the reference rollout pattern."""
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["MIMO_STREAM_RAW"] = "1"
    task = load_rollout_task(args.task)
    return run_rollout(args, task)


if __name__ == "__main__":
    sys.exit(main())
