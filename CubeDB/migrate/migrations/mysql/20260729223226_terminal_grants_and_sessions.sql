-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Web Terminal one-time grants and payload-free session audit records.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260729223226_terminal_grants_and_sessions', 60);

CREATE TABLE IF NOT EXISTS `terminal_grants` (
  `id` varchar(36) NOT NULL,
  `token_hash` varchar(64) NOT NULL,
  `kind` varchar(8) NOT NULL,
  `user_id` varchar(64) NOT NULL,
  `sandbox_id` varchar(64) NOT NULL,
  `container_id` varchar(128) NOT NULL,
  `session_id` varchar(36) DEFAULT NULL,
  `cols` smallint NOT NULL,
  `rows` smallint NOT NULL,
  `resume_offset` bigint NOT NULL DEFAULT 0,
  `created_at` datetime(3) NOT NULL,
  `expires_at` datetime(3) NOT NULL,
  `consumed_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_terminal_grants_token_hash` (`token_hash`),
  KEY `idx_terminal_grants_expires` (`expires_at`),
  KEY `idx_terminal_grants_user_created` (`user_id`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `terminal_sessions` (
  `id` varchar(36) NOT NULL,
  `user_id` varchar(64) NOT NULL,
  `sandbox_id` varchar(64) NOT NULL,
  `container_id` varchar(128) NOT NULL,
  `cubelet_host` varchar(64) NOT NULL DEFAULT '',
  `opened_at` datetime(3) NOT NULL,
  `last_seen_at` datetime(3) NOT NULL,
  `closed_at` datetime(3) DEFAULT NULL,
  `close_reason` varchar(32) DEFAULT NULL,
  `exit_code` int DEFAULT NULL,
  `bytes_in` bigint NOT NULL DEFAULT 0,
  `bytes_out` bigint NOT NULL DEFAULT 0,
  `resume_count` int NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  KEY `idx_terminal_sessions_user_active` (`user_id`, `closed_at`),
  KEY `idx_terminal_sessions_sandbox_opened` (`sandbox_id`, `opened_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20260729223226_terminal_grants_and_sessions');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260729223226_terminal_grants_and_sessions', 60);

DROP TABLE IF EXISTS `terminal_sessions`;
DROP TABLE IF EXISTS `terminal_grants`;

SELECT RELEASE_LOCK('cubemaster_migration_20260729223226_terminal_grants_and_sessions');
