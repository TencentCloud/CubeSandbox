-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Operator-managed machine type catalog. Membership stays on the node label
-- cube.cubesandbox.io/machine-type so CubeMaster restore placement keeps
-- reading the existing node view. Rows are deleted physically so a name can
-- be recreated after removal. (PostgreSQL)

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261118170000_machine_types', 60);

CREATE TABLE IF NOT EXISTS t_cube_machine_type (
  id           bigserial PRIMARY KEY,
  name         varchar(64)  NOT NULL DEFAULT '',
  display_name varchar(128) NOT NULL DEFAULT '',
  description  varchar(512) NOT NULL DEFAULT '',
  created_by   varchar(128) NOT NULL DEFAULT '',
  updated_by   varchar(128) NOT NULL DEFAULT '',
  created_at   timestamptz,
  updated_at   timestamptz,
  CONSTRAINT uniq_machine_type_name UNIQUE (name)
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261118170000_machine_types'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261118170000_machine_types', 60);

DROP TABLE IF EXISTS t_cube_machine_type;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261118170000_machine_types'));
