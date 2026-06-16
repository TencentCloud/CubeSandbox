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

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0007_template_image_pull_progress', 60);

ALTER TABLE `t_cube_template_image_job`
  ADD COLUMN `pull_total_bytes` bigint NOT NULL DEFAULT 0,
  ADD COLUMN `pull_downloaded_bytes` bigint NOT NULL DEFAULT 0,
  ADD COLUMN `pull_total_layers` int NOT NULL DEFAULT 0,
  ADD COLUMN `pull_completed_layers` int NOT NULL DEFAULT 0,
  ADD COLUMN `pull_speed_bps` bigint NOT NULL DEFAULT 0;

SELECT RELEASE_LOCK('cubemaster_migration_0007_template_image_pull_progress');

-- +goose Down

CALL cubemaster_acquire_migration_lock('cubemaster_migration_0007_template_image_pull_progress', 60);

ALTER TABLE `t_cube_template_image_job`
  DROP COLUMN `pull_total_bytes`,
  DROP COLUMN `pull_downloaded_bytes`,
  DROP COLUMN `pull_total_layers`,
  DROP COLUMN `pull_completed_layers`,
  DROP COLUMN `pull_speed_bps`;

SELECT RELEASE_LOCK('cubemaster_migration_0007_template_image_pull_progress');