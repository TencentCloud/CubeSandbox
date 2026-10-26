-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Snapshots own rootfs references independently of their source templates.

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260916120000_snapshot_rootfs', 60);
CALL cubemaster_add_column_if_missing(
  't_cube_snapshot', 'rootfs_artifact_id',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'rootfs artifact reference'"
);

CALL cubemaster_add_column_if_missing(
  't_cube_snapshot', 'cleanup_artifact_ids_json',
  "text NOT NULL COMMENT 'durable artifact cleanup targets after reference release'"
);

-- Match request extraction: top-level annotation, then the first nonempty
-- container image annotation. Empty requests represent legacy snapshots.
UPDATE t_cube_snapshot s
SET rootfs_artifact_id = COALESCE(
  NULLIF(TRIM(JSON_UNQUOTE(JSON_EXTRACT(NULLIF(s.request_json, ''), '$.annotations."cube.master.rootfs.artifact.id"'))), ''),
  (SELECT TRIM(c.artifact_id)
   FROM JSON_TABLE(COALESCE(NULLIF(s.request_json, ''), '{}'), '$.containers[*]'
     COLUMNS (position FOR ORDINALITY,
              artifact_id VARCHAR(128) PATH '$.image.annotations."cube.master.rootfs.artifact.id"')) c
   WHERE TRIM(c.artifact_id) <> '' ORDER BY c.position LIMIT 1),
  '')
WHERE s.rootfs_artifact_id = '' AND COALESCE(s.cleanup_artifact_ids_json, '') = '';
CALL cubemaster_add_index_if_missing(
  't_cube_snapshot', 'idx_cube_snapshot_rootfs_artifact',
  'ADD INDEX `idx_cube_snapshot_rootfs_artifact` (`rootfs_artifact_id`, `deleted_at`, `snapshot_id`)'
);
SELECT RELEASE_LOCK('cubemaster_migration_20260916120000_snapshot_rootfs');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260916120000_snapshot_rootfs', 60);
CALL cubemaster_drop_index_if_exists('t_cube_snapshot', 'idx_cube_snapshot_rootfs_artifact');
CALL cubemaster_drop_column_if_exists('t_cube_snapshot', 'cleanup_artifact_ids_json');
CALL cubemaster_drop_column_if_exists('t_cube_snapshot', 'rootfs_artifact_id');
SELECT RELEASE_LOCK('cubemaster_migration_20260916120000_snapshot_rootfs');
