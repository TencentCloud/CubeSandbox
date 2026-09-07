-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- v0.2.2 -> HEAD consolidated migration.
--
-- Why a single file:
--   The leap from v0.2.2 to HEAD is one inseparable upgrade unit. Splitting
--   it across multiple files would mean an instance that crashes between
--   sub-steps comes back up in an in-between state that no model code
--   speaks. Bundling everything keeps "what schema does HEAD expect"
--   answerable in one place.
--
-- Step ordering (the order matters; do not shuffle):
--
--   (A) Acquire the per-file lock via the helper procedure created in 0000.
--   (B) ADD COLUMNs (t_cube_template_image_job: sandbox_id / resource_*).
--   (C) Data normalize for the rows that pre-date the new schema; this MUST
--       run before any UNIQUE index that references the columns being
--       normalized, otherwise the index creation would fail on legacy
--       duplicates.
--   (D) ADD INDEXes for t_cube_template_image_job.
--   (E) ADD COLUMNs + INDEXes for t_cube_template_definition (the 7 fields
--       and 4 indexes the snapshot/Kind/StorageBackend work introduced).
--   (F) CREATE the new tables (t_cube_sandbox_spec, t_cube_snapshot_runtime_ref).
--   (G) DROP the deprecated physical columns from t_cube_template_replica.
--       This is unrecoverable data loss — putting it last means if any
--       earlier step fails we roll back to a state that still keeps the
--       old physical columns intact for forensic recovery.
--   (H) Release the per-file lock.

-- +goose NO TRANSACTION
-- +goose Up

-- (A) Acquire inner lock.
CALL cubemaster_acquire_migration_lock('cubemaster_migration_0001_v0_2_2_to_head', 60);

-- Preflight: fail before doing any irreversible DDL if the database is not a
-- supported v0.2.2/intermediate schema.
CALL cubemaster_assert_table_exists('t_cube_template_image_job');
CALL cubemaster_assert_table_exists('t_cube_template_definition');
CALL cubemaster_assert_table_exists('t_cube_template_replica');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'job_id');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'request_id');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'operation');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'node_id');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'source_image_ref');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'retry_of_job_id');
CALL cubemaster_assert_column_exists('t_cube_template_image_job', 'status');
CALL cubemaster_assert_column_exists('t_cube_template_definition', 'status');
CALL cubemaster_assert_column_exists('t_cube_template_definition', 'request_json');
CALL cubemaster_assert_column_exists('t_cube_template_replica', 'template_id');
CALL cubemaster_assert_column_exists('t_cube_template_replica', 'spec');

-- (B) t_cube_template_image_job: add HEAD's three snapshot/resource columns.
CALL cubemaster_add_column_if_missing('t_cube_template_image_job', 'sandbox_id',    "varchar(128) NOT NULL DEFAULT '' COMMENT 'sandbox id for snapshot operations' AFTER `request_id`");
CALL cubemaster_add_column_if_missing('t_cube_template_image_job', 'resource_type', "varchar(32)  NOT NULL DEFAULT '' COMMENT 'operation resource type' AFTER `sandbox_id`");
CALL cubemaster_add_column_if_missing('t_cube_template_image_job', 'resource_id',   "varchar(128) NOT NULL DEFAULT '' COMMENT 'operation resource id' AFTER `resource_type`");

-- (C) Data normalize for t_cube_template_image_job before unique-index creation.
-- Legacy rows may have empty request_id and/or operation. HEAD code requires
-- (request_id, operation) to be unique. Mirror the Go-side normalizer from
-- pkg/templatecenter/template_image.go normalizeTemplateImageJobRequestIDs.
--
-- Step (C.1): fill empty request_id with 'legacy-' + job_id.
UPDATE `t_cube_template_image_job`
   SET `request_id` = CONCAT('legacy-', `job_id`)
 WHERE `request_id` = '' AND `job_id` <> '';

