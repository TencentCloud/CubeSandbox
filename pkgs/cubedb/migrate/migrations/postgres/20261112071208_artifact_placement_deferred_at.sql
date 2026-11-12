-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Artifact node placement deferred_at (PostgreSQL). See the MySQL twin.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261112071208_placement_deferred_at', 60);

SELECT cubemaster_assert_table_exists('t_cube_artifact_node_placement');

SELECT cubemaster_add_column_if_missing('t_cube_artifact_node_placement', 'deferred_at', 'bigint NOT NULL DEFAULT 0');

SELECT cubemaster_add_index_if_missing(
  't_cube_artifact_node_placement',
  'idx_placement_deferred',
  'CREATE INDEX idx_placement_deferred ON t_cube_artifact_node_placement (deferred_at)'
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261112071208_placement_deferred_at'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261112071208_placement_deferred_at', 60);

DROP INDEX IF EXISTS idx_placement_deferred;

SELECT cubemaster_drop_column_if_exists('t_cube_artifact_node_placement', 'deferred_at');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261112071208_placement_deferred_at'));
