-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Operation audit log for node management (isolate/unisolate/label changes).

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260805170000_node_operation', 60);

CREATE TABLE IF NOT EXISTS `t_cube_node_operation` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(128) NOT NULL,
  `type` varchar(32) NOT NULL DEFAULT '',
  `operator` varchar(128) NOT NULL DEFAULT '',
  `detail` text,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_node_id` (`node_id`),
  KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20260805170000_node_operation');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260805170000_node_operation', 60);

DROP TABLE IF EXISTS `t_cube_node_operation`;

SELECT RELEASE_LOCK('cubemaster_migration_20260805170000_node_operation');
