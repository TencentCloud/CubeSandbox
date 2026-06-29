-- Copyright (c) 2026 Tencent Inc.
-- SPDX-License-Identifier: Apache-2.0
--
-- AgentHub template persistence mode (PostgreSQL).

-- +goose NO TRANSACTION
-- +goose Up

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0008_agenthub_template_persistence_mode', 60);

SELECT cubemaster_add_column_if_missing('t_agenthub_instance', 'persistence_mode', 'varchar(32) DEFAULT NULL');
SELECT cubemaster_add_column_if_missing('t_agenthub_template', 'persistence_mode', 'varchar(32) DEFAULT NULL');

-- Backfill template persistence_mode from instance.
UPDATE t_agenthub_template t
   SET persistence_mode = i.persistence_mode
  FROM t_agenthub_instance i
 WHERE i.agent_id = t.source_agent_id
   AND i.deleted_at IS NULL
   AND t.persistence_mode IS NULL
   AND t.source_agent_id <> 'market';

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0008_agenthub_template_persistence_mode'));

-- +goose Down

SELECT cubemaster_acquire_migration_lock('cubemaster_migration_0008_agenthub_template_persistence_mode', 60);

SELECT cubemaster_drop_column_if_exists('t_agenthub_template', 'persistence_mode');
SELECT cubemaster_drop_column_if_exists('t_agenthub_instance', 'persistence_mode');

SELECT pg_advisory_unlock(hashtext('cubemaster_migration_0008_agenthub_template_persistence_mode'));
