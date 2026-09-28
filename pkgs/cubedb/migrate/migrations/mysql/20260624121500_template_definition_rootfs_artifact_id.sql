-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add an indexed rootfs artifact reference on template definitions so cleanup
-- can count live owners without scanning request_json under artifact row locks.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260624121500_tpl_def_rootfs_idx', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'rootfs_artifact_id',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'rootfs artifact id' AFTER `rootfs_size_bytes_at_snapshot`"
);

UPDATE `t_cube_template_definition` AS d
JOIN `t_cube_rootfs_artifact` AS a
  ON d.`rootfs_artifact_id` = ''
 AND a.`artifact_id` <> ''
 AND INSTR(d.`request_json`, a.`artifact_id`) > 0
SET d.`rootfs_artifact_id` = a.`artifact_id`;

CALL cubemaster_add_index_if_missing(
  't_cube_template_definition',
  'idx_template_definition_rootfs_artifact',
  'ADD INDEX `idx_template_definition_rootfs_artifact` (`rootfs_artifact_id`, `deleted_at`, `template_id`)'
);

SELECT RELEASE_LOCK('cubemaster_migration_20260624121500_tpl_def_rootfs_idx');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260624121500_tpl_def_rootfs_idx', 60);

CALL cubemaster_drop_index_if_exists(
  't_cube_template_definition',
  'idx_template_definition_rootfs_artifact'
);

CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'rootfs_artifact_id');

SELECT RELEASE_LOCK('cubemaster_migration_20260624121500_tpl_def_rootfs_idx');
