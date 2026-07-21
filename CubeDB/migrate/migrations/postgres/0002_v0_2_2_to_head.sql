-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- v0.2.2 -> HEAD consolidated migration (PostgreSQL).
-- See mysql/0002_v0_2_2_to_head.sql for detailed commentary.

-- +goose NO TRANSACTION
-- +goose Up

-- (A) Acquire inner lock.
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0002_v0_2_2_to_head', 60);

-- Preflight checks.
SELECT cubemaster_assert_table_exists('t_cube_template_image_job');
SELECT cubemaster_assert_table_exists('t_cube_template_definition');
SELECT cubemaster_assert_table_exists('t_cube_template_replica');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'job_id');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'request_id');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'operation');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'node_id');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'source_image_ref');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'retry_of_job_id');
SELECT cubemaster_assert_column_exists('t_cube_template_image_job', 'status');
SELECT cubemaster_assert_column_exists('t_cube_template_definition', 'status');
SELECT cubemaster_assert_column_exists('t_cube_template_definition', 'request_json');
SELECT cubemaster_assert_column_exists('t_cube_template_replica', 'template_id');
SELECT cubemaster_assert_column_exists('t_cube_template_replica', 'spec');

-- (B) t_cube_template_image_job: add HEAD's three snapshot/resource columns.
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'sandbox_id',    'varchar(128) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'resource_type', 'varchar(32)  NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_image_job', 'resource_id',   'varchar(128) NOT NULL DEFAULT ''''');

-- (C) Data normalize for t_cube_template_image_job.
-- (C.1): fill empty request_id with 'legacy-' + job_id.
UPDATE t_cube_template_image_job
   SET request_id = 'legacy-' || job_id
 WHERE request_id = '' AND job_id <> '';

-- (C.2): infer operation for rows where it is empty.
UPDATE t_cube_template_image_job
   SET operation = CASE
         WHEN TRIM(node_id) <> '' AND TRIM(source_image_ref) = ''                          THEN 'COMMIT'
         WHEN TRIM(source_image_ref) <> '' AND TRIM(retry_of_job_id) = ''                  THEN 'CREATE'
         WHEN TRIM(source_image_ref) <> '' AND TRIM(retry_of_job_id) <> ''                 THEN 'REDO'
         ELSE 'LEGACY'
       END
 WHERE operation = '';

-- (C.3): break ties for remaining (request_id, operation) duplicates.
UPDATE t_cube_template_image_job t
   SET request_id = t.request_id || '#' || t.job_id
  FROM (
    SELECT MIN(id) AS keep_id, request_id, operation
      FROM t_cube_template_image_job
     WHERE request_id <> ''
     GROUP BY request_id, operation
    HAVING COUNT(*) > 1
  ) dup
 WHERE dup.request_id = t.request_id
   AND dup.operation = t.operation
   AND t.id <> dup.keep_id;

-- (D) t_cube_template_image_job: add HEAD's three new indexes.
CREATE INDEX IF NOT EXISTS idx_template_image_sandbox_status ON t_cube_template_image_job (sandbox_id, status);
CREATE INDEX IF NOT EXISTS idx_template_image_resource_status ON t_cube_template_image_job (resource_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_template_image_request_operation ON t_cube_template_image_job (request_id, operation);

-- (E) t_cube_template_definition: add HEAD's 7 new columns and 4 new indexes.
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'kind',                          'varchar(32) NOT NULL DEFAULT ''template''');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'origin_sandbox_id',             'varchar(128) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'origin_node_id',                'varchar(128) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'display_name',                  'varchar(256) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'storage_backend',               'varchar(32) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'retain',                        'boolean NOT NULL DEFAULT false');
SELECT cubemaster_add_column_if_missing('t_cube_template_definition', 'rootfs_size_bytes_at_snapshot', 'bigint NOT NULL DEFAULT 0');
CREATE INDEX IF NOT EXISTS idx_template_kind_status ON t_cube_template_definition (kind, status);
CREATE INDEX IF NOT EXISTS idx_snapshot_origin_sandbox ON t_cube_template_definition (origin_sandbox_id);
CREATE INDEX IF NOT EXISTS idx_snapshot_origin_node ON t_cube_template_definition (origin_node_id);
CREATE INDEX IF NOT EXISTS idx_template_storage_backend ON t_cube_template_definition (storage_backend);

