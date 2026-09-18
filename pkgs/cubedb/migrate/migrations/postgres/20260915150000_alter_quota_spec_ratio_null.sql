-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- paused_resource_release_ratio becomes nullable: NULL = inherit the
-- cluster-level default (t_system_setting), a value (incl. 0) = explicit
-- per-node override. Existing rows keep their explicit values. (PostgreSQL)

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null', 60);

ALTER TABLE t_cube_node_quota_spec
  ALTER COLUMN paused_resource_release_ratio DROP NOT NULL,
  ALTER COLUMN paused_resource_release_ratio SET DEFAULT NULL;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null', 60);

UPDATE t_cube_node_quota_spec SET paused_resource_release_ratio = 0 WHERE paused_resource_release_ratio IS NULL;
ALTER TABLE t_cube_node_quota_spec
  ALTER COLUMN paused_resource_release_ratio SET NOT NULL,
  ALTER COLUMN paused_resource_release_ratio SET DEFAULT 0;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null'));
