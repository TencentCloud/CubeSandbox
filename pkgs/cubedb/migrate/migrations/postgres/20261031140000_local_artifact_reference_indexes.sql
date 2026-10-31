-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_template_job_artifact_live
 ON t_cube_template_image_job (artifact_id, status, deleted_at);
CREATE INDEX IF NOT EXISTS idx_template_replica_artifact_live
 ON t_cube_template_replica (artifact_id, deleted_at);

-- +goose Down
DROP INDEX IF EXISTS idx_template_replica_artifact_live;
DROP INDEX IF EXISTS idx_template_job_artifact_live;
