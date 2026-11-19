-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261119090000_volume_admission', 60);
CALL cubemaster_assert_table_exists('t_cube_volume');
CALL cubemaster_add_column_if_missing('t_cube_volume', 'in_flight_creates', 'BIGINT NOT NULL DEFAULT 0');
CALL cubemaster_add_column_if_missing('t_cube_volume', 'deleting', 'BOOLEAN NOT NULL DEFAULT FALSE');
SELECT RELEASE_LOCK('cubemaster_migration_20261119090000_volume_admission');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261119090000_volume_admission', 60);
CALL cubemaster_drop_column_if_exists('t_cube_volume', 'deleting');
CALL cubemaster_drop_column_if_exists('t_cube_volume', 'in_flight_creates');
SELECT RELEASE_LOCK('cubemaster_migration_20261119090000_volume_admission');
