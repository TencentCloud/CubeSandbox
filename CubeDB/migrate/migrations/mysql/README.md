<!--
Copyright (c) 2026 Tencent Inc.
SPDX-License-Identifier: Apache-2.0
-->

# CubeMaster MySQL Migration Conventions

This directory is applied automatically at process startup by
[`pkg/base/dao/migrate`](../../migrate.go) via
[`github.com/pressly/goose/v3`](https://github.com/pressly/goose).

## Background: why sequential numbers are no longer allowed

goose uses the numeric prefix of a migration filename as its "version," and the
`goose_db_version` table stores **only this integer**. The apply logic is
`if dbAppliedVersions[v] { continue }`: once an integer version is recorded as
applied, goose **never re-reads that file and never compares its content again**.

Sequential numbers (`0001`, `0002`, …) get reused in concurrent, multi-branch
development. A typical incident:

1. Developer A writes `0010_aaaaa.sql` on top of upstream `0009` and deploys it
   to a staging environment → the DB records version `10`.
2. Upstream merges `0010_bbbb.sql` in parallel.
3. Developer A rebases, renaming `0010_aaaaa.sql` → `0011_aaaaa.sql`.
   On disk: `0010_bbbb.sql` (version 10) + `0011_aaaaa.sql` (version 11).
4. Staging is redeployed: the DB already has version `10` →
   `0010_bbbb.sql` is `continue`-d and **silently skipped, never executed**.

Root cause: **the migration version identity is a reusable / reassignable
integer**.

## Naming conventions

### Frozen historical block (do NOT modify)

`0001` – `0010` are the v0.2.2 baseline and early increments that have already
been applied in every environment. **These filenames and their contents must
never be modified, renamed, or deleted** (CI enforces this). If you need to
correct their behaviour, add a new timestamped migration instead of editing
the old files.

### New migrations: use UTC timestamps

All new migrations **MUST** use a 14-digit UTC timestamp prefix:

```
YYYYMMDDhhmmss_<description>.sql
```

Example: `20260622143000_add_foo_column.sql`.

Rationale:

- A timestamp is determined at write time, globally unique, and **never needs
  renaming on rebase** → the version identity cannot be reused, making the
  incident above structurally impossible.
- Timestamps (e.g. `20260622143000`) are orders of magnitude larger than the
  frozen block's `1`–`10`, so they sort naturally after the historical block,
  preserving ordering.
- **Always use UTC** (e.g. `date -u +%Y%m%d%H%M%S`) to avoid ordering /
  collision ambiguity across developer timezones.

### Creating a new migration

Use the helper script (generates a UTC-timestamped filename and template stub):

```bash
scripts/new-migration.sh add_foo_column
```

Manual creation is also fine, but you must follow the format above and the
content rules below.

## Migration content rules

- Add `-- +goose NO TRANSACTION` at the top (MySQL DDL does implicit commits;
  goose transactions are meaningless) together with `-- +goose Up`.
- Use the idempotent stored procedures provided by the baseline:
  `cubemaster_add_column_if_missing` / `cubemaster_drop_column_if_exists` /
  `cubemaster_add_index_if_missing` / `cubemaster_drop_index_if_exists` /
  `cubemaster_assert_table_exists`, etc. Idempotent migrations are the
  prerequisite for out-of-order application (`WithAllowOutofOrder`) and safe
  manual remediation.
- Wrap every migration with
  `CALL cubemaster_acquire_migration_lock('cubemaster_migration_<version>_<name>', 60);`
  at the top and
  `SELECT RELEASE_LOCK('cubemaster_migration_<version>_<name>');` at the
  bottom. The lock name must match the filename.
- Provide a symmetric `-- +goose Down` (the `0001` baseline is the only
  exception — it is irreversible).

## Applied migrations are immutable (runtime enforcement)

The `migrate` package records a content fingerprint (table
`t_cubemaster_migration_fingerprint`) for every version that goose has
**actually applied**. If the on-disk content of an already-applied version is
later changed, the next startup will **fail loudly** instead of skipping
silently. Operators may temporarily bypass this check by setting
`CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK=1`.
