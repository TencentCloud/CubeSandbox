-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Standalone (deleted_at) indexes for the scheduled tombstone purger
-- (issue #973). PostgreSQL counterpart of
-- mysql/20260731120000_soft_delete_purge_indexes.sql. A `deleted_at < cutoff`
-- scan needs a LEADING deleted_at index; without it the purge is a full table
-- scan every tick (and on PG, mass DELETEs without indexed selection amplify
-- dead-tuple bloat between autovacuum runs).
--
-- Why plain CREATE INDEX (not CONCURRENTLY): a non-concurrent CREATE INDEX is
-- atomic — it either builds the index or errors, so it can NEVER leave an INVALID
-- index behind. CREATE INDEX CONCURRENTLY avoids the ACCESS EXCLUSIVE lock but
-- can leave an INVALID index if interrupted, and recovering from that requires a
-- PL/pgSQL DO/function block — which goose's NO TRANSACTION splitter cannot parse
-- (it splits on ';' inside `$$`; see postgres/20260701040100_snapshot_runtime_active_binding.sql
-- for the documented constraint). An invalid index would silently make every
-- purge pass a full table scan, so the atomic plain build is the safer choice
-- here. The cost is a one-time ACCESS EXCLUSIVE lock during this startup
-- migration (the service is not serving yet). For larger PostgreSQL deployments,
-- apply this migration during a low-traffic or maintenance window.
--
-- Deployments running with CUBE_AUTO_MIGRATION=false must apply these out-of-band.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260731120000_soft_delete_purge_idx', 60);

CREATE INDEX IF NOT EXISTS idx_sandbox_spec_deleted_at      ON t_cube_sandbox_spec      (deleted_at);
CREATE INDEX IF NOT EXISTS idx_instance_info_deleted_at     ON t_cube_instance_info     (deleted_at);
CREATE INDEX IF NOT EXISTS idx_instance_userdata_deleted_at ON t_cube_instance_userdata (deleted_at);
CREATE INDEX IF NOT EXISTS idx_template_replica_deleted_at  ON t_cube_template_replica  (deleted_at);
CREATE INDEX IF NOT EXISTS idx_agenthub_snapshot_deleted_at ON t_agenthub_snapshot      (deleted_at);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260731120000_soft_delete_purge_idx'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260731120000_soft_delete_purge_idx', 60);

DROP INDEX IF EXISTS idx_agenthub_snapshot_deleted_at;
DROP INDEX IF EXISTS idx_template_replica_deleted_at;
DROP INDEX IF EXISTS idx_instance_userdata_deleted_at;
DROP INDEX IF EXISTS idx_instance_info_deleted_at;
DROP INDEX IF EXISTS idx_sandbox_spec_deleted_at;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260731120000_soft_delete_purge_idx'));
