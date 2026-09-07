-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Persist the state-management mode captured when a digital assistant is
-- published as an AgentHub template. Assistants created from such templates
-- inherit this mode instead of accepting an arbitrary create-time override.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0008_agenthub_template_persistence_mode', 60);

SET @agenthub_instance_persistence_mode_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'persistence_mode'
);
SET @agenthub_instance_persistence_mode_sql := IF(
  @agenthub_instance_persistence_mode_exists = 0,
  'ALTER TABLE `t_agenthub_instance` ADD COLUMN `persistence_mode` varchar(32) DEFAULT NULL AFTER `gateway_token`',
  'SELECT 1'
);
PREPARE stmt FROM @agenthub_instance_persistence_mode_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @agenthub_template_persistence_mode_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_template'
    AND COLUMN_NAME = 'persistence_mode'
);
SET @agenthub_template_persistence_mode_sql := IF(
  @agenthub_template_persistence_mode_exists = 0,
  'ALTER TABLE `t_agenthub_template` ADD COLUMN `persistence_mode` varchar(32) DEFAULT NULL AFTER `version`',
  'SELECT 1'
);
PREPARE stmt FROM @agenthub_template_persistence_mode_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

UPDATE t_agenthub_template t
JOIN t_agenthub_instance i ON i.agent_id = t.source_agent_id AND i.deleted_at IS NULL
SET t.persistence_mode = i.persistence_mode
WHERE t.persistence_mode IS NULL
  AND t.source_agent_id <> 'market';

SELECT RELEASE_LOCK('cubemaster_migration_0008_agenthub_template_persistence_mode');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0008_agenthub_template_persistence_mode', 60);

SET @agenthub_template_persistence_mode_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_template'
    AND COLUMN_NAME = 'persistence_mode'
);
SET @agenthub_template_persistence_mode_down_sql := IF(
  @agenthub_template_persistence_mode_exists > 0,
  'ALTER TABLE `t_agenthub_template` DROP COLUMN `persistence_mode`',
  'SELECT 1'
);
PREPARE stmt FROM @agenthub_template_persistence_mode_down_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @agenthub_instance_persistence_mode_exists := (
  SELECT COUNT(*)
  FROM INFORMATION_SCHEMA.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 't_agenthub_instance'
    AND COLUMN_NAME = 'persistence_mode'
);
SET @agenthub_instance_persistence_mode_down_sql := IF(
  @agenthub_instance_persistence_mode_exists > 0,
  'ALTER TABLE `t_agenthub_instance` DROP COLUMN `persistence_mode`',
  'SELECT 1'
);
PREPARE stmt FROM @agenthub_instance_persistence_mode_down_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SELECT RELEASE_LOCK('cubemaster_migration_0008_agenthub_template_persistence_mode');
