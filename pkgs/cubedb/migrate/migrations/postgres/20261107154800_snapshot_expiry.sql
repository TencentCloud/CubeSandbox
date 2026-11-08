-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Optional expiry timestamps let the snapshot reconciler retire user snapshots
-- without affecting historical snapshots that have no expiration.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261107154800_snapshot_expiry', 60);

ALTER TABLE t_cube_snapshot
  ADD COLUMN IF NOT EXISTS expires_at timestamp(3) with time zone NULL;
CREATE INDEX IF NOT EXISTS idx_cube_snapshot_expires_at
  ON t_cube_snapshot (expires_at);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261107154800_snapshot_expiry'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261107154800_snapshot_expiry', 60);

DROP INDEX IF EXISTS idx_cube_snapshot_expires_at;
ALTER TABLE t_cube_snapshot DROP COLUMN IF EXISTS expires_at;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261107154800_snapshot_expiry'));
