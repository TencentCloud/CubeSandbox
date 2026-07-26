// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
)

// testDialectForced allows tests to force a specific dialect without opening
// a real database connection. Set to "postgres" in tests to exercise the PG
// branches of all dialect functions. Production code must never touch this.
var testDialectForced string

// IsPostgres reports whether the underlying database driver is PostgreSQL.
// Returns false for MySQL (the default) and for the not-yet-opened case.
func IsPostgres() bool {
	if testDialectForced != "" {
		return strings.EqualFold(testDialectForced, "postgres")
	}
	return strings.EqualFold(dao.DriverName(), "postgres")
}

// insertIgnore returns a clause that makes an INSERT silently skip rows
// that would violate a unique constraint:
//   - MySQL:      "INSERT IGNORE"
//   - PostgreSQL: "INSERT" (caller must append "ON CONFLICT DO NOTHING"
//     via onConflictDoNothing)
//
// Use insertIgnorePrefix + onConflictSuffix together.
func insertIgnorePrefix() string {
	if IsPostgres() {
		return "INSERT"
	}
	return "INSERT IGNORE"
}

// onConflictDoNothing returns the PostgreSQL "ON CONFLICT DO NOTHING" clause
// (with a leading space), or "" for MySQL where INSERT IGNORE already
// provides the semantics.
func onConflictDoNothing() string {
	if IsPostgres() {
		return " ON CONFLICT DO NOTHING"
	}
	return ""
}

// upsertSettingSQL returns the dialect-correct UPSERT SQL for a (key, value)
// settings table. MySQL uses ON DUPLICATE KEY UPDATE; PostgreSQL uses
// ON CONFLICT (setting_key) DO UPDATE.
func upsertSettingSQL(table string) string {
	if IsPostgres() {
		return "INSERT INTO " + table + " (setting_key, setting_value) VALUES (?, ?)" +
			" ON CONFLICT (setting_key) DO UPDATE SET setting_value = EXCLUDED.setting_value"
	}
	return "INSERT INTO " + table + " (setting_key, setting_value) VALUES (?, ?)" +
		" ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value)"
}

// upsertInstanceSQL returns the dialect-correct UPSERT SQL for
// t_agenthub_instance. The ON CONFLICT key is agent_id.
func upsertInstanceSQL() string {
	if IsPostgres() {
		return `INSERT INTO t_agenthub_instance (
			agent_id, sandbox_id, template_id, name, engine, env, model, version, status,
			bots, avatar, avatar_tone, domain, gateway_port, env_port, gateway_token,
			persistence_mode, rootfs_source_type, rootfs_source_id,
			openclaw_persist_id, openclaw_state_path,
			wecom_bot_id, wecom_bot_secret,
			last_error, setup_exit_code, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (agent_id) DO UPDATE SET
			sandbox_id = EXCLUDED.sandbox_id, template_id = EXCLUDED.template_id,
			name = EXCLUDED.name, engine = EXCLUDED.engine, env = EXCLUDED.env,
			model = EXCLUDED.model, version = EXCLUDED.version, status = EXCLUDED.status,
			bots = EXCLUDED.bots, avatar = EXCLUDED.avatar, avatar_tone = EXCLUDED.avatar_tone,
			domain = EXCLUDED.domain, gateway_token = EXCLUDED.gateway_token,
			persistence_mode = EXCLUDED.persistence_mode,
			rootfs_source_type = EXCLUDED.rootfs_source_type,
			rootfs_source_id = EXCLUDED.rootfs_source_id,
			openclaw_persist_id = EXCLUDED.openclaw_persist_id,
			openclaw_state_path = EXCLUDED.openclaw_state_path,
			wecom_bot_id = EXCLUDED.wecom_bot_id,
			wecom_bot_secret = EXCLUDED.wecom_bot_secret,
			last_error = EXCLUDED.last_error,
			setup_exit_code = EXCLUDED.setup_exit_code,
			deleted_at = NULL`
	}
	return `INSERT INTO t_agenthub_instance (
			agent_id, sandbox_id, template_id, name, engine, env, model, version, status,
			bots, avatar, avatar_tone, domain, gateway_port, env_port, gateway_token,
			persistence_mode, rootfs_source_type, rootfs_source_id,
			openclaw_persist_id, openclaw_state_path,
			wecom_bot_id, wecom_bot_secret,
			last_error, setup_exit_code, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			sandbox_id = VALUES(sandbox_id), template_id = VALUES(template_id),
			name = VALUES(name), engine = VALUES(engine), env = VALUES(env),
			model = VALUES(model), version = VALUES(version), status = VALUES(status),
			bots = VALUES(bots), avatar = VALUES(avatar), avatar_tone = VALUES(avatar_tone),
			domain = VALUES(domain), gateway_token = VALUES(gateway_token),
			persistence_mode = VALUES(persistence_mode),
			rootfs_source_type = VALUES(rootfs_source_type),
			rootfs_source_id = VALUES(rootfs_source_id),
			openclaw_persist_id = VALUES(openclaw_persist_id),
			openclaw_state_path = VALUES(openclaw_state_path),
			wecom_bot_id = VALUES(wecom_bot_id),
			wecom_bot_secret = VALUES(wecom_bot_secret),
			last_error = VALUES(last_error),
			setup_exit_code = VALUES(setup_exit_code),
			deleted_at = NULL`
}

