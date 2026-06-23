-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Node isolation (cordon) fields. An operator can manually isolate a compute
-- node from the WebUI so the scheduler stops placing new instances on it. The
-- state lives on the registration row (not the heartbeat status row) because it
-- is an administrative, slow-changing attribute that must survive cubelet
-- re-registration and cubemaster restarts.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260623132449_node_isolation', 60);

SET @node_registration_isolated_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated'
);
SET @node_registration_isolated_sql := IF(
  @node_registration_isolated_exists = 0,
  'ALTER TABLE `t_cube_node_registration` ADD COLUMN `isolated` tinyint(1) NOT NULL DEFAULT 0 AFTER `max_mvm_num`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SET @node_registration_isolated_at_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated_at'
);
SET @node_registration_isolated_at_sql := IF(
  @node_registration_isolated_at_exists = 0,
  'ALTER TABLE `t_cube_node_registration` ADD COLUMN `isolated_at` bigint DEFAULT NULL AFTER `isolated`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_at_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SET @node_registration_isolated_by_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated_by'
);
SET @node_registration_isolated_by_sql := IF(
  @node_registration_isolated_by_exists = 0,
  'ALTER TABLE `t_cube_node_registration` ADD COLUMN `isolated_by` varchar(128) DEFAULT NULL AFTER `isolated_at`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_by_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SET @node_registration_isolated_reason_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated_reason'
);
SET @node_registration_isolated_reason_sql := IF(
  @node_registration_isolated_reason_exists = 0,
  'ALTER TABLE `t_cube_node_registration` ADD COLUMN `isolated_reason` varchar(512) DEFAULT NULL AFTER `isolated_by`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_reason_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SELECT RELEASE_LOCK('cubemaster_migration_20260623132449_node_isolation');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260623132449_node_isolation', 60);

SET @node_registration_isolated_reason_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated_reason'
);
SET @node_registration_isolated_reason_down_sql := IF(
  @node_registration_isolated_reason_exists > 0,
  'ALTER TABLE `t_cube_node_registration` DROP COLUMN `isolated_reason`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_reason_down_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SET @node_registration_isolated_by_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated_by'
);
SET @node_registration_isolated_by_down_sql := IF(
  @node_registration_isolated_by_exists > 0,
  'ALTER TABLE `t_cube_node_registration` DROP COLUMN `isolated_by`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_by_down_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SET @node_registration_isolated_at_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated_at'
);
SET @node_registration_isolated_at_down_sql := IF(
  @node_registration_isolated_at_exists > 0,
  'ALTER TABLE `t_cube_node_registration` DROP COLUMN `isolated_at`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_at_down_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SET @node_registration_isolated_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_cube_node_registration'
    AND COLUMN_NAME = 'isolated'
);
SET @node_registration_isolated_down_sql := IF(
  @node_registration_isolated_exists > 0,
  'ALTER TABLE `t_cube_node_registration` DROP COLUMN `isolated`',
  'SELECT 1'
);
PREPARE node_isolation_stmt FROM @node_registration_isolated_down_sql;
EXECUTE node_isolation_stmt;
DEALLOCATE PREPARE node_isolation_stmt;

SELECT RELEASE_LOCK('cubemaster_migration_20260623132449_node_isolation');
