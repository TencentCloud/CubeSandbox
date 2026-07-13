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

// buildDSN maps dao.Config to a libpq key=value DSN.
// Addr is host:port (SplitHostPort); bare host omits port= (libpq default 5432).
// sslmode from Extra["sslmode"], default "disable".
// WriteTimeoutSeconds → statement_timeout; ReadTimeoutSeconds → idle_in_transaction_session_timeout (ms).
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
	var hostPart string
	if err == nil {
		hostPart = fmt.Sprintf("host=%s port=%s", host, port)
	} else {
		hostPart = fmt.Sprintf("host=%s", cfg.Addr)
	}

	dsn := fmt.Sprintf(
		"%s user=%s password=%s dbname=%s sslmode=%s connect_timeout=%d",
		hostPart, cfg.User, cfg.Pwd, cfg.DBName, sslmode, connTimeout,
	)

	var options []string
	if cfg.WriteTimeoutSeconds > 0 {
		options = append(options, fmt.Sprintf("-c statement_timeout=%d", cfg.WriteTimeoutSeconds*1000))
	}
	if cfg.ReadTimeoutSeconds > 0 {
		options = append(options, fmt.Sprintf("-c idle_in_transaction_session_timeout=%d", cfg.ReadTimeoutSeconds*1000))
	}
	if len(options) > 0 {
		dsn += fmt.Sprintf(" options='%s'", strings.Join(options, " "))
	}

	return dsn
}
