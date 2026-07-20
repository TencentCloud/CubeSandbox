-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Enforce template-alias uniqueness on t_cube_template_definition.display_name.
--
-- A STORED generated column (alias_key) derives a nullable value from
-- display_name — non-NULL only for kind='template' rows with a non-empty
-- display_name, NULL for snapshots and alias-less templates. A plain UNIQUE
-- INDEX on alias_key then enforces uniqueness while allowing unlimited NULLs.
--
-- This mirrors the MySQL migration exactly (same generated column expression
-- and index) so the cross-dialect schema-alignment test sees identical
-- column sets and index signatures.
--
-- MySQL counterpart: mysql/20260704120000_template_alias_unique.sql

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260704120000_tpl_alias_unique', 60);

SELECT cubemaster_assert_table_exists('t_cube_template_definition');

-- De-duplicate any pre-existing non-empty display_name values on
-- template-kind rows so the unique index can be created. For each
-- duplicated value, keep the newest row (MAX id) and clear the rest.
UPDATE t_cube_template_definition AS t
   SET display_name = ''
  FROM (
    SELECT display_name, MAX(id) AS keep_id
      FROM t_cube_template_definition
     WHERE display_name <> '' AND kind = 'template'
     GROUP BY display_name
    HAVING COUNT(*) > 1
  ) AS d
 WHERE t.display_name = d.display_name
   AND t.kind = 'template'
   AND t.id <> d.keep_id;

-- Generated column: non-NULL only for kind='template' with non-empty
-- display_name. NULL everywhere else → exempt from the unique constraint.
SELECT cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'alias_key',
  'varchar(256) GENERATED ALWAYS AS (CASE WHEN kind = ''template'' AND display_name <> '''' THEN display_name ELSE NULL END) STORED'
);

SELECT cubemaster_add_index_if_missing(
  't_cube_template_definition',
  'idx_template_definition_alias_unique',
  'CREATE UNIQUE INDEX idx_template_definition_alias_unique ON t_cube_template_definition (alias_key)'
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260704120000_tpl_alias_unique'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260704120000_tpl_alias_unique', 60);

SELECT cubemaster_drop_index_if_exists(
  't_cube_template_definition',
  'idx_template_definition_alias_unique'
);

SELECT cubemaster_drop_column_if_exists(
  't_cube_template_definition',
  'alias_key'
);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260704120000_tpl_alias_unique'));
