-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Web Terminal one-time grants and payload-free session audit records.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260729223226_terminal_grants_and_sessions', 60);

CREATE TABLE IF NOT EXISTS terminal_grants (
  id varchar(36) NOT NULL,
  token_hash varchar(64) NOT NULL,
  kind varchar(8) NOT NULL,
  user_id varchar(64) NOT NULL,
  sandbox_id varchar(64) NOT NULL,
  container_id varchar(128) NOT NULL,
  session_id varchar(36),
  "cols" smallint NOT NULL,
  "rows" smallint NOT NULL,
  resume_offset bigint NOT NULL DEFAULT 0,
  created_at timestamp(3) NOT NULL,
  expires_at timestamp(3) NOT NULL,
  consumed_at timestamp(3),
  PRIMARY KEY (id),
  UNIQUE (token_hash)
);

CREATE INDEX IF NOT EXISTS idx_terminal_grants_expires
  ON terminal_grants (expires_at);
CREATE INDEX IF NOT EXISTS idx_terminal_grants_user_created
  ON terminal_grants (user_id, created_at);

CREATE TABLE IF NOT EXISTS terminal_sessions (
  id varchar(36) NOT NULL,
  user_id varchar(64) NOT NULL,
  sandbox_id varchar(64) NOT NULL,
  container_id varchar(128) NOT NULL,
  cubelet_host varchar(64) NOT NULL DEFAULT '',
  opened_at timestamp(3) NOT NULL,
  last_seen_at timestamp(3) NOT NULL,
  closed_at timestamp(3),
  close_reason varchar(32),
  exit_code integer,
  bytes_in bigint NOT NULL DEFAULT 0,
  bytes_out bigint NOT NULL DEFAULT 0,
  resume_count integer NOT NULL DEFAULT 0,
  PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_terminal_sessions_user_active
  ON terminal_sessions (user_id, closed_at);
CREATE INDEX IF NOT EXISTS idx_terminal_sessions_sandbox_opened
  ON terminal_sessions (sandbox_id, opened_at);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260729223226_terminal_grants_and_sessions'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260729223226_terminal_grants_and_sessions', 60);

DROP TABLE IF EXISTS terminal_sessions;
DROP TABLE IF EXISTS terminal_grants;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260729223226_terminal_grants_and_sessions'));
