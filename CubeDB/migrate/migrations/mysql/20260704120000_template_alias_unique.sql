-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Add a functional unique index on template_definitions.display_name so it
-- can serve as a stable template alias. MySQL does not support PostgreSQL-
-- style partial indexes (WHERE clauses on indexes), so we use a functional
-- index via NULLIF: NULLIF(display_name,'') returns NULL for the sentinel
-- empty-string value. MySQL unique indexes allow multiple NULLs but enforce
-- uniqueness on non-NULL values, giving us "UNIQUE WHERE display_name != ''".
-- Requires MySQL 8.0.13+ (functional key parts).

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260704120000_tpl_alias_unique', 60);

-- De-duplicate any pre-existing non-empty display_name values so the
-- unique index can be created. For each duplicated value, keep the newest
-- row (MAX id) and clear the rest. This is safe because display_name was
-- previously informational only (snapshot display names), not a lookup key.
UPDATE `t_cube_template_definition` AS t
JOIN (
  SELECT `display_name`, MAX(`id`) AS keep_id
  FROM `t_cube_template_definition`
  WHERE `display_name` <> ''
  GROUP BY `display_name`
  HAVING COUNT(*) > 1
) AS d
  ON t.`display_name` = d.`display_name`
 AND t.`id` <> d.`keep_id`
SET t.`display_name` = '';

-- NOTE: the empty-string literal inside NULLIF is escaped as '''' (each '
-- doubled) because the entire index definition is itself a single-quoted
-- SQL string passed to the stored procedure.
CALL cubemaster_add_index_if_missing(
  't_cube_template_definition',
  'idx_template_definition_alias_unique',
  'ADD UNIQUE INDEX `idx_template_definition_alias_unique` ((NULLIF(`display_name`, '''')))'
);

SELECT RELEASE_LOCK('cubemaster_migration_20260704120000_tpl_alias_unique');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260704120000_tpl_alias_unique', 60);

CALL cubemaster_drop_index_if_exists(
  't_cube_template_definition',
  'idx_template_definition_alias_unique'
);

SELECT RELEASE_LOCK('cubemaster_migration_20260704120000_tpl_alias_unique');
