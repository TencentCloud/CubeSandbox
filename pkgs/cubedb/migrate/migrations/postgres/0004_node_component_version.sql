-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Node-component version inventory (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0004_node_component_version', 60);

CREATE TABLE IF NOT EXISTS t_cube_node_component_version (
  id bigserial NOT NULL,
  node_id varchar(128) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL DEFAULT '',
  commit varchar(64) NOT NULL DEFAULT '',
  build_time varchar(64) NOT NULL DEFAULT '',
  source varchar(32) NOT NULL DEFAULT '',
  reported_unix bigint NOT NULL DEFAULT 0,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_node_component ON t_cube_node_component_version (node_id, component);
CREATE INDEX IF NOT EXISTS idx_ncv_node ON t_cube_node_component_version (node_id);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0004_node_component_version'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0004_node_component_version', 60);

DROP TABLE IF EXISTS t_cube_node_component_version;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0004_node_component_version'));
