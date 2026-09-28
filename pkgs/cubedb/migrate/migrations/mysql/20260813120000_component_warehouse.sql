-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Component warehouse: CubeOps-owned inventory of versioned component trees
-- extracted from one-click packages. Coverage uses t_component_node_install,
-- not the live heartbeat matrix.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260813120000_warehouse', 60);

CREATE TABLE IF NOT EXISTS `t_component_warehouse` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `arch` varchar(16) NOT NULL,
  `component` varchar(64) NOT NULL,
  `version` varchar(128) NOT NULL,
  `source` varchar(32) NOT NULL DEFAULT '',
  `source_ref` varchar(256) NOT NULL DEFAULT '',
  `object_key` varchar(512) NOT NULL DEFAULT '',
  `size_bytes` bigint NOT NULL DEFAULT 0,
  `checksum` varchar(128) NOT NULL DEFAULT '',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_wh_arch_comp_ver` (`arch`, `component`, `version`),
  KEY `idx_wh_component` (`component`, `version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_component_import_job` (
  `id` varchar(36) NOT NULL,
  `source` varchar(32) NOT NULL,
  `source_ref` varchar(256) NOT NULL DEFAULT '',
  `tag` varchar(128) NOT NULL DEFAULT '',
  `arch` varchar(16) NOT NULL DEFAULT '',
  `status` varchar(32) NOT NULL DEFAULT 'pending',
  `error` text,
  `bytes_total` bigint NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_import_status` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_component_preinstall_job` (
  `id` varchar(36) NOT NULL,
  `node_id` varchar(128) NOT NULL,
  `arch` varchar(16) NOT NULL,
  `component` varchar(64) NOT NULL,
  `version` varchar(128) NOT NULL,
  `status` varchar(32) NOT NULL DEFAULT 'pending',
  `error` text,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_preinstall_node_status` (`node_id`, `status`),
  KEY `idx_preinstall_comp` (`arch`, `component`, `version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_component_node_install` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(128) NOT NULL,
  `arch` varchar(16) NOT NULL,
  `component` varchar(64) NOT NULL,
  `version` varchar(128) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_node_install` (`node_id`, `arch`, `component`, `version`),
  KEY `idx_node_install_node` (`node_id`),
  KEY `idx_node_install_comp` (`arch`, `component`, `version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20260813120000_warehouse');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260813120000_warehouse', 60);

DROP TABLE IF EXISTS `t_component_node_install`;
DROP TABLE IF EXISTS `t_component_preinstall_job`;
DROP TABLE IF EXISTS `t_component_import_job`;
DROP TABLE IF EXISTS `t_component_warehouse`;

SELECT RELEASE_LOCK('cubemaster_migration_20260813120000_warehouse');
