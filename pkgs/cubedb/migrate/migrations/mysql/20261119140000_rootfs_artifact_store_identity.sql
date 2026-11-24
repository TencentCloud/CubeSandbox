-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Historical rows are deliberately not backfilled: their bucket identity must
-- be established from the original deployment, never from the current default.

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261119140000_artifact_store', 60);
CALL cubemaster_add_column_if_missing('t_cube_rootfs_artifact', 'bucket', "varchar(255) NOT NULL DEFAULT '' COMMENT 'S3 bucket at upload time; empty for fs or historical rows'");
SELECT RELEASE_LOCK('cubemaster_migration_20261119140000_artifact_store');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261119140000_artifact_store', 60);
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'bucket');
SELECT RELEASE_LOCK('cubemaster_migration_20261119140000_artifact_store');
