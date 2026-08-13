-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Record per-node host facts (CPU feature set, host kernel, KVM ABI) and freeze
-- the origin-node host fingerprint onto snapshots so the control plane can judge
-- whether a snapshot created on one node may be restored on another.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260810120000_host_facts', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'host_facts_json',
  "text COMMENT 'host CPU/kernel/KVM facts json' AFTER `max_mvm_num`"
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
CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'cpu_vendor',
  "varchar(32) NOT NULL DEFAULT '' COMMENT 'host CPU vendor (informational; empty on ARM)' AFTER `host_facts_json`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'cpu_model',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'host CPU model name (informational)' AFTER `cpu_vendor`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'cpuid_hash',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'CPU identity+feature hash (required restore-compat key)' AFTER `cpu_model`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_node_registration',
  'host_kernel_release',
  "varchar(128) NOT NULL DEFAULT '' COMMENT 'host kernel release (required restore-compat key)' AFTER `cpuid_hash`"
);

-- Composite index on ONLY the two required (blocking) restore-compat keys. The
-- vendor/model columns are informational and never gate the query — indexing
-- them would not help and would wrongly imply they filter. This index is also
-- ARM-safe: on aarch64 cpu_vendor/cpu_model are empty but cpuid_hash and
-- host_kernel_release are populated, so the lookup still discriminates.
CALL cubemaster_add_index_if_missing(
  't_cube_node_registration',
  'idx_node_host_compat',
  'ADD INDEX `idx_node_host_compat` (`cpuid_hash`, `host_kernel_release`)'
);

CALL cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'origin_host_facts_json',
  "text COMMENT 'origin node host facts frozen at snapshot create' AFTER `rootfs_artifact_id`"
);

SELECT RELEASE_LOCK('cubemaster_migration_20260810120000_host_facts');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260810120000_host_facts', 60);

CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'origin_host_facts_json');
CALL cubemaster_drop_index_if_exists('t_cube_node_registration', 'idx_node_host_compat');
CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'host_kernel_release');
CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpuid_hash');
CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpu_model');
CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'cpu_vendor');
CALL cubemaster_drop_column_if_exists('t_cube_node_registration', 'host_facts_json');

SELECT RELEASE_LOCK('cubemaster_migration_20260810120000_host_facts');
