-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- No schema change. PostgreSQL is already UTF-8; this version matches MySQL.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261112035937_convert_legacy_utf8mb4', 60);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261112035937_convert_legacy_utf8mb4'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261112035937_convert_legacy_utf8mb4', 60);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261112035937_convert_legacy_utf8mb4'));
