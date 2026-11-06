-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Persist stable artifact ownership separately from short-lived download
-- credentials. Existing rows become generation 1 and can be backfilled with
-- the current TemplateCenter address when a ticket is issued.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261022090000_artifact_stable_locator', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'generation',
  "bigint NOT NULL DEFAULT 1 COMMENT 'Published artifact generation' AFTER `artifact_id`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'storage_ref',
  "varchar(2048) NOT NULL DEFAULT '' COMMENT 'Stable non-secret artifact store locator' AFTER `object_key`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'owner_tc_download_base',
  "varchar(2048) NOT NULL DEFAULT '' COMMENT 'TemplateCenter origin serving fs-backed artifact downloads' AFTER `storage_ref`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20261022090000_artifact_stable_locator');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261022090000_artifact_stable_locator', 60);

CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'owner_tc_download_base');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'storage_ref');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'generation');

SELECT RELEASE_LOCK('cubemaster_migration_20261022090000_artifact_stable_locator');
