-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Artifact node placement table (FIX-1, last-owner-cleanup).
--
-- t_cube_template_replica is a per-template *reference* record; once a
-- template stops referencing an artifact its replica rows are deleted. That
-- makes it impossible for the last deleter (or GC) to know which nodes still
-- physically hold a shared, fingerprint-deduped artifact's ext4 files, which
-- previously leaked cubebox_os_image directories on nodes.
--
-- This migration introduces an independent placement record written when an
-- artifact is distributed to a node and cleared only when the artifact itself
-- is fully removed. It also backfills existing replica bindings so the first
-- post-upgrade cleanup does not miss already-distributed artifacts.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260623145000_artifact_node_placement', 60);

CREATE TABLE IF NOT EXISTS `t_cube_artifact_node_placement` (
  `artifact_id` varchar(128) NOT NULL,
  `node_id` varchar(128) NOT NULL,
  `node_ip` varchar(64) NOT NULL DEFAULT '',
  `created_at` bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (`artifact_id`, `node_id`),
  KEY `idx_artifact` (`artifact_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Backfill from existing replica bindings. Newer rows have a stable node_id,
-- while legacy/degenerate rows may only carry node_ip. Preserve those rows by
-- synthesising a deterministic placement key `ip:<node_ip>` for the primary
-- key; cleanup still uses the recorded node_ip for the Cubelet RPC address.
-- INSERT IGNORE keeps re-runs idempotent.
INSERT IGNORE INTO `t_cube_artifact_node_placement` (`artifact_id`, `node_id`, `node_ip`, `created_at`)
SELECT r.`artifact_id`,
       CASE
         WHEN r.`node_id` IS NOT NULL AND TRIM(r.`node_id`) <> '' THEN TRIM(r.`node_id`)
         ELSE CONCAT('ip:', TRIM(r.`node_ip`))
       END AS `placement_node_id`,
       COALESCE(MAX(NULLIF(TRIM(r.`node_ip`), '')), ''),
       UNIX_TIMESTAMP()
FROM `t_cube_template_replica` r
WHERE r.`artifact_id` IS NOT NULL
  AND r.`artifact_id` <> ''
  AND (
    (r.`node_id` IS NOT NULL AND TRIM(r.`node_id`) <> '')
    OR (r.`node_ip` IS NOT NULL AND TRIM(r.`node_ip`) <> '')
  )
GROUP BY r.`artifact_id`, `placement_node_id`;

SELECT RELEASE_LOCK('cubemaster_migration_20260623145000_artifact_node_placement');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260623145000_artifact_node_placement', 60);

DROP TABLE IF EXISTS `t_cube_artifact_node_placement`;

SELECT RELEASE_LOCK('cubemaster_migration_20260623145000_artifact_node_placement');
