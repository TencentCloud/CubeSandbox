-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Restart status lives in its own table. t_cube_sandbox_spec is the create
-- request and changes rarely; this row is rewritten on every restart event.
-- A missing row does not block a node-local restart.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260924120000_sbx_status', 60);

CREATE TABLE IF NOT EXISTS `t_cube_sandbox_status` (
  `id`                         bigint unsigned NOT NULL AUTO_INCREMENT,
  `sandbox_id`                 varchar(64)  NOT NULL,
  `host_id`                    varchar(64)  NOT NULL DEFAULT '',
  `phase`                      varchar(32)  NOT NULL DEFAULT '',
  `restart_policy`             varchar(32)  NOT NULL DEFAULT 'Never',
  `restart_state`              varchar(32)  NOT NULL DEFAULT '',
  `restart_count`              int          NOT NULL DEFAULT 0,
  `last_exit_code`             int          DEFAULT NULL,
  `last_exit_reason`           varchar(32)  NOT NULL DEFAULT '',
  `last_finished_at`           datetime(3)  DEFAULT NULL,
  `last_restart_at`            datetime(3)  DEFAULT NULL,
  `last_successful_restart_at` datetime(3)  DEFAULT NULL,
  `last_failed_restart_at`     datetime(3)  DEFAULT NULL,
  `next_restart_at`            datetime(3)  DEFAULT NULL,
  `status_seq`                 bigint       NOT NULL DEFAULT 0,
  `reported_at`                datetime(3)  NOT NULL,
  `created_at`                 datetime(3)  DEFAULT NULL,
  `updated_at`                 datetime(3)  DEFAULT NULL,
  `deleted_at`                 datetime(3)  DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_sandbox_status_sandbox_id` (`sandbox_id`),
  KEY `idx_sandbox_status_host` (`host_id`),
  KEY `idx_sandbox_status_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20260924120000_sbx_status');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260924120000_sbx_status', 60);

DROP TABLE IF EXISTS `t_cube_sandbox_status`;

SELECT RELEASE_LOCK('cubemaster_migration_20260924120000_sbx_status');
