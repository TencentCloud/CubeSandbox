-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- A pause binding remembers where its last Resume was sent. While that
-- attempt's outcome is unknown, every later Resume of the binding goes to the
-- same node, so a lost reply can never start the sandbox on a second node.

-- +goose Up
ALTER TABLE t_cube_pause_snapshot
  ADD COLUMN IF NOT EXISTS resume_target varchar(64) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS resume_state varchar(16) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS resume_attempt integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE t_cube_pause_snapshot
  DROP COLUMN IF EXISTS resume_attempt,
  DROP COLUMN IF EXISTS resume_state,
  DROP COLUMN IF EXISTS resume_target;
