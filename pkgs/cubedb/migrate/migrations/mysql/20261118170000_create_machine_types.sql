-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Operator-managed machine type catalog. Membership stays on the node label
-- cube.cubesandbox.io/machine-type so CubeMaster restore placement keeps
-- reading the existing node view. Rows are deleted physically so a name can
-- be recreated after removal.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261118170000_machine_types', 60);

CREATE TABLE IF NOT EXISTS `t_cube_machine_type` (
  `id`           bigint unsigned NOT NULL AUTO_INCREMENT,
  `name`         varchar(64)     NOT NULL DEFAULT '' COMMENT 'stable id; ^[a-z0-9][a-z0-9-]{0,62}$',
  `display_name` varchar(128)    NOT NULL DEFAULT '' COMMENT 'operator-facing name',
  `description`  varchar(512)    NOT NULL DEFAULT '' COMMENT 'optional note',
  `created_by`   varchar(128)    NOT NULL DEFAULT '',
  `updated_by`   varchar(128)    NOT NULL DEFAULT '',
  `created_at`   datetime(3)     DEFAULT NULL,
  `updated_at`   datetime(3)     DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_machine_type_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='machine type catalog; node membership is the machine-type label';

SELECT RELEASE_LOCK('cubemaster_migration_20261118170000_machine_types');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261118170000_machine_types', 60);

DROP TABLE IF EXISTS `t_cube_machine_type`;

SELECT RELEASE_LOCK('cubemaster_migration_20261118170000_machine_types');
