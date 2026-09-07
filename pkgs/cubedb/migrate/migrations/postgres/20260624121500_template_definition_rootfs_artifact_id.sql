-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add rootfs_artifact_id to template definitions (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260624121500_tpl_def_rootfs_idx', 60);

SELECT cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'rootfs_artifact_id',
  'varchar(128) NOT NULL DEFAULT '''''
);

-- Backfill: match artifact_id from request_json.
UPDATE t_cube_template_definition d
   SET rootfs_artifact_id = a.artifact_id
  FROM t_cube_rootfs_artifact a
 WHERE d.rootfs_artifact_id = ''
   AND a.artifact_id <> ''
   AND d.request_json LIKE '%' || a.artifact_id || '%';

CREATE INDEX IF NOT EXISTS idx_template_definition_rootfs_artifact ON t_cube_template_definition (rootfs_artifact_id, deleted_at, template_id);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260624121500_tpl_def_rootfs_idx'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260624121500_tpl_def_rootfs_idx', 60);

SELECT cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_template_definition_rootfs_artifact');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'rootfs_artifact_id');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260624121500_tpl_def_rootfs_idx'));
