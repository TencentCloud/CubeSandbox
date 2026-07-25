// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
)

// TestS6_PostgresURLSelectsPostgresDriver verifies that a postgres:// URL
// produces Driver="postgres" instead of silently falling back to MySQL.
//
// Before the S6 fix, DaoConfig hardcoded Driver="mysql" regardless of the
// URL scheme, so a PostgreSQL deployment would connect with the wrong driver
// and fail on dialect-specific SQL (INSERT IGNORE, ON DUPLICATE KEY,
// DATE_FORMAT).
//
// See review S6.
func TestS6_PostgresURLSelectsPostgresDriver(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "postgres://alice:s3cret@10.0.0.5:5432/mydb",
	}

	dc := cfg.DaoConfig()

	if dc.Driver != "postgres" {
		t.Errorf("Driver = %q, want postgres (S6: URL scheme must select driver)", dc.Driver)
	}
	if dc.User != "alice" {
		t.Errorf("User = %q, want alice", dc.User)
	}
	if dc.Addr != "10.0.0.5:5432" {
		t.Errorf("Addr = %q, want 10.0.0.5:5432", dc.Addr)
	}
	if dc.DBName != "mydb" {
		t.Errorf("DBName = %q, want mydb", dc.DBName)
	}
}

// TestS6_PostgresqlURLAlsoWorks verifies that "postgresql://" (the long
// form) is also recognized as the postgres driver.
func TestS6_PostgresqlURLAlsoWorks(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "postgresql://user:pass@host:5432/db",
	}

	dc := cfg.DaoConfig()

	if dc.Driver != "postgres" {
		t.Errorf("Driver = %q, want postgres for postgresql:// scheme (S6)", dc.Driver)
	}
}

// TestS6_PostgresDefaultPort verifies that a postgres:// URL without an
// explicit port defaults to 5432 (PG default), not 3306 (MySQL default).
func TestS6_PostgresDefaultPort(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "postgres://user:pass@host/db",
	}

	dc := cfg.DaoConfig()

	if dc.Addr != "host:5432" {
		t.Errorf("Addr = %q, want host:5432 (PG default port, S6)", dc.Addr)
	}
}

// TestS6_MySQLURLStillSelectsMySQLDriver is a regression guard: the S6 fix
// must not break the existing MySQL path.
func TestS6_MySQLURLStillSelectsMySQLDriver(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "mysql://user:pass@host:3306/db",
	}

	dc := cfg.DaoConfig()

	if dc.Driver != "mysql" {
		t.Errorf("Driver = %q, want mysql (S6 regression: MySQL must still work)", dc.Driver)
	}
}
