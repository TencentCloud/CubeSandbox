-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

BEGIN;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS last_login_at timestamptz;

UPDATE accounts
SET last_login_at = TIMESTAMPTZ '2026-01-01 00:00:00+00'
WHERE last_login_at IS NULL;

ALTER TABLE accounts
    ALTER COLUMN last_login_at SET NOT NULL;

INSERT INTO schema_migrations (version)
VALUES ('add_last_login')
ON CONFLICT (version) DO NOTHING;

COMMIT;
