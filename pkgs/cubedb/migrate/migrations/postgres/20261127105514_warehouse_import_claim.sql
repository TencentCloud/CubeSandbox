-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- New workers rotate the token on every claim and check it on updates.
-- Older workers leave the token in place and update jobs without checking it.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261127105514_warehouse_import_claim', 60);

SELECT cubemaster_assert_table_exists('t_component_import_job');
SELECT cubemaster_add_column_if_missing('t_component_import_job', 'claim_token', 'varchar(36)');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261127105514_warehouse_import_claim'));

-- +goose Down
-- Stop all CubeOps replicas that use claim_token before dropping the column.

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261127105514_warehouse_import_claim', 60);

SELECT cubemaster_drop_column_if_exists('t_component_import_job', 'claim_token');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261127105514_warehouse_import_claim'));
