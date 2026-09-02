-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Drop the unused retain column from snapshot and template definition
-- tables. It was never written or consumed by control-plane logic.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260902140000_drop_retain', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_snapshot', 'retain');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'retain');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260902140000_drop_retain'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260902140000_drop_retain', 60);

SELECT cubemaster_add_column_if_missing('t_cube_snapshot', 'retain', 'boolean NOT NULL DEFAULT false');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'retain', 'boolean NOT NULL DEFAULT false');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260902140000_drop_retain'));
