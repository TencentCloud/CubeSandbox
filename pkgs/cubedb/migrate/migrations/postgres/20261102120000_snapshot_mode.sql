-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Snapshot artifacts carry an explicit mode. Historical rows are full.

-- +goose Up
ALTER TABLE t_cube_snapshot
  ADD COLUMN IF NOT EXISTS mode varchar(16) NOT NULL DEFAULT 'full';

-- +goose Down
ALTER TABLE t_cube_snapshot DROP COLUMN IF EXISTS mode;
