-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- A pause binding remembers where its last Resume was sent. While that
-- attempt's outcome is unknown, every later Resume of the binding goes to the
-- same node, so a lost reply can never start the sandbox on a second node.

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261116120000_pause_resume_attempt', 60);
CALL cubemaster_add_column_if_missing(
  't_cube_pause_snapshot', 'resume_target',
  "varchar(64) NOT NULL DEFAULT '' COMMENT 'node ip of the latest resume attempt'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_pause_snapshot', 'resume_state',
  "varchar(16) NOT NULL DEFAULT '' COMMENT 'latest resume attempt: inflight|failed'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_pause_snapshot', 'resume_attempt',
  "int NOT NULL DEFAULT 0 COMMENT 'resume attempts sent for this binding'"
);
SELECT RELEASE_LOCK('cubemaster_migration_20261116120000_pause_resume_attempt');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261116120000_pause_resume_attempt', 60);
CALL cubemaster_drop_column_if_exists('t_cube_pause_snapshot', 'resume_attempt');
CALL cubemaster_drop_column_if_exists('t_cube_pause_snapshot', 'resume_state');
CALL cubemaster_drop_column_if_exists('t_cube_pause_snapshot', 'resume_target');
SELECT RELEASE_LOCK('cubemaster_migration_20261116120000_pause_resume_attempt');
