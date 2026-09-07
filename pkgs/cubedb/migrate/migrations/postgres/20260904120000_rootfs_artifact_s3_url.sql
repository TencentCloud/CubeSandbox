-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add artifact_url column to t_cube_rootfs_artifact for S3/MinIO presigned
-- download URLs. When set, distribution uses this URL directly instead of
-- building a local HTTP URL from master_node_ip.

-- +goose Up

ALTER TABLE t_cube_rootfs_artifact ADD COLUMN IF NOT EXISTS artifact_url varchar(2048) NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS artifact_url;
