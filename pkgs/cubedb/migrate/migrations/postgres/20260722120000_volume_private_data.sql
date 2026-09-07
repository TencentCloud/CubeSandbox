-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add private_data to t_cube_volume (PostgreSQL).
-- Idempotent for existing tables via cubemaster_add_column_if_missing.
-- varchar(1024) matches MySQL / token column width.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260722120000_volume_private_data', 60);

SELECT cubemaster_assert_table_exists('t_cube_volume');

SELECT cubemaster_add_column_if_missing('t_cube_volume', 'private_data', 'varchar(1024) NOT NULL DEFAULT ''''');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260722120000_volume_private_data'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260722120000_volume_private_data', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_volume', 'private_data');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260722120000_volume_private_data'));
