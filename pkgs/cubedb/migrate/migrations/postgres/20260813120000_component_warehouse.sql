-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Component warehouse (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260813120000_warehouse', 60);

CREATE TABLE IF NOT EXISTS t_component_warehouse (
  id bigserial NOT NULL,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  source varchar(32) NOT NULL DEFAULT '',
  source_ref varchar(256) NOT NULL DEFAULT '',
  object_key varchar(512) NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  checksum varchar(128) NOT NULL DEFAULT '',
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_wh_arch_comp_ver UNIQUE (arch, component, version)
);
CREATE INDEX IF NOT EXISTS idx_wh_component ON t_component_warehouse (component, version);

CREATE TABLE IF NOT EXISTS t_component_import_job (
  id varchar(36) NOT NULL,
  source varchar(32) NOT NULL,
  source_ref varchar(256) NOT NULL DEFAULT '',
  tag varchar(128) NOT NULL DEFAULT '',
  arch varchar(16) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT 'pending',
  error text,
  bytes_total bigint NOT NULL DEFAULT 0,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS idx_import_status ON t_component_import_job (status, created_at);

CREATE TABLE IF NOT EXISTS t_component_preinstall_job (
  id varchar(36) NOT NULL,
  node_id varchar(128) NOT NULL,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  status varchar(32) NOT NULL DEFAULT 'pending',
  error text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS idx_preinstall_node_status ON t_component_preinstall_job (node_id, status);
CREATE INDEX IF NOT EXISTS idx_preinstall_comp ON t_component_preinstall_job (arch, component, version);

CREATE TABLE IF NOT EXISTS t_component_node_install (
  id bigserial NOT NULL,
  node_id varchar(128) NOT NULL,
  arch varchar(16) NOT NULL,
  component varchar(64) NOT NULL,
  version varchar(128) NOT NULL,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  CONSTRAINT uk_node_install UNIQUE (node_id, arch, component, version)
);
CREATE INDEX IF NOT EXISTS idx_node_install_node ON t_component_node_install (node_id);
CREATE INDEX IF NOT EXISTS idx_node_install_comp ON t_component_node_install (arch, component, version);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260813120000_warehouse'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260813120000_warehouse', 60);

DROP TABLE IF EXISTS t_component_node_install;
DROP TABLE IF EXISTS t_component_preinstall_job;
DROP TABLE IF EXISTS t_component_import_job;
DROP TABLE IF EXISTS t_component_warehouse;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260813120000_warehouse'));
