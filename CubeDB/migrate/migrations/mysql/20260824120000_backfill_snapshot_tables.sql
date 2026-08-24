-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Best-effort copy of legacy kind=snapshot / kind=pause_snapshot rows out of
-- t_cube_template_definition. Only the lookup keys are required
-- (snapshot_id, origin_sandbox_id / sandbox_id, status, backend) — dest
-- tables already index those. Extra columns (host facts, export uuids,
-- replica node_ip) are not read and are not required.
--
-- Any mismatch (missing table/column, type drift, duplicate) is swallowed:
-- goose still records this version so Master/install does not fail. Later
-- restarts skip the file via goose_db_version.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260824120000_backfill_snapshot_tables', 60);

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_try_backfill_snap_20260824;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE PROCEDURE cubemaster_try_backfill_snap_20260824()
BEGIN
  DECLARE CONTINUE HANDLER FOR SQLEXCEPTION BEGIN END;

  INSERT INTO `t_cube_snapshot` (
    `snapshot_id`,
    `origin_sandbox_id`,
    `origin_node_id`,
    `status`,
    `backend`,
    `request_json`
  )
  SELECT
    d.`template_id`,
    IFNULL(d.`origin_sandbox_id`, ''),
    IFNULL(d.`origin_node_id`, ''),
    IFNULL(d.`status`, ''),
    IFNULL(d.`storage_backend`, ''),
    IFNULL(d.`request_json`, '')
  FROM `t_cube_template_definition` d
  WHERE LOWER(d.`kind`) = 'snapshot'
    AND d.`template_id` <> ''
    AND NOT EXISTS (
      SELECT 1 FROM `t_cube_snapshot` s WHERE s.`snapshot_id` = d.`template_id`
    );

  INSERT INTO `t_cube_pause_snapshot` (
    `snapshot_id`,
    `sandbox_id`,
    `node_id`,
    `status`,
    `backend`
  )
  SELECT
    d.`template_id`,
    d.`origin_sandbox_id`,
    IFNULL(d.`origin_node_id`, ''),
    IFNULL(d.`status`, ''),
    IFNULL(d.`storage_backend`, '')
  FROM `t_cube_template_definition` d
  WHERE LOWER(d.`kind`) = 'pause_snapshot'
    AND d.`template_id` <> ''
    AND d.`origin_sandbox_id` <> ''
    AND NOT EXISTS (
      SELECT 1 FROM `t_cube_pause_snapshot` p
      WHERE p.`snapshot_id` = d.`template_id`
         OR p.`sandbox_id` = d.`origin_sandbox_id`
    );
END;
-- +goose StatementEnd

CALL cubemaster_try_backfill_snap_20260824();
DROP PROCEDURE IF EXISTS cubemaster_try_backfill_snap_20260824;

SELECT RELEASE_LOCK('cubemaster_migration_20260824120000_backfill_snapshot_tables');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260824120000_backfill_snapshot_tables', 60);

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_try_undo_backfill_snap_20260824;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE PROCEDURE cubemaster_try_undo_backfill_snap_20260824()
BEGIN
  DECLARE CONTINUE HANDLER FOR SQLEXCEPTION BEGIN END;

  DELETE s FROM `t_cube_snapshot` s
  INNER JOIN `t_cube_template_definition` d
    ON d.`template_id` = s.`snapshot_id`
   AND LOWER(d.`kind`) = 'snapshot';

  DELETE p FROM `t_cube_pause_snapshot` p
  INNER JOIN `t_cube_template_definition` d
    ON d.`template_id` = p.`snapshot_id`
   AND LOWER(d.`kind`) = 'pause_snapshot';
END;
-- +goose StatementEnd

CALL cubemaster_try_undo_backfill_snap_20260824();
DROP PROCEDURE IF EXISTS cubemaster_try_undo_backfill_snap_20260824;

SELECT RELEASE_LOCK('cubemaster_migration_20260824120000_backfill_snapshot_tables');
