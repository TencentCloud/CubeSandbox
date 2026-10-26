// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// TestS6_Dialect_PostgresInsertIgnore verifies that in PostgreSQL mode, the
// dialect helpers produce INSERT + ON CONFLICT DO NOTHING (not INSERT IGNORE).
func TestS6_Dialect_PostgresInsertIgnore(t *testing.T) {
	testDialectForced = "postgres"
	defer func() { testDialectForced = "" }()

	// PG uses plain INSERT (no IGNORE keyword)
	prefix := insertIgnorePrefix()
	if prefix != "INSERT" {
		t.Errorf("insertIgnorePrefix() = %q, want %q (PG)", prefix, "INSERT")
	}
	if strings.Contains(prefix, "IGNORE") {
		t.Errorf("insertIgnorePrefix() must NOT contain IGNORE in PG mode: %s", prefix)
	}

	// PG uses ON CONFLICT DO NOTHING suffix
	suffix := onConflictDoNothing()
	if !strings.Contains(suffix, "ON CONFLICT DO NOTHING") {
		t.Errorf("onConflictDoNothing() = %q, want ON CONFLICT DO NOTHING (PG)", suffix)
	}

	// UPSERT uses ON CONFLICT ... DO UPDATE SET EXCLUDED.col
	sql := upsertSettingSQL("t_test")
	if !strings.Contains(sql, "ON CONFLICT (setting_key) DO UPDATE") {
		t.Errorf("PG upsert SQL missing ON CONFLICT ... DO UPDATE: %s", sql)
	}
	if !strings.Contains(sql, "EXCLUDED.setting_value") {
		t.Errorf("PG upsert SQL missing EXCLUDED.setting_value: %s", sql)
	}
	if strings.Contains(sql, "ON DUPLICATE KEY") {
		t.Errorf("PG upsert SQL should NOT contain ON DUPLICATE KEY: %s", sql)
	}

	// to_char for PG
	ts := formatTimestamp("created_at")
	if !strings.Contains(ts, "to_char") {
		t.Errorf("PG formatTimestamp missing to_char: %s", ts)
	}
	if !strings.Contains(ts, "AT TIME ZONE 'UTC'") {
		t.Errorf("PG formatTimestamp missing AT TIME ZONE: %s", ts)
	}
	if strings.Contains(ts, "DATE_FORMAT") {
		t.Errorf("PG formatTimestamp should NOT use DATE_FORMAT: %s", ts)
	}
}

// TestS6_Dialect_UpsertInstancePostgres verifies the instance upsert SQL
// uses PG's ON CONFLICT ... EXCLUDED.col syntax.
func TestS6_Dialect_UpsertInstancePostgres(t *testing.T) {
	testDialectForced = "postgres"
	defer func() { testDialectForced = "" }()

	sql := upsertInstanceSQL()
	if !strings.Contains(sql, "ON CONFLICT (agent_id) DO UPDATE") {
		t.Errorf("PG upsertInstanceSQL missing ON CONFLICT: %s", sql)
	}
	if !strings.Contains(sql, "EXCLUDED.sandbox_id") {
		t.Errorf("PG upsertInstanceSQL missing EXCLUDED.col: %s", sql)
	}
	if strings.Contains(sql, "ON DUPLICATE KEY") {
		t.Errorf("PG upsertInstanceSQL should NOT use ON DUPLICATE KEY: %s", sql)
	}
	if strings.Contains(sql, "VALUES(sandbox_id)") {
		t.Errorf("PG upsertInstanceSQL should NOT use VALUES(col): %s", sql)
	}
}

// TestS6_Dialect_UpsertSnapshotPostgres verifies snapshot upsert SQL for PG.
func TestS6_Dialect_UpsertSnapshotPostgres(t *testing.T) {
	testDialectForced = "postgres"
	defer func() { testDialectForced = "" }()

	sql := UpsertSnapshotSQL(
		"snapshot_id, agent_id, name",
		"?, ?, ?",
		"agent_id = EXCLUDED.agent_id", // PG form
		"agent_id = VALUES(agent_id)",  // MySQL form (should be ignored in PG mode)
	)
	if !strings.Contains(sql, "ON CONFLICT (snapshot_id) DO UPDATE") {
		t.Errorf("PG UpsertSnapshotSQL missing ON CONFLICT: %s", sql)
	}
	if !strings.Contains(sql, "EXCLUDED.agent_id") {
		t.Errorf("PG UpsertSnapshotSQL missing EXCLUDED.col: %s", sql)
	}
	if strings.Contains(sql, "VALUES(agent_id)") {
		t.Errorf("PG UpsertSnapshotSQL should NOT use VALUES(col): %s", sql)
	}
	if strings.Contains(sql, "ON DUPLICATE KEY") {
		t.Errorf("PG UpsertSnapshotSQL should NOT use ON DUPLICATE KEY: %s", sql)
	}
}

// TestS6_Dialect_UpsertTemplatePostgres verifies template upsert SQL for PG.
func TestS6_Dialect_UpsertTemplatePostgres(t *testing.T) {
	testDialectForced = "postgres"
	defer func() { testDialectForced = "" }()

	sql := UpsertTemplateSQL()
	if !strings.Contains(sql, "ON CONFLICT (template_id) DO UPDATE") {
		t.Errorf("PG UpsertTemplateSQL missing ON CONFLICT: %s", sql)
	}
	if strings.Contains(sql, "ON DUPLICATE KEY") {
		t.Errorf("PG UpsertTemplateSQL should NOT use ON DUPLICATE KEY: %s", sql)
	}
	// PG uses false (not 0) for boolean recommended
	if !strings.Contains(sql, "false") {
		t.Errorf("PG UpsertTemplateSQL missing boolean literal 'false': %s", sql)
	}
	if strings.Contains(sql, ", 0,") {
		t.Errorf("PG UpsertTemplateSQL should use false not 0: %s", sql)
	}
}

// TestS6_Dialect_ModeIsolation verifies that forcing postgres only affects
// the current test, not others (no state leak).
func TestS6_Dialect_ModeIsolation(t *testing.T) {
	// Without forcing, defaults to MySQL (dao not opened)
	if IsPostgres() {
		t.Fatal("IsPostgres() should return false when testDialectForced is empty")
	}
	if insertIgnorePrefix() != "INSERT IGNORE" {
		t.Error("default mode should be MySQL")
	}
}
