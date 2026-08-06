// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"testing"
)

// TestDaoConfig_DatabaseURLWins proves R06: when DatabaseURL is set,
// DaoConfig() parses it and produces a dao.Config whose User/Pwd/Addr/DBName
// come ENTIRELY from the URL — the individual MySQL* fields are ignored even
// if they are empty or hold conflicting values.
//
// Before the R06 fix, DaoConfig() always used the MySQL* fields, so a config
// that only set database_url (common with DATABASE_URL env var) would connect
// with empty user/db or fall back to localhost — the URL was "accepted" by
// Load() but silently ignored at connection time.
func TestDaoConfig_DatabaseURLWins(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "mysql://alice:s3cret@10.0.0.5:3307/mydb",
		// MySQL* fields deliberately left empty — simulates a deployment
		// that only sets DATABASE_URL (the recommended single-source form).
	}

	dc := cfg.DaoConfig()

	if dc.Driver != "mysql" {
		t.Errorf("Driver = %q, want mysql", dc.Driver)
	}
	if dc.User != "alice" {
		t.Errorf("User = %q, want alice (from URL)", dc.User)
	}
	if dc.Pwd != "s3cret" {
		t.Errorf("Pwd = %q, want s3cret (from URL)", dc.Pwd)
	}
	if dc.Addr != "10.0.0.5:3307" {
		t.Errorf("Addr = %q, want 10.0.0.5:3307 (from URL)", dc.Addr)
	}
	if dc.DBName != "mydb" {
		t.Errorf("DBName = %q, want mydb (from URL)", dc.DBName)
	}
}

// TestDaoConfig_DatabaseURLWinsOverConflictingMySQLFields proves the URL wins
// EVEN WHEN the MySQL* fields are populated with different values. This is the
// exact R06 regression scenario: both forms set, URL must take full precedence
// with no field mixing.
func TestDaoConfig_DatabaseURLWinsOverConflictingMySQLFields(t *testing.T) {
	cfg := &Config{
		DatabaseURL:   "mysql://alice:s3cret@10.0.0.5:3307/mydb",
		MySQLHost:     "wrong-host",
		MySQLPort:     9999,
		MySQLUser:     "wrong-user",
		MySQLPassword: "wrong-pass",
		MySQLDB:       "wrong-db",
	}

	dc := cfg.DaoConfig()

	if dc.User != "alice" {
		t.Errorf("User = %q, want alice (URL must win over MySQLUser)", dc.User)
	}
	if dc.Pwd != "s3cret" {
		t.Errorf("Pwd = %q, want s3cret (URL must win over MySQLPassword)", dc.Pwd)
	}
	if dc.Addr != "10.0.0.5:3307" {
		t.Errorf("Addr = %q, want 10.0.0.5:3307 (URL must win over MySQLHost/Port)", dc.Addr)
	}
	if dc.DBName != "mydb" {
		t.Errorf("DBName = %q, want mydb (URL must win over MySQLDB)", dc.DBName)
	}
}

// TestDaoConfig_FallsBackToMySQLFields proves that when DatabaseURL is NOT set,
// the individual MySQL* fields are used (backward compatibility with
// deployments that configure via CUBE_SANDBOX_MYSQL_* env vars).
func TestDaoConfig_FallsBackToMySQLFields(t *testing.T) {
	cfg := &Config{
		MySQLHost:     "db.internal",
		MySQLPort:     3306,
		MySQLUser:     "svc",
		MySQLPassword: "svcpass",
		MySQLDB:       "svcdb",
	}

	dc := cfg.DaoConfig()

	if dc.User != "svc" {
		t.Errorf("User = %q, want svc", dc.User)
	}
	if dc.Pwd != "svcpass" {
		t.Errorf("Pwd = %q, want svcpass", dc.Pwd)
	}
	if dc.Addr != "db.internal:3306" {
		t.Errorf("Addr = %q, want db.internal:3306", dc.Addr)
	}
	if dc.DBName != "svcdb" {
		t.Errorf("DBName = %q, want svcdb", dc.DBName)
	}
}

