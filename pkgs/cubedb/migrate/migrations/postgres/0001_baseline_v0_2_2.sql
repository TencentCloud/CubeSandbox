-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- Baseline schema for CubeMaster v0.2.2 (PostgreSQL).
-- This file is the PG equivalent of mysql/0001_baseline_v0_2_2.sql.
-- See that file for detailed commentary on the two-layer locking scheme
-- and the fresh-deploy vs upgrade-path semantics.

-- +goose NO TRANSACTION
-- +goose Up

-- Helper function used by every migration to acquire its per-file advisory
-- lock or abort with a clean RAISE EXCEPTION.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_acquire_migration_lock(lock_name text, timeout_sec integer)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  lock_id bigint := hashtext(lock_name);
  deadline timestamptz := clock_timestamp() + make_interval(secs => timeout_sec);
  acquired boolean;
BEGIN
  LOOP
    SELECT pg_try_advisory_lock(lock_id) INTO acquired;
    IF acquired THEN
      RETURN;
    END IF;
    IF clock_timestamp() >= deadline THEN
      RAISE EXCEPTION 'cubemaster_acquire_migration_lock: failed to acquire lock % (id=%) after %s',
        lock_name, lock_id, timeout_sec;
    END IF;
    PERFORM pg_sleep(0.2);
  END LOOP;
END;
$$;
-- +goose StatementEnd

-- Helper that drops a column only if it exists.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_drop_column_if_exists(tbl text, col text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
     WHERE table_schema = current_schema()
       AND table_name = tbl
       AND column_name = col
  ) THEN
    EXECUTE format('ALTER TABLE %I DROP COLUMN %I', tbl, col);
  END IF;
END;
$$;
-- +goose StatementEnd

-- Helper that adds a column only if it does not yet exist.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_add_column_if_missing(tbl text, col text, coldef text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
     WHERE table_schema = current_schema()
       AND table_name = tbl
       AND column_name = col
  ) THEN
    EXECUTE format('ALTER TABLE %I ADD COLUMN %I %s', tbl, col, coldef);
  END IF;
END;
$$;
-- +goose StatementEnd

-- Helper that adds an index only if it does not yet exist. idxdef must be a
-- complete CREATE INDEX statement body after "CREATE INDEX IF NOT EXISTS".
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_add_index_if_missing(tbl text, idx text, idxdef text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_indexes
     WHERE schemaname = current_schema()
       AND tablename = tbl
       AND indexname = idx
  ) THEN
    EXECUTE idxdef;
  END IF;
END;
$$;
-- +goose StatementEnd

-- Helper that drops an index only if it exists.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_drop_index_if_exists(tbl text, idx text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_indexes
     WHERE schemaname = current_schema()
       AND tablename = tbl
       AND indexname = idx
  ) THEN
    EXECUTE format('DROP INDEX %I', idx);
  END IF;
END;
$$;
-- +goose StatementEnd

-- Preflight helpers fail early with a clear migration error.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_assert_table_exists(tbl text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = current_schema()
       AND table_name = tbl
  ) THEN
    RAISE EXCEPTION 'cubemaster schema preflight failed: required table missing: %', tbl;
  END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cubemaster_assert_column_exists(tbl text, col text)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
     WHERE table_schema = current_schema()
       AND table_name = tbl
       AND column_name = col
  ) THEN
    RAISE EXCEPTION 'cubemaster schema preflight failed: required column missing: %.%', tbl, col;
  END IF;
END;
$$;
-- +goose StatementEnd

-- Acquire this file's inner lock now that the function exists.
SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0001_baseline_v0_2_2', 60);

