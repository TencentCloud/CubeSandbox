-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Snapshots own rootfs references independently of their source templates.

-- +goose Up
ALTER TABLE t_cube_snapshot ADD COLUMN IF NOT EXISTS rootfs_artifact_id varchar(128) NOT NULL DEFAULT '';
ALTER TABLE t_cube_snapshot ADD COLUMN IF NOT EXISTS cleanup_artifact_ids_json text NOT NULL DEFAULT '';
UPDATE t_cube_snapshot s
SET rootfs_artifact_id = COALESCE(
  NULLIF(BTRIM(NULLIF(s.request_json, '')::jsonb #>> '{annotations,cube.master.rootfs.artifact.id}'), ''),
  (SELECT BTRIM(c.value #>> '{image,annotations,cube.master.rootfs.artifact.id}')
   FROM jsonb_array_elements(COALESCE(NULLIF(s.request_json, '')::jsonb -> 'containers', '[]'::jsonb))
     WITH ORDINALITY c(value, position)
   WHERE BTRIM(c.value #>> '{image,annotations,cube.master.rootfs.artifact.id}') <> ''
   ORDER BY c.position LIMIT 1),
  '')
WHERE s.rootfs_artifact_id = '' AND COALESCE(s.cleanup_artifact_ids_json, '') = '';
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_rootfs_artifact
  ON t_cube_snapshot (rootfs_artifact_id, deleted_at, snapshot_id);

-- +goose Down
DROP INDEX IF EXISTS idx_cube_snapshot_rootfs_artifact;
ALTER TABLE t_cube_snapshot DROP COLUMN IF EXISTS cleanup_artifact_ids_json;
ALTER TABLE t_cube_snapshot DROP COLUMN IF EXISTS rootfs_artifact_id;
