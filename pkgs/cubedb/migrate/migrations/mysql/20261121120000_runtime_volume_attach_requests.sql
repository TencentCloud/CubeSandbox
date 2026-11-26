-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261121120000_runtime_attach', 60);

CREATE TABLE IF NOT EXISTS `t_cube_runtime_volume_attach` (
  `request_key` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `sandbox_id` varchar(128) NOT NULL,
  `request_id` varchar(256) NOT NULL,
  `execution_id` varchar(256) NOT NULL,
  `volume_id` varchar(128) NOT NULL,
  `container_path` varchar(4096) NOT NULL,
  `readonly` boolean NOT NULL,
  `spec_row_id` bigint unsigned NOT NULL,
  `volume_record_id` bigint unsigned NOT NULL,
  `claim_token` varchar(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `outcome` varchar(16) NOT NULL,
  PRIMARY KEY (`request_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20261121120000_runtime_attach');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261121120000_runtime_attach', 60);

DROP TABLE IF EXISTS `t_cube_runtime_volume_attach`;

SELECT RELEASE_LOCK('cubemaster_migration_20261121120000_runtime_attach');