// UpsertSnapshotSQL returns the dialect-correct UPSERT SQL for
// t_agenthub_snapshot. The ON CONFLICT key is snapshot_id.
// cols and placeholders parameterise the column list. updateColsPG is the
// PostgreSQL "EXCLUDED.col" form; updateColsMySQL is the MySQL
// "VALUES(col)" form. The function picks the right one per driver.
func UpsertSnapshotSQL(cols, placeholders, updateColsPG, updateColsMySQL string) string {
	updateCols := updateColsMySQL
	if IsPostgres() {
		updateCols = updateColsPG
	}
	if IsPostgres() {
		return "INSERT INTO t_agenthub_snapshot (" + cols + ", deleted_at) VALUES (" + placeholders + ", NULL)" +
			" ON CONFLICT (snapshot_id) DO UPDATE SET " + updateCols + ", deleted_at = NULL"
	}
	return "INSERT INTO t_agenthub_snapshot (" + cols + ", deleted_at) VALUES (" + placeholders + ", NULL)" +
		" ON DUPLICATE KEY UPDATE " + updateCols + ", deleted_at = NULL"
}

// UpsertTemplateSQL returns the dialect-correct UPSERT SQL for
// t_agenthub_template. The ON CONFLICT key is template_id.
func UpsertTemplateSQL() string {
	if IsPostgres() {
		return `INSERT INTO t_agenthub_template (
			  template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
			  model, version, persistence_mode, recommended, deleted_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, false, NULL)
			ON CONFLICT (template_id) DO UPDATE SET
			  name = EXCLUDED.name, source_agent_id = EXCLUDED.source_agent_id,
			  source_snapshot_id = EXCLUDED.source_snapshot_id, source_sandbox_id = EXCLUDED.source_sandbox_id,
			  model = EXCLUDED.model, version = EXCLUDED.version,
			  persistence_mode = EXCLUDED.persistence_mode, deleted_at = NULL`
	}
	return `INSERT INTO t_agenthub_template (
			  template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
			  model, version, persistence_mode, recommended, deleted_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)
			ON DUPLICATE KEY UPDATE
			  name = VALUES(name), source_agent_id = VALUES(source_agent_id),
			  source_snapshot_id = VALUES(source_snapshot_id), source_sandbox_id = VALUES(source_sandbox_id),
			  model = VALUES(model), version = VALUES(version),
			  persistence_mode = VALUES(persistence_mode), deleted_at = NULL`
}

// formatTimestamp returns the dialect-correct expression for formatting a
// timestamp column as an ISO-8601 string. MySQL uses DATE_FORMAT; PostgreSQL
// uses to_char with the equivalent pattern.
func formatTimestamp(col string) string {
	if IsPostgres() {
		return "to_char(" + col + " AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
	}
	return "DATE_FORMAT(" + col + ", '%Y-%m-%dT%H:%i:%sZ')"
}
