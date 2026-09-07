-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Baseline schema for CubeMaster v0.2.2. This file represents the exact
-- table layout shipped with the v0.2.2 release. Two ways it gets applied:
--
--   * Fresh deploy (empty database)
--     -> every CREATE TABLE creates a table.
--
--   * Existing v0.2.2 deploy (upgrade path)
--     -> every CREATE TABLE IF NOT EXISTS becomes a no-op because the
--        tables were created by the v0.2.2 deploy/one-click/sql/001 SQL
--        and by the per-package "CREATE TABLE IF NOT EXISTS" calls that
--        v0.2.2 CubeMaster ran at startup. Goose records version=0 in
--        goose_db_version after this file completes, marking the
--        baseline as applied so the next migration (0001) proceeds.
--
-- This file also defines the reusable stored procedure
-- cubemaster_acquire_migration_lock used by every subsequent migration
-- to assert its per-file inner lock. The outer cluster-wide lock is
-- already held by goose via the SessionLocker passed in from the driver
-- (pkg/base/dao/driver/mysql).
--
-- Why two layers of locking:
--   * outer (goose SessionLocker) protects the whole goose.Up() so that
--     two CubeMaster instances starting up simultaneously serialise.
--   * inner (per-file CALL cubemaster_acquire_migration_lock) protects
--     the case where an operator runs a single .sql file by hand or via
--     goose CLI outside the embedded provider — the file is still safe
--     to run because it asserts its own lock.

-- +goose NO TRANSACTION
-- +goose Up

-- Helper procedure used by every migration (including this one) to
-- acquire its per-file lock or abort with a clean SQLSTATE.
-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_acquire_migration_lock;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_acquire_migration_lock(IN lock_name VARCHAR(255), IN timeout_sec INT)
BEGIN
  DECLARE got INT;
  SELECT GET_LOCK(lock_name, timeout_sec) INTO got;
  IF got IS NULL OR got <> 1 THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'cubemaster_acquire_migration_lock: failed to acquire migration lock';
  END IF;
END;
-- +goose StatementEnd

-- Helper that drops a column only if it exists. MySQL 8.0 has no native
-- ALTER TABLE ... DROP COLUMN IF EXISTS (unlike MariaDB), so we fake it
-- with INFORMATION_SCHEMA + dynamic SQL. Used by future migrations that
-- must be robust to source-schema variations.
-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_drop_column_if_exists;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_drop_column_if_exists(IN tbl VARCHAR(64), IN col VARCHAR(64))
BEGIN
  IF EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = tbl
       AND COLUMN_NAME = col
  ) THEN
    SET @cubemaster_drop_col_sql = CONCAT('ALTER TABLE `', tbl, '` DROP COLUMN `', col, '`');
    PREPARE cubemaster_drop_col_stmt FROM @cubemaster_drop_col_sql;
    EXECUTE cubemaster_drop_col_stmt;
    DEALLOCATE PREPARE cubemaster_drop_col_stmt;
  END IF;
END;
-- +goose StatementEnd

-- Helper that adds a column only if it does not yet exist. Symmetric to
-- cubemaster_drop_column_if_exists; needed by Down migrations.
-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_add_column_if_missing;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_add_column_if_missing(IN tbl VARCHAR(64), IN col VARCHAR(64), IN coldef TEXT)
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = tbl
       AND COLUMN_NAME = col
  ) THEN
    SET @cubemaster_add_col_sql = CONCAT('ALTER TABLE `', tbl, '` ADD COLUMN `', col, '` ', coldef);
    PREPARE cubemaster_add_col_stmt FROM @cubemaster_add_col_sql;
    EXECUTE cubemaster_add_col_stmt;
    DEALLOCATE PREPARE cubemaster_add_col_stmt;
  END IF;
END;
-- +goose StatementEnd

-- Helper that adds an index only if it does not yet exist. idxdef must be a
-- complete ADD clause, e.g. "ADD UNIQUE INDEX `idx_name` (`col`)".
-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_add_index_if_missing;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_add_index_if_missing(IN tbl VARCHAR(64), IN idx VARCHAR(64), IN idxdef TEXT)
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = tbl
       AND INDEX_NAME = idx
  ) THEN
    SET @cubemaster_add_idx_sql = CONCAT('ALTER TABLE `', tbl, '` ', idxdef);
    PREPARE cubemaster_add_idx_stmt FROM @cubemaster_add_idx_sql;
    EXECUTE cubemaster_add_idx_stmt;
    DEALLOCATE PREPARE cubemaster_add_idx_stmt;
  END IF;
