-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Persist stable artifact ownership separately from short-lived download
-- credentials.

-- +goose Up

ALTER TABLE t_cube_rootfs_artifact
  ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 1;

ALTER TABLE t_cube_rootfs_artifact
  ADD COLUMN IF NOT EXISTS storage_ref varchar(2048) NOT NULL DEFAULT '';

ALTER TABLE t_cube_rootfs_artifact
  ADD COLUMN IF NOT EXISTS owner_tc_download_base varchar(2048) NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS owner_tc_download_base;
ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS storage_ref;
ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS generation;
