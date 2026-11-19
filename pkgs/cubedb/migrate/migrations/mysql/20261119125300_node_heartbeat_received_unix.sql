-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Store server-side heartbeat receive time as Unix seconds, independently of
-- time zones. Existing rows stay unknown (0) until the next heartbeat.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261119125300_heartbeat_received_unix', 60);

CALL cubemaster_assert_table_exists('t_cube_node_status');

CALL cubemaster_add_column_if_missing(
  't_cube_node_status',
  'heartbeat_received_unix',
  "bigint NOT NULL DEFAULT 0 COMMENT 'unix seconds when CubeOps received the last heartbeat; 0 = unknown'"
);

SELECT RELEASE_LOCK('cubemaster_migration_20261119125300_heartbeat_received_unix');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261119125300_heartbeat_received_unix', 60);

CALL cubemaster_drop_column_if_exists('t_cube_node_status', 'heartbeat_received_unix');

SELECT RELEASE_LOCK('cubemaster_migration_20261119125300_heartbeat_received_unix');