END;
-- +goose StatementEnd

-- Helper that drops an index only if it exists. Used by Down migrations and
-- by emergency operator repair flows that may encounter a partial state.
-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_drop_index_if_exists;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_drop_index_if_exists(IN tbl VARCHAR(64), IN idx VARCHAR(64))
BEGIN
  IF EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = tbl
       AND INDEX_NAME = idx
  ) THEN
    SET @cubemaster_drop_idx_sql = CONCAT('ALTER TABLE `', tbl, '` DROP INDEX `', idx, '`');
    PREPARE cubemaster_drop_idx_stmt FROM @cubemaster_drop_idx_sql;
    EXECUTE cubemaster_drop_idx_stmt;
    DEALLOCATE PREPARE cubemaster_drop_idx_stmt;
  END IF;
END;
-- +goose StatementEnd

-- Preflight helpers fail early with a clear migration error when an operator
-- points CubeMaster at a database that is neither v0.2.2 nor a supported
-- partially-upgraded schema.
-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_assert_table_exists;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_assert_table_exists(IN tbl VARCHAR(64))
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.TABLES
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = tbl
  ) THEN
    SET @cubemaster_assert_table_msg = CONCAT('cubemaster schema preflight failed: required table missing: ', tbl);
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = @cubemaster_assert_table_msg;
  END IF;
END;
-- +goose StatementEnd

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_assert_column_exists;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE PROCEDURE cubemaster_assert_column_exists(IN tbl VARCHAR(64), IN col VARCHAR(64))
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = tbl
       AND COLUMN_NAME = col
  ) THEN
    SET @cubemaster_assert_col_msg = CONCAT('cubemaster schema preflight failed: required column missing: ', tbl, '.', col);
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = @cubemaster_assert_col_msg;
  END IF;
END;
-- +goose StatementEnd

-- Acquire this file's inner lock now that the procedure exists.
CALL cubemaster_acquire_migration_lock('cubemaster_migration_0001_baseline_v0_2_2', 60);

-- ---------------------------------------------------------------------
-- Host registry tables (originally in deploy/one-click/sql/001_schema_host_tables.sql)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `t_cube_host_info` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `ins_id` varchar(64) NOT NULL DEFAULT '',
  `ip` varchar(64) NOT NULL DEFAULT '',
  `region` varchar(64) NOT NULL DEFAULT 'local',
  `zone` varchar(64) NOT NULL DEFAULT 'local-a',
  `uuid` varchar(64) NOT NULL DEFAULT '',
  `instance_type` varchar(64) NOT NULL DEFAULT 'cubebox',
  `cube_cluster_label` varchar(64) NOT NULL DEFAULT 'default',
  `oss_cluster_label` varchar(64) NOT NULL DEFAULT '',
  `host_status` varchar(32) NOT NULL DEFAULT 'RUNNING',
  `live_status` varchar(32) NOT NULL DEFAULT 'LIVE',
  `quota_cpu` bigint NOT NULL DEFAULT 0,
  `quota_mem_mb` bigint NOT NULL DEFAULT 0,
  `cpu_total` bigint NOT NULL DEFAULT 0,
  `mem_mb_total` bigint NOT NULL DEFAULT 0,
  `data_disk_gb` bigint NOT NULL DEFAULT 0,
  `sys_disk_gb` bigint NOT NULL DEFAULT 0,
  `create_concurrent_num` bigint NOT NULL DEFAULT 0,
  `max_mvm_num` bigint NOT NULL DEFAULT 0,
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_host_info_ins_id` (`ins_id`),
  KEY `idx_host_info_ip` (`ip`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_cube_host_type` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `instance_type` varchar(128) NOT NULL DEFAULT '',
  `cpu_type` varchar(64) NOT NULL DEFAULT 'INTEL',
  `gpu_info` varchar(1024) NOT NULL DEFAULT '',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_host_type_instance_type` (`instance_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `t_cube_sub_host_info` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `ins_id` varchar(64) NOT NULL DEFAULT '',
  `host_ip` varchar(64) NOT NULL DEFAULT '',
  `device_class` varchar(64) NOT NULL DEFAULT '',
  `device_id` bigint NOT NULL DEFAULT 0,
  `instance_family` varchar(64) NOT NULL DEFAULT '',
  `dedicated_cluster_id` varchar(64) NOT NULL DEFAULT '',
  `virtual_node_quota` varchar(255) NOT NULL DEFAULT '[]',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_sub_host_info_ins_id` (`ins_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------
