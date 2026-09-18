-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Node registration: record the node's physical capacity (CPU count, total
-- memory MB) reported by cubelet, distinct from capacity_json which carries
-- the applied quota. The overcommit guards validate against these physical
-- totals; NULL/zero means "not reported by this cubelet" and falls back to the
-- legacy capacity-snapshot derivation.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260930100000_node_phys_capacity', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'cpu_count',
  "bigint NOT NULL DEFAULT 0 COMMENT 'physical CPU count (logical cores) reported by cubelet; 0 = not reported' AFTER `host_kernel_release`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'mem_total_mb',
  "bigint NOT NULL DEFAULT 0 COMMENT 'physical memory total (MB) reported by cubelet; 0 = not reported' AFTER `cpu_count`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260930100000_node_phys_capacity');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260930100000_node_phys_capacity', 60);

CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'mem_total_mb');
CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpu_count');

SELECT RELEASE_LOCK('cubemaster_migration_20260930100000_node_phys_capacity');
