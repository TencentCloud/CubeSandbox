-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- erofs/ext4 dual-format rootfs support: record which packaging format an
-- artifact was built in so CubeMaster can fill ImageSpec.StorageMedia for
-- Cubelet (which routes the download/destroy by that field) instead of
-- hardcoding ext4.
--
-- One new column mirrors the gorm tag on
-- pkg/base/db/models/template_image.go::RootfsArtifact.RootfsFormat:
--
--   rootfs_format  varchar(16) — "ext4" or "erofs"; the format
--                  CubeTemplateCenter built this artifact in. Drives
--                  ImageSpec.StorageMedia on distribution / destroy.
--
-- Append-only and idempotent (cubemaster_add_column_if_missing guards
-- re-runs). Rows predating this migration default to "ext4", which is
-- exactly the historical behavior — every existing artifact is an ext4
-- rootfs — so the model code's empty-string-means-ext4 fallback is only a
-- defense for rows written before this column existed and never read back
-- a wrong format.
--
-- No new index: rootfs_format is read on distribution/destroy, not a
-- lookup key.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261016120000_rootfs_format', 60);

-- Preflight: this migration only makes sense once 0001 has created the
-- artifact table.
CALL cubemaster_assert_table_exists('t_cube_rootfs_artifact');

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'rootfs_format',
  "varchar(16) NOT NULL DEFAULT 'ext4' COMMENT 'rootfs packaging format: ext4 or erofs' AFTER `cube_egress_ca_targets_written`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20261016120000_rootfs_format');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261016120000_rootfs_format', 60);

CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'rootfs_format');

SELECT RELEASE_LOCK('cubemaster_migration_20261016120000_rootfs_format');
