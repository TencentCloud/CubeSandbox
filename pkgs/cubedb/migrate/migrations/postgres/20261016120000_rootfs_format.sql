-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- PostgreSQL counterpart of mysql/20261016120000_rootfs_format.sql.
-- erofs/ext4 dual-format rootfs support: record which packaging format an
-- artifact was built in so CubeMaster can fill ImageSpec.StorageMedia for
-- Cubelet instead of hardcoding ext4.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261016120000_rootfs_format', 60);

SELECT cubemaster_assert_table_exists('t_cube_rootfs_artifact');

SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'rootfs_format',
  $$varchar(16) NOT NULL DEFAULT 'ext4'$$
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261016120000_rootfs_format'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261016120000_rootfs_format', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'rootfs_format');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261016120000_rootfs_format'));
