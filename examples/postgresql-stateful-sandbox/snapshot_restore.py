# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Restore PostgreSQL data and schema into a new sandbox from a snapshot."""

from pathlib import Path
from typing import Optional

from cubesandbox import Config, Sandbox
from env_utils import load_local_dotenv
from postgres_utils import (
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


def cleanup_resources(
    restore_sandbox: Optional[Sandbox],
    source_sandbox: Optional[Sandbox],
    snapshot_id: Optional[str],
    *,
    config: Config,
) -> None:
    """Best-effort cleanup in dependency order, reporting every failure."""
    errors = []

    for label, sandbox in (
        ("restore sandbox", restore_sandbox),
        ("source sandbox", source_sandbox),
    ):
        if sandbox is None:
            continue
        try:
            kill_sandbox(sandbox)
        except Exception as exc:  # noqa: BLE001 - cleanup must continue
            errors.append(f"{label} {sandbox.sandbox_id}: {exc}")

    if snapshot_id is not None:
        try:
            delete_snapshot(snapshot_id, config=config)
            wait_for_snapshot(snapshot_id, config=config, present=False)
        except Exception as exc:  # noqa: BLE001 - report after all cleanup attempts
            errors.append(f"snapshot {snapshot_id}: {exc}")

    if errors:
        raise RuntimeError("resource cleanup failed: " + "; ".join(errors))


def main() -> None:
    load_local_dotenv()
    config = cube_config()
    source_sandbox: Optional[Sandbox] = None
    restore_sandbox: Optional[Sandbox] = None
    snapshot_id: Optional[str] = None

    try:
        source_sandbox = create_secure_sandbox(config=config)
        source_sandbox_id = source_sandbox.sandbox_id
        print(f"source sandbox: {source_sandbox_id}")

        wait_for_postgres(source_sandbox)
        apply_sql_file(source_sandbox, EXAMPLE_DIR / "sql" / "base_schema.sql")
        baseline = sql(
            source_sandbox,
            "SELECT owner || ':' || balance::text FROM accounts ORDER BY owner;",
        )
        if baseline != "alice:100\nbob:200":
            raise AssertionError(f"unexpected baseline rows: {baseline!r}")

        checkpoint(source_sandbox)
        snapshot = source_sandbox.create_snapshot()
        snapshot_id = snapshot.snapshot_id
        if not snapshot_id:
            raise RuntimeError("snapshot creation returned an empty snapshot ID")
        print(f"snapshot: {snapshot_id}")
        wait_for_snapshot(
            snapshot_id,
            config=config,
            present=True,
            sandbox_id=source_sandbox_id,
        )

        sql(
            source_sandbox,
            """
            BEGIN;
            UPDATE accounts SET balance = 0;
            ALTER TABLE accounts
                ADD COLUMN poisoned boolean NOT NULL DEFAULT true;
            COMMIT;
            """,
        )
        if sql(source_sandbox, "SELECT sum(balance) FROM accounts;") != "0":
            raise AssertionError("the destructive data mutation was not applied")
        if sql(
            source_sandbox,
            """
            SELECT count(*)
            FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'accounts'
              AND column_name = 'poisoned';
            """,
        ) != "1":
            raise AssertionError("the destructive schema mutation was not applied")

        kill_sandbox(source_sandbox)
        source_sandbox = None

        restore_sandbox = create_secure_sandbox(snapshot_id, config=config)
        print(f"restore sandbox: {restore_sandbox.sandbox_id}")
        if restore_sandbox.sandbox_id == source_sandbox_id:
            raise AssertionError("snapshot restore reused the source sandbox ID")
        wait_for_postgres(restore_sandbox)

        restored = sql(
            restore_sandbox,
            "SELECT owner || ':' || balance::text FROM accounts ORDER BY owner;",
        )
        if restored != baseline:
            raise AssertionError(
                f"snapshot restored {restored!r}, expected {baseline!r}"
            )
        poisoned_columns = sql(
            restore_sandbox,
            """
            SELECT count(*)
            FROM information_schema.columns
            WHERE table_schema = 'public'
              AND table_name = 'accounts'
              AND column_name = 'poisoned';
            """,
        )
        if poisoned_columns != "0":
            raise AssertionError("snapshot restore retained the poisoned column")
    finally:
        cleanup_resources(
            restore_sandbox,
            source_sandbox,
            snapshot_id,
            config=config,
        )

    print("OK: snapshot restored PostgreSQL data and schema in a new sandbox")


if __name__ == "__main__":
    main()
