-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261119090000_volume_admission', 60);
SELECT cubemaster_assert_table_exists('t_cube_volume');
SELECT cubemaster_add_column_if_missing('t_cube_volume', 'in_flight_creates', 'BIGINT NOT NULL DEFAULT 0');
SELECT cubemaster_add_column_if_missing('t_cube_volume', 'deleting', 'BOOLEAN NOT NULL DEFAULT FALSE');
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261119090000_volume_admission'));

-- +goose Down
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261119090000_volume_admission', 60);
SELECT cubemaster_drop_column_if_exists('t_cube_volume', 'deleting');
SELECT cubemaster_drop_column_if_exists('t_cube_volume', 'in_flight_creates');
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261119090000_volume_admission'));
