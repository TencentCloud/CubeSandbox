# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run two isolated PostgreSQL schema migrations from one CubeSnapshot."""

from concurrent.futures import Future, ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Optional

from cubesandbox import Config, Sandbox
from env_utils import load_local_dotenv
from postgres_utils import (
    POSTGRES_USER,
    apply_sql_file,
    checkpoint,
    create_secure_sandbox,
    cube_config,
    delete_snapshot,
    kill_sandbox,
    sql,
    wait_for_postgres,
    wait_for_snapshot,
)

EXAMPLE_DIR = Path(__file__).resolve().parent
REMOTE_MIGRATION_PATH = "/tmp/migration.sql"


def create_branches(
    snapshot_id: str,
    config: Config,
    branches: dict[str, Sandbox],
) -> None:
    """Populate a caller-owned map so partial branches remain cleanable."""
    first_error: Optional[BaseException] = None

    with ThreadPoolExecutor(max_workers=2) as executor:
        futures: dict[Future, str] = {
            executor.submit(
                create_secure_sandbox,
                snapshot_id,
                config=config,
            ): branch_name
            for branch_name in ("email", "last_login")
        }
        for future in as_completed(futures):
            branch_name = futures[future]
            try:
                branches[branch_name] = future.result()
                print(f"{branch_name} branch: {branches[branch_name].sandbox_id}")
            except BaseException as exc:  # noqa: BLE001 - collect sibling result
                if first_error is None:
                    first_error = exc

    if first_error is not None:
        raise first_error


def run_migration(sandbox: Sandbox, local_path: Path) -> str:
    """Apply one migration and return the uploaded file for isolation checks."""
    wait_for_postgres(sandbox)
    apply_sql_file(
        sandbox,
        local_path,
        remote_path=REMOTE_MIGRATION_PATH,
    )
    return sandbox.files.read(REMOTE_MIGRATION_PATH, user=POSTGRES_USER)


def assert_branch_state(
    sandbox: Sandbox,
    *,
    expected_column: str,
    expected_migration: str,
) -> None:
    """Assert one branch contains only its own schema and migration record."""
    inherited_rows = sql(
        sandbox,
        "SELECT owner || ':' || balance::text FROM accounts ORDER BY owner;",
    )
    if inherited_rows != "alice:100\nbob:200":
        raise AssertionError(f"branch inherited unexpected rows: {inherited_rows!r}")

    columns = sql(
        sandbox,
        """
        SELECT column_name
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'accounts'
          AND column_name IN ('email', 'last_login_at')
        ORDER BY column_name;
        """,
    )
    if columns != expected_column:
        raise AssertionError(
            f"expected only column {expected_column!r}, got {columns!r}"
        )

    migrations = sql(
        sandbox,
        """
        SELECT string_agg(
            version,
            ',' ORDER BY CASE WHEN version = 'base_v1' THEN 0 ELSE 1 END
        )
        FROM schema_migrations;
        """,
    )
    expected = f"base_v1,{expected_migration}"
    if migrations != expected:
        raise AssertionError(f"expected migrations {expected!r}, got {migrations!r}")


def cleanup_resources(
    source: Optional[Sandbox],
    branches: dict[str, Sandbox],
    snapshot_id: Optional[str],
    config: Config,
) -> None:
    """Attempt every cleanup in dependency order and report any failures."""
    cleanup_errors: list[BaseException] = []
    for branch in branches.values():
        try:
            kill_sandbox(branch)
        except BaseException as exc:  # noqa: BLE001 - attempt every cleanup
            cleanup_errors.append(exc)

    if source is not None:
        try:
            kill_sandbox(source)
        except BaseException as exc:  # noqa: BLE001 - snapshot must still be tried
            cleanup_errors.append(exc)

    if snapshot_id is not None:
        try:
            delete_snapshot(snapshot_id, config=config)
            wait_for_snapshot(snapshot_id, config=config, present=False)
        except BaseException as exc:  # noqa: BLE001 - include cleanup failure
            cleanup_errors.append(exc)

    if cleanup_errors:
        details = "; ".join(
            f"{type(error).__name__}: {error}" for error in cleanup_errors
        )
        raise RuntimeError(f"resource cleanup failed: {details}") from cleanup_errors[0]


def main() -> None:
    load_local_dotenv()
    config = cube_config()
    source: Optional[Sandbox] = None
    branches: dict[str, Sandbox] = {}
    snapshot_id: Optional[str] = None

    try:
        source = create_secure_sandbox(config=config)
        source_id = source.sandbox_id
        print(f"source sandbox: {source_id}")
        wait_for_postgres(source)
        apply_sql_file(source, EXAMPLE_DIR / "sql" / "base_schema.sql")

        source_columns = sql(
            source,
            """
            SELECT count(*)
            FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'accounts'
              AND column_name IN ('email', 'last_login_at');
            """,
        )
        if source_columns != "0":
            raise AssertionError("source unexpectedly contains a branch migration")

        checkpoint(source)
        snapshot = source.create_snapshot()
        snapshot_id = snapshot.snapshot_id
        if not snapshot_id:
            raise RuntimeError("snapshot creation returned an empty snapshot ID")
        print(f"snapshot: {snapshot_id}")
        wait_for_snapshot(
            snapshot_id,
            config=config,
            present=True,
            sandbox_id=source_id,
        )

        kill_sandbox(source)
        source = None

        create_branches(snapshot_id, config, branches)
        branch_ids = {branch.sandbox_id for branch in branches.values()}
        if len(branch_ids) != 2 or source_id in branch_ids:
            raise AssertionError("source and branch sandboxes do not have distinct IDs")

        email_path = EXAMPLE_DIR / "sql" / "add_email.sql"
        last_login_path = EXAMPLE_DIR / "sql" / "add_last_login.sql"
        with ThreadPoolExecutor(max_workers=2) as executor:
            email_future = executor.submit(
                run_migration,
                branches["email"],
                email_path,
            )
            last_login_future = executor.submit(
                run_migration,
                branches["last_login"],
                last_login_path,
            )
            uploaded_email = email_future.result()
            uploaded_last_login = last_login_future.result()

        if uploaded_email != email_path.read_text(encoding="utf-8"):
            raise AssertionError("email branch migration file changed after upload")
        if uploaded_last_login != last_login_path.read_text(encoding="utf-8"):
            raise AssertionError("last-login branch migration file changed after upload")
        if uploaded_email == uploaded_last_login:
            raise AssertionError("branch migration files unexpectedly have identical content")

        assert_branch_state(
            branches["email"],
            expected_column="email",
            expected_migration="add_email",
        )
        assert_branch_state(
            branches["last_login"],
            expected_column="last_login_at",
            expected_migration="add_last_login",
        )
    finally:
        cleanup_resources(source, branches, snapshot_id, config)

    print("OK: isolated PostgreSQL migration branches passed")


if __name__ == "__main__":
    main()
