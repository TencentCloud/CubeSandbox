-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Preserve template-inventory provenance and a server-owned ordering token for
-- node status observations (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260921070000_node_status_report_order', 60);

SELECT cubemaster_add_column_if_missing(
  't_cube_node_status',
  'local_templates_reported',
  'boolean NOT NULL DEFAULT false'
);

SELECT cubemaster_add_column_if_missing(
  't_cube_node_status',
  'heartbeat_order_unix_milli',
  'bigint NOT NULL DEFAULT 0'
);

SELECT cubemaster_add_column_if_missing(
  't_cube_node_status',
  'last_request_id',
  'varchar(64) NOT NULL DEFAULT '''''
);

UPDATE t_cube_node_status
SET heartbeat_order_unix_milli = heartbeat_unix * 1000
WHERE heartbeat_order_unix_milli = 0 AND heartbeat_unix > 0;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260921070000_node_status_report_order'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260921070000_node_status_report_order', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_node_status', 'last_request_id');
SELECT cubemaster_drop_column_if_exists('t_cube_node_status', 'heartbeat_order_unix_milli');
SELECT cubemaster_drop_column_if_exists('t_cube_node_status', 'local_templates_reported');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260921070000_node_status_report_order'));
