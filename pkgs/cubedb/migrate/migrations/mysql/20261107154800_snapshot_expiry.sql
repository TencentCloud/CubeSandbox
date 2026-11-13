-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Optional expiry timestamps let the snapshot reconciler retire user snapshots
-- without affecting historical snapshots that have no expiration.

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261107154800_snapshot_expiry', 60);
CALL cubemaster_add_column_if_missing(
  't_cube_snapshot', 'expires_at',
  'datetime(3) NULL DEFAULT NULL'
);
CALL cubemaster_add_index_if_missing(
  't_cube_snapshot', 'idx_cube_snapshot_expires_at',
  'ADD INDEX `idx_cube_snapshot_expires_at` (`expires_at`)'
);
SELECT RELEASE_LOCK('cubemaster_migration_20261107154800_snapshot_expiry');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261107154800_snapshot_expiry', 60);
CALL cubemaster_drop_index_if_exists('t_cube_snapshot', 'idx_cube_snapshot_expires_at');
CALL cubemaster_drop_column_if_exists('t_cube_snapshot', 'expires_at');
SELECT RELEASE_LOCK('cubemaster_migration_20261107154800_snapshot_expiry');
