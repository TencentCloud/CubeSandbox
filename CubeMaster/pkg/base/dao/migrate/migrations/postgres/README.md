<!--
Copyright (c) 2026 Tencent Inc.
SPDX-License-Identifier: Apache-2.0
-->

# CubeMaster PostgreSQL Migration Conventions

This directory is applied automatically at process startup by
[`pkg/base/dao/migrate`](../../migrate.go) via
[`github.com/pressly/goose/v3`](https://github.com/pressly/goose).

## Relationship to MySQL migrations

The PostgreSQL migrations produce an **identical logical schema** to their MySQL
counterparts. Each migration version number matches 1:1 with the MySQL version,
ensuring that both engines land on the same HEAD. The SQL syntax differs only
where MySQL and PostgreSQL diverge (data types, DDL constructs, locking
mechanisms, etc.).

## Naming conventions

Same as MySQL: frozen historical block (`0001`–`0010`) followed by UTC
timestamp-prefixed migrations (`YYYYMMDDhhmmss_<description>.sql`).

## Migration content rules

- Add `-- +goose NO TRANSACTION` at the top together with `-- +goose Up`.
  PostgreSQL DDL IS transactional, but we keep the same goose mode for
  consistency and because advisory locks + explicit transactions can interact
  awkwardly.
- Use `IF NOT EXISTS` / `IF EXISTS` for idempotency.
- Wrap every migration with advisory lock acquisition at the top:
  `SELECT cubemaster_acquire_migration_lock('cubemaster_migration_<version>_<name>', 60);`
  and release at the bottom:
  `SELECT pg_advisory_unlock(hashtext('cubemaster_migration_<version>_<name>'));`
- Provide a symmetric `-- +goose Down` (the `0001` baseline is the only
  exception — it is irreversible).

## Type mapping from MySQL

| MySQL | PostgreSQL |
|-------|-----------|
| `bigint unsigned` | `bigint` (with CHECK >= 0 where needed) |
| `int unsigned` | `integer` |
| `tinyint(1)` | `boolean` |
| `mediumtext` / `text` | `text` |
| `varchar(N)` | `varchar(N)` |
| `datetime` | `timestamp` |
| `AUTO_INCREMENT` | `BIGSERIAL` / `SERIAL` |
| `ENGINE=InnoDB` | (removed) |
| `DEFAULT CHARSET=utf8mb3/utf8mb4` | (removed; database encoding) |
| backtick quoting | double-quote or no quoting |
| `GET_LOCK` / `RELEASE_LOCK` | `pg_advisory_lock` / `pg_advisory_unlock` |
| `ON DUPLICATE KEY UPDATE` | `ON CONFLICT ... DO UPDATE` |
| `INSERT IGNORE` | `INSERT ... ON CONFLICT DO NOTHING` |
| `UPDATE ... JOIN` | `UPDATE ... FROM ... WHERE` |
| stored procedures (`CREATE PROCEDURE`) | PL/pgSQL functions (`CREATE FUNCTION`) |
