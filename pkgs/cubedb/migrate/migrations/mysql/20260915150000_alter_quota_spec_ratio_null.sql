-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- paused_resource_release_ratio becomes nullable: NULL = inherit the
-- cluster-level default (t_system_setting), a value (incl. 0) = explicit
-- per-node override. Existing rows keep their explicit values.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null', 60);

ALTER TABLE `t_cube_node_quota_spec`
  MODIFY COLUMN `paused_resource_release_ratio` double NULL DEFAULT NULL COMMENT 'fraction of paused sandbox quota released to scheduler, [0,1]; NULL = inherit cluster default';

SELECT RELEASE_LOCK('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null', 60);

UPDATE `t_cube_node_quota_spec` SET `paused_resource_release_ratio` = 0 WHERE `paused_resource_release_ratio` IS NULL;
ALTER TABLE `t_cube_node_quota_spec`
  MODIFY COLUMN `paused_resource_release_ratio` double NOT NULL DEFAULT 0 COMMENT 'fraction of paused sandbox quota released to scheduler, [0,1]';

SELECT RELEASE_LOCK('cubemaster_migration_20260915150000_alter_quota_spec_ratio_null');
