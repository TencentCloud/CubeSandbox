-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Quota spec: drop the per-node overcommit guard columns. The guards are now
-- fixed ceilings (CPU 20x, Mem 3x) enforced in code by both CubeOps and
-- ops-agent; no per-node configuration. (PostgreSQL)

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260916200000_quota_spec_drop_overcommit', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_node_quota_spec', 'max_cpu_overcommit');
SELECT cubemaster_drop_column_if_exists('t_cube_node_quota_spec', 'max_mem_overcommit');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260916200000_quota_spec_drop_overcommit'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260916200000_quota_spec_drop_overcommit', 60);

SELECT cubemaster_add_column_if_missing('t_cube_node_quota_spec', 'max_cpu_overcommit', 'double precision NOT NULL DEFAULT 8');
SELECT cubemaster_add_column_if_missing('t_cube_node_quota_spec', 'max_mem_overcommit', 'double precision NOT NULL DEFAULT 2');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260916200000_quota_spec_drop_overcommit'));
