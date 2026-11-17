-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Node registration: report the effective paused-release ratio on heartbeat.
-- (The node_managed column on t_cube_node_quota_spec was folded into its
-- CREATE TABLE migration.)

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915200000_quota_spec_node_managed', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'paused_release_ratio',
  "double NULL DEFAULT NULL COMMENT 'paused-release ratio reported by cubelet heartbeats; NULL = not reported'"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260915200000_quota_spec_node_managed');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915200000_quota_spec_node_managed', 60);

CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'paused_release_ratio');

SELECT RELEASE_LOCK('cubemaster_migration_20260915200000_quota_spec_node_managed');
