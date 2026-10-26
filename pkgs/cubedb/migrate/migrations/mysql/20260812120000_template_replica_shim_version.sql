-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Template replica shim version pin (MySQL).

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260812120000_shim_ver', 60);

CALL cubemaster_assert_table_exists('t_cube_template_replica');

CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'shim_version',
  "VARCHAR(128) NOT NULL DEFAULT '' COMMENT 'cube-shim version bound when this replica was created'"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260812120000_shim_ver');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260812120000_shim_ver', 60);

CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'shim_version');

SELECT RELEASE_LOCK('cubemaster_migration_20260812120000_shim_ver');
