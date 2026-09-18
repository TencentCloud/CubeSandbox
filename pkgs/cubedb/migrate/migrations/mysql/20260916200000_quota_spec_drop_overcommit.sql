-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Quota spec: drop the per-node overcommit guard columns. The guards are now
-- fixed ceilings (CPU 20x, Mem 3x) enforced in code by both CubeOps and
-- ops-agent; no per-node configuration.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260916200000_quota_spec_drop_overcommit', 60);

CALL cubemaster_drop_column_if_exists('t_cube_node_quota_spec', 'max_cpu_overcommit');
CALL cubemaster_drop_column_if_exists('t_cube_node_quota_spec', 'max_mem_overcommit');

SELECT RELEASE_LOCK('cubemaster_migration_20260916200000_quota_spec_drop_overcommit');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260916200000_quota_spec_drop_overcommit', 60);

CALL cubemaster_add_column_if_missing('t_cube_node_quota_spec', 'max_cpu_overcommit', "double NOT NULL DEFAULT 8 COMMENT 'push-side guard: mcpu_limit may not exceed cpuTotal*1000*this'");
CALL cubemaster_add_column_if_missing('t_cube_node_quota_spec', 'max_mem_overcommit', "double NOT NULL DEFAULT 2 COMMENT 'push-side guard: mem_limit may not exceed memTotal*this'");

SELECT RELEASE_LOCK('cubemaster_migration_20260916200000_quota_spec_drop_overcommit');
