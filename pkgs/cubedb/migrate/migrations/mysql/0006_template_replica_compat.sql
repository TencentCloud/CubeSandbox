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

CALL cubemaster_assert_table_exists('t_cube_template_replica');

CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'guest_image_version',
  "VARCHAR(128) NOT NULL DEFAULT '' COMMENT 'guest-image version bound when this replica was created'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'agent_version',
  "VARCHAR(128) NOT NULL DEFAULT '' COMMENT 'cube-agent version bound when this replica was created'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'kernel_version',
  "VARCHAR(256) NOT NULL DEFAULT '' COMMENT 'kernel artifact identity bound when this replica was created'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'compat_status',
  "VARCHAR(32) NOT NULL DEFAULT 'UNKNOWN' COMMENT 'template replica compatibility status: OK/STALE/UNKNOWN'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'compat_policy',
  "VARCHAR(32) NOT NULL DEFAULT 'STRICT' COMMENT 'compatibility policy: STRICT/GUEST_ONLY'"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_replica',
  'compat_checked_unix',
  "BIGINT NOT NULL DEFAULT 0 COMMENT 'last compatibility check unix timestamp'"
);

CALL cubemaster_add_index_if_missing(
  't_cube_template_replica',
  'idx_node_compat',
  'ADD INDEX `idx_node_compat` (`node_id`, `compat_status`)'
);

SELECT RELEASE_LOCK('cubemaster_migration_0006_template_replica_compat');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_0006_template_replica_compat', 60);

CALL cubemaster_drop_index_if_exists('t_cube_template_replica', 'idx_node_compat');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'compat_checked_unix');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'compat_policy');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'compat_status');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'kernel_version');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'agent_version');
CALL cubemaster_drop_column_if_exists('t_cube_template_replica', 'guest_image_version');

SELECT RELEASE_LOCK('cubemaster_migration_0006_template_replica_compat');
