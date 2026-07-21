-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Fence concurrent rootfs artifact builders across CubeMaster replicas.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260720090000_rootfs_artifact_build_lease', 60);

SELECT cubemaster_assert_table_exists('t_cube_rootfs_artifact');

SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'build_owner_token',
  'varchar(128) NOT NULL DEFAULT '''''
);
SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'build_generation',
  'bigint NOT NULL DEFAULT 0'
);
SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'build_lease_expire_at',
  'bigint NOT NULL DEFAULT 0'
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260720090000_rootfs_artifact_build_lease'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260720090000_rootfs_artifact_build_lease', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'build_lease_expire_at');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'build_generation');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'build_owner_token');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260720090000_rootfs_artifact_build_lease'));
