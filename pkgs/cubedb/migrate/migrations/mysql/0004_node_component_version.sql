-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Node-component version inventory (version reporting & visualization).
--
-- One row per (node_id, component): records the real version / commit /
-- build_time of every component actually installed on a node, reported by
-- cubelet on register & heartbeat. Powers the control-plane version matrix
-- (GET /internal/meta/version-matrix) and node-detail version blocks.
--
-- Deliberately NOT soft-deleted: stale components (a component removed from
-- a node) are physically deleted by the writer. Combined with the
-- (node_id, component) unique key this avoids the GORM soft-delete + unique
-- key trap where a re-reported component would resurrect a tombstoned row
-- that Find() can never see again.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0004_node_component_version', 60);

CREATE TABLE IF NOT EXISTS `t_cube_node_component_version` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(128) NOT NULL,
  `component` varchar(64) NOT NULL,
  `version` varchar(128) NOT NULL DEFAULT '',
  `commit` varchar(64) NOT NULL DEFAULT '',
  `build_time` varchar(64) NOT NULL DEFAULT '',
  `source` varchar(32) NOT NULL DEFAULT '',
  `reported_unix` bigint NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_node_component` (`node_id`, `component`),
  KEY `idx_ncv_node` (`node_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_0004_node_component_version');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0004_node_component_version', 60);

DROP TABLE IF EXISTS `t_cube_node_component_version`;

SELECT RELEASE_LOCK('cubemaster_migration_0004_node_component_version');
