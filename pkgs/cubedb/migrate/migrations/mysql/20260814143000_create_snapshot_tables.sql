-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Snapshot + pause-snapshot tables (xfs|s3 backend, remote_status, export
-- uuids) and CoW backend on the running-sandbox spec row. Template
-- definition / replica tables are left unchanged.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260814143000_create_snapshot_tables', 60);

CREATE TABLE IF NOT EXISTS `t_cube_snapshot` (
  `id`                            bigint unsigned NOT NULL AUTO_INCREMENT,
  `created_at`                    datetime(3)     DEFAULT NULL,
  `updated_at`                    datetime(3)     DEFAULT NULL,
  `deleted_at`                    datetime(3)     DEFAULT NULL,
  `snapshot_id`                   varchar(128)    NOT NULL DEFAULT '' COMMENT 'business snapshot id (snap-*)',
  `origin_sandbox_id`             varchar(128)    NOT NULL DEFAULT '' COMMENT 'sandbox this snapshot was taken from',
  `origin_node_id`                varchar(128)    NOT NULL DEFAULT '' COMMENT 'node that produced the snapshot',
  `origin_node_ip`                varchar(64)     NOT NULL DEFAULT '' COMMENT 'origin node ip',
  `display_name`                  varchar(256)    NOT NULL DEFAULT '' COMMENT 'optional display name',
  `instance_type`                 varchar(64)     NOT NULL DEFAULT '',
  `version`                       varchar(32)     NOT NULL DEFAULT '',
  `status`                        varchar(32)     NOT NULL DEFAULT '' COMMENT 'CREATING/READY/FAILED/DELETING',
  `last_error`                    text,
  `retain`                        tinyint(1)      NOT NULL DEFAULT 0,
  `rootfs_size_bytes_at_snapshot` bigint unsigned NOT NULL DEFAULT 0,
  `origin_host_facts_json`        mediumtext,
  `request_json`                  mediumtext      NOT NULL COMMENT 'create-from-snapshot spec',
  `backend`                       varchar(16)     NOT NULL DEFAULT '' COMMENT 'xfs or s3',
  `remote_status`                 varchar(32)     NOT NULL DEFAULT '' COMMENT 'S3 sync status; empty for xfs',
  `export_uuids`                  mediumtext      COMMENT 'JSON cubecow export_uuids: rootfs/memory/metadata',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_cube_snapshot_id` (`snapshot_id`),
  KEY `idx_cube_snapshot_origin_sandbox` (`origin_sandbox_id`),
  KEY `idx_cube_snapshot_origin_node` (`origin_node_id`),
  KEY `idx_cube_snapshot_backend` (`backend`),
  KEY `idx_cube_snapshot_status` (`status`),
  KEY `idx_cube_snapshot_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='User Commit snapshots (not templates)';

CREATE TABLE IF NOT EXISTS `t_cube_pause_snapshot` (
  `id`                 bigint unsigned NOT NULL AUTO_INCREMENT,
  `created_at`         datetime(3)     DEFAULT NULL,
  `updated_at`         datetime(3)     DEFAULT NULL,
  `snapshot_id`        varchar(128)    NOT NULL DEFAULT '' COMMENT 'pause snap id (snap-*)',
  `sandbox_id`         varchar(128)    NOT NULL DEFAULT '' COMMENT 'paused sandbox id; one binding',
  `node_id`            varchar(128)    NOT NULL DEFAULT '',
  `node_ip`            varchar(64)     NOT NULL DEFAULT '',
  `instance_type`      varchar(64)     NOT NULL DEFAULT '',
  `status`             varchar(32)     NOT NULL DEFAULT '' COMMENT 'CREATING/READY/FAILED',
  `last_error`         text,
  `plugin_volume_ids`  text            COMMENT 'JSON array of plugin volume ids detached at pause',
  `backend`            varchar(16)     NOT NULL DEFAULT '' COMMENT 'xfs or s3',
  `remote_status`      varchar(32)     NOT NULL DEFAULT '' COMMENT 'S3 sync status; empty for xfs',
  `origin_host_facts_json` mediumtext COMMENT 'frozen origin cpuid_hash/kernel at pause',
  `export_uuids`       mediumtext      COMMENT 'JSON cubecow export_uuids: rootfs/memory/metadata',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_pause_snapshot_id` (`snapshot_id`),
  UNIQUE KEY `uniq_pause_sandbox_id` (`sandbox_id`),
  KEY `idx_pause_snapshot_status` (`status`),
  KEY `idx_pause_snapshot_backend` (`backend`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Pause binding: sandbox <-> pause-snap <-> node';

CALL cubemaster_assert_table_exists('t_cube_sandbox_spec');

CALL cubemaster_add_column_if_missing(
  't_cube_sandbox_spec',
  'backend',
  "varchar(16) NOT NULL DEFAULT '' COMMENT 'xfs or s3; empty means xfs'"
);

CALL cubemaster_add_index_if_missing(
  't_cube_sandbox_spec',
  'idx_sandbox_spec_backend',
  "ADD INDEX `idx_sandbox_spec_backend` (`backend`)"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260814143000_create_snapshot_tables');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260814143000_create_snapshot_tables', 60);

CALL cubemaster_drop_index_if_exists('t_cube_sandbox_spec', 'idx_sandbox_spec_backend');
CALL cubemaster_drop_column_if_exists('t_cube_sandbox_spec', 'backend');
DROP TABLE IF EXISTS `t_cube_pause_snapshot`;
DROP TABLE IF EXISTS `t_cube_snapshot`;

SELECT RELEASE_LOCK('cubemaster_migration_20260814143000_create_snapshot_tables');
