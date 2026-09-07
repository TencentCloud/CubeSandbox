-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- HEAD + 1: cube-egress CA bake metadata on t_cube_rootfs_artifact.
--
-- Three new columns mirror the gorm tags on
-- pkg/base/db/models/template_image.go::RootfsArtifact:
--
--   cube_egress_ca_baked            tinyint(1) — did the bake actually
--                                     write the CA into the rootfs?
--   cube_egress_ca_fingerprint      varchar(128) — sha256(PEM) hex; folds
--                                     into TemplateSpecFingerprint so a
--                                     CA rotation invalidates artifact
--                                     reuse automatically (see design/
--                                     cube-egress-ca-bake.md §D5).
--   cube_egress_ca_targets_written  int — count of bundle/anchor files
--                                     touched in the rootfs; for audit.
--
-- All three are append-only and idempotent (cubemaster_add_column_if_missing
-- guards against re-runs). No data normalisation needed: rows pre-dating
-- this migration default to baked=0 / fingerprint='' / targets_written=0,
-- which is exactly the "no CA was baked" state the model code already
-- handles.
--
-- No new indexes: these columns are read by template-info / fingerprint
-- folding only, not used as lookup keys.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0004_cube_egress', 60);

-- Preflight: this migration only makes sense once 0001 has created the
-- artifact table.
CALL cubemaster_assert_table_exists('t_cube_rootfs_artifact');

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'cube_egress_ca_baked',
  "tinyint(1) NOT NULL DEFAULT 0 COMMENT 'did cube-egress CA bake write the rootfs' AFTER `gc_deadline`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'cube_egress_ca_fingerprint',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'sha256(cube-egress CA PEM), hex' AFTER `cube_egress_ca_baked`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'cube_egress_ca_targets_written',
  "int NOT NULL DEFAULT 0 COMMENT 'number of bundle/anchor files touched by the bake' AFTER `cube_egress_ca_fingerprint`"
);

SELECT RELEASE_LOCK('cubemaster_migration_0004_cube_egress');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0004_cube_egress', 60);

CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'cube_egress_ca_targets_written');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'cube_egress_ca_fingerprint');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'cube_egress_ca_baked');

SELECT RELEASE_LOCK('cubemaster_migration_0004_cube_egress');
