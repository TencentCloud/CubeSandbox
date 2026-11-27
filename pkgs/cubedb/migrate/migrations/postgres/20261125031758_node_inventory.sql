-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261125031758_node_inventory', 60);
SELECT cubemaster_assert_table_exists('t_cube_node_status');

CREATE TABLE IF NOT EXISTS t_cube_node_inventory (
  status_id bigint PRIMARY KEY,
  digest varchar(64) NOT NULL,
  images_json text NOT NULL,
  local_templates_json text NOT NULL,
  FOREIGN KEY (status_id) REFERENCES t_cube_node_status (id) ON DELETE CASCADE
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261125031758_node_inventory'));

-- +goose Down
-- Stop CubeOps writers before restoring inline inventories for an older binary.
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261125031758_node_inventory', 60);

-- A retry can reach this block after the table was dropped but before goose
-- recorded the downgrade. Keep the restore and drop in a single statement.
-- +goose StatementBegin
DO $$
BEGIN
  IF to_regclass('t_cube_node_inventory') IS NOT NULL THEN
    UPDATE t_cube_node_status AS s
    SET images_json = COALESCE(s.images_json, i.images_json),
        local_templates_json = COALESCE(s.local_templates_json, i.local_templates_json)
    FROM t_cube_node_inventory AS i
    WHERE i.status_id = s.id AND (s.images_json IS NULL OR s.local_templates_json IS NULL);

    DROP TABLE t_cube_node_inventory;
  END IF;
END;
$$;
-- +goose StatementEnd

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261125031758_node_inventory'));
