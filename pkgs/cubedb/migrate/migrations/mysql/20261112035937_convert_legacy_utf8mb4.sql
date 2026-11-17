-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Remaining utf8mb3 tables become utf8mb4 / utf8mb4_0900_ai_ci.
-- Skip a table that is already converted. Fail if alias_key would no longer be unique.
-- Down fails once a supplementary character has been stored.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261112035937_convert_legacy_utf8mb4', 60);

CALL cubemaster_assert_table_exists('t_cube_instance_info');
CALL cubemaster_assert_table_exists('t_cube_instance_userdata');
CALL cubemaster_assert_table_exists('t_cube_node_registration');
CALL cubemaster_assert_table_exists('t_cube_node_status');
CALL cubemaster_assert_table_exists('t_cube_template_definition');
CALL cubemaster_assert_table_exists('t_cube_template_replica');
CALL cubemaster_assert_table_exists('t_cube_rootfs_artifact');
CALL cubemaster_assert_table_exists('t_cube_template_image_job');
CALL cubemaster_assert_table_exists('t_cube_sandbox_spec');
CALL cubemaster_assert_table_exists('t_cube_snapshot_runtime_ref');
CALL cubemaster_assert_table_exists('t_cube_snapshot_runtime_active');
CALL cubemaster_assert_column_exists('t_cube_template_definition', 'alias_key');

SET @legacy_alias_dups := (
  SELECT COUNT(*) FROM (
    SELECT 1
      FROM `t_cube_template_definition`
     WHERE `alias_key` IS NOT NULL
     GROUP BY CONVERT(`alias_key` USING utf8mb4) COLLATE utf8mb4_0900_ai_ci
    HAVING COUNT(*) > 1
  ) AS alias_dups
);
SET @legacy_alias_dup_sql := IF(
  @legacy_alias_dups > 0,
  'SIGNAL SQLSTATE ''45000'' SET MESSAGE_TEXT = ''t_cube_template_definition.alias_key duplicates under utf8mb4_0900_ai_ci''',
  'SELECT 1'
);
PREPARE legacy_alias_dup_stmt FROM @legacy_alias_dup_sql;
EXECUTE legacy_alias_dup_stmt;
DEALLOCATE PREPARE legacy_alias_dup_stmt;

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_convert_table_charset;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE PROCEDURE cubemaster_convert_table_charset(
  IN p_table VARCHAR(64),
  IN p_from_charset VARCHAR(32),
  IN p_target_charset VARCHAR(32),
  IN p_target_collation VARCHAR(64)
)
BEGIN
  IF EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = p_table
       AND (
         (p_from_charset = 'utf8mb3' AND CHARACTER_SET_NAME IN ('utf8', 'utf8mb3'))
         OR (p_from_charset <> 'utf8mb3' AND CHARACTER_SET_NAME = p_from_charset)
       )
  ) THEN
    SET @cubemaster_convert_charset_sql = CONCAT(
      'ALTER TABLE `', REPLACE(p_table, '`', '``'),
      '` CONVERT TO CHARACTER SET ', p_target_charset,
      ' COLLATE ', p_target_collation);
    PREPARE cubemaster_convert_charset_stmt FROM @cubemaster_convert_charset_sql;
    EXECUTE cubemaster_convert_charset_stmt;
    DEALLOCATE PREPARE cubemaster_convert_charset_stmt;
  END IF;
END;
-- +goose StatementEnd

CALL cubemaster_convert_table_charset('t_cube_instance_info', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_instance_userdata', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_node_registration', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_node_status', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_template_definition', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_template_replica', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_rootfs_artifact', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_template_image_job', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_sandbox_spec', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_snapshot_runtime_ref', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');
CALL cubemaster_convert_table_charset('t_cube_snapshot_runtime_active', 'utf8mb3', 'utf8mb4', 'utf8mb4_0900_ai_ci');

DROP PROCEDURE IF EXISTS cubemaster_convert_table_charset;

SELECT RELEASE_LOCK('cubemaster_migration_20261112035937_convert_legacy_utf8mb4');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261112035937_convert_legacy_utf8mb4', 60);

-- +goose StatementBegin
DROP PROCEDURE IF EXISTS cubemaster_convert_table_charset;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE PROCEDURE cubemaster_convert_table_charset(
  IN p_table VARCHAR(64),
  IN p_from_charset VARCHAR(32),
  IN p_target_charset VARCHAR(32),
  IN p_target_collation VARCHAR(64)
)
BEGIN
  IF EXISTS (
    SELECT 1 FROM INFORMATION_SCHEMA.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = p_table
       AND (
         (p_from_charset = 'utf8mb3' AND CHARACTER_SET_NAME IN ('utf8', 'utf8mb3'))
         OR (p_from_charset <> 'utf8mb3' AND CHARACTER_SET_NAME = p_from_charset)
       )
  ) THEN
    SET @cubemaster_convert_charset_sql = CONCAT(
      'ALTER TABLE `', REPLACE(p_table, '`', '``'),
      '` CONVERT TO CHARACTER SET ', p_target_charset,
      ' COLLATE ', p_target_collation);
    PREPARE cubemaster_convert_charset_stmt FROM @cubemaster_convert_charset_sql;
    EXECUTE cubemaster_convert_charset_stmt;
    DEALLOCATE PREPARE cubemaster_convert_charset_stmt;
  END IF;
END;
-- +goose StatementEnd

CALL cubemaster_convert_table_charset('t_cube_instance_info', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_instance_userdata', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_node_registration', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_node_status', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_template_definition', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_template_replica', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_rootfs_artifact', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_template_image_job', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_sandbox_spec', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_snapshot_runtime_ref', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');
CALL cubemaster_convert_table_charset('t_cube_snapshot_runtime_active', 'utf8mb4', 'utf8mb3', 'utf8mb3_general_ci');

DROP PROCEDURE IF EXISTS cubemaster_convert_table_charset;

SELECT RELEASE_LOCK('cubemaster_migration_20261112035937_convert_legacy_utf8mb4');
