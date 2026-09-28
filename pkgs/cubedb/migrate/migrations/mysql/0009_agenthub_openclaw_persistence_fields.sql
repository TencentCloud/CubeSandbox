-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub OpenClaw persistence metadata. CubeMaster owns shared database
-- schema migration through embedded goose migrations; CubeAPI only reads and
-- writes these fields.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0009_agenthub_openclaw_persistence_fields', 60);

SET @agenthub_instance_rootfs_source_type_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'rootfs_source_type'
);
SET @agenthub_instance_rootfs_source_type_sql := IF(
  @agenthub_instance_rootfs_source_type_exists = 0,
  'ALTER TABLE `t_agenthub_instance` ADD COLUMN `rootfs_source_type` varchar(32) DEFAULT NULL AFTER `persistence_mode`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_rootfs_source_type_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_rootfs_source_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'rootfs_source_id'
);
SET @agenthub_instance_rootfs_source_id_sql := IF(
  @agenthub_instance_rootfs_source_id_exists = 0,
  'ALTER TABLE `t_agenthub_instance` ADD COLUMN `rootfs_source_id` varchar(128) DEFAULT NULL AFTER `rootfs_source_type`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_rootfs_source_id_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_openclaw_persist_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'openclaw_persist_id'
);
SET @agenthub_instance_openclaw_persist_id_sql := IF(
  @agenthub_instance_openclaw_persist_id_exists = 0,
  'ALTER TABLE `t_agenthub_instance` ADD COLUMN `openclaw_persist_id` varchar(128) DEFAULT NULL AFTER `rootfs_source_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_openclaw_persist_id_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_openclaw_state_path_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'openclaw_state_path'
);
SET @agenthub_instance_openclaw_state_path_sql := IF(
  @agenthub_instance_openclaw_state_path_exists = 0,
  'ALTER TABLE `t_agenthub_instance` ADD COLUMN `openclaw_state_path` varchar(512) DEFAULT NULL AFTER `openclaw_persist_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_openclaw_state_path_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_snapshot_kind_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'snapshot_kind'
);
SET @agenthub_snapshot_snapshot_kind_sql := IF(
  @agenthub_snapshot_snapshot_kind_exists = 0,
  'ALTER TABLE `t_agenthub_snapshot` ADD COLUMN `snapshot_kind` varchar(32) DEFAULT NULL AFTER `status`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_snapshot_kind_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_rootfs_source_type_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'rootfs_source_type'
);
SET @agenthub_snapshot_rootfs_source_type_sql := IF(
  @agenthub_snapshot_rootfs_source_type_exists = 0,
  'ALTER TABLE `t_agenthub_snapshot` ADD COLUMN `rootfs_source_type` varchar(32) DEFAULT NULL AFTER `published_template_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_rootfs_source_type_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_rootfs_source_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'rootfs_source_id'
);
SET @agenthub_snapshot_rootfs_source_id_sql := IF(
  @agenthub_snapshot_rootfs_source_id_exists = 0,
  'ALTER TABLE `t_agenthub_snapshot` ADD COLUMN `rootfs_source_id` varchar(128) DEFAULT NULL AFTER `rootfs_source_type`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_rootfs_source_id_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_rootfs_snapshot_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'rootfs_snapshot_id'
);
SET @agenthub_snapshot_rootfs_snapshot_id_sql := IF(
  @agenthub_snapshot_rootfs_snapshot_id_exists = 0,
  'ALTER TABLE `t_agenthub_snapshot` ADD COLUMN `rootfs_snapshot_id` varchar(128) DEFAULT NULL AFTER `rootfs_source_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_rootfs_snapshot_id_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_openclaw_state_snapshot_path_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'openclaw_state_snapshot_path'
);
SET @agenthub_snapshot_openclaw_state_snapshot_path_sql := IF(
  @agenthub_snapshot_openclaw_state_snapshot_path_exists = 0,
  'ALTER TABLE `t_agenthub_snapshot` ADD COLUMN `openclaw_state_snapshot_path` varchar(512) DEFAULT NULL AFTER `rootfs_snapshot_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_openclaw_state_snapshot_path_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SELECT RELEASE_LOCK('cubemaster_migration_0009_agenthub_openclaw_persistence_fields');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0009_agenthub_openclaw_persistence_fields', 60);

