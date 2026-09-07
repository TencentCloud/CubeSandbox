-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Record per-node host facts (CPU feature set, host kernel, KVM ABI) and freeze
-- the origin-node host fingerprint onto snapshots (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260810120000_host_facts', 60);

SELECT cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'host_facts_json',
  'text'
);

-- Promote the queryable host-fact keys to their own columns so the
-- compatible-nodes lookup can filter server-side with a single indexed SELECT
-- (WHERE cpuid_hash = ? AND host_kernel_release = ?) instead of loading and
-- JSON-decoding every node. The full fact set (including the taint gate and the
-- informational dimensions) stays in host_facts_json — these columns are a
-- denormalised query surface, not the source of truth.
--
-- No backfill is needed (or possible): host_facts_json is added by THIS same
-- migration, so there is no pre-existing blob to promote from — every row starts
-- with both the blob and the promoted columns empty. The columns (and the blob)
-- are populated together by persistHostFacts on each node's next heartbeat, which
-- also keeps them in lockstep. Until a node has heartbeated once post-upgrade it
-- is absent from compatible-nodes; this self-heals within one heartbeat interval
-- (≤ NodeStatusUpdateFrequency), and a stale-heartbeat node would be excluded by
-- the health-timeout join anyway.
SELECT cubemaster_add_column_if_missing('t_cube_node_registration', 'cpu_vendor', $$varchar(32) NOT NULL DEFAULT ''$$);
SELECT cubemaster_add_column_if_missing('t_cube_node_registration', 'cpu_model', $$varchar(128) NOT NULL DEFAULT ''$$);
SELECT cubemaster_add_column_if_missing('t_cube_node_registration', 'cpuid_hash', $$varchar(128) NOT NULL DEFAULT ''$$);
SELECT cubemaster_add_column_if_missing('t_cube_node_registration', 'host_kernel_release', $$varchar(128) NOT NULL DEFAULT ''$$);

-- Composite index on ONLY the two required (blocking) restore-compat keys. The
-- vendor/model columns are informational and never gate the query. ARM-safe: on
-- aarch64 cpu_vendor/cpu_model are empty but cpuid_hash and host_kernel_release
-- are populated, so the lookup still discriminates.
SELECT cubemaster_add_index_if_missing(
  't_cube_node_registration',
  'idx_node_host_compat',
  'CREATE INDEX idx_node_host_compat ON t_cube_node_registration (cpuid_hash, host_kernel_release)'
);

SELECT cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'origin_host_facts_json',
  'text'
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260810120000_host_facts'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260810120000_host_facts', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'origin_host_facts_json');
SELECT cubemaster_drop_index_if_exists('t_cube_node_registration', 'idx_node_host_compat');
SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'host_kernel_release');
SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpuid_hash');
SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpu_model');
SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpu_vendor');
SELECT cubemaster_drop_column_if_exists('t_cube_node_registration', 'host_facts_json');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260810120000_host_facts'));
