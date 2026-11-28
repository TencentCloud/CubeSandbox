-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- New workers rotate the token on every claim and check it on updates.
-- Older workers leave the token in place and update jobs without checking it.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261127105514_warehouse_import_claim', 60);

CALL cubemaster_assert_table_exists('t_component_import_job');
CALL cubemaster_add_column_if_missing('t_component_import_job', 'claim_token', 'varchar(36)');

SELECT RELEASE_LOCK('cubemaster_migration_20261127105514_warehouse_import_claim');

-- +goose Down
-- Stop all CubeOps replicas that use claim_token before dropping the column.

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261127105514_warehouse_import_claim', 60);

CALL cubemaster_drop_column_if_exists('t_component_import_job', 'claim_token');

SELECT RELEASE_LOCK('cubemaster_migration_20261127105514_warehouse_import_claim');
