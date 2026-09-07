-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Snapshot + pause-snapshot tables (PostgreSQL counterpart of
-- mysql/20260814143000_create_snapshot_tables.sql) and CoW backend on
-- the running-sandbox spec row. Template tables unchanged.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260814143000_create_snapshot_tables', 60);

CREATE TABLE IF NOT EXISTS t_cube_snapshot (
  id bigserial NOT NULL,
  created_at timestamp DEFAULT NULL,
  updated_at timestamp DEFAULT NULL,
  deleted_at timestamp DEFAULT NULL,
  snapshot_id varchar(128) NOT NULL DEFAULT '',
  origin_sandbox_id varchar(128) NOT NULL DEFAULT '',
  origin_node_id varchar(128) NOT NULL DEFAULT '',
  origin_node_ip varchar(64) NOT NULL DEFAULT '',
  display_name varchar(256) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT '',
  version varchar(32) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  last_error text,
  retain boolean NOT NULL DEFAULT false,
  rootfs_size_bytes_at_snapshot bigint NOT NULL DEFAULT 0,
  origin_host_facts_json text,
  request_json text NOT NULL DEFAULT '',
  backend varchar(16) NOT NULL DEFAULT '',
  remote_status varchar(32) NOT NULL DEFAULT '',
  export_uuids text,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_cube_snapshot_id ON t_cube_snapshot (snapshot_id);
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_origin_sandbox ON t_cube_snapshot (origin_sandbox_id);
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_origin_node ON t_cube_snapshot (origin_node_id);
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_backend ON t_cube_snapshot (backend);
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_status ON t_cube_snapshot (status);
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_deleted_at ON t_cube_snapshot (deleted_at);

CREATE TABLE IF NOT EXISTS t_cube_pause_snapshot (
  id bigserial NOT NULL,
  created_at timestamp DEFAULT NULL,
  updated_at timestamp DEFAULT NULL,
  snapshot_id varchar(128) NOT NULL DEFAULT '',
  sandbox_id varchar(128) NOT NULL DEFAULT '',
  node_id varchar(128) NOT NULL DEFAULT '',
  node_ip varchar(64) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  last_error text,
  plugin_volume_ids text,
  backend varchar(16) NOT NULL DEFAULT '',
  remote_status varchar(32) NOT NULL DEFAULT '',
  origin_host_facts_json text,
  export_uuids text,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_pause_snapshot_id ON t_cube_pause_snapshot (snapshot_id);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_pause_sandbox_id ON t_cube_pause_snapshot (sandbox_id);
CREATE INDEX IF NOT EXISTS idx_pause_snapshot_status ON t_cube_pause_snapshot (status);
CREATE INDEX IF NOT EXISTS idx_pause_snapshot_backend ON t_cube_pause_snapshot (backend);

SELECT cubemaster_assert_table_exists('t_cube_sandbox_spec');

SELECT cubemaster_add_column_if_missing('t_cube_sandbox_spec', 'backend', 'varchar(16) NOT NULL DEFAULT ''''');

CREATE INDEX IF NOT EXISTS idx_sandbox_spec_backend ON t_cube_sandbox_spec (backend);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260814143000_create_snapshot_tables'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260814143000_create_snapshot_tables', 60);

SELECT cubemaster_drop_index_if_exists('t_cube_sandbox_spec', 'idx_sandbox_spec_backend');
SELECT cubemaster_drop_column_if_exists('t_cube_sandbox_spec', 'backend');
DROP TABLE IF EXISTS t_cube_pause_snapshot;
DROP TABLE IF EXISTS t_cube_snapshot;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260814143000_create_snapshot_tables'));
