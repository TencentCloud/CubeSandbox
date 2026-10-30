-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Append-only sandbox lifecycle history (created / paused / resumed /
-- killed). Rows outlive the sandbox so its history stays readable after
-- deletion.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261029120000_sbx_event', 60);

CREATE TABLE IF NOT EXISTS `t_cube_sandbox_event` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `sandbox_id` varchar(64) NOT NULL,
  `event` varchar(32) NOT NULL,
  `template_id` varchar(128) NOT NULL DEFAULT '',
  `reason` varchar(64) NOT NULL DEFAULT '',
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_sandbox_event_sandbox_id` (`sandbox_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20261029120000_sbx_event');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261029120000_sbx_event', 60);

DROP TABLE IF EXISTS `t_cube_sandbox_event`;

SELECT RELEASE_LOCK('cubemaster_migration_20261029120000_sbx_event');
