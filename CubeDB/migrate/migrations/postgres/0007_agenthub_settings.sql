-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub global settings (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0007_agenthub_settings', 60);

CREATE TABLE IF NOT EXISTS t_agenthub_setting (
  setting_key varchar(128) NOT NULL,
  setting_value text DEFAULT NULL,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (setting_key)
);

CREATE TABLE IF NOT EXISTS t_agenthub_user (
  username varchar(128) NOT NULL,
  password varchar(255) NOT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (username)
);

CREATE TABLE IF NOT EXISTS t_agenthub_session (
  token varchar(128) NOT NULL,
  username varchar(128) NOT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at timestamp NOT NULL,
  PRIMARY KEY (token)
);
CREATE INDEX IF NOT EXISTS idx_agenthub_session_expires ON t_agenthub_session (expires_at);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0007_agenthub_settings'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0007_agenthub_settings', 60);

DROP TABLE IF EXISTS t_agenthub_session;
DROP TABLE IF EXISTS t_agenthub_user;
DROP TABLE IF EXISTS t_agenthub_setting;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0007_agenthub_settings'));
