-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

BEGIN;

CREATE TABLE IF NOT EXISTS accounts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner text UNIQUE NOT NULL,
    balance integer NOT NULL CHECK (balance >= 0)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO accounts (owner, balance)
VALUES
    ('alice', 100),
    ('bob', 200)
ON CONFLICT (owner) DO UPDATE
SET balance = EXCLUDED.balance;

INSERT INTO schema_migrations (version)
VALUES ('base_v1')
ON CONFLICT (version) DO NOTHING;

COMMIT;
