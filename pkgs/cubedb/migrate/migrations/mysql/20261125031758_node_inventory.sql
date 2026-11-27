-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261125031758_node_inventory', 60);
CALL cubemaster_assert_table_exists('t_cube_node_status');

CREATE TABLE IF NOT EXISTS t_cube_node_inventory (
  status_id bigint unsigned PRIMARY KEY,
  digest varchar(64) NOT NULL,
  images_json longtext NOT NULL,
  local_templates_json longtext NOT NULL,
  FOREIGN KEY (status_id) REFERENCES t_cube_node_status (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SELECT RELEASE_LOCK('cubemaster_migration_20261125031758_node_inventory');

-- +goose Down
-- Stop CubeOps writers before restoring inline inventories for an older binary.
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261125031758_node_inventory', 60);

-- MySQL commits the DROP before goose records the downgrade. A retry
-- after that point has no remaining inventory rows to restore.
SET @restore_inventory_sql = IF(
  EXISTS (SELECT 1 FROM information_schema.tables
          WHERE table_schema = DATABASE() AND table_name = 't_cube_node_inventory'),
  'UPDATE t_cube_node_status AS s
JOIN t_cube_node_inventory AS i ON i.status_id = s.id
SET s.images_json = COALESCE(s.images_json, i.images_json),
    s.local_templates_json = COALESCE(s.local_templates_json, i.local_templates_json)
WHERE s.images_json IS NULL OR s.local_templates_json IS NULL',
  'SELECT 1'
);
PREPARE restore_inventory FROM @restore_inventory_sql;
EXECUTE restore_inventory;
DEALLOCATE PREPARE restore_inventory;

DROP TABLE IF EXISTS t_cube_node_inventory;

SELECT RELEASE_LOCK('cubemaster_migration_20261125031758_node_inventory');
