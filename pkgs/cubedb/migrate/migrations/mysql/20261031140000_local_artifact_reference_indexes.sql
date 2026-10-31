-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose NO TRANSACTION
-- +goose Up
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261031140000_local_artifact', 60);
CALL cubemaster_add_index_if_missing('t_cube_template_image_job', 'idx_template_job_artifact_live',
 'ADD INDEX `idx_template_job_artifact_live` (`artifact_id`, `status`, `deleted_at`)');
CALL cubemaster_add_index_if_missing('t_cube_template_replica', 'idx_template_replica_artifact_live',
 'ADD INDEX `idx_template_replica_artifact_live` (`artifact_id`, `deleted_at`)');
SELECT RELEASE_LOCK('cubemaster_migration_20261031140000_local_artifact');

-- +goose Down
CALL cubemaster_acquire_migration_lock('cubemaster_migration_20261031140000_local_artifact', 60);
CALL cubemaster_drop_index_if_exists('t_cube_template_replica', 'idx_template_replica_artifact_live');
CALL cubemaster_drop_index_if_exists('t_cube_template_image_job', 'idx_template_job_artifact_live');
SELECT RELEASE_LOCK('cubemaster_migration_20261031140000_local_artifact');
