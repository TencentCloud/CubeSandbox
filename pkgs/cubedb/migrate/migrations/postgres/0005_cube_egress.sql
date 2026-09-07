-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- CubeEgress CA bake metadata on t_cube_rootfs_artifact (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0005_cube_egress', 60);

SELECT cubemaster_assert_table_exists('t_cube_rootfs_artifact');

SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'cube_egress_ca_baked',
  'boolean NOT NULL DEFAULT false'
);
SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'cube_egress_ca_fingerprint',
  'varchar(128) NOT NULL DEFAULT '''''
);
SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'cube_egress_ca_targets_written',
  'integer NOT NULL DEFAULT 0'
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0005_cube_egress'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0005_cube_egress', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'cube_egress_ca_targets_written');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'cube_egress_ca_fingerprint');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'cube_egress_ca_baked');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0005_cube_egress'));
