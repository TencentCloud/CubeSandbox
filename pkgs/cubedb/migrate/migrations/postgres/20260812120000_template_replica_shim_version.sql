-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Template replica shim version pin (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260812120000_shim_ver', 60);

SELECT cubemaster_assert_table_exists('t_cube_template_replica');

SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'shim_version', 'varchar(128) NOT NULL DEFAULT ''''');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260812120000_shim_ver'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260812120000_shim_ver', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'shim_version');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260812120000_shim_ver'));
