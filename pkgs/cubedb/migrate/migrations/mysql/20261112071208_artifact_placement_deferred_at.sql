-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Artifact node placement deferred_at (MySQL).
--
-- deferred_at > 0 marks a placement whose node stayed absent from the node
-- view past the grace period: node-side cleanup is postponed, the artifact is
-- finalised anyway, and the row survives as the durable record that the node
-- may still hold the artifact's files.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261112071208_placement_deferred_at', 60);

CALL cubemaster_assert_table_exists('t_cube_artifact_node_placement');

CALL cubemaster_add_column_if_missing(
  't_cube_artifact_node_placement',
  'deferred_at',
  "bigint NOT NULL DEFAULT 0 COMMENT 'unix seconds when node-side cleanup was deferred; 0 = not deferred'"
);

CALL cubemaster_add_index_if_missing(
  't_cube_artifact_node_placement',
  'idx_placement_deferred',
  'ADD INDEX `idx_placement_deferred` (`deferred_at`)'
);

SELECT RELEASE_LOCK('cubemaster_migration_20261112071208_placement_deferred_at');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261112071208_placement_deferred_at', 60);

CALL cubemaster_drop_index_if_exists('t_cube_artifact_node_placement', 'idx_placement_deferred');

CALL cubemaster_drop_column_if_exists('t_cube_artifact_node_placement', 'deferred_at');

SELECT RELEASE_LOCK('cubemaster_migration_20261112071208_placement_deferred_at');
