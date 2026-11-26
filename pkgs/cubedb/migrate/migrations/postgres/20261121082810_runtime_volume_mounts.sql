-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- NULL means the sandbox has no confirmed runtime volume mounts.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261121082810_runtime_volume_mounts', 60);
SELECT cubemaster_assert_table_exists('t_cube_sandbox_spec');
SELECT cubemaster_add_column_if_missing('t_cube_sandbox_spec', 'runtime_volume_mounts', 'text NULL');
SELECT cubemaster_add_column_if_missing('t_cube_sandbox_spec', 'pending_runtime_attach', 'varchar(36) NULL');
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261121082810_runtime_volume_mounts'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261121082810_runtime_volume_mounts', 60);
SELECT cubemaster_drop_column_if_exists('t_cube_sandbox_spec', 'pending_runtime_attach');
SELECT cubemaster_drop_column_if_exists('t_cube_sandbox_spec', 'runtime_volume_mounts');
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261121082810_runtime_volume_mounts'));