-- Step (C.2): infer operation for rows where it is empty (UPPERCASE values
-- matching JobOperationCommit / Create / Redo / Legacy in Go).
UPDATE `t_cube_template_image_job`
   SET `operation` = CASE
         WHEN TRIM(`node_id`) <> '' AND TRIM(`source_image_ref`) = ''                                 THEN 'COMMIT'
         WHEN TRIM(`source_image_ref`) <> '' AND TRIM(`retry_of_job_id`) = ''                         THEN 'CREATE'
         WHEN TRIM(`source_image_ref`) <> '' AND TRIM(`retry_of_job_id`) <> ''                        THEN 'REDO'
         ELSE 'LEGACY'
       END
 WHERE `operation` = '';

-- Step (C.3): break ties for any remaining (request_id, operation) duplicates
-- by appending the job_id to request_id of the older rows. This preserves
-- the latest row's request_id intact (matching the Go-side "seen" logic that
-- iterates ASC by id and only mutates the first-seen-then-duplicate).
--
-- Expected runtime: O(seconds) on tables up to ~1M rows. t_cube_template_image_job
-- is operational metadata and typical production deployments stay well below
-- that scale, so a single self-join is acceptable here. Batched execution is
-- intentionally not implemented; if a deployment ever exceeds ~1M rows, run
-- this UPDATE manually in chunks (e.g. WHERE id BETWEEN ...) before applying
-- migration 0002, then re-run goose to record completion.
UPDATE `t_cube_template_image_job` t
  JOIN (
    SELECT MIN(id) AS keep_id, `request_id`, `operation`
      FROM `t_cube_template_image_job`
     WHERE `request_id` <> ''
     GROUP BY `request_id`, `operation`
    HAVING COUNT(*) > 1
  ) dup
    ON dup.request_id = t.request_id AND dup.operation = t.operation
   AND t.id <> dup.keep_id
   SET t.`request_id` = CONCAT(t.`request_id`, '#', t.`job_id`);

-- (D) t_cube_template_image_job: add HEAD's three new indexes. We do NOT
-- recreate idx_template_image_request_id / template_status / template_attempt
-- because those already exist in v0.2.2 (see 0000 baseline).
CALL cubemaster_add_index_if_missing('t_cube_template_image_job', 'idx_template_image_sandbox_status',      "ADD INDEX `idx_template_image_sandbox_status` (`sandbox_id`, `status`)");
CALL cubemaster_add_index_if_missing('t_cube_template_image_job', 'idx_template_image_resource_status',     "ADD INDEX `idx_template_image_resource_status` (`resource_id`, `status`)");
CALL cubemaster_add_index_if_missing('t_cube_template_image_job', 'idx_template_image_request_operation',   "ADD UNIQUE INDEX `idx_template_image_request_operation` (`request_id`, `operation`)");

-- (E) t_cube_template_definition: add HEAD's 7 new columns and 4 new indexes.
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'kind',                          "varchar(32)      NOT NULL DEFAULT 'template' COMMENT 'template kind' AFTER `status`");
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'origin_sandbox_id',             "varchar(128)     NOT NULL DEFAULT '' COMMENT 'origin sandbox id for snapshots' AFTER `kind`");
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'origin_node_id',                "varchar(128)     NOT NULL DEFAULT '' COMMENT 'origin node id for snapshots' AFTER `origin_sandbox_id`");
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'display_name',                  "varchar(256)     NOT NULL DEFAULT '' COMMENT 'display name for snapshots' AFTER `origin_node_id`");
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'storage_backend',               "varchar(32)      NOT NULL DEFAULT '' COMMENT 'storage backend' AFTER `display_name`");
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'retain',                        "tinyint(1)       NOT NULL DEFAULT 0 COMMENT 'retain snapshot from gc' AFTER `storage_backend`");
CALL cubemaster_add_column_if_missing('t_cube_template_definition', 'rootfs_size_bytes_at_snapshot', "bigint unsigned  NOT NULL DEFAULT 0 COMMENT 'rootfs size at snapshot time' AFTER `retain`");
CALL cubemaster_add_index_if_missing('t_cube_template_definition', 'idx_template_kind_status',       "ADD INDEX `idx_template_kind_status` (`kind`, `status`)");
CALL cubemaster_add_index_if_missing('t_cube_template_definition', 'idx_snapshot_origin_sandbox',    "ADD INDEX `idx_snapshot_origin_sandbox` (`origin_sandbox_id`)");
CALL cubemaster_add_index_if_missing('t_cube_template_definition', 'idx_snapshot_origin_node',       "ADD INDEX `idx_snapshot_origin_node` (`origin_node_id`)");
CALL cubemaster_add_index_if_missing('t_cube_template_definition', 'idx_template_storage_backend',   "ADD INDEX `idx_template_storage_backend` (`storage_backend`)");

