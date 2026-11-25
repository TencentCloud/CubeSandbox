-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261123172000_volume_delete_retryable', 60);
CALL cubemaster_add_column_if_missing('t_cube_volume', 'delete_retryable', 'BOOLEAN NOT NULL DEFAULT FALSE');
SELECT RELEASE_LOCK('cubemaster_migration_20261123172000_volume_delete_retryable');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261123172000_volume_delete_retryable', 60);
CALL cubemaster_drop_column_if_exists('t_cube_volume', 'delete_retryable');
SELECT RELEASE_LOCK('cubemaster_migration_20261123172000_volume_delete_retryable');
