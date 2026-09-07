-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub digital assistant schema.
--
-- CubeMaster owns shared-database schema migration through embedded goose
-- migrations. Keep this schema here so fresh installs and upgrades are
-- recorded in goose_db_version instead of relying on one-click shell scripts.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0003_agenthub_instances', 60);

CREATE TABLE IF NOT EXISTS `t_agenthub_instance` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `agent_id` varchar(128) NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `template_id` varchar(128) NOT NULL,
  `name` varchar(128) NOT NULL,
  `engine` varchar(32) NOT NULL,
  `env` varchar(32) NOT NULL,
  `model` varchar(128) NOT NULL,
  `version` varchar(64) NOT NULL,
  `status` varchar(32) NOT NULL,
  `bots` json DEFAULT NULL,
  `avatar` varchar(128) NOT NULL,
  `avatar_tone` varchar(32) NOT NULL,
  `domain` varchar(255) NOT NULL DEFAULT '',
  `gateway_port` int NOT NULL DEFAULT 18789,
  `env_port` int NOT NULL DEFAULT 8080,
  `gateway_token` varchar(255) DEFAULT NULL,
  `wecom_bot_id` varchar(255) DEFAULT NULL,
  `wecom_bot_secret` varchar(255) DEFAULT NULL,
  `last_error` text DEFAULT NULL,
  `setup_exit_code` int DEFAULT NULL,
  `base_snapshot_id` varchar(128) DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_agent_id` (`agent_id`),
  UNIQUE KEY `uk_agenthub_sandbox_id` (`sandbox_id`),
  KEY `idx_agenthub_status` (`status`),
  KEY `idx_agenthub_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_agenthub_snapshot` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `snapshot_id` varchar(128) NOT NULL,
  `agent_id` varchar(128) NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `name` varchar(255) DEFAULT NULL,
  `status` varchar(32) NOT NULL DEFAULT 'unknown',
  `origin_sandbox_id` varchar(128) DEFAULT NULL,
  `published_template_id` varchar(128) DEFAULT NULL,
  `parent_snapshot_id` varchar(128) DEFAULT NULL,
  `is_healthy` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_snapshot_id` (`snapshot_id`),
  KEY `idx_agenthub_snapshot_agent` (`agent_id`, `deleted_at`),
  KEY `idx_agenthub_snapshot_sandbox` (`sandbox_id`, `deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_agenthub_template` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `template_id` varchar(128) NOT NULL,
  `name` varchar(255) NOT NULL,
  `source_agent_id` varchar(128) NOT NULL,
  `source_snapshot_id` varchar(128) NOT NULL,
  `source_sandbox_id` varchar(128) NOT NULL,
  `model` varchar(128) NOT NULL,
  `version` varchar(64) NOT NULL,
  `recommended` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_template_id` (`template_id`),
  KEY `idx_agenthub_template_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_agenthub_operation` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `operation_id` varchar(128) NOT NULL,
  `agent_id` varchar(128) NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `operation_type` varchar(32) NOT NULL,
  `status` varchar(32) NOT NULL,
  `target_id` varchar(128) DEFAULT NULL,
  `error_message` text DEFAULT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_agenthub_operation_id` (`operation_id`),
  KEY `idx_agenthub_operation_agent` (`agent_id`, `created_at`),
  KEY `idx_agenthub_operation_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_0003_agenthub_instances');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0003_agenthub_instances', 60);

DROP TABLE IF EXISTS `t_agenthub_operation`;
DROP TABLE IF EXISTS `t_agenthub_template`;
DROP TABLE IF EXISTS `t_agenthub_snapshot`;
DROP TABLE IF EXISTS `t_agenthub_instance`;

SELECT RELEASE_LOCK('cubemaster_migration_0003_agenthub_instances');
