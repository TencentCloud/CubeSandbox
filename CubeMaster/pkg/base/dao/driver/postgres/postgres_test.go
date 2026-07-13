// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

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
	parsed, err := pgx.ParseConfig(buildDSN(cfg))
	if err != nil {
		t.Fatalf("pgx.ParseConfig: %v", err)
	}
	if parsed.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1", parsed.Host)
	}
	if parsed.Port != 5432 {
		t.Fatalf("Port = %d, want 5432", parsed.Port)
	}
	if parsed.User != "cube" || parsed.Password != "cube_pass" || parsed.Database != "cube_mvp" {
		t.Fatalf("user/pass/db = %q/%q/%q", parsed.User, parsed.Password, parsed.Database)
	}
	if parsed.ConnectTimeout != 10*time.Second {
		t.Fatalf("ConnectTimeout = %v, want 10s", parsed.ConnectTimeout)
	}
}

func TestBuildDSNDefaultTimeout(t *testing.T) {
	cfg := dao.Config{
		Addr:   "localhost",
		User:   "u",
		Pwd:    "p",
		DBName: "db",
	}
	parsed, err := pgx.ParseConfig(buildDSN(cfg))
	if err != nil {
		t.Fatalf("pgx.ParseConfig: %v", err)
	}
	if parsed.Host != "localhost" {
		t.Fatalf("Host = %q, want localhost", parsed.Host)
	}
	if parsed.ConnectTimeout.Seconds() != 5 {
		t.Fatalf("ConnectTimeout = %v, want 5s", parsed.ConnectTimeout)
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
	base := dao.Config{
		Addr:               "localhost",
		User:               "u",
		Pwd:                "p",
		DBName:             "db",
		ConnTimeoutSeconds: 5,
	}

	t.Run("max of read and write", func(t *testing.T) {
		cfg := base
		cfg.ReadTimeoutSeconds = 30
		cfg.WriteTimeoutSeconds = 10
		parsed, err := pgx.ParseConfig(buildDSN(cfg))
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		opts := parsed.RuntimeParams["options"]
		if !strings.Contains(opts, "statement_timeout=30000") {
			t.Fatalf("expected statement_timeout=30000 in options %q", opts)
		}
		if strings.Contains(opts, "idle_in_transaction_session_timeout") {
			t.Fatalf("ReadTimeout must not map to idle_in_transaction, options=%q", opts)
		}
	})

	t.Run("read only", func(t *testing.T) {
		cfg := base
		cfg.ReadTimeoutSeconds = 30
		parsed, err := pgx.ParseConfig(buildDSN(cfg))
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		if !strings.Contains(parsed.RuntimeParams["options"], "statement_timeout=30000") {
			t.Fatalf("options=%q", parsed.RuntimeParams["options"])
		}
	})

	t.Run("write only", func(t *testing.T) {
		cfg := base
		cfg.WriteTimeoutSeconds = 10
		parsed, err := pgx.ParseConfig(buildDSN(cfg))
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		if !strings.Contains(parsed.RuntimeParams["options"], "statement_timeout=10000") {
			t.Fatalf("options=%q", parsed.RuntimeParams["options"])
		}
	})

	t.Run("extra idle opt-in", func(t *testing.T) {
		cfg := base
		cfg.WriteTimeoutSeconds = 10
		cfg.Extra = map[string]string{"idle_in_transaction_session_timeout": "60000"}
		parsed, err := pgx.ParseConfig(buildDSN(cfg))
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		opts := parsed.RuntimeParams["options"]
		if !strings.Contains(opts, "statement_timeout=10000") {
			t.Fatalf("options=%q", opts)
		}
		if !strings.Contains(opts, "idle_in_transaction_session_timeout=60000") {
			t.Fatalf("options=%q", opts)
		}
	})

	t.Run("extra statement_timeout overrides max", func(t *testing.T) {
		cfg := base
		cfg.ReadTimeoutSeconds = 30
		cfg.WriteTimeoutSeconds = 10
		cfg.Extra = map[string]string{"statement_timeout": "45000"}
		parsed, err := pgx.ParseConfig(buildDSN(cfg))
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		opts := parsed.RuntimeParams["options"]
		if !strings.Contains(opts, "statement_timeout=45000") {
			t.Fatalf("options=%q", opts)
		}
		if strings.Contains(opts, "statement_timeout=30000") {
			t.Fatalf("max-derived timeout should be overridden, options=%q", opts)
		}
	})
}

func TestBuildDSNSpecialPasswords(t *testing.T) {
	// Regression: unquoted keyword=value DSN silently truncated passwords with spaces.
	passwords := []string{
		"cube_pass",
		"my pass",
		"my pass'secure",
		`a\b`,
		"a=b",
		"p@ss:word",
		"pct%25val",
	}
	for _, pwd := range passwords {
		t.Run(pwd, func(t *testing.T) {
			cfg := dao.Config{
				Addr:               "127.0.0.1:5432",
				User:               "cube",
				Pwd:                pwd,
				DBName:             "cube_mvp",
				ConnTimeoutSeconds: 5,
			}
			parsed, err := pgx.ParseConfig(buildDSN(cfg))
			if err != nil {
				t.Fatalf("ParseConfig: %v (dsn=%s)", err, buildDSN(cfg))
			}
			if parsed.Password != pwd {
				t.Fatalf("Password = %q, want %q (dsn=%s)", parsed.Password, pwd, buildDSN(cfg))
			}
			if parsed.User != "cube" || parsed.Database != "cube_mvp" {
				t.Fatalf("user/db corrupted: %q / %q", parsed.User, parsed.Database)
			}
		})
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
