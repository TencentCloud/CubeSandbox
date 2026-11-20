-- +goose NO TRANSACTION
-- +goose Up
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261015120000_rootfs', 60);
SELECT cubemaster_add_column_if_missing('t_cube_rootfs_artifact', 'rootfs_format', $$varchar(16) NOT NULL DEFAULT 'ext4'$$);
SELECT cubemaster_add_column_if_missing('t_cube_rootfs_artifact', 'rootfs_path', $$varchar(2048) NOT NULL DEFAULT ''$$);
SELECT cubemaster_add_column_if_missing('t_cube_rootfs_artifact', 'rootfs_sha256', $$varchar(128) NOT NULL DEFAULT ''$$);
SELECT cubemaster_add_column_if_missing('t_cube_rootfs_artifact', 'rootfs_size_bytes', $$bigint NOT NULL DEFAULT 0$$);
-- Copy the complete legacy representation, including soft-deleted artifacts.
-- Never overwrite a canonical identity already populated by a previous run.
UPDATE t_cube_rootfs_artifact
SET rootfs_path = ext4_path, rootfs_sha256 = ext4_sha256, rootfs_size_bytes = ext4_size_bytes
WHERE COALESCE(rootfs_path, '') = '' AND COALESCE(rootfs_sha256, '') = '' AND COALESCE(rootfs_size_bytes, 0) = 0;
UPDATE t_cube_template_image_job SET phase = 'BUILDING_ROOTFS' WHERE phase = 'BUILDING_EXT4';
UPDATE t_cube_template_image_job SET resume_phase = 'BUILDING_ROOTFS' WHERE resume_phase = 'BUILDING_EXT4';
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261015120000_rootfs'));

-- +goose Down
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20261015120000_rootfs', 60);
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'rootfs_size_bytes');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'rootfs_sha256');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'rootfs_path');
SELECT cubemaster_drop_column_if_exists('t_cube_rootfs_artifact', 'rootfs_format');
UPDATE t_cube_template_image_job SET phase = 'BUILDING_EXT4' WHERE phase = 'BUILDING_ROOTFS';
UPDATE t_cube_template_image_job SET resume_phase = 'BUILDING_EXT4' WHERE resume_phase = 'BUILDING_ROOTFS';
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20261015120000_rootfs'));
