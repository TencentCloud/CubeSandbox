-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Standalone (deleted_at) indexes for the scheduled tombstone purger
-- (issue #973). A `deleted_at < cutoff` scan needs a LEADING deleted_at index;
-- without it the purge is a full table scan every tick. The genuine tombstone
-- accumulators below either had no deleted_at index at all or only a composite
-- whose leading column is not deleted_at:
--   t_cube_sandbox_spec, t_cube_instance_info, t_cube_instance_userdata,
--   t_cube_template_replica, t_agenthub_snapshot.
-- (t_agenthub_instance and t_agenthub_template already have one and are skipped.)
--
-- NOTE: adding a secondary InnoDB index is online (no exclusive table lock) since
-- MySQL 5.6, but still does a one-time build on large existing tables. Deployments
-- running with CUBE_AUTO_MIGRATION=false must apply these statements out-of-band.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260731120000_soft_delete_purge_idx', 60);

-- t_cube_sandbox_spec
SET @idx_exists := (
  SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_cube_sandbox_spec' AND INDEX_NAME = 'idx_sandbox_spec_deleted_at'
);
SET @sql := IF(@idx_exists = 0,
  'ALTER TABLE `t_cube_sandbox_spec` ADD INDEX `idx_sandbox_spec_deleted_at` (`deleted_at`)', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- t_cube_instance_info
SET @idx_exists := (
  SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_cube_instance_info' AND INDEX_NAME = 'idx_instance_info_deleted_at'
);
SET @sql := IF(@idx_exists = 0,
  'ALTER TABLE `t_cube_instance_info` ADD INDEX `idx_instance_info_deleted_at` (`deleted_at`)', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- t_cube_instance_userdata
SET @idx_exists := (
  SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_cube_instance_userdata' AND INDEX_NAME = 'idx_instance_userdata_deleted_at'
);
SET @sql := IF(@idx_exists = 0,
  'ALTER TABLE `t_cube_instance_userdata` ADD INDEX `idx_instance_userdata_deleted_at` (`deleted_at`)', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- t_cube_template_replica
SET @idx_exists := (
  SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_cube_template_replica' AND INDEX_NAME = 'idx_template_replica_deleted_at'
);
SET @sql := IF(@idx_exists = 0,
  'ALTER TABLE `t_cube_template_replica` ADD INDEX `idx_template_replica_deleted_at` (`deleted_at`)', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- t_agenthub_snapshot (only had composites (agent_id,deleted_at)/(sandbox_id,deleted_at))
SET @idx_exists := (
  SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 't_agenthub_snapshot' AND INDEX_NAME = 'idx_agenthub_snapshot_deleted_at'
);
SET @sql := IF(@idx_exists = 0,
  'ALTER TABLE `t_agenthub_snapshot` ADD INDEX `idx_agenthub_snapshot_deleted_at` (`deleted_at`)', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SELECT RELEASE_LOCK('cubemaster_migration_20260731120000_soft_delete_purge_idx');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260731120000_soft_delete_purge_idx', 60);

SET @sql := IF((SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='t_cube_sandbox_spec' AND INDEX_NAME='idx_sandbox_spec_deleted_at')>0, 'ALTER TABLE `t_cube_sandbox_spec` DROP INDEX `idx_sandbox_spec_deleted_at`', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @sql := IF((SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='t_cube_instance_info' AND INDEX_NAME='idx_instance_info_deleted_at')>0, 'ALTER TABLE `t_cube_instance_info` DROP INDEX `idx_instance_info_deleted_at`', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @sql := IF((SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='t_cube_instance_userdata' AND INDEX_NAME='idx_instance_userdata_deleted_at')>0, 'ALTER TABLE `t_cube_instance_userdata` DROP INDEX `idx_instance_userdata_deleted_at`', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @sql := IF((SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='t_cube_template_replica' AND INDEX_NAME='idx_template_replica_deleted_at')>0, 'ALTER TABLE `t_cube_template_replica` DROP INDEX `idx_template_replica_deleted_at`', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @sql := IF((SELECT COUNT(1) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='t_agenthub_snapshot' AND INDEX_NAME='idx_agenthub_snapshot_deleted_at')>0, 'ALTER TABLE `t_agenthub_snapshot` DROP INDEX `idx_agenthub_snapshot_deleted_at`', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SELECT RELEASE_LOCK('cubemaster_migration_20260731120000_soft_delete_purge_idx');
