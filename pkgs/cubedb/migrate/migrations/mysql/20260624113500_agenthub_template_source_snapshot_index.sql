-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add an index for AgentHub template reverse lookups by source snapshot.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx', 60);

SET @agenthub_template_source_snapshot_index_exists := (
  SELECT COUNT(1)
  FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_template'
    AND INDEX_NAME = 'idx_agenthub_template_source_snapshot'
);

SET @agenthub_template_source_snapshot_index_sql := IF(
  @agenthub_template_source_snapshot_index_exists = 0,
  'ALTER TABLE `t_agenthub_template` ADD INDEX `idx_agenthub_template_source_snapshot` (`source_snapshot_id`, `deleted_at`, `template_id`)',
  'SELECT 1'
);
PREPARE stmt FROM @agenthub_template_source_snapshot_index_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SELECT RELEASE_LOCK('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx', 60);

SET @agenthub_template_source_snapshot_index_exists := (
  SELECT COUNT(1)
  FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_template'
    AND INDEX_NAME = 'idx_agenthub_template_source_snapshot'
);

SET @agenthub_template_source_snapshot_index_down_sql := IF(
  @agenthub_template_source_snapshot_index_exists > 0,
  'ALTER TABLE `t_agenthub_template` DROP INDEX `idx_agenthub_template_source_snapshot`',
  'SELECT 1'
);
PREPARE stmt FROM @agenthub_template_source_snapshot_index_down_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SELECT RELEASE_LOCK('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx');
