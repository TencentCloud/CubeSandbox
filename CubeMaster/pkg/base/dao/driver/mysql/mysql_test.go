// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package mysql

import (
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
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

	defaultLocker, ok := d.SessionLocker(dao.Config{}).(*sessionLocker)
	if !ok {
		t.Fatalf("SessionLocker type = %T, want *sessionLocker", d.SessionLocker(dao.Config{}))
	}
	if defaultLocker.timeout != defaultLockTimeoutSeconds {
		t.Fatalf("default timeout = %d, want %d", defaultLocker.timeout, defaultLockTimeoutSeconds)
	}
}

func TestBuildDSNDefaults(t *testing.T) {
	cfg := dao.Config{
		Addr:   "127.0.0.1:3306",
		User:   "cube",
		Pwd:    "cube_pass",
		DBName: "cube_mvp",
	}
	parsed, err := mysqldriver.ParseDSN(buildDSN(cfg))
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if parsed.User != "cube" || parsed.Passwd != "cube_pass" || parsed.DBName != "cube_mvp" {
		t.Fatalf("identity = %q/%q/%q", parsed.User, parsed.Passwd, parsed.DBName)
	}
	if parsed.Addr != "127.0.0.1:3306" || parsed.Net != "tcp" {
		t.Fatalf("net/addr = %q/%q", parsed.Net, parsed.Addr)
	}
	if parsed.Timeout != 5*time.Second || parsed.ReadTimeout != 5*time.Second || parsed.WriteTimeout != 5*time.Second {
		t.Fatalf("timeouts = %v/%v/%v", parsed.Timeout, parsed.ReadTimeout, parsed.WriteTimeout)
	}
	if !parsed.ParseTime {
		t.Fatal("ParseTime want true")
	}
	dsn := buildDSN(cfg)
	if !strings.Contains(dsn, "charset=utf8") {
		t.Fatalf("expected charset=utf8 in DSN, got %s", dsn)
	}
}

func TestBuildDSNConfiguredTimeouts(t *testing.T) {
	cfg := dao.Config{
		Addr:                "127.0.0.1:3306",
		User:                "cube",
		Pwd:                 "cube_pass",
		DBName:              "cube_mvp",
		ConnTimeoutSeconds:  3,
		ReadTimeoutSeconds:  7,
		WriteTimeoutSeconds: 9,
	}
	parsed, err := mysqldriver.ParseDSN(buildDSN(cfg))
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if parsed.Timeout != 3*time.Second || parsed.ReadTimeout != 7*time.Second || parsed.WriteTimeout != 9*time.Second {
		t.Fatalf("timeouts = %v/%v/%v", parsed.Timeout, parsed.ReadTimeout, parsed.WriteTimeout)
	}
}

func TestBuildDSNSpecialCredentials(t *testing.T) {
	cases := []struct {
		name string
		user string
		pwd  string
		db   string
	}{
		{name: "plain", user: "cube", pwd: "cube_pass", db: "cube_mvp"},
		{name: "space_in_password", user: "cube", pwd: "my pass", db: "cube_mvp"},
		{name: "at_and_colon_in_password", user: "cube", pwd: "p@ss:word", db: "cube_mvp"},
		{name: "slash_in_password", user: "cube", pwd: "x/y", db: "cube_mvp"},
		{name: "question_and_percent", user: "cube", pwd: "a%b?c", db: "cube_mvp"},
		{name: "at_in_user", user: "u@me", pwd: "p:w", db: "cube_mvp"},
		{name: "space_in_dbname", user: "cube", pwd: "pass", db: "cube db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := dao.Config{
				Addr:   "127.0.0.1:3306",
				User:   tc.user,
				Pwd:    tc.pwd,
				DBName: tc.db,
			}
			dsn := buildDSN(cfg)
			parsed, err := mysqldriver.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("ParseDSN: %v (dsn=%s)", err, dsn)
			}
			if parsed.User != tc.user {
				t.Fatalf("User = %q, want %q", parsed.User, tc.user)
			}
			if parsed.Passwd != tc.pwd {
				t.Fatalf("Passwd = %q, want %q", parsed.Passwd, tc.pwd)
			}
			if parsed.DBName != tc.db {
				t.Fatalf("DBName = %q, want %q", parsed.DBName, tc.db)
			}
			if parsed.Addr != "127.0.0.1:3306" {
				t.Fatalf("Addr = %q", parsed.Addr)
			}
		})
	}
}
