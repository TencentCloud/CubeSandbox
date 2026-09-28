-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Enforce template-alias uniqueness on t_cube_template_definition.display_name.
--
-- A STORED generated column (alias_key) derives a nullable value from
-- display_name — non-NULL only for kind='template' rows with a non-empty
-- display_name, NULL for snapshots and alias-less templates. A plain UNIQUE
-- INDEX on alias_key then enforces uniqueness while allowing unlimited NULLs
-- (both MySQL and PostgreSQL exempt NULLs from unique constraints).
--
-- This approach replaces a MySQL functional CASE index because functional
-- indexes report COLUMN_NAME=NULL in INFORMATION_SCHEMA.STATISTICS, which
-- breaks the cross-dialect schema-alignment test and produces mismatched
-- index signatures vs PostgreSQL's partial index. A generated column yields
-- identical index signatures (cols=alias_key) in both dialects.
--
-- PostgreSQL counterpart: postgres/20260704120000_template_alias_unique.sql

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260704120000_tpl_alias_unique', 60);

-- De-duplicate any pre-existing non-empty display_name values on
-- template-kind rows so the unique index can be created. For each
-- duplicated value, keep the newest row (MAX id) and clear the rest.
-- Snapshot-kind rows are intentionally excluded: their display_name is a
-- free-form label and may legitimately repeat a template alias.
UPDATE `t_cube_template_definition` AS t
JOIN (
  SELECT `display_name`, MAX(`id`) AS keep_id
  FROM `t_cube_template_definition`
  WHERE `display_name` <> '' AND `kind` = 'template'
  GROUP BY `display_name`
  HAVING COUNT(*) > 1
) AS d
  ON t.`display_name` = d.`display_name`
 AND t.`kind` = 'template'
 AND t.`id` <> d.`keep_id`
SET t.`display_name` = '';

-- Generated column: non-NULL only for kind='template' with non-empty
-- display_name. NULL everywhere else → exempt from the unique constraint.
CALL cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'alias_key',
  'varchar(256) GENERATED ALWAYS AS (CASE WHEN `kind` = ''template'' AND `display_name` <> '''' THEN `display_name` ELSE NULL END) STORED'
);

CALL cubemaster_add_index_if_missing(
  't_cube_template_definition',
  'idx_template_definition_alias_unique',
  'ADD UNIQUE INDEX `idx_template_definition_alias_unique` (`alias_key`)'
);

SELECT RELEASE_LOCK('cubemaster_migration_20260704120000_tpl_alias_unique');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260704120000_tpl_alias_unique', 60);

CALL cubemaster_drop_index_if_exists(
  't_cube_template_definition',
  'idx_template_definition_alias_unique'
);

CALL cubemaster_drop_column_if_exists(
  't_cube_template_definition',
  'alias_key'
);

SELECT RELEASE_LOCK('cubemaster_migration_20260704120000_tpl_alias_unique');
