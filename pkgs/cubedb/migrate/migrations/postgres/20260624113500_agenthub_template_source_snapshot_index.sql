-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub template reverse lookup by source snapshot (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx', 60);

CREATE INDEX IF NOT EXISTS idx_agenthub_template_source_snapshot ON t_agenthub_template (source_snapshot_id, deleted_at, template_id);

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx', 60);

DROP INDEX IF EXISTS idx_agenthub_template_source_snapshot;

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_20260624113500_agenthub_tpl_srcsnap_idx'));
