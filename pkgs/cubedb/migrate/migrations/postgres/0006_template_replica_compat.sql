-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Template replica compatibility metadata (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0006_template_replica_compat', 60);

SELECT cubemaster_assert_table_exists('t_cube_template_replica');

SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'guest_image_version', 'varchar(128) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'agent_version',       'varchar(128) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'kernel_version',      'varchar(256) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'compat_status',       'varchar(32) NOT NULL DEFAULT ''UNKNOWN''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'compat_policy',       'varchar(32) NOT NULL DEFAULT ''STRICT''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'compat_checked_unix', 'bigint NOT NULL DEFAULT 0');

CREATE INDEX IF NOT EXISTS idx_node_compat ON t_cube_template_replica (node_id, compat_status);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0006_template_replica_compat'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0006_template_replica_compat', 60);

SELECT cubemaster_drop_index_if_exists('t_cube_template_replica', 'idx_node_compat');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'compat_checked_unix');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'compat_policy');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'compat_status');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'kernel_version');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'agent_version');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'guest_image_version');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0006_template_replica_compat'));
