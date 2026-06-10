-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--

-- Template replica compatibility metadata.
--
-- Stores the guest environment identity that a READY template replica was
-- built against. Existing replicas start as UNKNOWN and can be adopted or
-- rebuilt explicitly after the migration.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0006_template_replica_compat', 60);

ALTER TABLE `t_cube_template_replica`
  ADD COLUMN `guest_image_version` VARCHAR(128) NOT NULL DEFAULT '' COMMENT 'guest-image version bound when this replica was created',
  ADD COLUMN `agent_version` VARCHAR(128) NOT NULL DEFAULT '' COMMENT 'cube-agent version bound when this replica was created',
  ADD COLUMN `kernel_version` VARCHAR(256) NOT NULL DEFAULT '' COMMENT 'kernel artifact identity bound when this replica was created',
  ADD COLUMN `compat_status` VARCHAR(32) NOT NULL DEFAULT 'UNKNOWN' COMMENT 'template replica compatibility status: OK/STALE/UNKNOWN',
  ADD COLUMN `compat_policy` VARCHAR(32) NOT NULL DEFAULT 'STRICT' COMMENT 'compatibility policy: STRICT/GUEST_ONLY',
  ADD COLUMN `compat_checked_unix` BIGINT NOT NULL DEFAULT 0 COMMENT 'last compatibility check unix timestamp';

ALTER TABLE `t_cube_template_replica`
  ADD INDEX `idx_node_compat` (`node_id`, `compat_status`);

SELECT RELEASE_LOCK('cubemaster_migration_0006_template_replica_compat');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_0006_template_replica_compat', 60);

ALTER TABLE `t_cube_template_replica`
  DROP INDEX `idx_node_compat`,
  DROP COLUMN `guest_image_version`,
  DROP COLUMN `agent_version`,
  DROP COLUMN `kernel_version`,
  DROP COLUMN `compat_status`,
  DROP COLUMN `compat_policy`,
  DROP COLUMN `compat_checked_unix`;

SELECT RELEASE_LOCK('cubemaster_migration_0006_template_replica_compat');
