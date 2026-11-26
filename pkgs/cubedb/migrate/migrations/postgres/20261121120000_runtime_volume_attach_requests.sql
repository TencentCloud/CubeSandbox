-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261121120000_runtime_attach', 60);

CREATE TABLE IF NOT EXISTS t_cube_runtime_volume_attach (
  request_key char(64) NOT NULL,
  sandbox_id varchar(128) NOT NULL,
  request_id varchar(256) NOT NULL,
  execution_id varchar(256) NOT NULL,
  volume_id varchar(128) NOT NULL,
  container_path varchar(4096) NOT NULL,
  readonly boolean NOT NULL,
  spec_row_id bigint NOT NULL,
  volume_record_id bigint NOT NULL,
  claim_token char(36) NOT NULL,
  outcome varchar(16) NOT NULL,
  PRIMARY KEY (request_key)
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261121120000_runtime_attach'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261121120000_runtime_attach', 60);

DROP TABLE IF EXISTS t_cube_runtime_volume_attach;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261121120000_runtime_attach'));
