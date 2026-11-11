-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261110110000_jfs_template_variants', 60);

SELECT cubemaster_assert_table_exists('t_cube_template_replica');
SELECT cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'template_variant_id',
  'varchar(160) NOT NULL DEFAULT '''''
);
CREATE INDEX IF NOT EXISTS idx_template_variant
  ON t_cube_template_replica (template_id, template_variant_id, status);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261110110000_jfs_template_variants'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261110110000_jfs_template_variants', 60);
DROP INDEX IF EXISTS idx_template_variant;
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'template_variant_id');
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261110110000_jfs_template_variants'));
