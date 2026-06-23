-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Source-image pull progress for template image jobs.
--
-- Adds best-effort byte/layer counters that the template-from-image runner
-- streams from `docker pull` / `skopeo copy` so cubemastercli can render a live
-- pull progress bar. All columns are additive with a zero default, so existing
-- rows and code paths that ignore them stay valid.

-- +goose NO TRANSACTION
-- +goose Up

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0010_template_image_pull_progress', 60);

CALL cubemaster_assert_table_exists('t_cube_template_image_job');

CALL cubemaster_add_column_if_missing(
  't_cube_template_image_job',
  'pull_total_bytes',
  "bigint NOT NULL DEFAULT 0 COMMENT 'source image pull total bytes' AFTER `artifact_status`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_image_job',
  'pull_downloaded_bytes',
  "bigint NOT NULL DEFAULT 0 COMMENT 'source image pull downloaded bytes' AFTER `pull_total_bytes`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_image_job',
  'pull_total_layers',
  "int NOT NULL DEFAULT 0 COMMENT 'source image pull total layers' AFTER `pull_downloaded_bytes`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_image_job',
  'pull_completed_layers',
  "int NOT NULL DEFAULT 0 COMMENT 'source image pull completed layers' AFTER `pull_total_layers`"
);
CALL cubemaster_add_column_if_missing(
  't_cube_template_image_job',
  'pull_speed_bps',
  "bigint NOT NULL DEFAULT 0 COMMENT 'source image pull speed bytes per second' AFTER `pull_completed_layers`"
);

SELECT RELEASE_LOCK('cubemaster_migration_0010_template_image_pull_progress');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0010_template_image_pull_progress', 60);

CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_speed_bps');
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_completed_layers');
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_total_layers');
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_downloaded_bytes');
CALL cubemaster_drop_column_if_exists('t_cube_template_image_job', 'pull_total_bytes');

SELECT RELEASE_LOCK('cubemaster_migration_0010_template_image_pull_progress');
