-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261110110000_jfs_template_variants', 60);

CALL cubemaster_assert_table_exists('t_cube_template_replica');
CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'template_variant_id',
  "VARCHAR(160) NOT NULL DEFAULT '' COMMENT 'internal JFS package id for this host-fact variant'"
);
CALL cubemaster_add_index_if_missing(
  't_cube_template_replica',
  'idx_template_variant',
  'ADD INDEX `idx_template_variant` (`template_id`, `template_variant_id`, `status`)'
);

SELECT RELEASE_LOCK('cubemaster_migration_20261110110000_jfs_template_variants');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261110110000_jfs_template_variants', 60);
CALL cubemaster_drop_index_if_exists('t_cube_template_replica', 'idx_template_variant');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'template_variant_id');
SELECT RELEASE_LOCK('cubemaster_migration_20261110110000_jfs_template_variants');
