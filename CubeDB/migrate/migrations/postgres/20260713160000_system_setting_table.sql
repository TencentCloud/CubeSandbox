-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- System-level settings and users split from AgentHub tables.
-- jwt_secret, secret_master_key → t_system_setting
-- admin user → t_system_user
-- AgentHub-specific keys (llm_*, openclaw_*) stay in t_agenthub_setting.
--
-- PostgreSQL counterpart of mysql/20260713160000_system_setting_table.sql.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260713160000_system_setting', 60);

-- System-level configuration table (KV structure, same as t_agenthub_setting).
CREATE TABLE IF NOT EXISTS t_system_setting (
  setting_key varchar(128) NOT NULL,
  setting_value text DEFAULT NULL,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (setting_key)
);

-- System user table (ops console admin accounts, migrated from t_agenthub_user).
CREATE TABLE IF NOT EXISTS t_system_user (
  username varchar(128) NOT NULL,
  password varchar(255) NOT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (username)
);

-- Migrate system-level keys from t_agenthub_setting to t_system_setting.
-- ON CONFLICT mirrors mysql's INSERT IGNORE semantics (skip rows that would
-- duplicate an existing primary key).
INSERT INTO t_system_setting (setting_key, setting_value)
SELECT setting_key, setting_value
  FROM t_agenthub_setting
 WHERE setting_key IN ('jwt_secret', 'secret_master_key')
ON CONFLICT (setting_key) DO NOTHING;

-- Migrate users from t_agenthub_user to t_system_user.
INSERT INTO t_system_user (username, password, created_at, updated_at)
SELECT username, password, created_at, updated_at
  FROM t_agenthub_user
ON CONFLICT (username) DO NOTHING;

-- NOTE: We intentionally do NOT delete jwt_secret / secret_master_key from
-- t_agenthub_setting here. Keeping the old keys in place allows a safe
-- rollback to the previous CubeAPI binary, which still reads from
-- t_agenthub_setting. A follow-up migration (after the upgrade window is
-- confirmed stable) will remove them. See R15 in the ops-extraction review.

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260713160000_system_setting'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260713160000_system_setting', 60);

-- Move system-level keys back to t_agenthub_setting.
INSERT INTO t_agenthub_setting (setting_key, setting_value)
SELECT setting_key, setting_value FROM t_system_setting
WHERE setting_key IN ('jwt_secret', 'secret_master_key')
ON CONFLICT (setting_key) DO NOTHING;

-- Move users back to t_agenthub_user.
INSERT INTO t_agenthub_user (username, password, created_at, updated_at)
SELECT username, password, created_at, updated_at FROM t_system_user
ON CONFLICT (username) DO NOTHING;

DROP TABLE IF EXISTS t_system_user;
DROP TABLE IF EXISTS t_system_setting;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260713160000_system_setting'));
