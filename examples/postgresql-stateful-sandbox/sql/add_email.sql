-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

BEGIN;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS email text;

UPDATE accounts
SET email = owner || '@example.test'
WHERE email IS NULL;

ALTER TABLE accounts
    ALTER COLUMN email SET NOT NULL;

INSERT INTO schema_migrations (version)
VALUES ('add_email')
ON CONFLICT (version) DO NOTHING;

COMMIT;