// TestDaoConfig_DefaultPort proves that a URL without an explicit port
// defaults to 3306 — a common omission in DATABASE_URL strings.
func TestDaoConfig_DefaultPort(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "mysql://alice:s3cret@db.internal/mydb",
	}

	dc := cfg.DaoConfig()

	if dc.Addr != "db.internal:3306" {
		t.Errorf("Addr = %q, want db.internal:3306 (default port)", dc.Addr)
	}
}

// TestDaoConfig_PostgresSSLModeForwarded proves the sslmode (and other query
// params) from a postgres:// DATABASE_URL reach dao.Config.Extra, which the
// postgres driver reads to build the DSN. Without forwarding, CubeOps would
// silently connect with sslmode=disable and fail against a TLS-enforcing
// server — the exact gap raised in PR review (CubeMaster reads extra.sslmode
// from conf.yaml and succeeds, so only CubeOps breaks).
func TestDaoConfig_PostgresSSLModeForwarded(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "postgres://alice:s3cret@10.0.0.5:5432/mydb?sslmode=require",
	}

	dc := cfg.DaoConfig()

	if dc.Driver != "postgres" {
		t.Errorf("Driver = %q, want postgres", dc.Driver)
	}
	if got := dc.Extra["sslmode"]; got != "require" {
		t.Errorf("Extra[sslmode] = %q, want require (forwarded from URL query)", got)
	}
}

// TestDaoConfig_PostgresMultipleQueryParamsForwarded proves that all query
// params (not just sslmode) are forwarded, so the postgres driver's
// statement_timeout / idle_in_transaction_session_timeout overrides also work.
func TestDaoConfig_PostgresMultipleQueryParamsForwarded(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "postgres://alice:s3cret@10.0.0.5:5432/mydb?sslmode=verify-full&statement_timeout=3000",
	}

	dc := cfg.DaoConfig()

	if got := dc.Extra["sslmode"]; got != "verify-full" {
		t.Errorf("Extra[sslmode] = %q, want verify-full", got)
	}
	if got := dc.Extra["statement_timeout"]; got != "3000" {
		t.Errorf("Extra[statement_timeout] = %q, want 3000", got)
	}
}

// TestDaoConfig_NoQueryParamsYieldsNilExtra proves that a URL without a query
// string produces a nil Extra (not an empty non-nil map), so mysql and query-
// less postgres configs behave exactly as before this change.
func TestDaoConfig_NoQueryParamsYieldsNilExtra(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "mysql://alice:s3cret@10.0.0.5:3307/mydb",
	}

	dc := cfg.DaoConfig()

	if dc.Extra != nil {
		t.Errorf("Extra = %v, want nil (no query string in URL)", dc.Extra)
	}
}

// TestDaoConfig_FullLoadToDaoConfig proves the end-to-end data flow that R06
// is about: Load() accepts a YAML with only database_url, and DaoConfig()
// correctly translates it — no field is lost between config loading and the
// dao.Config handed to store.New().
func TestDaoConfig_FullLoadToDaoConfig(t *testing.T) {
	dir := t.TempDir()
	yamlPath := dir + "/ops.yaml"
	yamlContent := []byte(`database_url: "mysql://loader:loaderpass@192.168.1.10:3306/loaderdb"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dc := cfg.DaoConfig()

	if dc.User != "loader" {
		t.Errorf("User = %q, want loader", dc.User)
	}
	if dc.Pwd != "loaderpass" {
		t.Errorf("Pwd = %q, want loaderpass", dc.Pwd)
	}
	if dc.Addr != "192.168.1.10:3306" {
		t.Errorf("Addr = %q, want 192.168.1.10:3306", dc.Addr)
	}
	if dc.DBName != "loaderdb" {
		t.Errorf("DBName = %q, want loaderdb", dc.DBName)
	}
}
