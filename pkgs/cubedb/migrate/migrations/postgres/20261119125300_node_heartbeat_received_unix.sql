-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- PostgreSQL twin of the MySQL heartbeat receive-time migration.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261119125300_heartbeat_received_unix', 60);

SELECT cubemaster_assert_table_exists('t_cube_node_status');

SELECT cubemaster_add_column_if_missing(
  't_cube_node_status',
  'heartbeat_received_unix',
  'bigint NOT NULL DEFAULT 0'
);
COMMENT ON COLUMN t_cube_node_status.heartbeat_received_unix IS 'unix seconds when CubeOps received the last heartbeat; 0 = unknown';

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261119125300_heartbeat_received_unix'));

-- +goose Down
-- Revert every CubeOps replica first: a new CubeOps cannot write heartbeats
-- without this column, so nodes would turn unhealthy and unschedulable.

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261119125300_heartbeat_received_unix', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_node_status', 'heartbeat_received_unix');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261119125300_heartbeat_received_unix'));
