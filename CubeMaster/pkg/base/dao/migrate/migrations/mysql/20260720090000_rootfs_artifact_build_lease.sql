-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Fence concurrent rootfs artifact builders across CubeMaster replicas.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260720090000_rootfs_artifact_build_lease', 60);

CALL cubemaster_assert_table_exists('t_cube_rootfs_artifact');

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'build_owner_token',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'opaque token of the active artifact builder' AFTER `gc_deadline`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'build_generation',
  "bigint NOT NULL DEFAULT 0 COMMENT 'monotonic artifact build generation' AFTER `build_owner_token`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'build_lease_expire_at',
  "bigint NOT NULL DEFAULT 0 COMMENT 'unix seconds when the active build lease expires' AFTER `build_generation`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260720090000_rootfs_artifact_build_lease');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260720090000_rootfs_artifact_build_lease', 60);

CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'build_lease_expire_at');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'build_generation');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'build_owner_token');

SELECT RELEASE_LOCK('cubemaster_migration_20260720090000_rootfs_artifact_build_lease');
