-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- NULL means the sandbox has no confirmed runtime volume mounts.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261121082810_runtime_volume_mounts', 60);
CALL cubemaster_assert_table_exists('t_cube_sandbox_spec');
CALL cubemaster_add_column_if_missing('t_cube_sandbox_spec', 'runtime_volume_mounts', 'text NULL');
CALL cubemaster_add_column_if_missing('t_cube_sandbox_spec', 'pending_runtime_attach', 'varchar(36) NULL');
SELECT RELEASE_LOCK('cubemaster_migration_20261121082810_runtime_volume_mounts');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261121082810_runtime_volume_mounts', 60);
CALL cubemaster_drop_column_if_exists('t_cube_sandbox_spec', 'pending_runtime_attach');
CALL cubemaster_drop_column_if_exists('t_cube_sandbox_spec', 'runtime_volume_mounts');
SELECT RELEASE_LOCK('cubemaster_migration_20261121082810_runtime_volume_mounts');