-- (F.1) New table: t_cube_sandbox_spec (HEAD addition for snapshot/inspect flows).
CREATE TABLE IF NOT EXISTS `t_cube_sandbox_spec` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `sandbox_id` varchar(128) NOT NULL COMMENT 'sandbox id',
  `template_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'base template id at create time',
  `instance_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'instance type',
  `network_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'network type',
  `host_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'host id where sandbox runs',
  `host_ip` varchar(64) NOT NULL DEFAULT '' COMMENT 'host ip where sandbox runs',
  `request_json` mediumtext NOT NULL COMMENT 'canonical create request json',
  `backfilled` tinyint(1) NOT NULL DEFAULT 0 COMMENT 'whether spec was reconstructed from base template (override-lossy)',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_sandbox_spec_sandbox_id` (`sandbox_id`),
  KEY `idx_sandbox_spec_template_id` (`template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- (F.2) New table: t_cube_snapshot_runtime_ref (HEAD addition for snapshot lifecycle).
CREATE TABLE IF NOT EXISTS `t_cube_snapshot_runtime_ref` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `snapshot_id` varchar(128) NOT NULL COMMENT 'snapshot id',
  `sandbox_id` varchar(128) NOT NULL COMMENT 'sandbox id',
  `node_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'node id',
  `node_ip` varchar(64) NOT NULL DEFAULT '' COMMENT 'node ip',
  `binding_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'runtime binding type',
  `memory_vol` varchar(256) NOT NULL DEFAULT '' COMMENT 'snapshot memory volume name',
  `memory_dev` varchar(256) NOT NULL DEFAULT '' COMMENT 'snapshot memory device path',
  `rootfs_vol` varchar(256) NOT NULL DEFAULT '' COMMENT 'runtime rootfs volume after restore',
  `sandbox_gen` int unsigned NOT NULL DEFAULT 0 COMMENT 'runtime rootfs generation',
  `status` varchar(32) NOT NULL DEFAULT '' COMMENT 'runtime ref status',
  `attached_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'when runtime ref becomes active',
  `released_at` datetime DEFAULT NULL COMMENT 'when runtime ref is released',
  `last_seen_at` datetime DEFAULT NULL COMMENT 'last observed timestamp',
  `last_error` text COMMENT 'last reconcile or release error',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_snapshot_runtime_ref_snapshot_status` (`snapshot_id`, `status`),
  KEY `idx_snapshot_runtime_ref_sandbox_status` (`sandbox_id`, `status`),
  KEY `idx_snapshot_runtime_ref_node_status` (`node_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- (G) t_cube_template_replica: drop the deprecated physical-layout
-- columns. Cubelet's local snapshot catalog is now the single source of
-- truth for physical layout; the master-side replica row only tracks
-- lifecycle. This step is intentionally LAST: it is the only unrecoverable
-- step in the whole file, so if anything above fails the operator still
-- has the original columns for forensic recovery.
--
-- snapshot_path is the only physical column that existed in v0.2.2; the
-- other 8 (rootfs_vol, memory_vol, rootfs_kind, memory_kind, rootfs_dev,
-- memory_dev, meta_dir, build_rootfs_vol) were added by intermediate
-- versions whose self-init code we are about to delete. Use IF EXISTS
-- so this migration is correct on three source schemas:
--   * v0.2.2 (only snapshot_path exists)
--   * intermediate HEAD (all 9 exist)
--   * fresh deploy after baseline 0001 (only snapshot_path exists)
--
-- All deprecated columns are conditionally dropped so a crash after this
-- point can be followed by a clean rerun of the migration.
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'snapshot_path');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'rootfs_vol');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'memory_vol');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'rootfs_kind');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'memory_kind');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'rootfs_dev');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'memory_dev');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'meta_dir');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'build_rootfs_vol');

-- (H) Release inner lock. Outer goose SessionLocker stays held until the
-- whole Up() finishes.
SELECT RELEASE_LOCK('cubemaster_migration_0001_v0_2_2_to_head');

-- +goose Down
-- The Down path restores schema only; data lost in steps (C) and (G) is
-- not reconstructable. Operators relying on Down to recover physical-layout
-- columns must restore from a v0.2.2 backup.

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0001_v0_2_2_to_head', 60);

-- Reverse (G): re-add the deprecated physical columns. Use the
-- conditional add helper so the Down is idempotent across partial states.
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'snapshot_path',    "varchar(1024) NOT NULL DEFAULT '' COMMENT 'snapshot path' AFTER `spec`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'rootfs_vol',       "varchar(256)  NOT NULL DEFAULT '' COMMENT 'rootfs cubecow object name' AFTER `snapshot_path`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'memory_vol',       "varchar(256)  NOT NULL DEFAULT '' COMMENT 'memory cubecow object name' AFTER `rootfs_vol`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'rootfs_kind',      "varchar(16)   NOT NULL DEFAULT '' COMMENT 'rootfs cubecow object kind' AFTER `memory_vol`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'memory_kind',      "varchar(16)   NOT NULL DEFAULT '' COMMENT 'memory cubecow object kind' AFTER `rootfs_kind`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'rootfs_dev',       "varchar(256)  NOT NULL DEFAULT '' COMMENT 'rootfs device path' AFTER `memory_kind`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'memory_dev',       "varchar(256)  NOT NULL DEFAULT '' COMMENT 'memory device path' AFTER `rootfs_dev`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'meta_dir',         "varchar(1024) NOT NULL DEFAULT '' COMMENT 'snapshot metadata directory' AFTER `memory_dev`");
CALL cubemaster_add_column_if_missing('t_cube_template_replica', 'build_rootfs_vol', "varchar(256)  NOT NULL DEFAULT '' COMMENT 'build rootfs cubecow object name' AFTER `meta_dir`");

-- Reverse (F): drop the two new tables.
DROP TABLE IF EXISTS `t_cube_snapshot_runtime_ref`;
DROP TABLE IF EXISTS `t_cube_sandbox_spec`;

-- Reverse (E): drop t_cube_template_definition's new indexes and columns.
CALL cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_template_storage_backend');
CALL cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_snapshot_origin_node');
CALL cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_snapshot_origin_sandbox');
CALL cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_template_kind_status');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'rootfs_size_bytes_at_snapshot');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'retain');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'storage_backend');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'display_name');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'origin_node_id');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'origin_sandbox_id');
CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'kind');

-- Reverse (D): drop t_cube_template_image_job's new indexes.
CALL cubemaster_drop_index_if_exists('t_cube_template_image_job', 'idx_template_image_request_operation');
CALL cubemaster_drop_index_if_exists('t_cube_template_image_job', 'idx_template_image_resource_status');
CALL cubemaster_drop_index_if_exists('t_cube_template_image_job', 'idx_template_image_sandbox_status');

-- Reverse (C): cannot be reversed. Operators who need the original
-- request_id / operation values must restore from a v0.2.2 backup.

-- Reverse (B): drop the three columns.
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'resource_id');
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'resource_type');
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'sandbox_id');

SELECT RELEASE_LOCK('cubemaster_migration_0001_v0_2_2_to_head');
