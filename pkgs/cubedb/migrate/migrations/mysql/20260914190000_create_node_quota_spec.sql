-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Desired node quota (ops-agent push source of truth). The scheduler does NOT
-- read this table: it consumes the effective quota reported by cubelet on
-- register/heartbeat (t_cube_node_registration). Revision is a monotonic
-- counter bumped on every write; ops-agent uses it to skip no-op rewrites
-- during pull reconcile.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260914190000_create_node_quota_spec', 60);

CREATE TABLE IF NOT EXISTS `t_cube_node_quota_spec` (
  `id`                            bigint unsigned NOT NULL AUTO_INCREMENT,
  `created_at`                    datetime(3)     DEFAULT NULL,
  `updated_at`                    datetime(3)     DEFAULT NULL,
  `deleted_at`                    datetime(3)     DEFAULT NULL,
  `node_id`                       varchar(128)    NOT NULL DEFAULT '' COMMENT 'node identity (registration node_id)',
  `mcpu_limit`                    bigint          NOT NULL DEFAULT 0 COMMENT 'desired cpu quota in milli-cores; 0 = cubelet default overcommit',
  `mem_limit`                     varchar(64)     NOT NULL DEFAULT '' COMMENT 'desired mem quota quantity (e.g. 256Gi); empty = default',
  `mvm_limit`                     bigint          NOT NULL DEFAULT 0 COMMENT 'desired max mvm num; 0 = default',
  `creation_concurrent_num`       bigint          NOT NULL DEFAULT 0,
  `paused_resource_release_ratio` double          NOT NULL DEFAULT 0 COMMENT 'fraction of paused sandbox quota released to scheduler, [0,1]',
  `max_cpu_overcommit`            double          NOT NULL DEFAULT 8 COMMENT 'push-side guard: mcpu_limit may not exceed cpuTotal*1000*this',
  `max_mem_overcommit`            double          NOT NULL DEFAULT 2 COMMENT 'push-side guard: mem_limit may not exceed memTotal*this',
  `revision`                      bigint          NOT NULL DEFAULT 0 COMMENT 'monotonic; bumped on every write, pull reconcile skip marker',
  `updated_by`                    varchar(128)    NOT NULL DEFAULT '' COMMENT 'operator that last wrote the spec',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_quota_spec_node` (`node_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='desired node quota; effective quota lives in t_cube_node_registration';

SELECT RELEASE_LOCK('cubemaster_migration_20260914190000_create_node_quota_spec');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260914190000_create_node_quota_spec', 60);

DROP TABLE IF EXISTS `t_cube_node_quota_spec`;

SELECT RELEASE_LOCK('cubemaster_migration_20260914190000_create_node_quota_spec');
