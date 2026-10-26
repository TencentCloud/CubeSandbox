-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Blobstore schema for the same unreleased change:
--   1. t_cube_rootfs_artifact.storage_backend + object_key (locator columns).
--   2. leftover t_component_warehouse.rel_path → object_key.
-- 20260813120000 originally created rel_path; that same goose version was
-- later rewritten in-tree to object_key. goose never re-runs an applied
-- version, so those clusters still have rel_path while CubeOps SELECTs
-- object_key (MySQL 1054). CREATE TABLE IF NOT EXISTS in 20260813 is a
-- no-op on an existing table; this file is the actual warehouse upgrade.
-- Do not edit 20260813120000.
--
-- Warehouse Up: no-op if the table is missing; add object_key; copy
-- rel_path when both columns exist; drop rel_path. Idempotent on HEAD
-- (object_key present, no rel_path).
--
-- Warehouse Down restores rel_path from object_key but must NOT drop
-- object_key: HEAD CREATE and CubeOps both require that column.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915120000_blobstore', 60);

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'storage_backend',
  "varchar(16) NOT NULL DEFAULT '' COMMENT 's3|fs; empty means infer from artifact_url' AFTER `artifact_url`"
);

CALL cubemaster_add_column_if_missing(
  't_cube_rootfs_artifact',
  'object_key',
  "varchar(512) NOT NULL DEFAULT '' COMMENT 'blobstore object key' AFTER `storage_backend`"
);

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_wh_rel_path_to_object_key;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE PROCEDURE cubemaster_wh_rel_path_to_object_key()
BEGIN
  IF EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.TABLES
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = 't_component_warehouse'
  ) THEN
    CALL cubemaster_add_column_if_missing(
      't_component_warehouse',
      'object_key',
      'varchar(512) NOT NULL DEFAULT '''' AFTER `source_ref`'
    );

    IF EXISTS (
      SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE()
         AND TABLE_NAME = 't_component_warehouse'
         AND COLUMN_NAME = 'rel_path'
    ) THEN
      UPDATE `t_component_warehouse`
         SET `object_key` = `rel_path`
       WHERE `object_key` = '' AND `rel_path` <> '';
    END IF;

    CALL cubemaster_drop_column_if_exists('t_component_warehouse', 'rel_path');
  END IF;
END;
-- +goose StatementEnd

CALL cubemaster_wh_rel_path_to_object_key();
DROP PROCEDURE IF EXISTS cubemaster_wh_rel_path_to_object_key;

SELECT RELEASE_LOCK('cubemaster_migration_20260915120000_blobstore');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260915120000_blobstore', 60);

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_wh_rel_path_from_object_key;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE PROCEDURE cubemaster_wh_rel_path_from_object_key()
BEGIN
  IF EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.TABLES
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = 't_component_warehouse'
  ) THEN
    CALL cubemaster_add_column_if_missing(
      't_component_warehouse',
      'rel_path',
      'varchar(512) NOT NULL DEFAULT '''' AFTER `source_ref`'
    );

    IF EXISTS (
      SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE()
         AND TABLE_NAME = 't_component_warehouse'
         AND COLUMN_NAME = 'object_key'
    ) THEN
      UPDATE `t_component_warehouse`
         SET `rel_path` = `object_key`
       WHERE `rel_path` = '' AND `object_key` <> '';
    END IF;
  END IF;
END;
-- +goose StatementEnd

CALL cubemaster_wh_rel_path_from_object_key();
DROP PROCEDURE IF EXISTS cubemaster_wh_rel_path_from_object_key;

CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'object_key');
CALL cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'storage_backend');

SELECT RELEASE_LOCK('cubemaster_migration_20260915120000_blobstore');
