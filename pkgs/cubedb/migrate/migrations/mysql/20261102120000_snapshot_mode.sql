-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Snapshot artifacts carry an explicit mode. Historical rows are full.

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261102120000_snapshot_mode', 60);
CALL cubemaster_add_column_if_missing(
  't_cube_snapshot', 'mode',
  "varchar(16) NOT NULL DEFAULT 'full' COMMENT 'snapshot artifact mode: full|fs-only'"
);
SELECT RELEASE_LOCK('cubemaster_migration_20261102120000_snapshot_mode');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261102120000_snapshot_mode', 60);
CALL cubemaster_drop_column_if_exists('t_cube_snapshot', 'mode');
SELECT RELEASE_LOCK('cubemaster_migration_20261102120000_snapshot_mode');
