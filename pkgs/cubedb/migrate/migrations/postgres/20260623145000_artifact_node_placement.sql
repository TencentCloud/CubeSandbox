-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Artifact node placement table (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260623145000_artifact_node_placement', 60);

CREATE TABLE IF NOT EXISTS t_cube_artifact_node_placement (
  artifact_id varchar(128) NOT NULL,
  node_id varchar(128) NOT NULL,
  node_ip varchar(64) NOT NULL DEFAULT '',
  created_at bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (artifact_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_artifact_node_placement_artifact ON t_cube_artifact_node_placement (artifact_id);

-- Backfill from existing replica bindings.
INSERT INTO t_cube_artifact_node_placement (artifact_id, node_id, node_ip, created_at)
SELECT r.artifact_id,
       CASE
         WHEN r.node_id IS NOT NULL AND TRIM(r.node_id) <> '' THEN TRIM(r.node_id)
         ELSE 'ip:' || TRIM(r.node_ip)
       END AS placement_node_id,
       COALESCE(MAX(NULLIF(TRIM(r.node_ip), '')), ''),
       EXTRACT(EPOCH FROM CURRENT_TIMESTAMP)::bigint
FROM t_cube_template_replica r
WHERE r.artifact_id IS NOT NULL
  AND r.artifact_id <> ''
  AND (
    (r.node_id IS NOT NULL AND TRIM(r.node_id) <> '')
    OR (r.node_ip IS NOT NULL AND TRIM(r.node_ip) <> '')
  )
GROUP BY r.artifact_id, placement_node_id
ON CONFLICT DO NOTHING;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260623145000_artifact_node_placement'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260623145000_artifact_node_placement', 60);

DROP TABLE IF EXISTS t_cube_artifact_node_placement;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260623145000_artifact_node_placement'));
