-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- PostgreSQL counterpart of mysql/20260924120000_create_sandbox_status_table.sql.
-- Restart status is stored apart from t_cube_sandbox_spec so frequent status
-- writes do not rewrite the create request.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260924120000_sbx_status', 60);

CREATE TABLE IF NOT EXISTS t_cube_sandbox_status (
  id bigserial NOT NULL,
  sandbox_id varchar(64) NOT NULL,
  host_id varchar(64) NOT NULL DEFAULT '',
  phase varchar(32) NOT NULL DEFAULT '',
  restart_policy varchar(32) NOT NULL DEFAULT 'Never',
  restart_state varchar(32) NOT NULL DEFAULT '',
  restart_count int NOT NULL DEFAULT 0,
  last_exit_code int DEFAULT NULL,
  last_exit_reason varchar(32) NOT NULL DEFAULT '',
  last_finished_at timestamp DEFAULT NULL,
  last_restart_at timestamp DEFAULT NULL,
  last_successful_restart_at timestamp DEFAULT NULL,
  last_failed_restart_at timestamp DEFAULT NULL,
  next_restart_at timestamp DEFAULT NULL,
  status_seq bigint NOT NULL DEFAULT 0,
  reported_at timestamp NOT NULL,
  created_at timestamp DEFAULT NULL,
  updated_at timestamp DEFAULT NULL,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_sandbox_status_sandbox_id ON t_cube_sandbox_status (sandbox_id);
CREATE INDEX IF NOT EXISTS idx_sandbox_status_host ON t_cube_sandbox_status (host_id);
CREATE INDEX IF NOT EXISTS idx_sandbox_status_deleted_at ON t_cube_sandbox_status (deleted_at);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260924120000_sbx_status'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260924120000_sbx_status', 60);

DROP TABLE IF EXISTS t_cube_sandbox_status;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260924120000_sbx_status'));
