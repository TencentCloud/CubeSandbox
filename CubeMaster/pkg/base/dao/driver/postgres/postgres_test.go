// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
)

func TestSessionLockerUsesConfiguredTimeout(t *testing.T) {
	d := &driver{}
	locker, ok := d.SessionLocker(dao.Config{MigrationLockTimeoutSeconds: 7}).(*sessionLocker)
	if !ok {
		t.Fatalf("SessionLocker type = %T, want *sessionLocker", d.SessionLocker(dao.Config{}))
	}
	if locker.timeout != 7 {
		t.Fatalf("timeout = %d, want 7", locker.timeout)
	}
	if locker.id != advisoryLockID {
		t.Fatalf("id = %d, want %d", locker.id, advisoryLockID)
	}

	defaultLocker, ok := d.SessionLocker(dao.Config{}).(*sessionLocker)
	if !ok {
		t.Fatalf("SessionLocker type = %T, want *sessionLocker", d.SessionLocker(dao.Config{}))
	}
	if defaultLocker.timeout != defaultLockTimeoutSeconds {
		t.Fatalf("default timeout = %d, want %d", defaultLocker.timeout, defaultLockTimeoutSeconds)
	}
}

func TestBuildDSN(t *testing.T) {
	cfg := dao.Config{
		Addr:               "127.0.0.1:5432",
		User:               "cube",
		Pwd:                "cube_pass",
		DBName:             "cube_mvp",
		ConnTimeoutSeconds: 10,
	}
	got := buildDSN(cfg)
	want := "host=127.0.0.1 port=5432 user=cube password=cube_pass dbname=cube_mvp sslmode=disable connect_timeout=10"
	if got != want {
		t.Fatalf("buildDSN:\n  got:  %s\n  want: %s", got, want)
	}
	parsed, err := pgx.ParseConfig(got)
	if err != nil {
		t.Fatalf("pgx.ParseConfig: %v", err)
	}
	if parsed.Host != "127.0.0.1" {
		t.Fatalf("parsed Host = %q, want 127.0.0.1", parsed.Host)
	}
	if parsed.Port != 5432 {
		t.Fatalf("parsed Port = %d, want 5432", parsed.Port)
	}
}

func TestBuildDSNDefaultTimeout(t *testing.T) {
	cfg := dao.Config{
		Addr:   "localhost",
		User:   "u",
		Pwd:    "p",
		DBName: "db",
	}
	got := buildDSN(cfg)
	want := "host=localhost user=u password=p dbname=db sslmode=disable connect_timeout=5"
	if got != want {
		t.Fatalf("buildDSN default timeout:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestBuildDSNWithSSLMode(t *testing.T) {
	cfg := dao.Config{
		Addr:               "pg.example.com",
		User:               "u",
		Pwd:                "p",
		DBName:             "db",
		ConnTimeoutSeconds: 5,
		Extra:              map[string]string{"sslmode": "require"},
	}
	got := buildDSN(cfg)
	if !strings.Contains(got, "sslmode=require") {
		t.Fatalf("expected sslmode=require in DSN, got: %s", got)
	}
	if strings.Contains(got, "sslmode=disable") {
		t.Fatalf("should not contain sslmode=disable when Extra overrides it, got: %s", got)
	}
}

func TestBuildDSNWithTimeouts(t *testing.T) {
	cfg := dao.Config{
		Addr:                "localhost",
		User:                "u",
		Pwd:                 "p",
		DBName:              "db",
		ConnTimeoutSeconds:  5,
		ReadTimeoutSeconds:  30,
		WriteTimeoutSeconds: 10,
	}
	got := buildDSN(cfg)
	if !strings.Contains(got, "statement_timeout=10000") {
		t.Fatalf("expected statement_timeout=10000 in DSN options, got: %s", got)
	}
	if !strings.Contains(got, "idle_in_transaction_session_timeout=30000") {
		t.Fatalf("expected idle_in_transaction_session_timeout=30000 in DSN options, got: %s", got)
	}
}

func TestOpenInvalidDSN(t *testing.T) {
	d := &driver{}
	// Use a clearly unreachable host. Open itself should succeed (pgx defers
	// the actual TCP dial), but gorm.Open will fail because it pings.
	_, _, err := d.Open(context.Background(), dao.Config{
		Addr:               "192.0.2.1:1", // RFC 5737 TEST-NET, guaranteed unreachable
		User:               "nobody",
		Pwd:                "x",
		DBName:             "nonexistent",
		ConnTimeoutSeconds: 1,
	})
	if err == nil {
		t.Fatal("expected Open to fail with unreachable host, got nil")
	}
}
