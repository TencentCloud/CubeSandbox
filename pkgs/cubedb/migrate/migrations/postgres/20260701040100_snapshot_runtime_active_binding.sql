-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Split snapshot runtime binding current state from historical runtime refs.
-- The active table is keyed by (sandbox_id, binding_type), so concurrent
-- writers serialize on the DB row instead of emulating current state with
-- UPDATE(status=RELEASED)+INSERT on the historical ref table.
--
-- PostgreSQL counterpart of mysql/20260701040100_snapshot_runtime_active_binding.sql.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260701040100_rt_active', 60);

SELECT cubemaster_assert_table_exists('t_cube_snapshot_runtime_ref');

-- Normalize empty binding_type on ACTIVE rows before unique projection.
UPDATE t_cube_snapshot_runtime_ref
   SET binding_type = CASE
         WHEN TRIM(binding_type) = '' THEN 'memory_backing'
         ELSE TRIM(binding_type)
       END,
       updated_at = CURRENT_TIMESTAMP
 WHERE status = 'ACTIVE';

-- Deduplicate ACTIVE rows sharing (sandbox_id, binding_type): keep MAX(id),
-- mark the rest RELEASED. Required before INSERT ... ON CONFLICT — Postgres
-- rejects a single statement that would update the same conflict target twice.
UPDATE t_cube_snapshot_runtime_ref AS old
   SET status = 'RELEASED',
       released_at = COALESCE(old.released_at, CURRENT_TIMESTAMP),
       updated_at = CURRENT_TIMESTAMP,
       last_error = 'deduplicated during snapshot runtime active migration'
  FROM (
    SELECT sandbox_id, binding_type, MAX(id) AS keep_id
      FROM t_cube_snapshot_runtime_ref
     WHERE status = 'ACTIVE'
     GROUP BY sandbox_id, binding_type
    HAVING COUNT(*) > 1
  ) AS dup
 WHERE old.sandbox_id = dup.sandbox_id
   AND old.binding_type = dup.binding_type
   AND old.id <> dup.keep_id
   AND old.status = 'ACTIVE';

CREATE TABLE IF NOT EXISTS t_cube_snapshot_runtime_active (
  sandbox_id varchar(128) NOT NULL,
  binding_type varchar(64) NOT NULL DEFAULT 'memory_backing',
  snapshot_id varchar(128) NOT NULL,
  node_id varchar(128) NOT NULL DEFAULT '',
  node_ip varchar(64) NOT NULL DEFAULT '',
  memory_vol varchar(256) NOT NULL DEFAULT '',
  rootfs_vol varchar(256) NOT NULL DEFAULT '',
  sandbox_gen integer NOT NULL DEFAULT 0,
  attached_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_seen_at timestamp DEFAULT NULL,
  last_error text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (sandbox_id, binding_type)
);
CREATE INDEX IF NOT EXISTS idx_snapshot_runtime_active_snapshot ON t_cube_snapshot_runtime_active (snapshot_id);
CREATE INDEX IF NOT EXISTS idx_snapshot_runtime_active_node ON t_cube_snapshot_runtime_active (node_id);
CREATE INDEX IF NOT EXISTS idx_snapshot_runtime_active_node_ip ON t_cube_snapshot_runtime_active (node_ip);

SELECT cubemaster_assert_column_exists('t_cube_snapshot_runtime_active', 'sandbox_id');
SELECT cubemaster_assert_column_exists('t_cube_snapshot_runtime_active', 'binding_type');
SELECT cubemaster_assert_column_exists('t_cube_snapshot_runtime_active', 'snapshot_id');

-- Index/PK structure is enforced by CREATE TABLE / CREATE INDEX above and by
-- the Go-side assertPGHeadSchema + TestSchemaAlignment_MySQL_vs_Postgres.
-- Avoid DO $$ ... $$ here: goose NO TRANSACTION splits on ';' and breaks
-- dollar-quoted PL/pgSQL blocks.

INSERT INTO t_cube_snapshot_runtime_active (
    sandbox_id,
    binding_type,
    snapshot_id,
    node_id,
    node_ip,
    memory_vol,
    rootfs_vol,
    sandbox_gen,
    attached_at,
    last_seen_at,
    last_error,
    created_at,
    updated_at
)
SELECT
    sandbox_id,
    binding_type,
    snapshot_id,
    node_id,
    node_ip,
    memory_vol,
    rootfs_vol,
    sandbox_gen,
    attached_at,
    last_seen_at,
    last_error,
    created_at,
    updated_at
  FROM t_cube_snapshot_runtime_ref
 WHERE status = 'ACTIVE'
ON CONFLICT (sandbox_id, binding_type) DO UPDATE SET
    snapshot_id = EXCLUDED.snapshot_id,
    node_id = EXCLUDED.node_id,
    node_ip = EXCLUDED.node_ip,
    memory_vol = EXCLUDED.memory_vol,
    rootfs_vol = EXCLUDED.rootfs_vol,
    sandbox_gen = EXCLUDED.sandbox_gen,
    attached_at = EXCLUDED.attached_at,
    last_seen_at = EXCLUDED.last_seen_at,
    last_error = EXCLUDED.last_error,
    updated_at = EXCLUDED.updated_at;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260701040100_rt_active'));

-- +goose Down
--
-- Down only removes the active-state projection. ACTIVE duplicate rows that
-- were marked RELEASED during Up are not restored; this mirrors the
-- normalization style used by prior migrations before adding uniqueness.

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260701040100_rt_active', 60);

DROP TABLE IF EXISTS t_cube_snapshot_runtime_active;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260701040100_rt_active'));
