-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub global settings (e.g. the shared LLM API key configured from
-- the WebUI). Keeping this in goose mirrors the AgentHub instance schema so
-- fresh installs and upgrades are recorded in goose_db_version.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0007_agenthub_settings', 60);

CREATE TABLE IF NOT EXISTS `t_agenthub_setting` (
  `setting_key` varchar(128) NOT NULL,
  `setting_value` text DEFAULT NULL,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`setting_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- WebUI account for the digital assistant console. The default admin/admin
-- account is seeded by CubeAPI on first connect with a bcrypt password hash
-- (so the password is never stored in plaintext); the password can then be
-- changed from the WebUI.
CREATE TABLE IF NOT EXISTS `t_agenthub_user` (
  `username` varchar(128) NOT NULL,
  `password` varchar(255) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_agenthub_session` (
  `token` varchar(128) NOT NULL,
  `username` varchar(128) NOT NULL,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `expires_at` datetime NOT NULL,
  PRIMARY KEY (`token`),
  KEY `idx_agenthub_session_expires` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_0007_agenthub_settings');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0007_agenthub_settings', 60);

DROP TABLE IF EXISTS `t_agenthub_session`;
DROP TABLE IF EXISTS `t_agenthub_user`;
DROP TABLE IF EXISTS `t_agenthub_setting`;

SELECT RELEASE_LOCK('cubemaster_migration_0007_agenthub_settings');
