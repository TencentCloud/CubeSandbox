-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Preserve template-inventory provenance and a server-owned ordering token for
-- node status observations.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260921070000_node_status_report_order', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_node_status',
  'local_templates_reported',
  "tinyint(1) NOT NULL DEFAULT '0' COMMENT 'whether template inventory has authoritative provenance' AFTER `local_templates_json`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_node_status',
  'heartbeat_order_unix_milli',
  "bigint NOT NULL DEFAULT '0' COMMENT 'server-owned monotonic status ordering token' AFTER `heartbeat_unix`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_node_status',
  'last_request_id',
  "varchar(64) NOT NULL DEFAULT '' COMMENT 'last applied cubelet status request' AFTER `heartbeat_order_unix_milli`"
);

UPDATE `t_cube_node_status`
SET `heartbeat_order_unix_milli` = `heartbeat_unix` * 1000
WHERE `heartbeat_order_unix_milli` = 0 AND `heartbeat_unix` > 0;

SELECT RELEASE_LOCK('cubemaster_migration_20260921070000_node_status_report_order');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260921070000_node_status_report_order', 60);

CALL cubemaster_drop_column_if_exists('t_cube_node_status', 'last_request_id');
CALL cubemaster_drop_column_if_exists('t_cube_node_status', 'heartbeat_order_unix_milli');
CALL cubemaster_drop_column_if_exists('t_cube_node_status', 'local_templates_reported');

SELECT RELEASE_LOCK('cubemaster_migration_20260921070000_node_status_report_order');
