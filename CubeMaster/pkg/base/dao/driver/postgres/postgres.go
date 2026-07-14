// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package postgres plugs the PostgreSQL engine into pkg/base/dao.
// Blank-import it from main.go so the driver registers with the dao registry.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver: "pgx"
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const (
	// DriverName is also the migrations sub-directory under migrate/migrations.
	DriverName = "postgres"

	// Outer goose.Up advisory lock id; MUST stay stable across versions or
	// old/new instances can both acquire the lock.
	advisoryLockID = 3764529487

	defaultLockTimeoutSeconds = 60
)

func init() {
	dao.Register(&driver{})
}

type driver struct{}

func (d *driver) Name() string { return DriverName }

func (d *driver) Open(ctx context.Context, cfg dao.Config) (*sql.DB, *gorm.DB, error) {
	_ = ctx
	dsn := buildDSN(cfg)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("sql.Open: %w", err)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxConnLifeTimeSeconds > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.MaxConnLifeTimeSeconds) * time.Second)
	}
	gormDB, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("gorm.Open: %w", err)
	}
	return sqlDB, gormDB, nil
}

func (d *driver) SessionLocker(cfg dao.Config) lock.SessionLocker {
	timeout := cfg.MigrationLockTimeoutSeconds
	if timeout <= 0 {
		timeout = defaultLockTimeoutSeconds
	}
	return &sessionLocker{
		id:      advisoryLockID,
		timeout: timeout,
	}
}

// buildDSN maps dao.Config to a postgres:// URI. url.UserPassword percent-encodes
// credentials so passwords with spaces/quotes/=/@ do not break libpq parsing.
//
// Addr is host:port (SplitHostPort); bare host omits port (libpq default 5432).
// sslmode from Extra["sslmode"], default "disable".
//
// PG has no client read/write I/O timeouts like MySQL. The shared yaml fields
// approximate a server-side statement budget:
// statement_timeout = max(ReadTimeoutSeconds, WriteTimeoutSeconds) ms, unless
// Extra["statement_timeout"] overrides (milliseconds). Transaction-idle killing
// must be opted in via Extra["idle_in_transaction_session_timeout"] (ms) — never
// derived from ReadTimeoutSeconds.
func buildDSN(cfg dao.Config) string {
	connTimeout := cfg.ConnTimeoutSeconds
	if connTimeout <= 0 {
		connTimeout = 5
	}

	sslmode := "disable"
	if v, ok := cfg.Extra["sslmode"]; ok && v != "" {
		sslmode = v
	}

	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		host = cfg.Addr
		port = ""
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Pwd),
		Path:   "/" + cfg.DBName,
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}

	q := url.Values{}
	q.Set("sslmode", sslmode)
	q.Set("connect_timeout", strconv.Itoa(connTimeout))

	var options []string
	if stmt := statementTimeoutMillis(cfg); stmt != "" {
		options = append(options, "-c statement_timeout="+stmt)
	}
	if v, ok := cfg.Extra["idle_in_transaction_session_timeout"]; ok && v != "" {
		options = append(options, "-c idle_in_transaction_session_timeout="+v)
	}
	if len(options) > 0 {
		q.Set("options", strings.Join(options, " "))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// statementTimeoutMillis returns the statement_timeout value in milliseconds,
// or "" when unset. Extra["statement_timeout"] wins over max(read, write).
func statementTimeoutMillis(cfg dao.Config) string {
	if v, ok := cfg.Extra["statement_timeout"]; ok && v != "" {
		return v
	}
	sec := cfg.ReadTimeoutSeconds
	if cfg.WriteTimeoutSeconds > sec {
		sec = cfg.WriteTimeoutSeconds
	}
	if sec <= 0 {
		return ""
	}
	return strconv.Itoa(sec * 1000)
}