-- instancecache tables (originally CREATE TABLE in pkg/instancecache/export.go)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `t_cube_instance_info` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `ins_id` varchar(64) NOT NULL COMMENT 'ins_id',
  `uuid` varchar(64) DEFAULT NULL COMMENT 'uuid',
  `host_ip` varchar(16) DEFAULT NULL COMMENT 'host_ip',
  `host_id` varchar(16) DEFAULT NULL COMMENT 'host_id',
  `ins_state` varchar(16) DEFAULT NULL COMMENT 'ins_state',
  `cpu` int DEFAULT '0' COMMENT 'cpu',
  `mem` int DEFAULT '0' COMMENT 'mem',
  `cpu_type` varchar(16) DEFAULT 'INTEL',
  `zone` varchar(32) DEFAULT NULL COMMENT 'zone',
  `region` varchar(64) DEFAULT NULL COMMENT 'region',
  `image_id` varchar(128) DEFAULT NULL COMMENT 'image_id',
  `system_disk` varchar(64) DEFAULT NULL COMMENT 'system_disk',
  `private_ip_addresses` varchar(128) NOT NULL DEFAULT '' COMMENT 'private_ip_addresses',
  `private_ip_cnt` tinyint DEFAULT '0',
  `private_ip` varchar(16) DEFAULT NULL COMMENT 'private_ip',
  `mac_address` varchar(16) DEFAULT NULL COMMENT 'mac_address',
  `data_disks` varchar(1800) DEFAULT NULL COMMENT 'data_disks',
  `security_ids` varchar(64) DEFAULT NULL COMMENT 'security_ids',
  `vpc_id` varchar(64) NOT NULL DEFAULT '' COMMENT 'vpc_id',
  `subnet_id` varchar(64) NOT NULL DEFAULT '' COMMENT 'subnet_id',
  `disk_state` varchar(64) DEFAULT NULL COMMENT 'disk_state',
  `fail_msg` text COMMENT 'fail_msg',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'created_at',
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'updated_at',
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ins_id` (`ins_id`),
  KEY `vpc-id` (`vpc_id`),
  KEY `private_ip_addresses` (`private_ip_addresses`),
  KEY `idx_private_ip` (`private_ip`),
  KEY `idx_uuid` (`uuid`),
  KEY `idx_private_ip_cnt` (`private_ip_cnt`),
  KEY `idx_describe` (`private_ip`,`private_ip_cnt`),
  KEY `idx_ins_state` (`ins_state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE IF NOT EXISTS `t_cube_instance_userdata` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `ins_id` varchar(64) NOT NULL COMMENT 'ins_id',
  `user_data` text COMMENT 'user_data',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'created_at',
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'updated_at',
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ins_id` (`ins_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- ---------------------------------------------------------------------
-- nodemeta tables (originally CREATE TABLE in pkg/nodemeta/service.go)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `t_cube_node_registration` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(128) NOT NULL,
  `host_ip` varchar(64) NOT NULL DEFAULT '',
  `grpc_port` int NOT NULL DEFAULT '0',
  `labels_json` longtext,
  `capacity_json` text,
  `allocatable_json` text,
  `instance_type` varchar(64) NOT NULL DEFAULT '',
  `cluster_label` varchar(128) NOT NULL DEFAULT '',
  `quota_cpu` bigint NOT NULL DEFAULT '0',
  `quota_mem_mb` bigint NOT NULL DEFAULT '0',
  `create_concurrent_num` bigint NOT NULL DEFAULT '0',
  `max_mvm_num` bigint NOT NULL DEFAULT '0',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_node_id` (`node_id`),
  KEY `idx_host_ip` (`host_ip`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE IF NOT EXISTS `t_cube_node_status` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` varchar(128) NOT NULL,
  `conditions_json` longtext,
  `images_json` longtext,
  `local_templates_json` longtext,
  `heartbeat_unix` bigint NOT NULL DEFAULT '0',
  `healthy` tinyint(1) NOT NULL DEFAULT '0',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_node_id` (`node_id`),
  KEY `idx_heartbeat` (`heartbeat_unix`),
  KEY `idx_healthy` (`healthy`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- ---------------------------------------------------------------------
-- templatecenter tables (v0.2.2 schema; HEAD additions are in 0001)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `t_cube_template_definition` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `template_id` varchar(128) NOT NULL COMMENT 'template id',
  `instance_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'instance type',
  `version` varchar(32) NOT NULL DEFAULT '' COMMENT 'template version',
  `status` varchar(32) NOT NULL DEFAULT '' COMMENT 'template status',
  `request_json` mediumtext NOT NULL COMMENT 'normalized template request json',
  `last_error` text COMMENT 'last error message',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_template_id` (`template_id`),
  KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- v0.2.2 template_replica still carried the physical-binding columns;
-- 0001 drops them as part of the v5 "thin replica" refactor.
CREATE TABLE IF NOT EXISTS `t_cube_template_replica` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `template_id` varchar(128) NOT NULL COMMENT 'template id',
  `node_id` varchar(128) NOT NULL COMMENT 'node id',
  `node_ip` varchar(64) NOT NULL DEFAULT '' COMMENT 'node ip',
  `instance_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'instance type',
  `spec` varchar(128) NOT NULL DEFAULT '' COMMENT 'resource spec',
  `snapshot_path` varchar(1024) NOT NULL DEFAULT '' COMMENT 'snapshot path (deprecated in 0001)',
  `status` varchar(32) NOT NULL DEFAULT '' COMMENT 'replica status',
  `phase` varchar(32) NOT NULL DEFAULT '' COMMENT 'replica phase',
  `artifact_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'replica artifact id',
  `last_job_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'last redo/create job id',
  `last_error_phase` varchar(64) NOT NULL DEFAULT '' COMMENT 'phase where last error happened',
  `cleanup_required` tinyint(1) NOT NULL DEFAULT 0 COMMENT 'needs cleanup before redo',
  `error_message` text COMMENT 'error message',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_template_node` (`template_id`,`node_id`),
  KEY `idx_template_status` (`template_id`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

CREATE TABLE IF NOT EXISTS `t_cube_rootfs_artifact` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `artifact_id` varchar(128) NOT NULL COMMENT 'artifact id',
  `template_spec_fingerprint` varchar(128) NOT NULL DEFAULT '' COMMENT 'immutable template fingerprint',
  `source_image_ref` varchar(1024) NOT NULL DEFAULT '' COMMENT 'source image ref',
  `source_image_digest` varchar(256) NOT NULL DEFAULT '' COMMENT 'source image digest',
  `master_node_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'master node id',
  `master_node_ip` varchar(256) NOT NULL DEFAULT '' COMMENT 'master node ip or host',
  `ext4_path` varchar(2048) NOT NULL DEFAULT '' COMMENT 'artifact ext4 path',
  `ext4_sha256` varchar(128) NOT NULL DEFAULT '' COMMENT 'artifact sha256',
  `ext4_size_bytes` bigint NOT NULL DEFAULT 0 COMMENT 'artifact size',
  `image_config_json` mediumtext COMMENT 'docker image config json',
  `generated_request_json` mediumtext COMMENT 'generated create request json',
  `writable_layer_size` varchar(64) NOT NULL DEFAULT '' COMMENT 'writable layer size',
  `download_token` varchar(256) NOT NULL DEFAULT '' COMMENT 'download token',
  `status` varchar(32) NOT NULL DEFAULT '' COMMENT 'artifact status',
  `last_error` text COMMENT 'last error',
  `gc_deadline` bigint NOT NULL DEFAULT 0 COMMENT 'gc deadline unix seconds',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_artifact_id` (`artifact_id`),
  UNIQUE KEY `idx_artifact_fingerprint` (`template_spec_fingerprint`),
  KEY `idx_artifact_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- v0.2.2 template_image_job schema (includes the columns and indexes
-- v0.2.2 itself ALTER-added: attempt_no, request_id, operation, ...,
-- idx_template_image_template_attempt, idx_template_image_template_status,
-- idx_template_image_request_id). HEAD's 3 new columns/indexes are in 0001.
CREATE TABLE IF NOT EXISTS `t_cube_template_image_job` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `job_id` varchar(128) NOT NULL COMMENT 'job id',
  `template_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'template id',
  `request_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'idempotent request id',
  `attempt_no` int NOT NULL DEFAULT 1 COMMENT 'attempt number',
  `retry_of_job_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'previous job id',
  `operation` varchar(32) NOT NULL DEFAULT '' COMMENT 'job operation',
  `redo_mode` varchar(32) NOT NULL DEFAULT '' COMMENT 'redo mode',
  `redo_scope_json` mediumtext COMMENT 'redo scope json',
  `resume_phase` varchar(64) NOT NULL DEFAULT '' COMMENT 'resume phase',
  `node_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'target node id for cleanup',
  `node_ip` varchar(256) NOT NULL DEFAULT '' COMMENT 'target node ip for cleanup',
  `snapshot_path` varchar(1024) NOT NULL DEFAULT '' COMMENT 'template snapshot path for cleanup',
  `artifact_id` varchar(128) NOT NULL DEFAULT '' COMMENT 'artifact id',
  `template_spec_fingerprint` varchar(128) NOT NULL DEFAULT '' COMMENT 'immutable template fingerprint',
  `source_image_ref` varchar(1024) NOT NULL DEFAULT '' COMMENT 'source image ref',
  `source_image_digest` varchar(256) NOT NULL DEFAULT '' COMMENT 'source image digest',
  `writable_layer_size` varchar(64) NOT NULL DEFAULT '' COMMENT 'writable layer size',
  `instance_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'instance type',
  `network_type` varchar(64) NOT NULL DEFAULT '' COMMENT 'network type',
  `status` varchar(32) NOT NULL DEFAULT '' COMMENT 'job status',
  `phase` varchar(64) NOT NULL DEFAULT '' COMMENT 'current phase',
  `progress` int NOT NULL DEFAULT 0 COMMENT 'progress percentage',
  `error_message` text COMMENT 'error message',
  `expected_node_count` int NOT NULL DEFAULT 0 COMMENT 'expected node count',
  `ready_node_count` int NOT NULL DEFAULT 0 COMMENT 'ready node count',
  `failed_node_count` int NOT NULL DEFAULT 0 COMMENT 'failed node count',
  `template_status` varchar(32) NOT NULL DEFAULT '' COMMENT 'template status',
  `artifact_status` varchar(32) NOT NULL DEFAULT '' COMMENT 'artifact status',
  `request_json` mediumtext COMMENT 'sanitized request json',
  `result_json` mediumtext COMMENT 'result json',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `deleted_at` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_template_image_job_id` (`job_id`),
  UNIQUE KEY `idx_template_image_template_attempt` (`template_id`,`attempt_no`),
  KEY `idx_template_image_request_id` (`request_id`),
  KEY `idx_template_image_status` (`status`),
  KEY `idx_template_image_template_status` (`template_id`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3;

-- Release the inner per-file lock. The outer goose SessionLocker remains
-- held until goose finishes the whole Up() run.
SELECT RELEASE_LOCK('cubemaster_migration_0001_baseline_v0_2_2');

-- +goose Down
-- The baseline is irreversible: it represents the v0.2.2 ground truth.
-- Down-migrating past it would drop every CubeMaster table including any
-- v0.2.2 data the operator wanted to preserve. Refuse explicitly so an
-- operator who accidentally runs `goose down` past version 0 gets a
-- clear, actionable error instead of silent data loss.
-- +goose StatementBegin
SIGNAL SQLSTATE '45000'
  SET MESSAGE_TEXT = 'cubemaster baseline migration (0001) is not reversible; restore from backup';
-- +goose StatementEnd
