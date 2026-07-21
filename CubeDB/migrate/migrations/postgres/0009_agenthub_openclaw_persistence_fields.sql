-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub OpenClaw persistence metadata (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0009_agenthub_openclaw_persistence_fields', 60);

-- t_agenthub_instance columns
SELECT cubemaster_add_column_if_missing('t_agenthub_instance', 'rootfs_source_type',   'varchar(32) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_instance', 'rootfs_source_id',     'varchar(128) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_instance', 'openclaw_persist_id',  'varchar(128) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_instance', 'openclaw_state_path',  'varchar(512) DEFAULT NULL');

-- t_agenthub_snapshot columns
SELECT cubemaster_add_column_if_missing('t_agenthub_snapshot', 'snapshot_kind',                  'varchar(32) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_snapshot', 'rootfs_source_type',             'varchar(32) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_snapshot', 'rootfs_source_id',               'varchar(128) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_snapshot', 'rootfs_snapshot_id',             'varchar(128) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_snapshot', 'openclaw_state_snapshot_path',   'varchar(512) DEFAULT NULL');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0009_agenthub_openclaw_persistence_fields'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0009_agenthub_openclaw_persistence_fields', 60);

SELECT cubemaster_drop_column_if_exists('t_agenthub_snapshot', 'openclaw_state_snapshot_path');
SELECT cubemaster_drop_column_if_exists('t_agenthub_snapshot', 'rootfs_snapshot_id');
SELECT cubemaster_drop_column_if_exists('t_agenthub_snapshot', 'rootfs_source_id');
SELECT cubemaster_drop_column_if_exists('t_agenthub_snapshot', 'rootfs_source_type');
SELECT cubemaster_drop_column_if_exists('t_agenthub_snapshot', 'snapshot_kind');

SELECT cubemaster_drop_column_if_exists('t_agenthub_instance', 'openclaw_state_path');
SELECT cubemaster_drop_column_if_exists('t_agenthub_instance', 'openclaw_persist_id');
SELECT cubemaster_drop_column_if_exists('t_agenthub_instance', 'rootfs_source_id');
SELECT cubemaster_drop_column_if_exists('t_agenthub_instance', 'rootfs_source_type');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0009_agenthub_openclaw_persistence_fields'));
