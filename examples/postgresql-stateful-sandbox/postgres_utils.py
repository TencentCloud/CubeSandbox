# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared CubeSandbox and PostgreSQL helpers for the stateful examples."""

import shlex
import time
from pathlib import Path
from typing import Optional

from cubesandbox import (
    CommandResult,
    Config,
    Sandbox,
    SandboxNotFoundError,
    TemplateNotFoundError,
)
from env_utils import require_env

POSTGRES_DATABASE = "postgres"
POSTGRES_SOCKET_DIRECTORY = "/var/run/postgresql"
POSTGRES_USER = "postgres"
SANDBOX_TIMEOUT_SECONDS = 600
DEFAULT_COMMAND_TIMEOUT_SECONDS = 60.0
DEFAULT_READY_TIMEOUT_SECONDS = 60.0


def cube_config() -> Config:
    """Build SDK configuration after validating the two required variables."""
    return Config(
        api_url=require_env("CUBE_API_URL"),
        template_id=require_env("CUBE_TEMPLATE_ID"),
    )


def create_secure_sandbox(
    template: Optional[str] = None,
    *,
    config: Optional[Config] = None,
) -> Sandbox:
    """Create a sandbox with outbound internet and public ingress disabled."""
    sdk_config = config or Config()
    template_id = template or sdk_config.template_id or require_env("CUBE_TEMPLATE_ID")
    return Sandbox.create(
        template=template_id,
        timeout=SANDBOX_TIMEOUT_SECONDS,
        allow_internet_access=False,
        network={"allow_public_traffic": False},
        config=sdk_config,
    )


def run_checked(
    sandbox: Sandbox,
    command: str,
    *,
    timeout: float = DEFAULT_COMMAND_TIMEOUT_SECONDS,
    user: str = POSTGRES_USER,
    cwd: Optional[str] = None,
) -> CommandResult:
    """Run a command and turn a non-zero process result into an exception."""
    result = sandbox.commands.run(
        command,
        timeout=timeout,
        user=user,
        cwd=cwd,
    )
    if result.exit_code != 0:
        raise RuntimeError(
            "command failed with exit code "
            f"{result.exit_code}: {command}\n"
            f"stdout:\n{result.stdout.rstrip()}\n"
            f"stderr:\n{result.stderr.rstrip()}"
        )
    return result


def wait_for_postgres(
    sandbox: Sandbox,
    *,
    timeout: float = DEFAULT_READY_TIMEOUT_SECONDS,
) -> None:
    """Wait until PostgreSQL accepts connections through its Unix socket."""
    deadline = time.monotonic() + timeout
    last_detail = "no readiness attempt completed"
    command = (
        "pg_isready --quiet --timeout=3 "
        f"--host={shlex.quote(POSTGRES_SOCKET_DIRECTORY)} "
        f"--username={shlex.quote(POSTGRES_USER)} "
        f"--dbname={shlex.quote(POSTGRES_DATABASE)}"
    )

    while True:
        try:
            result = sandbox.commands.run(
                command,
                timeout=5,
                user=POSTGRES_USER,
            )
            if result.exit_code == 0:
                return
            last_detail = (
                f"exit_code={result.exit_code}, "
                f"stderr={result.stderr.strip()!r}"
            )
        except Exception as exc:  # noqa: BLE001 - snapshot startup may drop envd
            last_detail = f"{type(exc).__name__}: {exc}"

        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(
                f"PostgreSQL did not become ready within {timeout:.0f}s "
                f"({last_detail})"
            )
        time.sleep(min(1.0, remaining))


def sql(
    sandbox: Sandbox,
    statement: str,
    *,
    database: str = POSTGRES_DATABASE,
) -> str:
    """Execute one SQL statement with psql and return stripped stdout."""
    command = " ".join(
        [
            "psql",
            "-X",
            "--no-align",
            "--tuples-only",
            "--quiet",
            "--set=ON_ERROR_STOP=1",
            f"--host={shlex.quote(POSTGRES_SOCKET_DIRECTORY)}",
            f"--username={shlex.quote(POSTGRES_USER)}",
            f"--dbname={shlex.quote(database)}",
            f"--command={shlex.quote(statement)}",
        ]
    )
    return run_checked(sandbox, command).stdout.strip()


def apply_sql_file(
    sandbox: Sandbox,
    local_path: Path,
    *,
    remote_path: str = "/tmp/migration.sql",
    database: str = POSTGRES_DATABASE,
) -> str:
    """Upload a local SQL file as postgres and execute it with psql."""
    sql_text = local_path.read_text(encoding="utf-8")
    sandbox.files.write(remote_path, sql_text, user=POSTGRES_USER)
    command = " ".join(
        [
            "psql",
            "-X",
            "--quiet",
            "--set=ON_ERROR_STOP=1",
            f"--host={shlex.quote(POSTGRES_SOCKET_DIRECTORY)}",
            f"--username={shlex.quote(POSTGRES_USER)}",
            f"--dbname={shlex.quote(database)}",
            f"--file={shlex.quote(remote_path)}",
        ]
    )
    return run_checked(sandbox, command).stdout.strip()


def checkpoint(sandbox: Sandbox) -> None:
    """Flush a PostgreSQL checkpoint after the caller has committed its work."""
    sql(sandbox, "CHECKPOINT;")


def snapshot_exists(
    snapshot_id: str,
    *,
    config: Config,
    sandbox_id: Optional[str] = None,
) -> bool:
    """Find one snapshot, stopping pagination as soon as its ID appears."""
    next_token = None
    seen_tokens = set()

    while True:
        items, next_token = Sandbox.list_snapshots(
            sandbox_id=sandbox_id,
            limit=100,
            next_token=next_token,
            config=config,
        )
        if any(item.snapshot_id == snapshot_id for item in items):
            return True
        if not next_token:
            return False
        if next_token in seen_tokens:
            raise RuntimeError("snapshot pagination returned a repeated next token")
        seen_tokens.add(next_token)


def wait_for_snapshot(
    snapshot_id: str,
    *,
    config: Config,
    present: bool,
    sandbox_id: Optional[str] = None,
    timeout: float = 30.0,
) -> None:
    """Wait for a snapshot list operation to reflect creation or deletion."""
    deadline = time.monotonic() + timeout
    while True:
        found = snapshot_exists(
            snapshot_id,
            config=config,
            sandbox_id=sandbox_id,
        )
        if found is present:
            return
        if time.monotonic() >= deadline:
            state = "appear in" if present else "disappear from"
            raise TimeoutError(
                f"snapshot {snapshot_id} did not {state} the snapshot list "
                f"within {timeout:.0f}s"
            )
        time.sleep(1.0)


def kill_sandbox(sandbox: Sandbox) -> None:
    """Destroy a sandbox and close local clients; tolerate prior deletion."""
    try:
        sandbox.kill()
    except SandboxNotFoundError:
        pass
    finally:
        sandbox.close()


def delete_snapshot(snapshot_id: str, *, config: Config) -> None:
    """Delete a snapshot; tolerate a snapshot that is already absent."""
    try:
        Sandbox.delete_snapshot(snapshot_id, config=config)
    except TemplateNotFoundError:
        # Cube stores snapshots as templates, and the SDK maps a missing
        # snapshot deletion (DELETE /templates/:id) to this exception.
        pass
