-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Desired node quota (ops-agent push source of truth). The scheduler does NOT
-- read this table: it consumes the effective quota reported by cubelet on
-- register/heartbeat (t_cube_node_registration). Revision is a monotonic
-- counter bumped on every write; ops-agent uses it to skip no-op rewrites
-- during pull reconcile. (PostgreSQL)

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260914190000_create_node_quota_spec', 60);

CREATE TABLE IF NOT EXISTS t_cube_node_quota_spec (
  id                            bigserial PRIMARY KEY,
  created_at                    timestamptz,
  updated_at                    timestamptz,
  deleted_at                    timestamptz,
  node_id                       varchar(128)  NOT NULL DEFAULT '' ,
  mcpu_limit                    bigint        NOT NULL DEFAULT 0,
  mem_limit                     varchar(64)   NOT NULL DEFAULT '',
  mvm_limit                     bigint        NOT NULL DEFAULT 0,
  creation_concurrent_num       bigint        NOT NULL DEFAULT 0,
  paused_resource_release_ratio double precision NOT NULL DEFAULT 0,
  max_cpu_overcommit            double precision NOT NULL DEFAULT 8,
  max_mem_overcommit            double precision NOT NULL DEFAULT 2,
  revision                      bigint        NOT NULL DEFAULT 0,
  updated_by                    varchar(128)  NOT NULL DEFAULT '',
  CONSTRAINT uniq_quota_spec_node UNIQUE (node_id)
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260914190000_create_node_quota_spec'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260914190000_create_node_quota_spec', 60);

DROP TABLE IF EXISTS t_cube_node_quota_spec;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260914190000_create_node_quota_spec'));
