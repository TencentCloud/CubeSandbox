-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- PostgreSQL counterpart of mysql/20260824120000_backfill_snapshot_tables.sql.
-- Lookup keys only; any failure is NOTICE + skip so goose/Master still start.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260824120000_backfill_snapshot_tables', 60);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_try_backfill_snap_20260824()
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  BEGIN
    INSERT INTO t_cube_snapshot (
      snapshot_id,
      origin_sandbox_id,
      origin_node_id,
      status,
      backend,
      request_json
    )
    SELECT
      d.template_id,
      COALESCE(d.origin_sandbox_id, ''),
      COALESCE(d.origin_node_id, ''),
      COALESCE(d.status, ''),
      COALESCE(d.storage_backend, ''),
      COALESCE(d.request_json, '')
    FROM t_cube_template_definition d
    WHERE LOWER(d.kind) = 'snapshot'
      AND d.template_id <> ''
      AND NOT EXISTS (
        SELECT 1 FROM t_cube_snapshot s WHERE s.snapshot_id = d.template_id
      );
  EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'snapshot backfill skipped: %', SQLERRM;
  END;

  BEGIN
    INSERT INTO t_cube_pause_snapshot (
      snapshot_id,
      sandbox_id,
      node_id,
      status,
      backend
    )
    SELECT
      d.template_id,
      d.origin_sandbox_id,
      COALESCE(d.origin_node_id, ''),
      COALESCE(d.status, ''),
      COALESCE(d.storage_backend, '')
    FROM t_cube_template_definition d
    WHERE LOWER(d.kind) = 'pause_snapshot'
      AND d.template_id <> ''
      AND d.origin_sandbox_id <> ''
      AND NOT EXISTS (
        SELECT 1 FROM t_cube_pause_snapshot p
        WHERE p.snapshot_id = d.template_id
           OR p.sandbox_id = d.origin_sandbox_id
      );
  EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'pause-snapshot backfill skipped: %', SQLERRM;
  END;
END;
$$;
-- +goose StatementEnd

SELECT cubemaster_try_backfill_snap_20260824();
DROP FUNCTION IF EXISTS cubemaster_try_backfill_snap_20260824();

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260824120000_backfill_snapshot_tables'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260824120000_backfill_snapshot_tables', 60);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_try_undo_backfill_snap_20260824()
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  BEGIN
    DELETE FROM t_cube_snapshot s
    USING t_cube_template_definition d
    WHERE d.template_id = s.snapshot_id
      AND LOWER(d.kind) = 'snapshot';
  EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'snapshot backfill undo skipped: %', SQLERRM;
  END;

  BEGIN
    DELETE FROM t_cube_pause_snapshot p
    USING t_cube_template_definition d
    WHERE d.template_id = p.snapshot_id
      AND LOWER(d.kind) = 'pause_snapshot';
  EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'pause-snapshot backfill undo skipped: %', SQLERRM;
  END;
END;
$$;
-- +goose StatementEnd

SELECT cubemaster_try_undo_backfill_snap_20260824();
DROP FUNCTION IF EXISTS cubemaster_try_undo_backfill_snap_20260824();

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260824120000_backfill_snapshot_tables'));
