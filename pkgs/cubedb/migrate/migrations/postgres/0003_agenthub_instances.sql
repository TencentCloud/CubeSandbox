-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub digital assistant schema (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0003_agenthub_instances', 60);

CREATE TABLE IF NOT EXISTS t_agenthub_instance (
  id bigserial NOT NULL,
  agent_id varchar(128) NOT NULL,
  sandbox_id varchar(128) NOT NULL,
  template_id varchar(128) NOT NULL,
  name varchar(128) NOT NULL,
  engine varchar(32) NOT NULL,
  env varchar(32) NOT NULL,
  model varchar(128) NOT NULL,
  version varchar(64) NOT NULL,
  status varchar(32) NOT NULL,
  bots jsonb DEFAULT NULL,
  avatar varchar(128) NOT NULL,
  avatar_tone varchar(32) NOT NULL,
  domain varchar(255) NOT NULL DEFAULT '',
  gateway_port integer NOT NULL DEFAULT 18789,
  env_port integer NOT NULL DEFAULT 8080,
  gateway_token varchar(255) DEFAULT NULL,
  wecom_bot_id varchar(255) DEFAULT NULL,
  wecom_bot_secret varchar(255) DEFAULT NULL,
  last_error text DEFAULT NULL,
  setup_exit_code integer DEFAULT NULL,
  base_snapshot_id varchar(128) DEFAULT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_agenthub_agent_id ON t_agenthub_instance (agent_id);
CREATE UNIQUE INDEX IF NOT EXISTS uk_agenthub_sandbox_id ON t_agenthub_instance (sandbox_id);
CREATE INDEX IF NOT EXISTS idx_agenthub_status ON t_agenthub_instance (status);
CREATE INDEX IF NOT EXISTS idx_agenthub_deleted_at ON t_agenthub_instance (deleted_at);

CREATE TABLE IF NOT EXISTS t_agenthub_snapshot (
  id bigserial NOT NULL,
  snapshot_id varchar(128) NOT NULL,
  agent_id varchar(128) NOT NULL,
  sandbox_id varchar(128) NOT NULL,
  name varchar(255) DEFAULT NULL,
  status varchar(32) NOT NULL DEFAULT 'unknown',
  origin_sandbox_id varchar(128) DEFAULT NULL,
  published_template_id varchar(128) DEFAULT NULL,
  parent_snapshot_id varchar(128) DEFAULT NULL,
  is_healthy boolean NOT NULL DEFAULT false,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_agenthub_snapshot_id ON t_agenthub_snapshot (snapshot_id);
CREATE INDEX IF NOT EXISTS idx_agenthub_snapshot_agent ON t_agenthub_snapshot (agent_id, deleted_at);
CREATE INDEX IF NOT EXISTS idx_agenthub_snapshot_sandbox ON t_agenthub_snapshot (sandbox_id, deleted_at);

CREATE TABLE IF NOT EXISTS t_agenthub_template (
  id bigserial NOT NULL,
  template_id varchar(128) NOT NULL,
  name varchar(255) NOT NULL,
  source_agent_id varchar(128) NOT NULL,
  source_snapshot_id varchar(128) NOT NULL,
  source_sandbox_id varchar(128) NOT NULL,
  model varchar(128) NOT NULL,
  version varchar(64) NOT NULL,
  recommended boolean NOT NULL DEFAULT false,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_agenthub_template_id ON t_agenthub_template (template_id);
CREATE INDEX IF NOT EXISTS idx_agenthub_template_deleted_at ON t_agenthub_template (deleted_at);

CREATE TABLE IF NOT EXISTS t_agenthub_operation (
  id bigserial NOT NULL,
  operation_id varchar(128) NOT NULL,
  agent_id varchar(128) NOT NULL,
  sandbox_id varchar(128) NOT NULL,
  operation_type varchar(32) NOT NULL,
  status varchar(32) NOT NULL,
  target_id varchar(128) DEFAULT NULL,
  error_message text DEFAULT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_agenthub_operation_id ON t_agenthub_operation (operation_id);
CREATE INDEX IF NOT EXISTS idx_agenthub_operation_agent ON t_agenthub_operation (agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_agenthub_operation_status ON t_agenthub_operation (status);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0003_agenthub_instances'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0003_agenthub_instances', 60);

DROP TABLE IF EXISTS t_agenthub_operation;
DROP TABLE IF EXISTS t_agenthub_template;
DROP TABLE IF EXISTS t_agenthub_snapshot;
DROP TABLE IF EXISTS t_agenthub_instance;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0003_agenthub_instances'));
