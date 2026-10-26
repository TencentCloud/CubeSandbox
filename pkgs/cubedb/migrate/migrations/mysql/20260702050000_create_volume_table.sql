-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Create the volume management table used by the Volume HTTP API.
-- See pkg/base/db/models/volume.go for the corresponding GORM model.
-- Rows are hard-deleted on DELETE /volumes (no soft-delete column).

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260702050000_create_volume_table', 60);

CREATE TABLE IF NOT EXISTS `t_cube_volume` (
  `id`          bigint unsigned  NOT NULL AUTO_INCREMENT COMMENT 'internal PK',
  `created_at`  datetime(3)      DEFAULT NULL,
  `updated_at`  datetime(3)      DEFAULT NULL,
  `volume_id`   varchar(128)     NOT NULL DEFAULT '' COMMENT 'stable business key (same as name when caller supplies name)',
  `name`        varchar(128)     NOT NULL DEFAULT '' COMMENT 'customer-specified or auto-generated label',
  `driver`      varchar(128)     NOT NULL DEFAULT '' COMMENT 'plugin name',
  `token`       varchar(1024)    NOT NULL DEFAULT '' COMMENT 'per-volume auth credential',
  `refcount`    bigint           NOT NULL DEFAULT 0 COMMENT 'number of nodes currently referencing the volume',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_volume_id` (`volume_id`),
  UNIQUE KEY `uniq_volume_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='CubeSandbox managed volumes';

SELECT RELEASE_LOCK('cubemaster_migration_20260702050000_create_volume_table');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260702050000_create_volume_table', 60);

DROP TABLE IF EXISTS `t_cube_volume`;

SELECT RELEASE_LOCK('cubemaster_migration_20260702050000_create_volume_table');
