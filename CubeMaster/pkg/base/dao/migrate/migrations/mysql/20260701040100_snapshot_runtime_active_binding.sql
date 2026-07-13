-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Split snapshot runtime binding current state from historical runtime refs.
-- The active table is keyed by (sandbox_id, binding_type), so concurrent
-- writers serialize on the DB row instead of emulating current state with
-- UPDATE(status=RELEASED)+INSERT on the historical ref table.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260701040100_rt_active', 60);

CALL cubemaster_assert_table_exists('t_cube_snapshot_runtime_ref');

UPDATE `t_cube_snapshot_runtime_ref`
   SET `binding_type` = IF(TRIM(`binding_type`) = '', 'memory_backing', TRIM(`binding_type`)),
       `updated_at` = CURRENT_TIMESTAMP
 WHERE `status` = 'ACTIVE';

UPDATE `t_cube_snapshot_runtime_ref` old
JOIN (
    SELECT `sandbox_id`, `binding_type`, MAX(`id`) AS keep_id
      FROM `t_cube_snapshot_runtime_ref`
     WHERE `status` = 'ACTIVE'
     GROUP BY `sandbox_id`, `binding_type`
    HAVING COUNT(*) > 1
) dup
  ON old.`sandbox_id` = dup.`sandbox_id`
 AND old.`binding_type` = dup.`binding_type`
 AND old.`id` <> dup.`keep_id`
   SET old.`status` = 'RELEASED',
       old.`released_at` = COALESCE(old.`released_at`, CURRENT_TIMESTAMP),
       old.`updated_at` = CURRENT_TIMESTAMP,
       old.`last_error` = 'deduplicated during snapshot runtime active migration';

CREATE TABLE IF NOT EXISTS `t_cube_snapshot_runtime_active` (
  `sandbox_id` varchar(128) NOT NULL,
  `binding_type` varchar(64) NOT NULL DEFAULT 'memory_backing',
  `snapshot_id` varchar(128) NOT NULL,
  `node_id` varchar(128) NOT NULL DEFAULT '',
  `node_ip` varchar(64) NOT NULL DEFAULT '',
  `memory_vol` varchar(256) NOT NULL DEFAULT '',
  `rootfs_vol` varchar(256) NOT NULL DEFAULT '',
  `sandbox_gen` int unsigned NOT NULL DEFAULT 0,
  `attached_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `last_seen_at` datetime DEFAULT NULL,
  `last_error` text,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`sandbox_id`, `binding_type`),
  KEY `idx_snapshot_runtime_active_snapshot` (`snapshot_id`),
  KEY `idx_snapshot_runtime_active_node` (`node_id`),
  KEY `idx_snapshot_runtime_active_node_ip` (`node_ip`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CALL cubemaster_assert_column_exists('t_cube_snapshot_runtime_active', 'sandbox_id');
CALL cubemaster_assert_column_exists('t_cube_snapshot_runtime_active', 'binding_type');
CALL cubemaster_assert_column_exists('t_cube_snapshot_runtime_active', 'snapshot_id');

SET @snapshot_runtime_active_pk_ok := (
  SELECT COUNT(1)
    FROM INFORMATION_SCHEMA.STATISTICS
   WHERE TABLE_SCHEMA = DATABASE()
     AND TABLE_NAME = 't_cube_snapshot_runtime_active'
     AND INDEX_NAME = 'PRIMARY'
     AND COLUMN_NAME IN ('sandbox_id', 'binding_type')
);
SET @snapshot_runtime_active_snapshot_idx_ok := (
  SELECT COUNT(1)
    FROM INFORMATION_SCHEMA.STATISTICS
   WHERE TABLE_SCHEMA = DATABASE()
     AND TABLE_NAME = 't_cube_snapshot_runtime_active'
     AND INDEX_NAME = 'idx_snapshot_runtime_active_snapshot'
     AND COLUMN_NAME = 'snapshot_id'
);
SET @snapshot_runtime_active_node_idx_ok := (
  SELECT COUNT(1)
    FROM INFORMATION_SCHEMA.STATISTICS
   WHERE TABLE_SCHEMA = DATABASE()
     AND TABLE_NAME = 't_cube_snapshot_runtime_active'
     AND INDEX_NAME = 'idx_snapshot_runtime_active_node'
     AND COLUMN_NAME = 'node_id'
);
SET @snapshot_runtime_active_node_ip_idx_ok := (
  SELECT COUNT(1)
    FROM INFORMATION_SCHEMA.STATISTICS
   WHERE TABLE_SCHEMA = DATABASE()
     AND TABLE_NAME = 't_cube_snapshot_runtime_active'
     AND INDEX_NAME = 'idx_snapshot_runtime_active_node_ip'
     AND COLUMN_NAME = 'node_ip'
);
SET @snapshot_runtime_active_schema_ok := (
  @snapshot_runtime_active_pk_ok = 2
  AND @snapshot_runtime_active_snapshot_idx_ok = 1
  AND @snapshot_runtime_active_node_idx_ok = 1
  AND @snapshot_runtime_active_node_ip_idx_ok = 1
);
SET @snapshot_runtime_active_schema_sql := IF(
  @snapshot_runtime_active_schema_ok,
  'SELECT 1',
  'SIGNAL SQLSTATE ''45000'' SET MESSAGE_TEXT = ''t_cube_snapshot_runtime_active schema mismatch'''
);
PREPARE stmt FROM @snapshot_runtime_active_schema_sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

INSERT INTO `t_cube_snapshot_runtime_active` (
    `sandbox_id`,
    `binding_type`,
    `snapshot_id`,
    `node_id`,
    `node_ip`,
    `memory_vol`,
    `rootfs_vol`,
    `sandbox_gen`,
    `attached_at`,
    `last_seen_at`,
    `last_error`,
    `created_at`,
    `updated_at`
)
SELECT
    `sandbox_id`,
    `binding_type`,
    `snapshot_id`,
    `node_id`,
    `node_ip`,
    `memory_vol`,
    `rootfs_vol`,
    `sandbox_gen`,
    `attached_at`,
    `last_seen_at`,
    `last_error`,
    `created_at`,
    `updated_at`
  FROM `t_cube_snapshot_runtime_ref`
 WHERE `status` = 'ACTIVE'
ON DUPLICATE KEY UPDATE
    `snapshot_id` = VALUES(`snapshot_id`),
    `node_id` = VALUES(`node_id`),
    `node_ip` = VALUES(`node_ip`),
    `memory_vol` = VALUES(`memory_vol`),
    `rootfs_vol` = VALUES(`rootfs_vol`),
    `sandbox_gen` = VALUES(`sandbox_gen`),
    `attached_at` = VALUES(`attached_at`),
    `last_seen_at` = VALUES(`last_seen_at`),
    `last_error` = VALUES(`last_error`),
    `updated_at` = VALUES(`updated_at`);

SELECT RELEASE_LOCK('cubemaster_migration_20260701040100_rt_active');

-- +goose Down
--
-- Down only removes the active-state projection. ACTIVE duplicate rows that
-- were marked RELEASED during Up are not restored; this mirrors the
-- normalization style used by prior migrations before adding uniqueness.

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260701040100_rt_active', 60);

DROP TABLE IF EXISTS `t_cube_snapshot_runtime_active`;

SELECT RELEASE_LOCK('cubemaster_migration_20260701040100_rt_active');