-- ---------------------------------------------------------------------
-- Host registry tables
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS t_cube_host_info (
  id bigserial NOT NULL,
  ins_id varchar(64) NOT NULL DEFAULT '',
  ip varchar(64) NOT NULL DEFAULT '',
  region varchar(64) NOT NULL DEFAULT 'local',
  zone varchar(64) NOT NULL DEFAULT 'local-a',
  uuid varchar(64) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT 'cubebox',
  cube_cluster_label varchar(64) NOT NULL DEFAULT 'default',
  oss_cluster_label varchar(64) NOT NULL DEFAULT '',
  host_status varchar(32) NOT NULL DEFAULT 'RUNNING',
  live_status varchar(32) NOT NULL DEFAULT 'LIVE',
  quota_cpu bigint NOT NULL DEFAULT 0,
  quota_mem_mb bigint NOT NULL DEFAULT 0,
  cpu_total bigint NOT NULL DEFAULT 0,
  mem_mb_total bigint NOT NULL DEFAULT 0,
  data_disk_gb bigint NOT NULL DEFAULT 0,
  sys_disk_gb bigint NOT NULL DEFAULT 0,
  create_concurrent_num bigint NOT NULL DEFAULT 0,
  max_mvm_num bigint NOT NULL DEFAULT 0,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_host_info_ins_id ON t_cube_host_info (ins_id);
CREATE INDEX IF NOT EXISTS idx_host_info_ip ON t_cube_host_info (ip);

CREATE TABLE IF NOT EXISTS t_cube_host_type (
  id bigserial NOT NULL,
  instance_type varchar(128) NOT NULL DEFAULT '',
  cpu_type varchar(64) NOT NULL DEFAULT 'INTEL',
  gpu_info varchar(1024) NOT NULL DEFAULT '',
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_host_type_instance_type ON t_cube_host_type (instance_type);

CREATE TABLE IF NOT EXISTS t_cube_sub_host_info (
  id bigserial NOT NULL,
  ins_id varchar(64) NOT NULL DEFAULT '',
  host_ip varchar(64) NOT NULL DEFAULT '',
  device_class varchar(64) NOT NULL DEFAULT '',
  device_id bigint NOT NULL DEFAULT 0,
  instance_family varchar(64) NOT NULL DEFAULT '',
  dedicated_cluster_id varchar(64) NOT NULL DEFAULT '',
  virtual_node_quota varchar(255) NOT NULL DEFAULT '[]',
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_sub_host_info_ins_id ON t_cube_sub_host_info (ins_id);

-- ---------------------------------------------------------------------
-- instancecache tables
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS t_cube_instance_info (
  id bigserial NOT NULL,
  ins_id varchar(64) NOT NULL,
  uuid varchar(64) DEFAULT NULL,
  host_ip varchar(16) DEFAULT NULL,
  host_id varchar(16) DEFAULT NULL,
  ins_state varchar(16) DEFAULT NULL,
  cpu integer DEFAULT 0,
  mem integer DEFAULT 0,
  cpu_type varchar(16) DEFAULT 'INTEL',
  zone varchar(32) DEFAULT NULL,
  region varchar(64) DEFAULT NULL,
  image_id varchar(128) DEFAULT NULL,
  system_disk varchar(64) DEFAULT NULL,
  private_ip_addresses varchar(128) NOT NULL DEFAULT '',
  private_ip_cnt smallint DEFAULT 0,
  private_ip varchar(16) DEFAULT NULL,
  mac_address varchar(16) DEFAULT NULL,
  data_disks varchar(1800) DEFAULT NULL,
  security_ids varchar(64) DEFAULT NULL,
  vpc_id varchar(64) NOT NULL DEFAULT '',
  subnet_id varchar(64) NOT NULL DEFAULT '',
  disk_state varchar(64) DEFAULT NULL,
  fail_msg text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instance_info_ins_id ON t_cube_instance_info (ins_id);
CREATE INDEX IF NOT EXISTS idx_instance_info_vpc_id ON t_cube_instance_info (vpc_id);
CREATE INDEX IF NOT EXISTS idx_instance_info_private_ip_addresses ON t_cube_instance_info (private_ip_addresses);
CREATE INDEX IF NOT EXISTS idx_instance_info_private_ip ON t_cube_instance_info (private_ip);
CREATE INDEX IF NOT EXISTS idx_instance_info_uuid ON t_cube_instance_info (uuid);
CREATE INDEX IF NOT EXISTS idx_instance_info_private_ip_cnt ON t_cube_instance_info (private_ip_cnt);
CREATE INDEX IF NOT EXISTS idx_instance_info_describe ON t_cube_instance_info (private_ip, private_ip_cnt);
CREATE INDEX IF NOT EXISTS idx_instance_info_ins_state ON t_cube_instance_info (ins_state);

CREATE TABLE IF NOT EXISTS t_cube_instance_userdata (
  id bigserial NOT NULL,
  ins_id varchar(64) NOT NULL,
  user_data text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instance_userdata_ins_id ON t_cube_instance_userdata (ins_id);

-- ---------------------------------------------------------------------
-- nodemeta tables
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS t_cube_node_registration (
  id bigserial NOT NULL,
  node_id varchar(128) NOT NULL,
  host_ip varchar(64) NOT NULL DEFAULT '',
  grpc_port integer NOT NULL DEFAULT 0,
  labels_json text,
  capacity_json text,
  allocatable_json text,
  instance_type varchar(64) NOT NULL DEFAULT '',
  cluster_label varchar(128) NOT NULL DEFAULT '',
  quota_cpu bigint NOT NULL DEFAULT 0,
  quota_mem_mb bigint NOT NULL DEFAULT 0,
  create_concurrent_num bigint NOT NULL DEFAULT 0,
  max_mvm_num bigint NOT NULL DEFAULT 0,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_node_registration_node_id ON t_cube_node_registration (node_id);
CREATE INDEX IF NOT EXISTS idx_node_registration_host_ip ON t_cube_node_registration (host_ip);

CREATE TABLE IF NOT EXISTS t_cube_node_status (
  id bigserial NOT NULL,
  node_id varchar(128) NOT NULL,
  conditions_json text,
  images_json text,
  local_templates_json text,
  heartbeat_unix bigint NOT NULL DEFAULT 0,
  healthy boolean NOT NULL DEFAULT false,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_node_status_node_id ON t_cube_node_status (node_id);
CREATE INDEX IF NOT EXISTS idx_node_status_heartbeat ON t_cube_node_status (heartbeat_unix);
CREATE INDEX IF NOT EXISTS idx_node_status_healthy ON t_cube_node_status (healthy);

-- ---------------------------------------------------------------------
-- templatecenter tables (v0.2.2 schema)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS t_cube_template_definition (
  id bigserial NOT NULL,
  template_id varchar(128) NOT NULL,
  instance_type varchar(64) NOT NULL DEFAULT '',
  version varchar(32) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  request_json text NOT NULL DEFAULT '',
  last_error text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_template_definition_template_id ON t_cube_template_definition (template_id);
CREATE INDEX IF NOT EXISTS idx_template_definition_status ON t_cube_template_definition (status);

CREATE TABLE IF NOT EXISTS t_cube_template_replica (
  id bigserial NOT NULL,
  template_id varchar(128) NOT NULL,
  node_id varchar(128) NOT NULL,
  node_ip varchar(64) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT '',
  spec varchar(128) NOT NULL DEFAULT '',
  snapshot_path varchar(1024) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  phase varchar(32) NOT NULL DEFAULT '',
  artifact_id varchar(128) NOT NULL DEFAULT '',
  last_job_id varchar(128) NOT NULL DEFAULT '',
  last_error_phase varchar(64) NOT NULL DEFAULT '',
  cleanup_required boolean NOT NULL DEFAULT false,
  error_message text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_template_replica_template_node ON t_cube_template_replica (template_id, node_id);
CREATE INDEX IF NOT EXISTS idx_template_replica_template_status ON t_cube_template_replica (template_id, status);

CREATE TABLE IF NOT EXISTS t_cube_rootfs_artifact (
  id bigserial NOT NULL,
  artifact_id varchar(128) NOT NULL,
  template_spec_fingerprint varchar(128) NOT NULL DEFAULT '',
  source_image_ref varchar(1024) NOT NULL DEFAULT '',
  source_image_digest varchar(256) NOT NULL DEFAULT '',
  master_node_id varchar(128) NOT NULL DEFAULT '',
  master_node_ip varchar(256) NOT NULL DEFAULT '',
  ext4_path varchar(2048) NOT NULL DEFAULT '',
  ext4_sha256 varchar(128) NOT NULL DEFAULT '',
  ext4_size_bytes bigint NOT NULL DEFAULT 0,
  image_config_json text,
  generated_request_json text,
  writable_layer_size varchar(64) NOT NULL DEFAULT '',
  download_token varchar(256) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  last_error text,
  gc_deadline bigint NOT NULL DEFAULT 0,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rootfs_artifact_artifact_id ON t_cube_rootfs_artifact (artifact_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rootfs_artifact_fingerprint ON t_cube_rootfs_artifact (template_spec_fingerprint);
CREATE INDEX IF NOT EXISTS idx_rootfs_artifact_status ON t_cube_rootfs_artifact (status);

CREATE TABLE IF NOT EXISTS t_cube_template_image_job (
  id bigserial NOT NULL,
  job_id varchar(128) NOT NULL,
  template_id varchar(128) NOT NULL DEFAULT '',
  request_id varchar(128) NOT NULL DEFAULT '',
  attempt_no integer NOT NULL DEFAULT 1,
  retry_of_job_id varchar(128) NOT NULL DEFAULT '',
  operation varchar(32) NOT NULL DEFAULT '',
  redo_mode varchar(32) NOT NULL DEFAULT '',
  redo_scope_json text,
  resume_phase varchar(64) NOT NULL DEFAULT '',
  node_id varchar(128) NOT NULL DEFAULT '',
  node_ip varchar(256) NOT NULL DEFAULT '',
  snapshot_path varchar(1024) NOT NULL DEFAULT '',
  artifact_id varchar(128) NOT NULL DEFAULT '',
  template_spec_fingerprint varchar(128) NOT NULL DEFAULT '',
  source_image_ref varchar(1024) NOT NULL DEFAULT '',
  source_image_digest varchar(256) NOT NULL DEFAULT '',
  writable_layer_size varchar(64) NOT NULL DEFAULT '',
  instance_type varchar(64) NOT NULL DEFAULT '',
  network_type varchar(64) NOT NULL DEFAULT '',
  status varchar(32) NOT NULL DEFAULT '',
  phase varchar(64) NOT NULL DEFAULT '',
  progress integer NOT NULL DEFAULT 0,
  error_message text,
  expected_node_count integer NOT NULL DEFAULT 0,
  ready_node_count integer NOT NULL DEFAULT 0,
  failed_node_count integer NOT NULL DEFAULT 0,
  template_status varchar(32) NOT NULL DEFAULT '',
  artifact_status varchar(32) NOT NULL DEFAULT '',
  request_json text,
  result_json text,
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at timestamp DEFAULT NULL,
  PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_template_image_job_id ON t_cube_template_image_job (job_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_template_image_template_attempt ON t_cube_template_image_job (template_id, attempt_no);
CREATE INDEX IF NOT EXISTS idx_template_image_request_id ON t_cube_template_image_job (request_id);
CREATE INDEX IF NOT EXISTS idx_template_image_status ON t_cube_template_image_job (status);
CREATE INDEX IF NOT EXISTS idx_template_image_template_status ON t_cube_template_image_job (template_id, status);

-- Release the inner per-file lock.
SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0001_baseline_v0_2_2'));

-- +goose Down
-- The baseline is irreversible: it represents the v0.2.2 ground truth.
DO $$ BEGIN
  RAISE EXCEPTION 'cubemaster baseline migration (0001) is not reversible; restore from backup';
END $$;
