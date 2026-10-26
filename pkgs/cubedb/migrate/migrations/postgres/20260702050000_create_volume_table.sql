-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Create the volume management table used by the Volume HTTP API.
-- PostgreSQL counterpart of mysql/20260702050000_create_volume_table.sql.
-- See pkg/base/db/models/volume.go for the corresponding GORM model.
-- Rows are hard-deleted on DELETE /volumes (no soft-delete column).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260702050000_create_volume_table', 60);

CREATE TABLE IF NOT EXISTS t_cube_volume (
  id bigserial NOT NULL,
  created_at timestamp DEFAULT NULL,
  updated_at timestamp DEFAULT NULL,
  volume_id varchar(128) NOT NULL DEFAULT '',
  name varchar(128) NOT NULL DEFAULT '',
  driver varchar(128) NOT NULL DEFAULT '',
  token varchar(1024) NOT NULL DEFAULT '',
  refcount bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_volume_id ON t_cube_volume (volume_id);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_volume_name ON t_cube_volume (name);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260702050000_create_volume_table'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260702050000_create_volume_table', 60);

DROP TABLE IF EXISTS t_cube_volume;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260702050000_create_volume_table'));
