-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Source-image pull progress for template image jobs (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0010_template_image_pull_progress', 60);

SELECT cubemaster_assert_table_exists('t_cube_template_image_job');

SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'pull_total_bytes',      'bigint NOT NULL DEFAULT 0');
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'pull_downloaded_bytes', 'bigint NOT NULL DEFAULT 0');
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'pull_total_layers',     'integer NOT NULL DEFAULT 0');
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'pull_completed_layers', 'integer NOT NULL DEFAULT 0');
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'pull_speed_bps',        'bigint NOT NULL DEFAULT 0');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0010_template_image_pull_progress'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0010_template_image_pull_progress', 60);

SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_speed_bps');
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_completed_layers');
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_total_layers');
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_downloaded_bytes');
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_total_bytes');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0010_template_image_pull_progress'));
