// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// TestS6_Dialect_MySQLInsertIgnore verifies that in MySQL mode, the dialect
// helpers produce INSERT IGNORE and ON DUPLICATE KEY UPDATE syntax.
//
// This test runs without a database connection — it verifies the SQL string
// generation only. The driver defaults to MySQL when dao is not opened.
//
// See review S6.
func TestS6_Dialect_MySQLInsertIgnore(t *testing.T) {
	// Without dao.Open(), DriverName() returns "" → IsPostgres() = false
	// → MySQL dialect is selected.
	if IsPostgres() {
		t.Skip("dao already opened as postgres — skipping MySQL dialect test")
	}

	// INSERT IGNORE prefix
	prefix := insertIgnorePrefix()
	if prefix != "INSERT IGNORE" {
		t.Errorf("insertIgnorePrefix() = %q, want %q (MySQL)", prefix, "INSERT IGNORE")
	}

	// ON CONFLICT DO NOTHING suffix should be empty (MySQL uses INSERT IGNORE)
	suffix := onConflictDoNothing()
	if suffix != "" {
		t.Errorf("onConflictDoNothing() = %q, want %q (MySQL uses INSERT IGNORE, no suffix)", suffix, "")
	}

	// UPSERT uses ON DUPLICATE KEY UPDATE
	sql := upsertSettingSQL("t_test")
	if !strings.Contains(sql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("MySQL upsert SQL missing ON DUPLICATE KEY UPDATE: %s", sql)
	}
	if strings.Contains(sql, "ON CONFLICT") {
		t.Errorf("MySQL upsert SQL should NOT contain ON CONFLICT: %s", sql)
	}

	// DATE_FORMAT for MySQL
	ts := formatTimestamp("created_at")
	if !strings.Contains(ts, "DATE_FORMAT") {
		t.Errorf("MySQL formatTimestamp missing DATE_FORMAT: %s", ts)
	}
	if strings.Contains(ts, "to_char") {
		t.Errorf("MySQL formatTimestamp should NOT use to_char: %s", ts)
	}
}

// TestS6_Dialect_UpsertInstanceMySQL verifies the instance upsert SQL uses
// MySQL's VALUES(col) syntax in the ON DUPLICATE KEY UPDATE clause.
func TestS6_Dialect_UpsertInstanceMySQL(t *testing.T) {
	if IsPostgres() {
		t.Skip("dao already opened as postgres — skipping MySQL dialect test")
	}

	sql := upsertInstanceSQL()
	if !strings.Contains(sql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("MySQL upsertInstanceSQL missing ON DUPLICATE KEY UPDATE: %s", sql)
	}
	if !strings.Contains(sql, "VALUES(sandbox_id)") {
		t.Errorf("MySQL upsertInstanceSQL missing VALUES(col) syntax: %s", sql)
	}
	if strings.Contains(sql, "EXCLUDED.") {
		t.Errorf("MySQL upsertInstanceSQL should NOT use EXCLUDED.col: %s", sql)
	}
}

// TestS6_Dialect_UpsertSnapshotMySQL verifies snapshot upsert SQL for MySQL.
func TestS6_Dialect_UpsertSnapshotMySQL(t *testing.T) {
	if IsPostgres() {
		t.Skip("dao already opened as postgres — skipping MySQL dialect test")
	}

	sql := UpsertSnapshotSQL(
		"snapshot_id, agent_id, name",
		"?, ?, ?",
		"agent_id = EXCLUDED.agent_id", // PG form (ignored in MySQL mode)
		"agent_id = VALUES(agent_id)",  // MySQL form
	)
	if !strings.Contains(sql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("MySQL UpsertSnapshotSQL missing ON DUPLICATE KEY UPDATE: %s", sql)
	}
	if !strings.Contains(sql, "VALUES(agent_id)") {
		t.Errorf("MySQL UpsertSnapshotSQL should use VALUES(col): %s", sql)
	}
	if strings.Contains(sql, "EXCLUDED.") {
		t.Errorf("MySQL UpsertSnapshotSQL should NOT use EXCLUDED.col: %s", sql)
	}
}

// TestS6_Dialect_UpsertTemplateMySQL verifies template upsert SQL for MySQL.
func TestS6_Dialect_UpsertTemplateMySQL(t *testing.T) {
	if IsPostgres() {
		t.Skip("dao already opened as postgres — skipping MySQL dialect test")
	}

	sql := UpsertTemplateSQL()
	if !strings.Contains(sql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("MySQL UpsertTemplateSQL missing ON DUPLICATE KEY UPDATE: %s", sql)
	}
	if strings.Contains(sql, "ON CONFLICT") {
		t.Errorf("MySQL UpsertTemplateSQL should NOT use ON CONFLICT: %s", sql)
	}
}
