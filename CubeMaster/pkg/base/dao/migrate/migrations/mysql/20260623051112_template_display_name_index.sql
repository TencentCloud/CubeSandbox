-- Index display_name for template name → templateID lookup and uniqueness.
--
-- PRODUCTION: run the audit queries below on staging before applying. This migration
-- clears reserved-prefix names and duplicate display names (prefers non-FAILED
-- survivor, then oldest id).
-- Review release notes with operators before rolling out to production.
--
-- Pre-migration audit (run manually on staging before apply):
--   Prefix violations (expect zero rows before apply):
--   SELECT template_id, display_name FROM t_cube_template_definition
--     WHERE kind = 'template' AND TRIM(display_name) <> ''
--       AND (LOWER(display_name) LIKE 'tpl-%' OR LOWER(display_name) LIKE 'snap-%');
--   Duplicate names (expect zero rows, or review losers below before apply):
--   SELECT LOWER(display_name) AS name_key, COUNT(*) AS cnt
--     FROM t_cube_template_definition
--     WHERE kind = 'template' AND TRIM(display_name) <> ''
--     GROUP BY LOWER(display_name) HAVING cnt > 1;
--   Rows that will lose display_name (keep_id is the survivor: non-FAILED first, then oldest id):
--   SELECT t.template_id, t.display_name, t.status, t.id AS loser_id, keep.keep_id, keep.keep_status
--     FROM t_cube_template_definition t
--     JOIN (
--       SELECT name_key, keep_id, keep_status FROM (
--         SELECT LOWER(display_name) AS name_key, id AS keep_id, status AS keep_status,
--                ROW_NUMBER() OVER (
--                  PARTITION BY LOWER(display_name)
--                  ORDER BY CASE WHEN UPPER(status) = 'FAILED' THEN 1 ELSE 0 END, id
--                ) AS rn
--           FROM t_cube_template_definition
--          WHERE kind = 'template' AND TRIM(display_name) <> ''
--       ) ranked WHERE rn = 1
--     ) keep ON LOWER(t.display_name) = keep.name_key
--    WHERE t.kind = 'template' AND TRIM(t.display_name) <> '' AND t.id <> keep.keep_id;

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260623051112_template_display_name_index', 60);

CALL cubemaster_assert_table_exists('t_cube_template_definition');

-- Drop invalid reserved-prefix names on templates (snapshots keep their names).
UPDATE `t_cube_template_definition`
   SET `display_name` = ''
 WHERE `kind` = 'template'
   AND TRIM(`display_name`) <> ''
   AND (
     LOWER(`display_name`) LIKE 'tpl-%'
     OR LOWER(`display_name`) LIKE 'snap-%'
   );

-- Resolve duplicate template display names: keep one row per name, preferring
-- non-FAILED status then oldest id; clear display_name on all other duplicates.
UPDATE `t_cube_template_definition` t
  JOIN (
    SELECT id
      FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                 PARTITION BY LOWER(`display_name`)
                 ORDER BY
                   CASE WHEN UPPER(`status`) = 'FAILED' THEN 1 ELSE 0 END,
                   id
               ) AS rn
          FROM `t_cube_template_definition`
         WHERE `kind` = 'template'
           AND TRIM(`display_name`) <> ''
      ) ranked
     WHERE rn > 1
  ) dup ON t.id = dup.id
   SET t.`display_name` = '';

CALL cubemaster_add_column_if_missing(
  't_cube_template_definition',
  'display_name_key',
  "varchar(256) GENERATED ALWAYS AS (IF(`display_name` = '' OR `kind` <> 'template', NULL, LOWER(`display_name`))) VIRTUAL AFTER `display_name`"
);

CALL cubemaster_add_index_if_missing(
  't_cube_template_definition',
  'idx_template_display_name_key',
  'ADD UNIQUE INDEX `idx_template_display_name_key` (`display_name_key`)'
);

SELECT RELEASE_LOCK('cubemaster_migration_20260623051112_template_display_name_index');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20260623051112_template_display_name_index', 60);

CALL cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_template_display_name_key');

CALL cubemaster_drop_column_if_exists('t_cube_template_definition', 'display_name_key');

SELECT RELEASE_LOCK('cubemaster_migration_20260623051112_template_display_name_index');
