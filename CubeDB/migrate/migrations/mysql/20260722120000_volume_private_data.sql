-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add private_data to t_cube_volume for Create→Attach plugin opaque state.
-- Idempotent for clusters that already have t_cube_volume from
-- 20260702050000_create_volume_table (with or without this column).
-- Column inherits table utf8mb4 (same as token).

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260722120000_volume_private_data', 60);

CALL cubemaster_assert_table_exists('t_cube_volume');

CALL cubemaster_add_column_if_missing(
  't_cube_volume',
  'private_data',
  "varchar(1024) NOT NULL DEFAULT '' COMMENT 'opaque plugin state from Create; forwarded to Attach' AFTER `token`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260722120000_volume_private_data');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260722120000_volume_private_data', 60);

CALL cubemaster_drop_column_if_exists('t_cube_volume', 'private_data');

SELECT RELEASE_LOCK('cubemaster_migration_20260722120000_volume_private_data');
