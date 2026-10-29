-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Node registration: record the node's physical capacity (CPU count, total
-- memory MB) reported by cubelet, distinct from capacity_json which carries
-- the applied quota. The overcommit guards validate against these physical
-- totals; zero means "not reported by this cubelet" and the guard fails closed
-- until both totals are reported. (PostgreSQL)

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260930100000_node_phys_capacity', 60);

SELECT cubemaster_add_column_if_missing('t_cube_node_registration', 'cpu_count', 'bigint NOT NULL DEFAULT 0');
COMMENT ON COLUMN t_cube_node_registration.cpu_count IS 'physical CPU count (logical cores) reported by cubelet; 0 = not reported';

SELECT cubemaster_add_column_if_missing('t_cube_node_registration', 'mem_total_mb', 'bigint NOT NULL DEFAULT 0');
COMMENT ON COLUMN t_cube_node_registration.mem_total_mb IS 'physical memory total (MB) reported by cubelet; 0 = not reported';

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260930100000_node_phys_capacity'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260930100000_node_phys_capacity', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'mem_total_mb');
SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpu_count');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260930100000_node_phys_capacity'));
