-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- PostgreSQL counterpart of mysql/20260915120000_blobstore_object_store.sql.
-- Blobstore schema for the same unreleased change:
--   1. t_cube_rootfs_artifact.storage_backend + object_key.
--   2. leftover t_component_warehouse.rel_path → object_key.
-- Do not edit 20260813120000. Warehouse Down restores rel_path from
-- object_key but must not drop object_key.

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260915120000_blobstore', 60);

SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'storage_backend',
  $$varchar(16) NOT NULL DEFAULT ''$$
);

SELECT cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'object_key',
  $$varchar(512) NOT NULL DEFAULT ''$$
);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_wh_rel_path_to_object_key()
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = current_schema()
       AND table_name = 't_component_warehouse'
  ) THEN
    PERFORM cubemaster_add_column_if_missing(
      't_component_warehouse',
      'object_key',
      'varchar(512) NOT NULL DEFAULT '''''
    );

    IF EXISTS (
      SELECT 1 FROM information_schema.columns
       WHERE table_schema = current_schema()
         AND table_name = 't_component_warehouse'
         AND column_name = 'rel_path'
    ) THEN
      UPDATE t_component_warehouse
         SET object_key = rel_path
       WHERE object_key = '' AND rel_path <> '';
    END IF;

    PERFORM cubemaster_drop_column_if_exists('t_component_warehouse', 'rel_path');
  END IF;
END;
$$;
-- +goose StatementEnd

SELECT cubemaster_wh_rel_path_to_object_key();
DROP FUNCTION IF EXISTS cubemaster_wh_rel_path_to_object_key();

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260915120000_blobstore'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260915120000_blobstore', 60);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_wh_rel_path_from_object_key()
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = current_schema()
       AND table_name = 't_component_warehouse'
  ) THEN
    PERFORM cubemaster_add_column_if_missing(
      't_component_warehouse',
      'rel_path',
      'varchar(512) NOT NULL DEFAULT '''''
    );

    IF EXISTS (
      SELECT 1 FROM information_schema.columns
       WHERE table_schema = current_schema()
         AND table_name = 't_component_warehouse'
         AND column_name = 'object_key'
    ) THEN
      UPDATE t_component_warehouse
         SET rel_path = object_key
       WHERE rel_path = '' AND object_key <> '';
    END IF;
  END IF;
END;
$$;
-- +goose StatementEnd

SELECT cubemaster_wh_rel_path_from_object_key();
DROP FUNCTION IF EXISTS cubemaster_wh_rel_path_from_object_key();

ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS object_key;
ALTER TABLE t_cube_rootfs_artifact DROP COLUMN IF EXISTS storage_backend;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260915120000_blobstore'));