SET @agenthub_snapshot_openclaw_state_snapshot_path_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'openclaw_state_snapshot_path'
);
SET @agenthub_snapshot_openclaw_state_snapshot_path_down_sql := IF(
  @agenthub_snapshot_openclaw_state_snapshot_path_exists > 0,
  'ALTER TABLE `t_agenthub_snapshot` DROP COLUMN `openclaw_state_snapshot_path`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_openclaw_state_snapshot_path_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_rootfs_snapshot_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'rootfs_snapshot_id'
);
SET @agenthub_snapshot_rootfs_snapshot_id_down_sql := IF(
  @agenthub_snapshot_rootfs_snapshot_id_exists > 0,
  'ALTER TABLE `t_agenthub_snapshot` DROP COLUMN `rootfs_snapshot_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_rootfs_snapshot_id_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_rootfs_source_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'rootfs_source_id'
);
SET @agenthub_snapshot_rootfs_source_id_down_sql := IF(
  @agenthub_snapshot_rootfs_source_id_exists > 0,
  'ALTER TABLE `t_agenthub_snapshot` DROP COLUMN `rootfs_source_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_rootfs_source_id_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_rootfs_source_type_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'rootfs_source_type'
);
SET @agenthub_snapshot_rootfs_source_type_down_sql := IF(
  @agenthub_snapshot_rootfs_source_type_exists > 0,
  'ALTER TABLE `t_agenthub_snapshot` DROP COLUMN `rootfs_source_type`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_rootfs_source_type_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_snapshot_snapshot_kind_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_snapshot'
    AND COLUMN_NAME = 'snapshot_kind'
);
SET @agenthub_snapshot_snapshot_kind_down_sql := IF(
  @agenthub_snapshot_snapshot_kind_exists > 0,
  'ALTER TABLE `t_agenthub_snapshot` DROP COLUMN `snapshot_kind`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_snapshot_snapshot_kind_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_openclaw_state_path_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'openclaw_state_path'
);
SET @agenthub_instance_openclaw_state_path_down_sql := IF(
  @agenthub_instance_openclaw_state_path_exists > 0,
  'ALTER TABLE `t_agenthub_instance` DROP COLUMN `openclaw_state_path`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_openclaw_state_path_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_openclaw_persist_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'openclaw_persist_id'
);
SET @agenthub_instance_openclaw_persist_id_down_sql := IF(
  @agenthub_instance_openclaw_persist_id_exists > 0,
  'ALTER TABLE `t_agenthub_instance` DROP COLUMN `openclaw_persist_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_openclaw_persist_id_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_rootfs_source_id_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'rootfs_source_id'
);
SET @agenthub_instance_rootfs_source_id_down_sql := IF(
  @agenthub_instance_rootfs_source_id_exists > 0,
  'ALTER TABLE `t_agenthub_instance` DROP COLUMN `rootfs_source_id`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_rootfs_source_id_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SET @agenthub_instance_rootfs_source_type_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'rootfs_source_type'
);
SET @agenthub_instance_rootfs_source_type_down_sql := IF(
  @agenthub_instance_rootfs_source_type_exists > 0,
  'ALTER TABLE `t_agenthub_instance` DROP COLUMN `rootfs_source_type`',
  'SELECT 1'
);
PREPARE agenthub_migration_stmt FROM @agenthub_instance_rootfs_source_type_down_sql;
EXECUTE agenthub_migration_stmt;
DEALLOCATE PREPARE agenthub_migration_stmt;

SELECT RELEASE_LOCK('cubemaster_migration_0009_agenthub_openclaw_persistence_fields');
