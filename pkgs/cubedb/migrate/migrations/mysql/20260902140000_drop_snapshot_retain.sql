-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Drop the unused retain column from snapshot and template definition
-- tables. It was never written or consumed by control-plane logic.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260902140000_drop_retain', 60);

CALL cubemaster_drop_column_if_exists('t_cube_snapshot', 'retain');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'retain');

SELECT RELEASE_LOCK('cubemaster_migration_20260902140000_drop_retain');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260902140000_drop_retain', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_snapshot',
  'retain',
  "tinyint(1) NOT NULL DEFAULT 0"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'retain',
  "tinyint(1) NOT NULL DEFAULT 0 COMMENT 'retain snapshot from gc' AFTER `storage_backend`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260902140000_drop_retain');
