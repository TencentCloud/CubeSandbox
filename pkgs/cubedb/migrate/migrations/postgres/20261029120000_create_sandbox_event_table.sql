-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- PostgreSQL counterpart of mysql/20261029120000_create_sandbox_event_table.sql.
-- Append-only sandbox lifecycle history that outlives the sandbox.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261029120000_sbx_event', 60);

CREATE TABLE IF NOT EXISTS t_cube_sandbox_event (
  id bigserial NOT NULL,
  sandbox_id varchar(64) NOT NULL,
  event varchar(32) NOT NULL,
  template_id varchar(128) NOT NULL DEFAULT '',
  reason varchar(64) NOT NULL DEFAULT '',
  created_at timestamp NOT NULL,
  PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS idx_sandbox_event_sandbox_id ON t_cube_sandbox_event (sandbox_id, id);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261029120000_sbx_event'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261029120000_sbx_event', 60);

DROP TABLE IF EXISTS t_cube_sandbox_event;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261029120000_sbx_event'));
