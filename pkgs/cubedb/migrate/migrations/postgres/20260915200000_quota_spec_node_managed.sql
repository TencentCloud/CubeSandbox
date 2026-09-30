-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Quota spec: node_managed marks rows created via the node-level API. Such
-- rows are fully authoritative (all five fields); rows created by the
-- cluster default (node_managed=false) are authoritative for the
-- paused-release ratio only. A sentinel row (node_id='*') stores the
-- cluster default. (PostgreSQL)

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260915200000_quota_spec_node_managed', 60);

ALTER TABLE t_cube_node_quota_spec
  ADD COLUMN node_managed boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN t_cube_node_quota_spec.node_managed IS 'true: row written via node-level API (all fields authoritative); false: cluster-managed row (ratio only)';

ALTER TABLE t_cube_node_registration
  ADD COLUMN paused_release_ratio double precision;
COMMENT ON COLUMN t_cube_node_registration.paused_release_ratio IS 'paused-release ratio reported by cubelet heartbeats; NULL = not reported';

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260915200000_quota_spec_node_managed'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260915200000_quota_spec_node_managed', 60);

ALTER TABLE t_cube_node_registration DROP COLUMN paused_release_ratio;
ALTER TABLE t_cube_node_quota_spec DROP COLUMN node_managed;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260915200000_quota_spec_node_managed'));
