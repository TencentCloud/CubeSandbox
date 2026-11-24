-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Do not assign historical artifacts to the current bucket without proof.

-- +goose Up
ALTER TABLE t_cube_rootfs_artifact ADD COLUMN IF NOT EXISTS bucket varchar(255) NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS bucket;
