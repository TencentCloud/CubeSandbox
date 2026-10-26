-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add artifact_url column to t_cube_rootfs_artifact for S3/MinIO presigned
-- download URLs. When set, distribution uses this URL directly instead of
-- building a local HTTP URL from master_node_ip.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260904120000_artifact_s3_url', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'artifact_url',
  "varchar(2048) NOT NULL DEFAULT '' COMMENT 'S3/MinIO presigned download URL; empty means local HTTP download' AFTER `download_token`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260904120000_artifact_s3_url');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260904120000_artifact_s3_url', 60);

CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'artifact_url');

SELECT RELEASE_LOCK('cubemaster_migration_20260904120000_artifact_s3_url');
