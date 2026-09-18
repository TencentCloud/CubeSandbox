-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Quota spec: node_managed marks rows created via the node-level API. Such
-- rows are fully authoritative (all five fields); rows created by the
-- cluster default (node_managed=0) are authoritative for the paused-release
-- ratio only. A sentinel row (node_id='*') stores the cluster default.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915200000_quota_spec_node_managed', 60);

ALTER TABLE `t_cube_node_quota_spec`
  ADD COLUMN `node_managed` tinyint(1) NOT NULL DEFAULT 0 COMMENT 'true: row written via node-level API (all fields authoritative); false: cluster-managed row (ratio only)' AFTER `updated_by`;

ALTER TABLE `t_cube_node_registration`
  ADD COLUMN `paused_release_ratio` double NULL DEFAULT NULL COMMENT 'paused-release ratio reported by cubelet heartbeats; NULL = not reported';

SELECT RELEASE_LOCK('cubemaster_migration_20260915200000_quota_spec_node_managed');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915200000_quota_spec_node_managed', 60);

ALTER TABLE `t_cube_node_registration` DROP COLUMN `paused_release_ratio`;
ALTER TABLE `t_cube_node_quota_spec` DROP COLUMN `node_managed`;

SELECT RELEASE_LOCK('cubemaster_migration_20260915200000_quota_spec_node_managed');