-- (F.1) New table: t_cube_sandbox_spec.
CREATE TABLE IF NOT EXISTS t_cube_sandbox_spec (
  id bigserial NOT NULL,
  sandbox_id varchar(128) NOT NULL,
  template_id varchar(128) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT '',
  network_type varchar(64) NOT NULL DEFAULT '',
  host_id varchar(128) NOT NULL DEFAULT '',
  host_ip varchar(64) NOT NULL DEFAULT '',
  request_json text NOT NULL DEFAULT '',
  backfilled boolean NOT NULL DEFAULT false,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sandbox_spec_sandbox_id ON t_cube_sandbox_spec (sandbox_id);
CREATE INDEX IF NOT EXISTS idx_sandbox_spec_template_id ON t_cube_sandbox_spec (template_id);

-- (F.2) New table: t_cube_snapshot_runtime_ref.
CREATE TABLE IF NOT EXISTS t_cube_snapshot_runtime_ref (
  id bigserial NOT NULL,
  snapshot_id varchar(128) NOT NULL,
  sandbox_id varchar(128) NOT NULL,
  node_id varchar(128) NOT NULL DEFAULT '',
  node_ip varchar(64) NOT NULL DEFAULT '',
  binding_type varchar(64) NOT NULL DEFAULT '',
  memory_vol varchar(256) NOT NULL DEFAULT '',
  memory_dev varchar(256) NOT NULL DEFAULT '',
  rootfs_vol varchar(256) NOT NULL DEFAULT '',
  sandbox_gen integer NOT NULL DEFAULT 0,
  status varchar(32) NOT NULL DEFAULT '',
  attached_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  released_at timestamp DEFAULT NULL,
  last_seen_at timestamp DEFAULT NULL,
  last_error text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS idx_snapshot_runtime_ref_snapshot_status ON t_cube_snapshot_runtime_ref (snapshot_id, status);
CREATE INDEX IF NOT EXISTS idx_snapshot_runtime_ref_sandbox_status ON t_cube_snapshot_runtime_ref (sandbox_id, status);
CREATE INDEX IF NOT EXISTS idx_snapshot_runtime_ref_node_status ON t_cube_snapshot_runtime_ref (node_id, status);

-- (G) t_cube_template_replica: drop deprecated physical columns.
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'snapshot_path');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'rootfs_vol');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'memory_vol');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'rootfs_kind');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'memory_kind');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'rootfs_dev');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'memory_dev');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'meta_dir');
SELECT cubemaster_drop_column_if_exists('t_cube_template_replica', 'build_rootfs_vol');

-- (H) Release inner lock.
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0002_v0_2_2_to_head'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0002_v0_2_2_to_head', 60);

-- Reverse (G): re-add deprecated physical columns.
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'snapshot_path',    'varchar(1024) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'rootfs_vol',       'varchar(256) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'memory_vol',       'varchar(256) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'rootfs_kind',      'varchar(16) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'memory_kind',      'varchar(16) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'rootfs_dev',       'varchar(256) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'memory_dev',       'varchar(256) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'meta_dir',         'varchar(1024) NOT NULL DEFAULT ''''');
SELECT cubemaster_add_column_if_missing('t_cube_template_replica', 'build_rootfs_vol', 'varchar(256) NOT NULL DEFAULT ''''');

-- Reverse (F): drop new tables.
DROP TABLE IF EXISTS t_cube_snapshot_runtime_ref;
DROP TABLE IF EXISTS t_cube_sandbox_spec;

-- Reverse (E): drop new indexes and columns.
SELECT cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_template_storage_backend');
SELECT cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_snapshot_origin_node');
SELECT cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_snapshot_origin_sandbox');
SELECT cubemaster_drop_index_if_exists('t_cube_template_definition', 'idx_template_kind_status');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'rootfs_size_bytes_at_snapshot');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'retain');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'storage_backend');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'display_name');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'origin_node_id');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'origin_sandbox_id');
SELECT cubemaster_drop_column_if_exists('t_cube_template_definition', 'kind');

-- Reverse (D): drop new indexes.
DROP INDEX IF EXISTS idx_template_image_request_operation;
DROP INDEX IF EXISTS idx_template_image_resource_status;
DROP INDEX IF EXISTS idx_template_image_sandbox_status;

-- Reverse (B): drop three columns.
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'resource_id');
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'resource_type');
SELECT cubemaster_drop_column_if_exists('t_cube_template_image_job', 'sandbox_id');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0002_v0_2_2_to_head'));
